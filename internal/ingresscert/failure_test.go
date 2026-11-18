// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// tokenInvalid makes the DNS provider refuse the domain's token on every record it is asked to create.
func (g *mgrRig) tokenInvalid() {
	g.t.Helper()
	g.prov.FailCreate(&dns.Error{Provider: "cloudflare", Op: "create record", Class: dns.ClassTokenInvalid, Status: 403})
}

// failedRound runs a round and waits for the one run it starts to end as a failure.
func (g *mgrRig) failedRound(m *Manager) {
	g.t.Helper()
	m.round(context.Background())
	if end := g.awaitEnd(); end.outcome != OutcomeFailed {
		g.t.Fatalf("the run ended %q, want failed", end.outcome)
	}
}

// ranNothing asserts that a round started no run: the hub holds no more jobs than it did before the round (it
// forgets finished ones as the clock moves, never invents any), no run ended, and the CA and the DNS provider saw
// nothing more than requestsBefore and callsBefore.
func (g *mgrRig) ranNothing(m *Manager, requestsBefore, callsBefore int) {
	g.t.Helper()
	jobsBefore := len(g.ingressJobs())
	m.round(context.Background())
	if n := len(g.ingressJobs()); n > jobsBefore {
		g.t.Errorf("%d jobs after a round, want no more than the %d before it: the certificate is backing off", n, jobsBefore)
	}
	if n := len(g.ended); n != 0 {
		g.t.Errorf("%d runs ended during a round that should start none", n)
	}
	if n := g.requests(); n != requestsBefore {
		g.t.Errorf("the CA saw %d requests, want still %d", n, requestsBefore)
	}
	if n := len(g.prov.Calls()); n != callsBefore {
		g.t.Errorf("the DNS provider saw %d calls, want still %d", n, callsBefore)
	}
}

// CRT-05, D-11, the tracer: a DNS-01 order the provider refuses is recorded on its certificate with the class, a
// fixed hint, the failure count, the instants and the next attempt on the published schedule (1 m, 10 m, 100 m,
// then 24 h). No run starts for it before that time, and one starts when it comes.
func TestTracerAFailedOrderIsRecordedAndBacksOff(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.tokenInvalid()
	m := g.newManager()

	var failedAt []time.Time
	for i, wait := range []time.Duration{time.Minute, 10 * time.Minute, 100 * time.Minute, 24 * time.Hour} {
		if i > 0 {
			// Before the next attempt nothing runs; at it, a run does.
			row := g.live("app.example.com")
			g.clock.Advance(row.NextAttemptAt.Sub(g.clock.Now()) / 2)
			g.ranNothing(m, g.requests(), len(g.prov.Calls()))
			g.clock.Advance(row.NextAttemptAt.Sub(g.clock.Now()))
		}
		g.failedRound(m)
		failedAt = append(failedAt, g.clock.Now())

		row := g.live("app.example.com")
		if row.Phase != PhaseQueued || row.LastErrorClass != acme.ClassDNSTokenInvalid || row.ConsecutiveFailures != i+1 {
			t.Fatalf("failure %d: row = phase %q class %q failures %d, want queued dns_token_invalid %d", i+1, row.Phase, row.LastErrorClass, row.ConsecutiveFailures, i+1)
		}
		if want := g.clock.Now().Add(wait); !row.NextAttemptAt.Equal(want) {
			t.Errorf("failure %d: next_attempt_at = %v, want now + %v = %v", i+1, row.NextAttemptAt, wait, want)
		}
		if !row.LastAttemptAt.Equal(g.clock.Now()) || !row.LastErrorAt.Equal(g.clock.Now()) {
			t.Errorf("failure %d: last attempt %v, last error %v, want now %v", i+1, row.LastAttemptAt, row.LastErrorAt, g.clock.Now())
		}
		newest := failedAt[max(0, len(failedAt)-acme.FailureGuardRuns):]
		if len(row.FailureTimes) != len(newest) {
			t.Fatalf("failure %d: failure_times = %v, want the newest %d: %v", i+1, row.FailureTimes, len(newest), newest)
		}
		for j := range newest {
			if !row.FailureTimes[j].Equal(newest[j]) {
				t.Errorf("failure %d: failure_times[%d] = %v, want %v", i+1, j, row.FailureTimes[j], newest[j])
			}
		}
		if len(row.ChainPEM) != 0 || row.KeySealed != "" {
			t.Errorf("failure %d: the row holds a chain or key though nothing was issued", i+1)
		}
		if got := FailureHint(row.LastErrorClass); got == "" || got != FailureHint(acme.ClassDNSTokenInvalid) {
			t.Errorf("failure %d: hint %q", i+1, got)
		}
	}
	if g.fake.Count(acmetest.EndpointNewOrder) == 0 {
		t.Error("the CA saw no order: the failures were not the provider's")
	}
}

