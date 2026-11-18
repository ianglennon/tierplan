// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingresscert

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// Phase 20 (D-34, D-44; CRT-01, CRT-04): the per-hostname path against a real ACME server. Pebble issues and
// renews a certificate for a claimed hostname over DNS-01; Pebble resolves the challenge through
// pebble-challtestsrv; TierPlan writes the TXT record through its own Cloudflare client to a fake Cloudflare
// API, and the fake tells the challenge test server what it holds. Pebble runs on real time, so these tests run
// on the real clock; backoff, the governor, grace and the rest run on the fake CA and the injected clock.

const (
	// pebbleSuffix is the domain registered for the install; the fake Cloudflare holds it as a zone.
	pebbleSuffix = "ingress.test"
	pebbleHost   = "app." + pebbleSuffix
	// pebbleProfile is named by every order: Pebble picks a profile at random for an order that names none.
	pebbleProfile = "default"
	// pebbleRunDeadline bounds one run against Pebble: the order, the validation, the finalize and the store.
	// It decides only how long a broken run takes to fail.
	pebbleRunDeadline = 3 * time.Minute
)

// pebbleRig is a manager over a real Pebble, with one domain, one stack and the fake Cloudflare.
type pebbleRig struct {
	t     *testing.T
	s     *Store
	dom   *ingressstore.Store
	fake  *dnstest.FakeCloudflare
	chall *acmetest.ChallTestSrv
	m     *Manager
	ended chan endEvent
}

