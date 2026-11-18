// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// seedIssuance writes one row of the issuance ledger, as a stored certificate does.
func (g *mgrRig) seedIssuance(issuer, registered, host, kind string, at time.Time) {
	g.t.Helper()
	if _, err := g.st.Write.Exec(
		`INSERT INTO ingress_cert_issuances (id, issuer, registered_domain, hostname, kind, issued_at) VALUES (?, ?, ?, ?, ?, ?)`,
		fmt.Sprintf("seed-%s-%s-%d", issuer, host, at.UnixNano()), issuer, registered, host, kind, at.UnixNano()); err != nil {
		g.t.Fatalf("seed an issuance: %v", err)
	}
}

// seedNew writes n new production certificates under example.com, spread evenly over the six days before now; the
// oldest is exactly six days old.
func (g *mgrRig) seedNew(n int) (oldest time.Time) {
	g.t.Helper()
	now := g.clock.Now()
	span := 6 * 24 * time.Hour
	for i := range n {
		at := now.Add(-span + time.Duration(i)*span/time.Duration(n))
		if i == 0 {
			oldest = at
		}
		g.seedIssuance(IssuerProduction, "example.com", fmt.Sprintf("seed%d.example.com", i), "new", at)
	}
	return oldest
}

// CRT-06, D-17, the tracer: with 40 new production certificates for example.com in the ledger over the last week,
// a newly claimed name is not ordered. It stays queued with the code, the figures and the instant the oldest
// issuance leaves the window, no order reaches the CA, and once the clock passes that instant it is issued.
func TestTracerTheFortyFirstNewNameWaitsWithAReason(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	oldest := g.seedNew(NewCertsCap)
	g.claim("app41.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()

	m.round(ctx)

	row := g.live("app41.example.com")
	if row.Phase != PhaseQueued || row.QueueCode != QueueRateLimitNew || row.QueueUsed != 40 || row.QueueCap != 40 || row.QueueDomain != "example.com" {
		t.Fatalf("row = phase %q code %q used %d cap %d domain %q, want queued rate_limit_new 40 of 40 for example.com",
			row.Phase, row.QueueCode, row.QueueUsed, row.QueueCap, row.QueueDomain)
	}
	slot := oldest.Add(Window)
	if !row.QueueNextSlotAt.Equal(slot) {
		t.Errorf("queue_next_slot_at = %v, want the oldest issuance plus 7 days = %v", row.QueueNextSlotAt, slot)
	}
	want := "rate limit: 40 of 50 new certificates for example.com in 7 days; next slot at " + slot.UTC().Format("2006-01-02 15:04:05") + " UTC"
	if got := QueueReason(row); got != want {
		t.Errorf("QueueReason = %q, want %q", got, want)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d new orders, want none while the budget is used", n)
	}
	if n := len(g.ingressJobs()); n != 0 {
		t.Errorf("%d jobs started for a name that waits", n)
	}

	// A round that finds the same answer changes nothing, and the answer survives a new manager (a restart).
	updated := row.UpdatedAt
	m = g.newManager()
	g.clock.Advance(time.Minute)
	m.round(ctx)
	if again := g.live("app41.example.com"); !again.UpdatedAt.Equal(updated) || again.QueueCode != QueueRateLimitNew {
		t.Errorf("after a restart and a round the row = updated %v code %q, want it untouched (%v)", again.UpdatedAt, again.QueueCode, updated)
	}

	// The slot opens: one second past it the name is ordered and issued.
	g.clock.Advance(slot.Add(time.Second).Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	done := g.live("app41.example.com")
	if done.Phase != PhaseIdle || len(done.ChainPEM) == 0 || done.QueueCode != "" || done.QueueUsed != 0 || !done.QueueNextSlotAt.IsZero() {
		t.Errorf("issued row = phase %q chain %d bytes queue %q/%d/%v, want idle with a chain and no queue reason",
			done.Phase, len(done.ChainPEM), done.QueueCode, done.QueueUsed, done.QueueNextSlotAt)
	}
}

// issuedOrQueued counts the rows of hosts that are idle with a chain and those still queued.
func (g *mgrRig) issuedOrQueued(hosts ...string) (issued, queued int) {
	g.t.Helper()
	for _, h := range hosts {
		switch row := g.live(h); {
		case row.Phase == PhaseIdle && len(row.ChainPEM) > 0:
			issued++
		case row.Phase == PhaseQueued && len(row.ChainPEM) == 0:
			queued++
		}
	}
	return issued, queued
}

// CRT-06 concurrency edge, D-14: candidates are chosen one at a time and a new name that has an order in flight
// counts against its registered domain's budget, so with 39 of 40 used and two new names due in the same round
// exactly one is ordered; the other waits with the budget's reason.
func TestTwoRunsCannotBothTakeTheLastSlot(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.seedNew(NewCertsCap - 1)
	g.claim("a.example.com", "b.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()

	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	// A further round, with the winner finished, still starts nothing for the loser.
	m.round(ctx)

	issued, queued := g.issuedOrQueued("a.example.com", "b.example.com")
	if issued != 1 || queued != 1 {
		t.Fatalf("%d issued and %d queued, want one of each", issued, queued)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d new orders, want one", n)
	}
	for _, h := range []string{"a.example.com", "b.example.com"} {
		if row := g.live(h); row.Phase == PhaseQueued && (row.QueueCode != QueueRateLimitNew || row.QueueUsed != NewCertsCap) {
			t.Errorf("%s waits with %q used %d, want rate_limit_new used %d", h, row.QueueCode, row.QueueUsed, NewCertsCap)
		}
	}
}

// D-14, PROH-20-05: renewals are exempt from the per-domain cap. With the budget used, a renewal that is due runs
// while a new name of the same registered domain waits.
func TestRenewalsBypassTheDomainCap(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("a.example.com")
	m.round(ctx)
	first = g.live("a.example.com")
	g.clock.Advance(first.RenewAt.Sub(g.clock.Now()))
	g.seedNew(NewCertsCap)
	g.claim("a.example.com", "b.example.com")

	m.round(ctx)
	if end := g.awaitEnd(); end.certID != first.ID || end.outcome != OutcomeRenewed {
		t.Fatalf("the run ended %+v, want the renewal of %s", end, first.ID)
	}
	b := g.live("b.example.com")
	if b.Phase != PhaseQueued || b.QueueCode != QueueRateLimitNew || b.QueueUsed != NewCertsCap {
		t.Errorf("b = phase %q code %q used %d, want queued rate_limit_new %d", b.Phase, b.QueueCode, b.QueueUsed, NewCertsCap)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d orders, want the first issue and the renewal", n)
	}
}

// ownRun writes a run of TierPlan's own HTTPS manager.
func (g *mgrRig) ownRun(id, kind, outcome, names string, finished time.Time) {
	g.t.Helper()
	if _, err := g.st.Write.Exec(
		`INSERT INTO acme_runs (id, kind, trigger, names, started_at, finished_at, outcome) VALUES (?, ?, 'schedule', ?, ?, ?, ?)`,
		id, kind, names, finished.Add(-time.Minute).UnixNano(), finished.UnixNano(), outcome); err != nil {
		g.t.Fatalf("seed an own run: %v", err)
	}
}

// ownRuns is how many rows acme_runs holds.
func (g *mgrRig) ownRuns() int {
	g.t.Helper()
	var n int
	if err := g.st.Read.QueryRow(`SELECT COUNT(*) FROM acme_runs`).Scan(&n); err != nil {
		g.t.Fatalf("count the own runs: %v", err)
	}
	return n
}

// D-38: TierPlan's own certificate counts toward its registered domain on production. Its succeeded issue run in
// the window is one more new certificate: with 39 in the ledger the budget is used and a new name waits. A failed
// run, a renewal-kind run and a run older than the window count for nothing, and the governor writes no row of the
// own tables.
func TestOwnCertificateCountsTowardItsRegisteredDomain(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.seedNew(NewCertsCap - 1)
	now := g.clock.Now()
	g.ownRun("own-ok", "issue", "succeeded", `["tierplan.example.com"]`, now.Add(-24*time.Hour))
	g.ownRun("own-failed", "issue", "failed", `["tierplan.example.com"]`, now.Add(-time.Hour))
	g.ownRun("own-renew", "renew", "succeeded", `["tierplan.example.com"]`, now.Add(-time.Hour))
	g.ownRun("own-old", "issue", "succeeded", `["tierplan.example.com"]`, now.Add(-Window-time.Hour))
	g.ownRun("own-other", "issue", "succeeded", `["tierplan.example.org"]`, now.Add(-time.Hour))
	before := g.ownRuns()
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()

	m.round(ctx)
	row := g.live("app.example.com")
	if row.Phase != PhaseQueued || row.QueueCode != QueueRateLimitNew || row.QueueUsed != 40 {
		t.Fatalf("row = phase %q code %q used %d, want queued rate_limit_new used 40 (39 and the own certificate)", row.Phase, row.QueueCode, row.QueueUsed)
	}
	if n := g.ownRuns(); n != before {
		t.Errorf("acme_runs holds %d rows, want the %d it had: the governor only reads it", n, before)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d orders", n)
	}
}

// D-14, T-20-32: a rateLimited answer with a Retry-After to a new-name order holds the other new names of the same
// issuer and registered domain until that instant, with its own code. A name of another registered domain is not
// held, and once the instant has come the held name is ordered.
func TestCARateLimitHoldsTheRegisteredDomain(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("a.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.rateLimit(2*time.Hour, "too many new certificates")
	m := g.newManager()
	g.failedRound(m)
	hold := g.live("a.example.com").RetryAfterUntil
	if want := g.clock.Now().Add(2 * time.Hour); !hold.Equal(want) {
		t.Fatalf("retry_after_until = %v, want %v", hold, want)
	}

	g.prov.AddZone("other.org", rigNS)
	if _, err := g.dom.CreateDomain(ctx, ingressstore.CreateDomainInput{
		EnvID: "env-1", Suffix: "other.org", Issuer: ingress.IssuerProduction, Token: []byte(rigToken), Actor: "user:1",
	}, nil); err != nil {
		t.Fatalf("CreateDomain other.org: %v", err)
	}
	g.claim("a.example.com", "b.example.com", "z.other.org")
	g.clock.Advance(10 * time.Minute)
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	if z := g.live("z.other.org"); z.Phase != PhaseIdle || len(z.ChainPEM) == 0 {
		t.Errorf("z.other.org = phase %q: another registered domain must not be held", z.Phase)
	}
	b := g.live("b.example.com")
	if b.Phase != PhaseQueued || b.QueueCode != QueueCAHold || !b.QueueNextSlotAt.Equal(hold) || b.QueueDomain != "example.com" {
		t.Errorf("b = phase %q code %q next %v domain %q, want queued ca_rate_limited until %v for example.com", b.Phase, b.QueueCode, b.QueueNextSlotAt, b.QueueDomain, hold)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d orders, want the refused one and z.other.org's", n)
	}

	g.clock.Advance(hold.Sub(g.clock.Now()))
	m.round(ctx)
	g.awaitEnd()
	g.awaitEnd()
	if issued, _ := g.issuedOrQueued("a.example.com", "b.example.com"); issued != 2 {
		t.Errorf("%d of a and b issued after the hold, want both", issued)
	}
}

// D-17, D-38: the queue reason is on the row, so a new process finds it and keeps it; the ledger loses only the rows
// older than IssuanceRetention at start, so a row that has left the window but is under eight days old stays.
func TestQueueSurvivesARestartAndOldIssuancesArePruned(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	now := g.clock.Now()
	g.seedNew(NewCertsCap)
	g.seedIssuance(IssuerProduction, "example.com", "old9.example.com", "new", now.Add(-9*24*time.Hour))
	g.seedIssuance(IssuerProduction, "example.com", "old75.example.com", "new", now.Add(-7*24*time.Hour-12*time.Hour))
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()
	var n int
	if err := g.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_cert_issuances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != NewCertsCap+1 {
		t.Fatalf("the ledger holds %d rows after Start, want %d: the nine day old row is pruned and the seven and a half day old one kept", n, NewCertsCap+1)
	}
	m.round(ctx)
	before := g.live("app.example.com")
	if before.QueueCode != QueueRateLimitNew {
		t.Fatalf("code = %q, want rate_limit_new", before.QueueCode)
	}

	// A new process over the same database sees the same row and its reason, and starts no order for it.
	m2 := g.newManager()
	again := g.live("app.example.com")
	if again.QueueCode != before.QueueCode || again.QueueUsed != before.QueueUsed || again.QueueCap != before.QueueCap ||
		again.QueueDomain != before.QueueDomain || !again.QueueNextSlotAt.Equal(before.QueueNextSlotAt) || QueueReason(again) != QueueReason(before) {
		t.Errorf("after a restart the row = %+v, want %+v", again, before)
	}
	m2.round(ctx)
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d orders after the restart", n)
	}
}

// D-13: an Admin's retry goes through the governor like any order. It queues the row for the next round, and that
// round still refuses while the budget is used; it does not clear the reason.
func TestRetryNowStillWaitsForTheGovernor(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.seedNew(NewCertsCap)
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()
	m.round(ctx)
	row := g.live("app.example.com")
	if row.QueueCode != QueueRateLimitNew {
		t.Fatalf("code = %q, want rate_limit_new", row.QueueCode)
	}

	if err := m.RetryNow(ctx, "env-1", row.ID, nil); err != nil {
		t.Fatalf("RetryNow: %v", err)
	}
	m.round(ctx)
	after := g.live("app.example.com")
	if after.Phase != PhaseQueued || after.QueueCode != QueueRateLimitNew || !after.QueueNextSlotAt.Equal(row.QueueNextSlotAt) {
		t.Errorf("after the retry the row = phase %q code %q next %v, want it still waiting for %v", after.Phase, after.QueueCode, after.QueueNextSlotAt, row.QueueNextSlotAt)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d orders for a retry that the budget refuses", n)
	}

	// At the slot the retry's order goes.
	g.clock.Advance(row.QueueNextSlotAt.Add(time.Second).Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
}
