// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package acmetest_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
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
			t.Fatalf("the run ended %s; last error: %s", run.Outcome, st.ACME.LastError)
		}
		return false, "outcome " + run.Outcome
	})
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
