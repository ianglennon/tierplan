// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package acmetest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/server/servertest"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

const (
	// publicName is the name the certificate is ordered for. Pebble's container resolves it to the host
	// (the engine's host-gateway), and the test client resolves it to the server's address.
	publicName = "tierplan.test"
	// profile is named by every order: Pebble picks one at random for an order that names none.
	profile = "default"
	actor   = "user:test"
	// runDeadline bounds a whole run against Pebble: the order, the validation, the finalize and the
	// swap. It decides only how long a broken run takes to fail.
	runDeadline = 3 * time.Minute
)

// pebbleServer is a TierPlan server that orders from a Pebble container, and the pieces around it.
type pebbleServer struct {
	s      *servertest.Server
	mgr    *acme.Manager
	pebble *acmetest.Pebble
	roots  []*x509.Certificate
}

// startPebbleServer runs Pebble, a relay in front of the server's port for Pebble's HTTP-01 check, and
// a real TierPlan server whose public name is publicName and whose ACME client trusts only the test CA
// that signed Pebble's HTTPS certificate.
func startPebbleServer(t *testing.T) *pebbleServer {
	t.Helper()
	relay := acmetest.NewRelay(t)
	pebble := acmetest.StartPebble(t, acmetest.PebbleOptions{HTTPPort: relay.Port(), Hosts: []string{publicName}})
	s := servertest.Start(t, servertest.Options{
		Host: "127.0.0.1", PublicHost: publicName,
		Env: map[string]string{
			"TIERPLAN_ACME_DIRECTORY_URL": pebble.DirectoryURL(),
			"TIERPLAN_ACME_CA_FILE":       pebble.CAFile(),
		},
	})
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Pebble's HTTP-01 check arrives on the relay's port, and goes to the server's one port, where the
	// listener tells a plain request from a TLS handshake.
	relay.SetTarget(net.JoinHostPort("127.0.0.1", u.Port()))
	roots := pebble.IssuerRoots(t)
	s.TrustCertificates(roots...)
	return &pebbleServer{s: s, mgr: s.Core().ACME(), pebble: pebble, roots: roots}
}

// register creates the account and saves the profile, as the Settings page does before an order.
func (p *pebbleServer) register(t *testing.T) {
	t.Helper()
	ctx := testsupport.Context(t)
	if _, err := p.mgr.Register(ctx, "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := p.mgr.SetProfile(ctx, profile); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
}

// acmeStatus is the part of GET /api/https/status these tests read.
type acmeStatus struct {
	Certificate struct {
		Source string `json:"source"`
		TestCA bool   `json:"testCA"`
	} `json:"certificate"`
	ACME struct {
		LastRun *struct {
			JobID   string `json:"jobId"`
			Kind    string `json:"kind"`
			Outcome string `json:"outcome"`
		} `json:"lastRun"`
		LastError json.RawMessage `json:"lastError"`
	} `json:"acme"`
}

func (p *pebbleServer) status(t *testing.T) acmeStatus {
	t.Helper()
	code, body := p.s.Do(http.MethodGet, "/api/https/status", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/https/status: %d %s", code, body)
	}
	var st acmeStatus
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode the status: %v: %s", err, body)
	}
	return st
}

// awaitRun waits until the run of job has ended and fails the test unless it succeeded, showing the
// last error the status holds.
func (p *pebbleServer) awaitRun(t *testing.T, job string) {
	t.Helper()
	// testsupport: external, Pebble and the job hub bring the run to its end
	testsupport.Poll(t, runDeadline, "the Let's Encrypt run "+job+" to end", func() (bool, string) {
		st := p.status(t)
		run := st.ACME.LastRun
		if run == nil || run.JobID != job {
			return false, "the last run is not this one yet"
		}
		switch run.Outcome {
		case acme.OutcomeSucceeded:
			return true, ""
		case acme.OutcomeFailed, acme.OutcomeCancelled:
			t.Fatalf("the run ended %s; last error: %s; runs: %s", run.Outcome, st.ACME.LastError, p.runHistory(t))
		}
		return false, "outcome " + run.Outcome
	})
}

