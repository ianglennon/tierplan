// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// startRunning leaves a deploy of st that wrote the claims of compose and was cut off by the server stopping: a new revision
// that is still pending, a deploy row that says running, and the claims of compose stored.
func (f *claimFix) startRunning(t *testing.T, st stackops.Stack, compose string) {
	t.Helper()
	ctx := testsupport.Context(t)
	rev, err := f.repo.NewRevision(ctx, st.ID, "user:test", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if _, err := f.repo.StartDeploy(ctx, st.ID, f.rig.envID, stackops.DeployKindDeploy, rev.Seq, "job-gone", "user:test"); err != nil {
		t.Fatalf("StartDeploy: %v", err)
	}
	f.claim(t, st, declsOf(t, compose, st.Target))
}

func TestInterruptedDeployClaimsRestored(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, false, nil)
	ctx := testsupport.Context(t)

	// An update whose deploy declared and claimed b.example.com on top of a good revision that declared a.example.com.
	cut := f.newStack(t, "cut", stacks.TargetSwarm, routed("a.example.com"))
	f.makeGood(t, cut)
	f.startRunning(t, cut, routed("b.example.com"))
	// A first deploy that claimed its name and never ended: it has no good revision to go back to.
	first := f.newStack(t, "first", stacks.TargetSwarm, routed("c.example.com"))
	f.startRunning(t, first, routed("c.example.com"))
	// A stack with nothing running is not touched, whatever it holds.
	idle := f.newStack(t, "idle", stacks.TargetSwarm, routed("d.example.com"))
	f.makeGood(t, idle)

	interrupted, err := f.repo.MarkInterrupted(ctx)
	if err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	if len(interrupted) != 2 {
		t.Fatalf("interrupted = %+v, want the two stacks with a running deploy", interrupted)
	}
	wantHeld(t, f, cut, "b.example.com") // not put back yet: MarkInterrupted only reports

	if err := stackops.RestoreInterruptedClaims(ctx, f.repo, f.ops, interrupted, audit.Actor{Type: audit.ActorSystem, ID: "system:startup", Name: "system:startup"}); err != nil {
		t.Fatalf("RestoreInterruptedClaims: %v", err)
	}

	wantHeld(t, f, cut, "a.example.com")
	wantHeld(t, f, first)
	wantHeld(t, f, idle, "d.example.com")
	// Restoring again changes nothing, as a second start must not.
	if err := stackops.RestoreInterruptedClaims(ctx, f.repo, f.ops, interrupted, audit.Actor{Type: audit.ActorSystem, ID: "system:startup", Name: "system:startup"}); err != nil {
		t.Fatalf("RestoreInterruptedClaims again: %v", err)
	}
	wantHeld(t, f, cut, "a.example.com")
	wantHeld(t, f, first)
}

// A name the stack's last good revision declared that another stack took meanwhile cannot be restored, so its claims are
// released: the same fallback a failed deploy makes.
func TestInterruptedDeployClaimsRestoreFallsBackToRelease(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, false, nil)
	ctx := testsupport.Context(t)
	cut := f.newStack(t, "cut", stacks.TargetSwarm, routed("a.example.com"))
	f.makeGood(t, cut)
	f.startRunning(t, cut, routed("b.example.com"))
	other := f.newStack(t, "other", stacks.TargetSwarm, routed("a.example.com"))
	f.claim(t, other, declsOf(t, routed("a.example.com"), stacks.TargetSwarm)) // a.example.com was let go by the failed revision
	interrupted, err := f.repo.MarkInterrupted(ctx)
	if err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	if err := stackops.RestoreInterruptedClaims(ctx, f.repo, f.ops, interrupted, audit.Actor{Type: audit.ActorSystem, ID: "system:startup", Name: "system:startup"}); err != nil {
		t.Fatalf("RestoreInterruptedClaims: %v", err)
	}

	wantHeld(t, f, cut)
	wantHeld(t, f, other, "a.example.com")
}
