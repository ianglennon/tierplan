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

// D-36: with the terms not acknowledged and no ingress account, the row waits with queue_code no_account and
// neither the CA nor the DNS provider is called. Once acknowledged, the account is registered lazily (in the
// ingress table, never the own one) and the certificate is issued.
func TestManagerWaitsForTheTermsAcknowledgement(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	m := g.newManager()

	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeQueued {
		t.Fatalf("the run ended %q, want queued", end.outcome)
	}
	row := g.live("app.example.com")
	if row.Phase != PhaseQueued || row.QueueCode != QueueNoAccount {
		t.Fatalf("row = phase %q queue %q, want queued no_account", row.Phase, row.QueueCode)
	}
	if g.fake.Count(acmetest.EndpointNewAccount) != 0 || g.fake.Count(acmetest.EndpointNewOrder) != 0 || g.requests() != 0 {
		t.Errorf("the CA saw %d requests, want none before the terms are acknowledged", g.requests())
	}
	if calls := g.prov.Calls(); len(calls) != 0 {
		t.Errorf("the DNS provider saw %+v, want no call", calls)
	}
	// A second round starts no job: the row already says what it waits for.
	m.round(ctx)
	if n := len(g.ingressJobs()); n != 1 {
		t.Errorf("%d jobs after a second round, want still 1", n)
	}

	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q after the acknowledgement, want issued", end.outcome)
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_acme_account`); n != 1 {
		t.Errorf("%d ingress accounts, want 1", n)
	}
	if n := g.count(`SELECT COUNT(*) FROM acme_account`); n != 0 {
		t.Errorf("%d own accounts, want none: the ingress account is its own", n)
	}
	if row := g.live("app.example.com"); row.Phase != PhaseIdle || row.QueueCode != "" {
		t.Errorf("row = phase %q queue %q, want idle with no queue reason", row.Phase, row.QueueCode)
	}
}

// D-32: while issuing is paused a round makes no request to the CA and none to the DNS provider, and creates
// nothing; a round after the pause ends issues.
func TestManagerMakesNoCallWhilePaused(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	m := g.newManager()
	m.round(ctx) // no terms yet: the row is made and waits
	g.awaitEnd()
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.claim("app.example.com", "web.example.com")
	if _, err := g.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = 'restore' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	m.round(ctx)
	if n := g.requests(); n != 0 {
		t.Errorf("the CA saw %d requests while paused, want none", n)
	}
	if calls := g.prov.Calls(); len(calls) != 0 {
		t.Errorf("the DNS provider saw %+v while paused, want no call", calls)
	}
	if row := g.live("app.example.com"); row.Phase != PhaseQueued {
		t.Errorf("phase = %q while paused, want queued", row.Phase)
	}
	if _, found, err := g.s.LiveCert(ctx, "web.example.com"); err != nil || found {
		t.Errorf("a claim made while paused got a row (found %v, %v): nothing is done while paused", found, err)
	}
	if n := len(g.ingressJobs()); n != 1 {
		t.Errorf("%d jobs, want only the first round's", n)
	}

	if _, err := g.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = '' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	m.round(ctx)
	g.awaitEnd()
	g.awaitEnd()
	if row := g.live("app.example.com"); row.Phase != PhaseIdle {
		t.Errorf("phase = %q after the pause, want idle", row.Phase)
	}
}

// D-40: ingress_env.enabled does not gate issuance. A claim in an environment whose ingress is off still gets
// its certificate.
func TestManagerIgnoresTheIngressSwitch(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if _, err := g.st.Write.Exec(`UPDATE ingress_env SET enabled = 0 WHERE env_id = 'env-1'`); err != nil {
		t.Fatal(err)
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_env WHERE env_id = 'env-1' AND enabled = 0`); n != 1 {
		t.Fatalf("the environment's ingress is not off (%d rows)", n)
	}
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()

	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	if row := g.live("app.example.com"); row.Phase != PhaseIdle || len(row.ChainPEM) == 0 {
		t.Errorf("row = phase %q chain %d bytes, want an issued row", row.Phase, len(row.ChainPEM))
	}
}

