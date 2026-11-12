// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The start-time issue of TIERPLAN_ACME=on (phase 16, SET-02, D-05 to D-09).

// cfProvider is the provider id the Cloudflare client answers to.
const cfProvider = "cloudflare"

// cfEnv is a scheduler over a fake CA that validates DNS-01 by looking the TXT record up in the fake
// Cloudflare API, with a DNS responder that serves what that API holds. The manager is built the way a
// process started with the environment is: Deps.Env says what the variables say.
type cfEnv struct {
	*mgrEnv
	cf    *dnstest.FakeCloudflare
	dns   *acmetest.DNSServer
	logs  *syncBuffer
	ended chan string
}

func newCFEnv(t *testing.T) *cfEnv {
	t.Helper()
	cf := dnstest.NewFakeCloudflare(t)
	srv := acmetest.StartDNSServer(t)
	cf.Sink = srv
	cf.AddZone("example.com", dnsNS)

	oe := newOrderEnvWith(t, acmetest.Options{
		ChallengeType: ChallengeDNS01,
		LookupTXT: func(fqdn string) []string {
			var out []string
			for _, r := range cf.Records() {
				if strings.EqualFold(r.Name, fqdn) {
					out = append(out, r.Value())
				}
			}
			return out
		},
	})
	hub := jobs.New(jobs.Options{Now: oe.clock.Now})
	t.Cleanup(hub.Close)
	return &cfEnv{
		mgrEnv: &mgrEnv{orderEnv: oe, hub: hub, inst: &recInstaller{}, urls: &urlSet{public: "https://" + testHost}},
		cf:     cf, dns: srv, logs: &syncBuffer{}, ended: make(chan string, 64),
	}
}

// manager is a Manager over the stored state with env as the variables, as a restarted process builds one.
func (e *cfEnv) manager(env config.ACMEEnv) *Manager {
	return NewManager(Deps{
		Store: e.store, Client: e.client, Challenges: e.tokens, Jobs: e.hub, Installer: e.inst, Clock: e.clock,
		PublicURL: e.urls.getPublic, AgentURL: e.urls.getAgent, BasePath: e.urls.getBase,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Env: env,
		Jitter:   func() float64 { return 0 },
		Rand:     func(n int64) int64 { return n / 2 },
		RunEnded: func(_, outcome string) { e.ended <- outcome },
		DNS: DNSDeps{
			Providers: map[string]dns.Factory{cfProvider: cloudflare.New(cloudflare.Options{
				BaseURL: e.cf.URL(), HTTPClient: e.cf.Client(), UserAgent: "TierPlan/test",
			})},
			NameserverAddrs: func(context.Context, string) ([]string, error) { return []string{e.dns.Addr()}, nil },
			ResolvConf:      func() ([]string, error) { return []string{e.dns.Addr()}, nil },
		},
	})
}

// dnsEnvVars is TIERPLAN_ACME=on with DNS-01, Cloudflare and the fake's token given by token.
func (e *cfEnv) dnsEnvVars(token string) config.ACMEEnv {
	on := true
	return config.ACMEEnv{
		Enabled: &on, AcceptTOS: true, Challenge: ChallengeDNS01, DNSProvider: cfProvider,
		CloudflareToken: token, TokenVar: config.EnvCloudflareTokenFile,
	}
}

// awaitEnd waits for the run a round started to end, and says how it ended.
func (e *cfEnv) awaitEnd() string {
	e.t.Helper()
	return testsupport.Await(e.t, "the start-time run to end", e.ended)
}

// sealedToken is the stored envelope of the provider's token, read from the table.
func (e *cfEnv) sealedToken() string {
	e.t.Helper()
	row, err := e.store.st.RQ.GetACMEDNSCredential(context.Background(), cfProvider)
	if err != nil {
		e.t.Fatalf("read the stored token: %v", err)
	}
	return row.TokenSealed
}

