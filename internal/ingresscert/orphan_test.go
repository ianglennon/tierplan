// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
)

// Phase 20 review WR-04: a row left ordering by a run that has ended (its release or failure write failed, or it
// panicked) is given back by the next round of the same process, not only by the next Start, and is ordered.
func TestARoundReleasesAReservationNoRunHolds(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager()
	m.reconcile(ctx) // the claim gets its queued row
	row := g.live("app.example.com")
	// A run reserved the row and ended without letting go of it: the row is ordering and no run of it is active.
	if won, err := g.s.MarkOrdering(ctx, row.ID); err != nil || !won {
		t.Fatalf("MarkOrdering = %v, %v", won, err)
	}

	m.round(ctx)
	if js := g.ingressJobs(); len(js) != 1 {
		t.Fatalf("hub jobs after the round = %d, want the stranded row ordered again", len(js))
	}
	if end := g.awaitEnd(); end.certID != row.ID || end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %+v, want %s issued", end, row.ID)
	}
	if got := g.live("app.example.com"); got.Phase != PhaseIdle || len(got.ChainPEM) == 0 {
		t.Errorf("row = phase %q chain %d bytes, want an issued row", got.Phase, len(got.ChainPEM))
	}
}

// The release never touches a row whose run is still active: a round while the run waits on the CA leaves it
// ordering, starts no second job and places no second order.
func TestARoundLeavesAnActiveRunsReservation(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	release := g.fake.HoldValidation()
	m := g.newManager()

	m.round(ctx)
	g.clk.WaitArmed(t) // the run waits on the CA
	m.round(ctx)
	if got := g.live("app.example.com"); got.Phase != PhaseOrdering {
		t.Fatalf("phase = %q during the run, want ordering", got.Phase)
	}
	if js := g.ingressJobs(); len(js) != 1 {
		t.Fatalf("hub jobs = %d during the run, want 1", len(js))
	}

	release()
	g.clk.Advance(acme.PollInterval)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d orders, want 1", n)
	}
}
