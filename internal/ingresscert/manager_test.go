// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// endEvent is one RunEnded call.
type endEvent struct{ certID, outcome string }

// tickClock is the clock a manager runs on in the tests that drive Run: it passes everything to the rig's clock and
// tells the test each time the manager arms its Tick timer, which is how a test learns that Run is parked.
type tickClock struct {
	*acmetest.Clock
	ticks chan struct{}
	armed atomic.Int64
}

func (c *tickClock) After(d time.Duration) <-chan time.Time {
	ch := c.Clock.After(d)
	if d == Tick {
		c.armed.Add(1)
		c.ticks <- struct{}{}
	}
	return ch
}

// mgrRig is the issue rig with an ingress store over the same database, one registered domain
// (example.com, production, the rig's token), one stack and a job hub, everything a Manager runs against.
type mgrRig struct {
	*issueRig
	dom      *ingressstore.Store
	notifier *ingress.Notifier
	hub      *jobs.Hub
	domain   ingress.Domain
	ended    chan endEvent
	clk      *tickClock
}

// newMgrRig builds the rig. profiles, when set, replaces the fake CA with one that offers those profiles and
// issues the default one for an order that names none.
func newMgrRig(t *testing.T, profiles map[string]time.Duration, defaultProfile string) *mgrRig {
	t.Helper()
	r := newIssueRig(t)
	if profiles != nil {
		r.fake = acmetest.NewFake(t, acmetest.Options{
			Clock: r.clock, ChallengeType: acme.ChallengeDNS01, Profiles: profiles, DefaultProfile: defaultProfile,
			LookupTXT: func(fqdn string) []string {
				var out []string
				for _, rec := range r.prov.Records() {
					if strings.EqualFold(rec.Name, fqdn) {
						out = append(out, rec.Value())
					}
				}
				return out
			},
		})
	}
	g := &mgrRig{issueRig: r, notifier: ingress.NewNotifier(), ended: make(chan endEvent, 64)}
	g.clk = &tickClock{Clock: r.clock, ticks: make(chan struct{}, 64)}
	g.dom = ingressstore.New(ingressstore.Deps{St: r.st, Vault: r.v, Now: r.clock.Now, Notifier: g.notifier})
	var err error
	g.domain, err = g.dom.CreateDomain(context.Background(), ingressstore.CreateDomainInput{
		EnvID: "env-1", Suffix: "example.com", Issuer: ingress.IssuerProduction, Token: []byte(rigToken), Actor: "user:1",
	}, nil)
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if _, err := r.st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-1', 'env-1', 'web', 'standalone', 'tester', 1)`); err != nil {
		t.Fatalf("seed the stack: %v", err)
	}
	g.hub = jobs.New(jobs.Options{Now: r.clock.Now})
	t.Cleanup(g.hub.Close)
	return g
}

// claim makes stack-1 claim exactly these hostnames, as a deploy does.
func (g *mgrRig) claim(hosts ...string) {
	g.t.Helper()
	var decls []ingress.Declaration
	for i, h := range hosts {
		decls = append(decls, ingress.Declaration{Service: "web", Name: string(rune('a' + i)), Host: h, Label: "tierplan.ingress.host", Port: 80})
	}
	if _, err := g.dom.ReplaceStackRoutes(context.Background(), ingressstore.ReplaceInput{
		EnvID: "env-1", StackID: "stack-1", Actor: "user:1", Decls: decls,
	}); err != nil {
		g.t.Fatalf("claim %v: %v", hosts, err)
	}
}