// With TIERPLAN_ACME=on, DNS-01 and the Cloudflare token in the environment, the start-time issue
// validates through DNS-01 with that token, and the token is not stored anywhere: it is read at start,
// held in memory, and removing the variable leaves the database as it was (D-05, T-16-24).
func TestEnvIssueAtStartThroughDNS01WithTheEnvToken(t *testing.T) {
	ctx := context.Background()
	e := newCFEnv(t)
	mgr := e.manager(e.dnsEnvVars(e.cf.Token))

	mgr.round(ctx)
	if got := e.awaitEnd(); got != OutcomeSucceeded {
		t.Fatalf("the start-time run ended %q, want succeeded\nlog:\n%s", got, e.logs.String())
	}

	runs := e.runs()
	if len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded || runs[0].ChallengeType != ChallengeDNS01 || runs[0].Trigger != TriggerSchedule {
		t.Fatalf("runs = %+v, want one succeeded scheduled dns-01 run", runs)
	}
	if e.inst.served() != 1 {
		t.Errorf("ServeACME was called %d times, want 1", e.inst.served())
	}
	if got := e.fake.TXTLookups("_acme-challenge." + testHost); got < 1 {
		t.Errorf("the CA looked up the TXT record %d times, want at least once", got)
	}

	// Cloudflare saw the environment's bearer token, and the record it created was deleted by its ID.
	var created, deleted int
	for _, r := range e.cf.Requests() {
		if !r.Bearer || r.LegacyAuth {
			t.Errorf("request %s %s carried bearer=%v legacy=%v, want only a bearer token", r.Method, r.Path, r.Bearer, r.LegacyAuth)
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/dns_records"):
			created++
		case r.Method == http.MethodDelete && strings.Contains(r.Path, "/dns_records/"):
			deleted++
		}
	}
	if created != 1 || deleted != 1 {
		t.Errorf("Cloudflare saw %d creates and %d deletes, want one of each", created, deleted)
	}
	if recs := e.cf.Records(); len(recs) != 0 {
		t.Errorf("Cloudflare still holds %+v after the run", recs)
	}
	if rows, err := e.store.DNSRecords(ctx); err != nil || len(rows) != 0 {
		t.Errorf("the record ledger holds %+v (%v) after the run, want it empty", rows, err)
	}

	// Nothing about the token reached the database or the log.
	if saved, err := e.store.DNSTokenSaved(ctx, cfProvider); err != nil || saved {
		t.Errorf("DNSTokenSaved = %v (%v), want no token stored for the environment's", saved, err)
	}
	if _, found, err := e.store.DNSToken(ctx, cfProvider); err != nil || found {
		t.Errorf("DNSToken found = %v (%v), want not found", found, err)
	}
	// The stored settings are the defaults: the variables decided the run without being written down.
	if st := e.state(); st.ChallengeType != ChallengeHTTP01 || len(st.DNSResolvers) != 0 {
		t.Errorf("stored challenge %q and resolvers %v, want the defaults: the variables were not written", st.ChallengeType, st.DNSResolvers)
	}
	if strings.Contains(e.logs.String(), e.cf.Token) {
		t.Error("the log holds the Cloudflare token")
	}
}