// D-16: at most MaxConcurrent runs at once, install-wide. With validation held, three claims put exactly two
// orders at the CA and two jobs in the hub; the third waits queued and is issued on the next round after the
// others finish.
func TestManagerRunsAtMostTwoOrdersAtOnce(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	hosts := []string{"a.example.com", "b.example.com", "c.example.com"}
	g.claim(hosts...)
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	release := g.fake.HoldValidation()
	m := g.newManager()

	m.round(ctx)
	g.clk.Clock.WaitArmed(t) // both runs wait on the CA
	g.clk.Clock.WaitArmed(t)
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != MaxConcurrent {
		t.Fatalf("the CA saw %d orders, want %d", n, MaxConcurrent)
	}
	running := 0
	for _, j := range g.ingressJobs() {
		if !j.Finished() {
			running++
		}
	}
	if running != MaxConcurrent {
		t.Errorf("%d jobs running, want %d", running, MaxConcurrent)
	}
	var queued []string
	for _, h := range hosts {
		if g.live(h).Phase == PhaseQueued {
			queued = append(queued, h)
		}
	}
	if len(queued) != 1 {
		t.Fatalf("%d rows queued, want 1: the cap is %d", len(queued), MaxConcurrent)
	}
	// A round while both runs are in flight starts nothing.
	m.round(ctx)
	if n := len(g.ingressJobs()); n != MaxConcurrent {
		t.Errorf("%d jobs after another round, want still %d", n, MaxConcurrent)
	}

	release()
	g.clk.Clock.Advance(acme.PollInterval)
	g.awaitEnd()
	g.awaitEnd()
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the third run ended %q, want issued", end.outcome)
	}
	for _, h := range hosts {
		if row := g.live(h); row.Phase != PhaseIdle {
			t.Errorf("%s is %q, want idle", h, row.Phase)
		}
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != len(hosts) {
		t.Errorf("the CA saw %d orders, want %d", n, len(hosts))
	}
}

// D-09: when the directory offers no classic profile, the order asks for none (the CA's default) and never for
// shortlived.
func TestManagerNeverAsksForShortlived(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, map[string]time.Duration{"shortlived": 160 * time.Hour, "default": 90 * 24 * time.Hour}, "default")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()

	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	orders := g.fake.Orders()
	if len(orders) != 1 || orders[0].Profile != "" {
		t.Errorf("the CA saw %+v, want one order that names no profile", orders)
	}
}

// The second gate: a row whose hostname is not below its domain's suffix is never ordered. It is a failure with
// backoff and an audit row that carries the class, and the CA and the DNS provider are not called.
func TestManagerRefusesAHostnameOutsideItsDomain(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	// The row is made under the claim's domain, then the domain's suffix changes underneath it.
	if _, err := g.st.Write.Exec(`UPDATE ingress_domains SET suffix = 'other.example.org' WHERE id = ?`, g.domain.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.s.CreatePending(ctx, PendingInput{
		Hostname: "app.example.com", EnvID: "env-1", DomainID: g.domain.ID, Issuer: IssuerProduction, CreatedBy: JobOwner, NotRequested: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	m := g.newManager()

	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeFailed {
		t.Fatalf("the run ended %q, want failed", end.outcome)
	}
	row := g.live("app.example.com")
	if row.Phase != PhaseQueued || row.LastErrorClass != acme.ClassIdentifierRejected || row.ConsecutiveFailures != 1 || !row.NextAttemptAt.After(g.clock.Now()) {
		t.Errorf("row = phase %q class %q failures %d next %v, want queued identifier_rejected with a backoff", row.Phase, row.LastErrorClass, row.ConsecutiveFailures, row.NextAttemptAt)
	}
	if g.requests() != 0 || len(g.prov.Calls()) != 0 {
		t.Errorf("the CA saw %d requests and the provider %d calls, want none", g.requests(), len(g.prov.Calls()))
	}
	var detail string
	if err := g.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = ?`, audit.ActionIngressCertFailed).Scan(&detail); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if !strings.Contains(detail, acme.ClassIdentifierRejected) {
		t.Errorf("audit details %q, want the class", detail)
	}
	// Backed off: the next round starts nothing.
	m.round(ctx)
	if n := len(g.ingressJobs()); n != 1 {
		t.Errorf("%d jobs, want 1: the row is backing off", n)
	}
}