// rateLimit makes the CA answer the next newOrder with rateLimited and a Retry-After.
func (g *mgrRig) rateLimit(retryAfter time.Duration, detail string) {
	g.t.Helper()
	g.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{
		Status: 429, Type: "rateLimited", Detail: detail, RetryAfterSeconds: int(retryAfter / time.Second),
	})
}

// failedAudit reads the details of every ingress.cert_failed audit row.
func (g *mgrRig) failedAudit() []string {
	g.t.Helper()
	rows, err := g.st.Read.Query(`SELECT details FROM audit_log WHERE action = ? ORDER BY id`, audit.ActionIngressCertFailed)
	if err != nil {
		g.t.Fatalf("read the failure audit rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			g.t.Fatalf("scan: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// D-11: a rate limit with a Retry-After keeps the row from running before it, over the soft backoff, and the order
// goes through once the time has come.
func TestRateLimitedHonoursRetryAfter(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.rateLimit(2*time.Hour, "too many new orders")
	m := g.newManager()

	g.failedRound(m)
	failedAt := g.clock.Now()
	row := g.live("app.example.com")
	if row.LastErrorClass != acme.ClassRateLimited || !row.RetryAfterUntil.Equal(failedAt.Add(2*time.Hour)) {
		t.Fatalf("row = class %q retry_after_until %v, want rate_limited at now + 2h", row.LastErrorClass, row.RetryAfterUntil)
	}
	if !row.NextAttemptAt.Equal(failedAt.Add(time.Minute)) {
		t.Errorf("next_attempt_at = %v, want the soft backoff of 1m", row.NextAttemptAt)
	}

	// The soft backoff is long over, the CA's wait is not.
	for _, after := range []time.Duration{time.Hour, 119 * time.Minute} {
		g.clock.Advance(failedAt.Add(after).Sub(g.clock.Now()))
		g.ranNothing(m, g.requests(), len(g.prov.Calls()))
	}
	g.clock.Advance(failedAt.Add(2 * time.Hour).Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run at +2h ended %q, want issued", end.outcome)
	}
}

// T-20-25: a Retry-After longer than a week is cut to the maximum, so a hostile value cannot stop a certificate for
// ever.
func TestRetryAfterIsCappedAtTheMaximum(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.rateLimit(30*24*time.Hour, "go away")
	m := g.newManager()

	g.failedRound(m)
	if row := g.live("app.example.com"); !row.RetryAfterUntil.Equal(g.clock.Now().Add(acme.MaxRetryAfter)) {
		t.Errorf("retry_after_until = %v, want now + %v", row.RetryAfterUntil, acme.MaxRetryAfter)
	}
}

// D-11, Pitfall 6: a token failure of one certificate holds the other certificates of its domain, whether they are
// queued or waiting for their renewal, until the same next attempt, so one bad token does not spend an order per
// hostname. A certificate of another domain is not held. A hold is not a failure.
func TestTokenFailureHoldsTheWholeDomain(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.secondEnv(ingress.IssuerProduction, rigToken)
	g.claim("b.example.com", "c.example.com")
	g.claimIn("env-2", "stack-2", "z.example.com")
	m := g.newManager()
	// b and c (this domain) and z (another) are issued first, with a provider that works.
	m.round(ctx)
	g.awaitEnd()
	g.awaitEnd()
	m.round(ctx)
	g.awaitEnd()
	for _, h := range []string{"b.example.com", "c.example.com", "z.example.com"} {
		if row := g.live(h); row.Phase != PhaseIdle || len(row.ChainPEM) == 0 {
			t.Fatalf("%s = phase %q chain %d bytes, want issued", h, row.Phase, len(row.ChainPEM))
		}
	}

	// d waits queued behind a short backoff of its own; a is new and due now.
	g.claim("a.example.com", "b.example.com", "c.example.com", "d.example.com")
	d, _, err := g.s.CreatePending(ctx, PendingInput{
		Hostname: "d.example.com", EnvID: "env-1", DomainID: g.domain.ID, Issuer: IssuerProduction, CreatedBy: JobOwner, NotRequested: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET next_attempt_at = ? WHERE id = ?`, g.clock.Now().Add(10*time.Second).UnixNano(), d.ID); err != nil {
		t.Fatal(err)
	}
	g.tokenInvalid()
	ordersBefore := g.fake.Count(acmetest.EndpointNewOrder)

	g.failedRound(m)

	failed := g.live("a.example.com")
	if failed.LastErrorClass != acme.ClassDNSTokenInvalid || failed.ConsecutiveFailures != 1 {
		t.Fatalf("a = class %q failures %d, want dns_token_invalid 1", failed.LastErrorClass, failed.ConsecutiveFailures)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder) - ordersBefore; n != 1 {
		t.Errorf("%d orders reached the CA, want 1: the other certificates of the domain are held", n)
	}
	for _, h := range []string{"b.example.com", "c.example.com", "d.example.com"} {
		row := g.live(h)
		if row.NextAttemptAt.Before(failed.NextAttemptAt) {
			t.Errorf("%s next_attempt_at = %v, want at least %v", h, row.NextAttemptAt, failed.NextAttemptAt)
		}
		if row.ConsecutiveFailures != 0 || row.LastErrorClass != "" {
			t.Errorf("%s = failures %d class %q: a hold is not a failure", h, row.ConsecutiveFailures, row.LastErrorClass)
		}
	}
	if row := g.live("b.example.com"); row.Phase != PhaseIdle || len(row.ChainPEM) == 0 {
		t.Errorf("b = phase %q chain %d bytes, a hold keeps the working certificate", row.Phase, len(row.ChainPEM))
	}
	if row := g.live("d.example.com"); row.Phase != PhaseQueued {
		t.Errorf("d = phase %q, want queued", row.Phase)
	}
	if row := g.live("z.example.com"); !row.NextAttemptAt.IsZero() {
		t.Errorf("z (another domain) next_attempt_at = %v, want it left alone", row.NextAttemptAt)
	}
	// d is due at its own time, which is earlier than the hold; the round after it starts nothing.
	g.clock.Advance(20 * time.Second)
	g.ranNothing(m, g.requests(), len(g.prov.Calls()))
}

// D-12: a certificate whose domain is gone, with no claim anywhere, stops with domain_removed. It is not a failure
// (no backoff, no ingress.cert_failed) and nothing is ordered for it however the clock moves.
func TestRemovedDomainIsNeverRetried(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	m.round(ctx) // the renewal clock
	first = g.live("app.example.com")
	g.claim()
	m.round(ctx)
	if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	ordersBefore := g.fake.Count(acmetest.EndpointNewOrder)

	for _, step := range []time.Duration{time.Minute, 24 * time.Hour, first.RenewAt.Sub(g.clock.Now()) + oneDay} {
		g.clock.Advance(step)
		g.ranNothing(m, g.requests(), len(g.prov.Calls()))
		row := g.live("app.example.com")
		if row.ID != first.ID || row.Phase != PhaseStopped || row.StoppedReason != StopDomainRemoved {
			t.Fatalf("after %v: row = phase %q stopped %q, want stopped %s", step, row.Phase, row.StoppedReason, StopDomainRemoved)
		}
		if row.ConsecutiveFailures != 0 || !row.NextAttemptAt.IsZero() || len(row.ChainPEM) == 0 {
			t.Errorf("after %v: failures %d next %v chain %d bytes, want a stop with no backoff that keeps the chain", step, row.ConsecutiveFailures, row.NextAttemptAt, len(row.ChainPEM))
		}
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != ordersBefore {
		t.Errorf("the CA saw %d orders after the domain went, want none", n-ordersBefore)
	}
	if rows := g.failedAudit(); len(rows) != 0 {
		t.Errorf("%d ingress.cert_failed rows, want none: a removed domain is not a failure", len(rows))
	}
}

// D-12, T-20-45: the run, not the scan, meets the deleted domain (a renewal that was due when the domain went). It
// stops the row without a failure, and that never blocks a claim: the domain registered again with another token
// and the hostname claimed again, the next round re-points the row, clears the stop, and the renewal runs with
// the new token.
func TestRemovedDomainStopNeverBlocksAReclaim(t *testing.T) {
	ctx := context.Background()
	const newToken = "domain-token-after-removal-31c9"
	g := newRenewRig(t, true)
	mod, tokens := g.recordTokens()
	m, first := g.issueOne("app.example.com", mod)
	m.round(ctx) // the renewal clock
	first = g.live("app.example.com")
	g.claim()
	m.round(ctx)
	if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	// The renewal was already due and reserved when the domain went: drive its run, with no scan in between.
	g.clock.Advance(first.RenewAt.Sub(g.clock.Now()))
	if ok, err := g.s.markRenewalDue(ctx, first.ID, g.clock.Now()); err != nil || !ok {
		t.Fatalf("markRenewalDue = %v, %v", ok, err)
	}
	if won, err := g.s.MarkOrdering(ctx, first.ID); err != nil || !won {
		t.Fatalf("MarkOrdering = %v, %v", won, err)
	}
	ordersBefore := g.fake.Count(acmetest.EndpointNewOrder)
	m.runCert(ctx, ctx, first.ID)

	row := g.live("app.example.com")
	if row.Phase != PhaseStopped || row.StoppedReason != StopDomainRemoved || row.ConsecutiveFailures != 0 || !row.NextAttemptAt.IsZero() {
		t.Fatalf("row = phase %q stopped %q failures %d next %v, want a stop that is not a failure", row.Phase, row.StoppedReason, row.ConsecutiveFailures, row.NextAttemptAt)
	}
	if rows := g.failedAudit(); len(rows) != 0 {
		t.Fatalf("%d ingress.cert_failed rows, want none", len(rows))
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder) - ordersBefore; n != 0 {
		t.Fatalf("%d orders reached the CA for a removed domain", n)
	}

	d2 := g.registerDomain("env-1", ingress.IssuerProduction, newToken)
	g.claim("app.example.com")
	tokens.reset()
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeRenewed || end.certID != first.ID {
		t.Fatalf("the renewal ended %+v, want %s renewed under the new domain", end, first.ID)
	}
	row = g.live("app.example.com")
	if row.ID != first.ID || row.DomainID != d2.ID || row.StoppedReason != "" || row.Phase != PhaseIdle || row.Serial == first.Serial {
		t.Errorf("row = id %s domain %s stopped %q phase %q serial %s, want the same row renewed under %s", row.ID, row.DomainID, row.StoppedReason, row.Phase, row.Serial, d2.ID)
	}
	if !tokens.has(newToken) || tokens.has(rigToken) {
		t.Errorf("the renewal built its DNS provider with tokens %v, want only the new domain's", tokens.seen)
	}
}

// D-10, CRT-04: a renewal that fails keeps the working certificate on the row: the chain and serial are the same,
// it is still ready and still served, and its state says the renewal failed.
func TestFailedRenewalKeepsTheWorkingCertificate(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, _ := g.issueOne("app.example.com")
	m.round(ctx) // the renewal clock
	first := g.live("app.example.com")
	if !first.Ready {
		t.Fatal("the issued certificate is not ready")
	}
	g.tokenInvalid()

	g.clock.Advance(first.RenewAt.Sub(g.clock.Now()))
	g.failedRound(m)

	row := g.live("app.example.com")
	if string(row.ChainPEM) != string(first.ChainPEM) || row.Serial != first.Serial || row.KeySealed != first.KeySealed {
		t.Error("the failed renewal changed the chain, the serial or the key")
	}
	if !row.Ready || !Ready(row, g.clock.Now(), true) {
		t.Error("the certificate is not ready after a failed renewal")
	}
	if got := StateOf(row, g.clock.Now()); got != StateRenewalFailed {
		t.Errorf("state = %q, want %q", got, StateRenewalFailed)
	}
	if row.Phase != PhaseQueued || row.LastErrorClass != acme.ClassDNSTokenInvalid || row.ConsecutiveFailures != 1 {
		t.Errorf("row = phase %q class %q failures %d, want queued dns_token_invalid 1", row.Phase, row.LastErrorClass, row.ConsecutiveFailures)
	}
	served, err := g.s.Servable(ctx, "env-1")
	if err != nil || len(served) != 1 || served[0].Hostname != "app.example.com" {
		t.Errorf("Servable = %+v, %v, want the working certificate", served, err)
	}
}

// T-20-25: after a failure, rounds that do not move the clock make no second order and no second provider call.
func TestNoTightLoopAfterAFailure(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.tokenInvalid()
	m := g.newManager()
	g.failedRound(m)
	orders, requests, calls, jobsBefore := g.fake.Count(acmetest.EndpointNewOrder), g.requests(), len(g.prov.Calls()), len(g.ingressJobs())

	for range 100 {
		m.round(ctx)
	}

	if n := g.fake.Count(acmetest.EndpointNewOrder); n != orders {
		t.Errorf("%d orders after 100 rounds, want still %d", n, orders)
	}
	if n := g.requests(); n != requests {
		t.Errorf("the CA saw %d requests, want still %d", n, requests)
	}
	if n := len(g.prov.Calls()); n != calls {
		t.Errorf("the DNS provider saw %d calls, want still %d", n, calls)
	}
	if n := len(g.ingressJobs()); n != jobsBefore {
		t.Errorf("%d jobs, want still %d", n, jobsBefore)
	}
	if row := g.live("app.example.com"); row.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1", row.ConsecutiveFailures)
	}
}

// T-20-26: the CA's problem text is stored cleaned and capped for the Admin API only. The audit row says the
// class and nothing the CA wrote, and the hint is a fixed sentence.
func TestFailureDetailStaysOutOfTheAudit(t *testing.T) {
	ctx := context.Background()
	const marker = "ca-wrote-this-marker-8841"
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.rateLimit(time.Hour, marker+"\x07 bell "+strings.Repeat("x", 3*acme.MaxDetailBytes))
	m := g.newManager()

	g.failedRound(m)

	row := g.live("app.example.com")
	if !strings.Contains(row.LastErrorDetail, marker) {
		t.Errorf("last_error_detail = %.80q..., want the CA's text for the Admin API", row.LastErrorDetail)
	}
	if len(row.LastErrorDetail) > acme.MaxDetailBytes || strings.ContainsRune(row.LastErrorDetail, '\x07') {
		t.Errorf("last_error_detail is %d bytes with a control character %v, want it capped and clean", len(row.LastErrorDetail), strings.ContainsRune(row.LastErrorDetail, '\x07'))
	}
	rows := g.failedAudit()
	if len(rows) != 1 {
		t.Fatalf("%d ingress.cert_failed rows, want 1", len(rows))
	}
	if strings.Contains(rows[0], marker) || !strings.Contains(rows[0], `"reason":"`+acme.ClassRateLimited+`"`) {
		t.Errorf("audit details %q, want the class and nothing the CA wrote", rows[0])
	}
	if strings.Contains(FailureHint(row.LastErrorClass), marker) {
		t.Error("the hint carries the CA's text")
	}
}

// Every class has its own fixed sentence, and a class that is not known gets the generic one.
func TestFailureHintIsFixedVocabulary(t *testing.T) {
	classes := []string{
		acme.ClassRateLimited, acme.ClassValidationUnreachable, acme.ClassValidationDNS, acme.ClassCAAForbids,
		acme.ClassIdentifierRejected, acme.ClassContactInvalid, acme.ClassTOSActionRequired, acme.ClassAccountGone,
		acme.ClassProfileInvalid, acme.ClassCAUnavailable, acme.ClassCAUntrusted, acme.ClassInternal,
		acme.ClassDNSTokenInvalid, acme.ClassDNSTokenScope, acme.ClassDNSZoneNotFound, acme.ClassDNSDelegationTarget,
		acme.ClassDNSNotVisible, acme.ClassDNSRateLimited, acme.ClassDNSUnavailable, acme.ClassValidationTXT,
	}
	seen := map[string]string{}
	for _, c := range classes {
		h := FailureHint(c)
		if h == "" || h == genericHint {
			t.Errorf("class %q has no hint of its own (%q)", c, h)
		}
		if other, dup := seen[h]; dup {
			t.Errorf("classes %q and %q share the hint %q", c, other, h)
		}
		seen[h] = c
	}
	if got := FailureHint("something_new"); got != genericHint {
		t.Errorf("an unknown class gets %q, want the generic hint", got)
	}
}

// 20-08 left this open: an account the CA says it no longer knows is forgotten, the failure is recorded, and the next
// attempt registers a new account and issues.
func TestAccountGoneIsForgottenAndRegisteredAgain(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 400, Type: "accountDoesNotExist", Detail: "no such account"})
	m := g.newManager()

	g.failedRound(m)

	row := g.live("app.example.com")
	if row.LastErrorClass != acme.ClassAccountGone || row.ConsecutiveFailures != 1 {
		t.Fatalf("row = class %q failures %d, want account_gone 1", row.LastErrorClass, row.ConsecutiveFailures)
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_acme_account`); n != 0 {
		t.Fatalf("%d ingress accounts after the CA forgot it, want none", n)
	}
	g.clock.Advance(row.NextAttemptAt.Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the next run ended %q, want issued", end.outcome)
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_acme_account`); n != 1 {
		t.Errorf("%d ingress accounts, want a new one", n)
	}
	if n := g.fake.Count(acmetest.EndpointNewAccount); n != 2 {
		t.Errorf("the CA saw %d new accounts, want 2", n)
	}
}