// A token the environment gives is used and a stored one is left alone: the stored envelope is
// byte-identical after the run, and the run used the variable's token, not the stored one (D-05).
func TestEnvIssueAtStartLeavesAStoredTokenUnchanged(t *testing.T) {
	ctx := context.Background()
	e := newCFEnv(t)
	const stored = "cf-stored-token-11f0b2c4" //nolint:gosec // G101: a recognisable fake token, so a test can prove it is not used
	if err := e.store.SaveDNSToken(ctx, cfProvider, stored, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	before := e.sealedToken()
	mgr := e.manager(e.dnsEnvVars(e.cf.Token))

	mgr.round(ctx)
	if got := e.awaitEnd(); got != OutcomeSucceeded {
		t.Fatalf("the start-time run ended %q, want succeeded with the environment's token\nlog:\n%s", got, e.logs.String())
	}

	if after := e.sealedToken(); before != after {
		t.Error("the stored token's envelope changed, want it untouched")
	}
	if tok, found, err := e.store.DNSToken(ctx, cfProvider); err != nil || !found || tok != stored {
		t.Errorf("the stored token reads %q (found %v, %v), want the one that was stored", tok, found, err)
	}
	if strings.Contains(e.logs.String(), e.cf.Token) || strings.Contains(e.logs.String(), stored) {
		t.Error("the log holds a Cloudflare token")
	}
}

// envLogged is buildEnv with a log the test reads. Each call is a process start over the same data.
func (s *schedEnv) envLogged(env config.ACMEEnv, logs *syncBuffer) *Manager {
	return NewManager(Deps{
		Store: s.store, Client: s.client, Challenges: s.tokens, Jobs: s.hub, Installer: s.inst, Clock: s.clock,
		PublicURL: s.urls.getPublic, AgentURL: s.urls.getAgent, BasePath: s.urls.getBase,
		Log: slog.New(slog.NewTextHandler(logs, nil)), Env: env,
		Jitter:   func() float64 { return 0 },
		Rand:     func(n int64) int64 { return n / 2 },
		RunEnded: func(_, outcome string) { s.ended <- outcome },
	})
}

// advanceTo moves the clock to at. The tests here drive rounds by hand, so no scheduler waits on it.
func (s *schedEnv) advanceTo(at time.Time) {
	s.t.Helper()
	s.clock.Advance(at.Sub(s.clock.Now()))
}

// requireOrders fails the test unless the run history holds exactly want runs and the CA was asked for
// exactly orders orders.
func (s *schedEnv) requireOrders(what string, runs, orders int) {
	s.t.Helper()
	if got := len(s.runs()); got != runs {
		s.t.Errorf("%s: the run history holds %d runs, want %d", what, got, runs)
	}
	if got := s.fake.Count(acmetest.EndpointNewOrder); got != orders {
		s.t.Errorf("%s: the CA saw %d orders, want %d", what, got, orders)
	}
}

// A restart loop never orders early (D-07, 14 D-22): with a CA that refuses, five restarts inside the
// first backoff step place exactly one order, because the ledger that holds the wait is in the database
// and a new process reads it. The next order waits for the backoff, and the one after for the longer one.
func TestEnvIssueRestartLoopOrdersOnce(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.failOrders(3, refusal)
	logs := &syncBuffer{}
	on := envSwitch(true)
	on.AcceptTOS = true

	s.envLogged(on, logs).round(ctx)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the first start-time run ended %q, want failed", got)
	}
	failedAt := s.clock.Now()
	if st := s.state(); st.ConsecutiveFailures != 1 || !st.NextAttemptAt.Equal(failedAt.Add(time.Minute)) {
		t.Fatalf("after one failure: %d failures, next attempt %v; want 1 and %v", st.ConsecutiveFailures, st.NextAttemptAt, failedAt.Add(time.Minute))
	}

	for i := range 5 {
		s.clock.Advance(10 * time.Second)
		s.envLogged(on, logs).round(ctx)
		if n := len(s.ended); n != 0 {
			t.Fatalf("restart %d started a run", i+1)
		}
	}
	s.requireOrders("five restarts inside the first backoff step", 1, 1)

	// The first step ends at a minute after the failure, and a restart then orders.
	s.advanceTo(failedAt.Add(time.Minute))
	s.envLogged(on, logs).round(ctx)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the second start-time run ended %q, want failed", got)
	}
	s.requireOrders("a restart as the first step ends", 2, 2)

	// The second failure waits ten minutes, whatever the number of restarts.
	failedAt = s.clock.Now()
	s.advanceTo(failedAt.Add(10*time.Minute - time.Second))
	for range 3 {
		s.envLogged(on, logs).round(ctx)
	}
	s.requireOrders("restarts a second before the second step ends", 2, 2)
	s.clock.Advance(time.Second)
	s.envLogged(on, logs).round(ctx)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the third start-time run ended %q, want failed", got)
	}
	s.requireOrders("a restart as the second step ends", 3, 3)
}

// Three failed runs inside an hour stop the start-time issue until the guard clears, even when the
// ledger's own step would allow an attempt (D-07, 14 D-22): the guard is issueLocked's, and the scheduler
// goes through it as an admin's click does.
func TestEnvIssueThreeFailedRunsInAnHourStopTheStartTimeIssue(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.failOrders(3, refusal)
	logs := &syncBuffer{}
	on := envSwitch(true)
	on.AcceptTOS = true
	first := s.clock.Now()

	for i, wait := range []time.Duration{0, time.Minute, 10 * time.Minute} {
		s.clock.Advance(wait)
		s.envLogged(on, logs).round(ctx)
		if got := s.awaitRun(); got != OutcomeFailed {
			t.Fatalf("run %d ended %q, want failed", i+1, got)
		}
	}
	s.requireOrders("three failed runs", 3, 3)

	// The ledger's step is cleared, as if its wait had run out; only the guard is left to hold.
	st := s.state()
	st.NextAttemptAt = time.Time{}
	if err := s.store.SaveState(ctx, st); err != nil {
		t.Fatal(err)
	}
	s.advanceTo(first.Add(FailureGuardWindow - time.Second))
	s.envLogged(on, logs).round(ctx)
	s.requireOrders("a second before the guard clears", 3, 3)

	s.clock.Advance(time.Second)
	s.envLogged(on, logs).round(ctx)
	s.awaitRun()
	s.requireOrders("as the guard clears", 4, 4)
}