// newManager is a Manager over the rig; mod may change the Deps before it is built.
func (g *mgrRig) newManager(mod ...func(*Deps)) *Manager {
	g.t.Helper()
	d := Deps{
		Store: g.s, Domains: g.dom, Notifier: g.notifier, Jobs: g.hub, Clock: g.clk,
		Config: config.Config{ACMEDirectoryURL: g.fake.DirectoryURL(), ACMECAFile: g.fake.CAFile(g.t)},
		DNS: acme.DNSDeps{
			Providers:       map[string]dns.Factory{ingress.ProviderCloudflare: g.prov.Factory(rigToken)},
			NameserverAddrs: g.engineNameservers, ResolvConf: g.engineResolvers,
		},
		Ledger: g.ledger, Log: slog.New(slog.DiscardHandler), Jitter: func() float64 { return 0 }, UserAgent: "tierplan-test/0",
		RunEnded: func(id, outcome string) { g.ended <- endEvent{id, outcome} },
	}
	for _, f := range mod {
		f(&d)
	}
	m, err := New(d)
	if err != nil {
		g.t.Fatalf("New: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		g.t.Fatalf("Start: %v", err)
	}
	return m
}

func (g *mgrRig) engineNameservers(context.Context, string) ([]string, error) {
	return []string{g.dns.Addr()}, nil
}

func (g *mgrRig) engineResolvers() ([]string, error) { return []string{g.dns.Addr()}, nil }

// awaitEnd waits for the next run to end.
func (g *mgrRig) awaitEnd() endEvent {
	g.t.Helper()
	return testsupport.Await(g.t, "a certificate run to end", g.ended)
}

// live is the live row of host.
func (g *mgrRig) live(host string) Cert {
	g.t.Helper()
	c, found, err := g.s.LiveCert(context.Background(), host)
	if err != nil || !found {
		g.t.Fatalf("LiveCert(%s) = found %v, %v", host, found, err)
	}
	return c
}

// requests is how many requests of any kind the fake CA has seen.
func (g *mgrRig) requests() int {
	n := 0
	for _, e := range []string{
		acmetest.EndpointDirectory, acmetest.EndpointNewAccount, acmetest.EndpointNewOrder, acmetest.EndpointOrder,
		acmetest.EndpointAuthz, acmetest.EndpointChallenge, acmetest.EndpointFinalize, acmetest.EndpointCert,
		acmetest.EndpointRenewalInfo,
	} {
		n += g.fake.Count(e)
	}
	return n
}

// ingressJobs are the hub's jobs of the certificate manager.
func (g *mgrRig) ingressJobs() []jobs.Job {
	return g.hub.List(func(j jobs.Job) bool { return j.Kind == JobKind })
}

// CRT-01, the tracer: a stack claims a hostname, one manager round notices, and a DNS-01 certificate is ordered
// from the fake CA with the token and issuer of the domain the claim sits under, in a bounded hub job and stored
// sealed. The only human step is the terms acknowledgement.
func TestTracerAClaimedHostnameGetsACertificate(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	versionBefore, configBefore := versions(t, g.st, "env-1")
	m := g.newManager()

	m.round(ctx)
	end := g.awaitEnd()

	row := g.live("app.example.com")
	if end.certID != row.ID || end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %+v, want %s issued", end, row.ID)
	}
	if row.Phase != PhaseIdle || row.Issuer != IssuerProduction || row.DomainID != g.domain.ID || row.EnvID != "env-1" {
		t.Errorf("row = phase %q issuer %q domain %q env %q, want an idle production row of the claim's domain", row.Phase, row.Issuer, row.DomainID, row.EnvID)
	}
	if !row.RequestedAt.IsZero() {
		t.Errorf("requested_at = %v: a claim is not a request ahead", row.RequestedAt)
	}
	if !slices.Equal(leaf(t, row.ChainPEM).DNSNames, []string{"app.example.com"}) {
		t.Errorf("the stored chain is for %v, want app.example.com", leaf(t, row.ChainPEM).DNSNames)
	}
	if _, err := g.s.OpenKey(row); err != nil {
		t.Errorf("OpenKey: %v", err)
	}
	if n := g.count(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressCertIssued); n != 1 {
		t.Errorf("%d ingress.cert_issued rows, want 1", n)
	}
	if versionAfter, configAfter := versions(t, g.st, "env-1"); versionAfter <= versionBefore || configAfter != configBefore {
		t.Errorf("versions certs %d -> %d, config %d -> %d: want certs raised and config unmoved", versionBefore, versionAfter, configBefore, configAfter)
	}
	js := g.ingressJobs()
	if len(js) != 1 || js[0].Owner != JobOwner || js[0].Meta["hostname"] != "app.example.com" {
		t.Errorf("hub jobs = %+v, want one %s job owned by %s", js, JobKind, JobOwner)
	}
	orders := g.fake.Orders()
	if g.fake.Count(acmetest.EndpointNewOrder) != 1 || len(orders) != 1 || orders[0].Profile != ProfileClassic {
		t.Errorf("the CA saw %d orders %+v, want one asking for %s", g.fake.Count(acmetest.EndpointNewOrder), orders, ProfileClassic)
	}
	if recs := g.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v", recs)
	}
}
