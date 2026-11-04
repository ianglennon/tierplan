// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Recovery after a restart (plan 12-24, IMG-07, D-12, D-14, RESEARCH Pitfall 13). A run row that is still running belongs to a process
// that is gone: the rig writes one, as an update that was interrupted would have left it, sets the engine to the state the
// interruption left, and runs the recovery. Every wait is on the rig's clock.

// interrupted writes the run row an interrupted update left: running, by user-1, with the one workload from a to b.
func (r *applyRig) interrupted(t *testing.T, kind, name, id, image string, from, to string) updates.Run {
	t.Helper()
	run := updates.Run{
		ID: "run-" + name, EnvID: r.envID, Kind: kind, Workload: name, WorkloadID: id,
		Items: []updates.RunItem{{Service: name, Image: image, FromDigest: from, ToDigest: to}},
		State: updates.RunRunning, Actor: "user-1", JobID: "job-gone", StartedAt: t0,
	}
	if err := r.runs.Insert(context.Background(), run); err != nil {
		t.Fatalf("write the interrupted run: %v", err)
	}
	return run
}

// recovery is the recovery over the rig's applier, on the rig's clock.
func (r *applyRig) recovery(stack func(ctx context.Context, run updates.Run) error) *updates.Recovery {
	return &updates.Recovery{Runs: &r.runs, Applier: r.applier, Stack: stack, Clock: r.clock}
}

// recoverNow runs the recovery and waits for it to return.
func (r *applyRig) recoverNow(t *testing.T, stack func(ctx context.Context, run updates.Run) error) {
	t.Helper()
	r.recovery(stack).Run(testsupport.Context(t))
}

// runOf reads a run by its ID.
func (r *applyRig) runOf(t *testing.T, id string) updates.Run {
	t.Helper()
	run, err := r.runs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read run %s: %v", id, err)
	}
	return run
}

// A service whose update a restart interrupted, after the swarm had taken the new digest, gets the previous digest back: the run
// reads rolled_back with the restart as its reason, the failure is remembered and audited, and the lock is free (plan 12-24).
func TestRecoveryRollsBackInterruptedService(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'b'))
	run := r.interrupted(t, "service", "web", "svc-web", "registry.test/team/web:1", dg('a'), dg('b'))

	r.recoverNow(t, nil)

	got := r.runOf(t, run.ID)
	if got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted || got.FinishedAt.IsZero() {
		t.Fatalf("run = %s %q, want rolled_back with server_restarted", got.State, got.Reason)
	}
	sent := r.fake.ServiceUpdates()
	if len(sent) != 1 {
		t.Fatalf("service updates = %d, want the one that puts the previous digest back", len(sent))
	}
	if image := sent[0].Spec.TaskTemplate.ContainerSpec.Image; image != "registry.test/team/web:1@"+dg('a') {
		t.Errorf("image written = %q, want the previous digest", image)
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 || rows[0]["_result"] != "success" || rows[0]["reason"] != updates.ReasonServerRestarted || rows[0]["workload"] != "web" {
		t.Errorf("rollback audit rows = %+v, want one success naming the restart", rows)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != updates.ReasonServerRestarted {
		t.Errorf("failure audit rows = %+v, want one naming the restart", rows)
	}
	if f, ok := r.failureOf(t, "web"); !ok || f.Digest != dg('b') || f.Reason != updates.ReasonServerRestarted || f.RollbackFailed {
		t.Errorf("failure = %+v %v, want the new digest remembered with the restart as the reason", f, ok)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after recovery")
	}
}