// A refusal of the preflight (ACME-10) starts nothing, charges nothing and is logged once however many
// rounds meet it; once the URL is put right the next round orders by itself (D-07).
func TestEnvIssuePreflightRefusalCostsNothing(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.urls.setPublic("https://192.0.2.10")
	logs := &syncBuffer{}
	on := envSwitch(true)
	on.AcceptTOS = true
	m := s.envLogged(on, logs)

	for range 4 {
		m.round(ctx)
		s.clock.Advance(Tick)
	}
	s.requireOrders("rounds with an IP address as the public URL", 0, 0)
	if st := s.state(); st.ConsecutiveFailures != 0 || !st.NextAttemptAt.IsZero() {
		t.Errorf("the ledger holds %d failures and next attempt %v, want a refusal to charge nothing", st.ConsecutiveFailures, st.NextAttemptAt)
	}
	if got := strings.Count(logs.String(), "cannot be ordered yet"); got != 1 {
		t.Errorf("the refusal was logged %d times in four rounds, want once:\n%s", got, logs.String())
	}

	s.urls.setPublic("https://" + testHost)
	m.round(ctx)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the run after the URL was put right ended %q, want succeeded", got)
	}
	s.requireOrders("after the URL was put right", 1, 1)
}

// DNS-01 with no token in the environment and none saved starts nothing and charges nothing, the status
// says no token is saved, and a token saved later is picked up by the next round (D-07).
func TestEnvIssueDNS01WithNoTokenCostsNothing(t *testing.T) {
	ctx := context.Background()
	e := newCFEnv(t)
	mgr := e.manager(e.dnsEnvVars(""))

	for range 3 {
		mgr.round(ctx)
		e.clock.Advance(Tick)
	}
	if got := len(e.runs()); got != 0 {
		t.Errorf("the run history holds %d runs, want none", got)
	}
	if got := e.fake.Count(acmetest.EndpointNewOrder); got != 0 {
		t.Errorf("the CA saw %d orders, want none", got)
	}
	if st := e.state(); st.ConsecutiveFailures != 0 || !st.NextAttemptAt.IsZero() {
		t.Errorf("the ledger holds %d failures and next attempt %v, want nothing charged", st.ConsecutiveFailures, st.NextAttemptAt)
	}
	if reqs := e.cf.Requests(); len(reqs) != 0 {
		t.Errorf("Cloudflare was asked %d times, want not at all without a token", len(reqs))
	}
	if got := strings.Count(e.logs.String(), "no DNS API token is set"); got != 1 {
		t.Errorf("the missing token was logged %d times in three rounds, want once:\n%s", got, e.logs.String())
	}
	st, err := mgr.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.DNS.TokenSaved || st.DNS.Provider != cfProvider {
		t.Errorf("status DNS = %+v, want provider %s with no token saved", st.DNS, cfProvider)
	}

	if err := e.store.SaveDNSToken(ctx, cfProvider, e.cf.Token, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	mgr.round(ctx)
	if got := e.awaitEnd(); got != OutcomeSucceeded {
		t.Fatalf("the run once a token was saved ended %q, want succeeded\nlog:\n%s", got, e.logs.String())
	}
}

// With TIERPLAN_ACME=on and the wanted names changed (a new TIERPLAN_PUBLIC_URL), TierPlan orders for the
// new names at start through the same backoff; with the variable unset it waits for the admin (D-08,
// 14 D-05).
func TestEnvIssueNamesChangedOrdersForTheNewNames(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	const newHost = "new.example.com"
	s.urls.setPublic("https://" + newHost)
	logs := &syncBuffer{}

	// Without the variable the stored certificate stays until the admin acts.
	s.mgr.round(ctx)
	s.requireOrders("a round with TIERPLAN_ACME unset", 1, 1)
	if st, err := s.mgr.Status(ctx); err != nil || !st.NamesMismatch {
		t.Fatalf("status names mismatch = %v (%v), want the mismatch the admin is told about", st.NamesMismatch, err)
	}

	on := envSwitch(true)
	on.AcceptTOS = true
	s.failOrders(1, refusal)
	s.envLogged(on, logs).round(ctx)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the order for the new names ended %q, want failed", got)
	}
	failedAt := s.clock.Now()
	s.requireOrders("the first order for the new names", 2, 2)

	// It goes through the same backoff: a restart before the step ends orders nothing.
	s.advanceTo(failedAt.Add(time.Minute - time.Second))
	s.envLogged(on, logs).round(ctx)
	s.requireOrders("a restart inside the backoff", 2, 2)

	s.clock.Advance(time.Second)
	s.envLogged(on, logs).round(ctx)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the order for the new names ended %q, want succeeded", got)
	}
	s.requireOrders("the order after the backoff", 3, 3)
	if orders := s.fake.Orders(); !slices.Equal(orders[len(orders)-1].Names, []string{newHost}) {
		t.Errorf("the last order was for %v, want [%s]", orders[len(orders)-1].Names, newHost)
	}
	if leaf := leafOf(t, s.inst.chains[len(s.inst.chains)-1]); !slices.Equal(leaf.DNSNames, []string{newHost}) {
		t.Errorf("the certificate served now covers %v, want [%s]", leaf.DNSNames, newHost)
	}

	// With the names matching, a further restart orders nothing.
	s.envLogged(on, logs).round(ctx)
	s.requireOrders("a round once the names match", 3, 3)
}