// runHistory is the run table and the open orders as text, for a failure message.
func (p *pebbleServer) runHistory(t *testing.T) string {
	t.Helper()
	ctx := testsupport.Context(t)
	rows, err := p.s.Core().Store().Read.QueryContext(ctx,
		`SELECT id, kind, outcome, error_class FROM acme_runs ORDER BY started_at`)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer func() { _ = rows.Close() }()
	var out string
	for rows.Next() {
		var id, kind, outcome, class string
		if err := rows.Scan(&id, &kind, &outcome, &class); err != nil {
			return out + "unreadable: " + err.Error()
		}
		out += "[" + id + " " + kind + " " + outcome + " " + class + "]"
	}
	return out
}

// presented is the certificate the server presents to a new handshake for publicName, verified against
// Pebble's roots.
func (p *pebbleServer) presented(t *testing.T) *x509.Certificate {
	t.Helper()
	pool := x509.NewCertPool()
	for _, c := range p.roots {
		pool.AddCert(c)
	}
	u, err := url.Parse(p.s.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: publicName}}
	conn, err := d.DialContext(testsupport.Context(t), "tcp", net.JoinHostPort("127.0.0.1", u.Port()))
	if err != nil {
		t.Fatalf("handshake with the server as %s, verified against Pebble's root: %v", publicName, err)
	}
	defer func() { _ = conn.Close() }()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("the connection is a %T, not a TLS connection", conn)
	}
	chains := tc.ConnectionState().VerifiedChains
	if len(chains) == 0 {
		t.Fatal("the presented certificate has no verified chain")
	}
	leaf := chains[0][0]
	if err := leaf.VerifyHostname(publicName); err != nil {
		t.Fatalf("the presented certificate is not for %s: %v", publicName, err)
	}
	return leaf
}

// issue registers, orders and waits until the certificate is the one served. It returns that
// certificate.
func (p *pebbleServer) issue(t *testing.T) *x509.Certificate {
	t.Helper()
	p.register(t)
	start, err := p.mgr.Issue(testsupport.Context(t), actor)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if start.Reused || start.JobID == "" {
		t.Fatalf("Issue = %+v; want a new run", start)
	}
	p.awaitRun(t, start.JobID)
	return p.presented(t)
}

// TestPebbleIssueOverHTTP01ThroughTheListener is the conformance test of the whole issue path: a real
// server orders from Pebble, Pebble checks the challenge with a plain HTTP request that arrives at the
// server's one port through the container's host gateway and a relay, and the next handshake presents
// the certificate Pebble issued.
func TestPebbleIssueOverHTTP01ThroughTheListener(t *testing.T) {
	p := startPebbleServer(t)
	leaf := p.issue(t)

	if leaf.Issuer.CommonName == leaf.Subject.CommonName && leaf.Subject.CommonName != "" {
		t.Errorf("the presented certificate is self-signed: %s", leaf.Subject)
	}
	st := p.status(t)
	if st.Certificate.Source != "lets_encrypt" || !st.Certificate.TestCA {
		t.Errorf("certificate source %q testCA %v; want lets_encrypt and true", st.Certificate.Source, st.Certificate.TestCA)
	}
}

// cutSolver is the solver of a process that dies mid-order: the tokens it presented are not withdrawn,
// because a process that died withdraws nothing. Pebble checks a submitted challenge on its own time, so
// withdrawing them when the run is cut would race with that check.
type cutSolver struct{ *acme.Challenges }

func (cutSolver) CleanUp(context.Context, string, string, string) error { return nil }

// cutAt is a step reporter that cancels the run's context when step starts.
type cutAt struct {
	step   string
	cancel context.CancelFunc
}

func (c cutAt) Running(step, _ string) {
	if step == c.step {
		c.cancel()
	}
}
func (cutAt) Done(string)            {}
func (cutAt) Skipped(string, string) {}
func (cutAt) Failed(string, error)   {}

