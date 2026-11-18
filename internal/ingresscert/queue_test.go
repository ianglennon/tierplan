// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
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