// After a restore TIERPLAN_ACME=on orders nothing until an admin resumes, and orders as soon as Resume is
// clicked (D-09).
func TestEnvIssueAfterARestoreWaitsForResume(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	st := s.state()
	st.PausedReason = PausedRestore
	if err := s.store.SaveState(ctx, st); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	on := envSwitch(true)
	on.AcceptTOS = true
	m := s.envLogged(on, logs)

	for range 3 {
		m.round(ctx)
		s.clock.Advance(Tick)
	}
	s.requireOrders("rounds while paused after a restore", 0, 0)

	if err := m.Resume(ctx, "user:1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	m.round(ctx)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the run after Resume ended %q, want succeeded", got)
	}
	s.requireOrders("the round after Resume", 1, 1)
}

// A token the environment gives that is well formed but that Cloudflare refuses does not stop startup:
// the run fails with the classified DNS error and is charged like any failed run (D-06).
func TestEnvIssueRefusedTokenFailsTheRunLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	e := newCFEnv(t)
	const wrong = "cf-refused-token-5d21e0a9" //nolint:gosec // G101: a recognisable fake token, so a test can prove it is never logged
	mgr := e.manager(e.dnsEnvVars(wrong))

	mgr.round(ctx)
	if got := e.awaitEnd(); got != OutcomeFailed {
		t.Fatalf("the run ended %q, want failed\nlog:\n%s", got, e.logs.String())
	}
	st := e.state()
	if st.LastErrorClass != ClassDNSTokenInvalid {
		t.Errorf("last error class = %q, want %s", st.LastErrorClass, ClassDNSTokenInvalid)
	}
	if st.ConsecutiveFailures != 1 || !st.NextAttemptAt.Equal(e.clock.Now().Add(time.Minute)) {
		t.Errorf("%d failures, next attempt %v; want one failure charged and a minute's wait", st.ConsecutiveFailures, st.NextAttemptAt)
	}
	if saved, err := e.store.DNSTokenSaved(ctx, cfProvider); err != nil || saved {
		t.Errorf("DNSTokenSaved = %v (%v), want the refused token not stored", saved, err)
	}
	if strings.Contains(e.logs.String(), wrong) {
		t.Error("the log holds the Cloudflare token")
	}

	// The failure is charged like any other: the next round waits.
	mgr.round(ctx)
	if got := len(e.runs()); got != 1 {
		t.Errorf("the run history holds %d runs after a round inside the backoff, want 1", got)
	}
}