// TestPebbleResumeAfterACut: a run cut short after its challenge was submitted is picked up by a new
// Orderer over the same store, with the same order URL and no second order row.
func TestPebbleResumeAfterACut(t *testing.T) {
	ctx := testsupport.Context(t)

	// The token handler is the relay's target: a plain HTTP server, as the sniffing listener hands it.
	tokens := acme.NewChallenges()
	web := httptest.NewServer(tokens.Handler())
	t.Cleanup(web.Close)
	relay := acmetest.NewRelay(t)
	relay.SetTarget(web.Listener.Addr().String())
	pebble := acmetest.StartPebble(t, acmetest.PebbleOptions{HTTPPort: relay.Port(), Hosts: []string{publicName}})

	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	db := storetest.New(t)
	store := acme.NewStore(db, v)
	dir, err := acme.NewDirectory(config.Config{ACMEDirectoryURL: pebble.DirectoryURL(), ACMECAFile: pebble.CAFile()})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	client := acme.NewClient(dir, store, stackops.RealClock(), "tierplan-test/0")
	if _, err := client.Register(ctx, "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}
	req := acme.OrderRequest{Kind: acme.KindIssue, Names: []string{publicName}, Profile: profile}
	solver := cutSolver{tokens}

	cutCtx, cut := context.WithCancel(ctx)
	defer cut()
	_, err = acme.NewOrderer(client, store, solver, stackops.RealClock()).Run(cutCtx, req, cutAt{step: acme.StepValidate, cancel: cut})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the cut run returned %v; want the context's cancellation", err)
	}
	open, _, found, err := store.OpenOrder(ctx)
	if err != nil || !found {
		t.Fatalf("OpenOrder after the cut = found %v, %v; want the order kept open", found, err)
	}
	if open.Stage != acme.StageChallengeSubmitted {
		t.Errorf("the open order is at stage %q; want %q", open.Stage, acme.StageChallengeSubmitted)
	}

	// What a restarted process builds: a new Orderer over the same store and the same answers.
	issued, err := acme.NewOrderer(client, store, solver, stackops.RealClock()).Run(ctx, req, nil)
	if err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if err := issued.Leaf.VerifyHostname(publicName); err != nil {
		t.Errorf("the resumed run issued a certificate not for %s: %v", publicName, err)
	}

	rows, err := db.Read.QueryContext(ctx, `SELECT order_url FROM acme_orders`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		urls = append(urls, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != open.URL {
		t.Errorf("order rows after the resume = %v; want exactly the order URL from before the cut, %s", urls, open.URL)
	}
	if _, _, found, err := store.OpenOrder(ctx); found || err != nil {
		t.Errorf("OpenOrder after the resume = found %v, %v; want the order closed", found, err)
	}
}

// TestPebbleRenewNowReplaces: Renew now is a replacement order Pebble accepts, and the new certificate
// is the one served.
func TestPebbleRenewNowReplaces(t *testing.T) {
	p := startPebbleServer(t)
	first := p.issue(t)

	start, err := p.mgr.RenewNow(testsupport.Context(t), actor)
	if err != nil {
		t.Fatalf("RenewNow: %v", err)
	}
	p.awaitRun(t, start.JobID)

	second := p.presented(t)
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Errorf("the served certificate still has serial %s after Renew now", first.SerialNumber)
	}
	if err := second.VerifyHostname(publicName); err != nil {
		t.Errorf("the renewed certificate is not for %s: %v", publicName, err)
	}
	if run := p.status(t).ACME.LastRun; run == nil || run.Kind != acme.KindRenew {
		t.Errorf("the last run = %+v; want a renewal", run)
	}
	var replaces string
	row := p.s.Core().Store().Read.QueryRowContext(testsupport.Context(t),
		`SELECT replaces FROM acme_orders WHERE kind = ? ORDER BY created_at DESC LIMIT 1`, acme.KindRenew)
	if err := row.Scan(&replaces); err != nil {
		t.Fatalf("read the renewal order: %v", err)
	}
	if replaces == "" {
		t.Error("the renewal order names no certificate it replaces")
	}
}