// newPebbleRig starts the challenge test server, Pebble on its network, and a manager whose both issuers are
// Pebble, with the host's claim in place and the terms acknowledged. The first round has not run.
func newPebbleRig(t *testing.T) *pebbleRig {
	t.Helper()
	ctx := context.Background()
	chall := acmetest.StartChallTestSrv(t)
	pebble := acmetest.StartPebble(t, acmetest.PebbleOptions{DNSServer: chall.InNetworkDNS(), Network: chall.Network()})

	fake := dnstest.NewFakeCloudflare(t)
	// *ChallTestSrv is a dnstest.TXTSink: what the fake holds is what Pebble sees at the challenge name.
	fake.Sink = chall
	fake.AddZone(pebbleSuffix, "ns1.cf.test", "ns2.cf.test")

	st := storetest.New(t)
	log := slog.New(slog.DiscardHandler)
	v, err := vault.LoadOrCreate("", t.TempDir(), log)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	seedEnv(t, st, "env-1", "one")
	if _, err := st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-1', 'env-1', 'web', 'standalone', 'tester', 1)`); err != nil {
		t.Fatalf("seed the stack: %v", err)
	}
	s := NewStore(StoreDeps{St: st, Vault: v, Audit: audit.NewWriter(st, v, log, time.Now)})
	notifier := ingress.NewNotifier()
	dom := ingressstore.New(ingressstore.Deps{St: st, Vault: v, Now: time.Now, Notifier: notifier})
	if _, err := dom.CreateDomain(ctx, ingressstore.CreateDomainInput{
		EnvID: "env-1", Suffix: pebbleSuffix, Issuer: ingress.IssuerProduction, Token: []byte(fake.Token), Actor: "user:1",
	}, nil); err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if _, err := dom.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
		EnvID: "env-1", StackID: "stack-1", Actor: "user:1",
		Decls: []ingress.Declaration{{Service: "web", Name: "a", Host: pebbleHost, Label: "tierplan.ingress.host", Port: 80}},
	}); err != nil {
		t.Fatalf("claim %s: %v", pebbleHost, err)
	}

	nameservers := func(context.Context, string) ([]string, error) { return []string{chall.DNSAddr()}, nil }
	resolvers := func() ([]string, error) { return []string{chall.DNSAddr()}, nil }
	g := &pebbleRig{t: t, s: s, dom: dom, fake: fake, chall: chall, ended: make(chan endEvent, 16)}
	g.m, err = New(Deps{
		Store: s, Domains: dom, Notifier: notifier, Jobs: jobsHub(t),
		// Both issuers map to Pebble, and a name under .test passes CheckName because the directory is overridden.
		Config: config.Config{ACMEDirectoryURL: pebble.DirectoryURL(), ACMECAFile: pebble.CAFile()},
		DNS: acme.DNSDeps{
			Providers:       map[string]dns.Factory{ingress.ProviderCloudflare: cloudflare.New(cloudflare.Options{BaseURL: fake.URL(), HTTPClient: fake.Client()})},
			NameserverAddrs: nameservers, ResolvConf: resolvers,
		},
		Ledger: acme.NewStore(st, v), Log: log, UserAgent: "tierplan-test/0", Profile: pebbleProfile,
		RunEnded: func(id, outcome string) { g.ended <- endEvent{id, outcome} },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	if err := g.m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return g
}

// jobsHub is a job hub on the real clock, closed when the test ends.
func jobsHub(t *testing.T) *jobs.Hub {
	t.Helper()
	hub := jobs.New(jobs.Options{Now: time.Now})
	t.Cleanup(hub.Close)
	return hub
}

// roundAndAwait runs one round, waits for the run it starts to end with the outcome want, and returns the live row.
func (g *pebbleRig) roundAndAwait(want string) Cert {
	g.t.Helper()
	g.m.round(context.Background())
	var end endEvent
	select {
	case end = <-g.ended:
	// wall-clock: Pebble validates and issues in real time, so the wait is a failure bound, not a delay
	case <-time.After(pebbleRunDeadline):
		g.t.Fatalf("no certificate run ended within %v", pebbleRunDeadline)
	}
	if end.outcome != want {
		row, _, _ := g.s.LiveCert(context.Background(), pebbleHost)
		g.t.Fatalf("the run ended %q, want %q; row: phase %q, last error %s: %s", end.outcome, want, row.Phase, row.LastErrorClass, row.LastErrorDetail)
	}
	return g.live()
}

// live is the live row of the claimed host.
func (g *pebbleRig) live() Cert {
	g.t.Helper()
	c, found, err := g.s.LiveCert(context.Background(), pebbleHost)
	if err != nil || !found {
		g.t.Fatalf("LiveCert(%s) = found %v, %v", pebbleHost, found, err)
	}
	return c
}

// requireNoTXT asserts that what a run promises is true: the fake Cloudflare holds no record, and the challenge
// test server serves none at the challenge name.
func (g *pebbleRig) requireNoTXT() {
	g.t.Helper()
	if recs := g.fake.Records(); len(recs) != 0 {
		g.t.Errorf("the fake Cloudflare still holds %d record(s): %+v", len(recs), recs)
	}
}

// requireIssued asserts what a stored certificate of the claimed host has to be.
func (g *pebbleRig) requireIssued(c Cert) {
	g.t.Helper()
	if c.Phase != PhaseIdle || len(c.ChainPEM) == 0 || c.Serial == "" {
		g.t.Fatalf("row = phase %q serial %q chain %d bytes, want idle with a chain", c.Phase, c.Serial, len(c.ChainPEM))
	}
	leaf, err := leafOf(c.ChainPEM)
	if err != nil {
		g.t.Fatalf("the stored chain: %v", err)
	}
	if err := leaf.VerifyHostname(pebbleHost); err != nil {
		g.t.Errorf("the certificate is not for %s: %v", pebbleHost, err)
	}
	if c.KeySealed == "" {
		g.t.Errorf("the row holds no sealed key")
	}
	if _, err := g.s.OpenKey(c); err != nil {
		g.t.Errorf("OpenKey: %v", err)
	}
}

// TestPebbleIssuesAPerHostnameCertificate is the tracer (D-34, D-44, CRT-01): a claimed hostname under a .test
// domain is issued a DNS-01 certificate by the manager with the domain's token, stored sealed, and no TXT
// record is left at the fake Cloudflare.
func TestPebbleIssuesAPerHostnameCertificate(t *testing.T) {
	g := newPebbleRig(t)

	row := g.roundAndAwait(OutcomeIssued)

	g.requireIssued(row)
	g.requireNoTXT()
	if n := countRows(t, g, `SELECT COUNT(*) FROM ingress_cert_issuances WHERE kind = 'new'`); n != 1 {
		t.Errorf("%d new issuances, want 1", n)
	}
	var created bool
	for _, r := range g.fake.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/dns_records") {
			created = true
		}
	}
	if !created {
		t.Errorf("the fake Cloudflare saw no record created: %+v", g.fake.Requests())
	}
}

// countRows runs a count query on the rig's database.
func countRows(t *testing.T, g *pebbleRig, query string, args ...any) int {
	t.Helper()
	var n int
	if err := g.s.st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
