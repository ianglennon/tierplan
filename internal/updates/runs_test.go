// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Update runs (plan 12-40, IMG-06): recorded first, found while they run, ended once.

func runStore(r *rig) updates.RunStore {
	return updates.RunStore{Q: r.st.Q, W: r.st.Write, Now: r.clk.Now}
}

func sampleRun(id, env string) updates.Run {
	return updates.Run{
		ID: id, EnvID: env, BulkID: "bulk-1", Kind: "service", Workload: "web", WorkloadID: "svc-web",
		Items: []updates.RunItem{
			{Service: "web", Image: "nginx:1.27", FromDigest: digestD, ToDigest: digestE},
			{Service: "sidecar", Image: "busybox:1", FromDigest: digestE, ToDigest: digestD},
		},
		State: updates.RunRunning, Actor: "ann", JobID: "job-1",
	}
}

func TestRunRecordedFoundAndFinishedOnce(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := runStore(r)

	if err := s.Insert(ctx, sampleRun("run-1", "env-a")); err != nil {
		t.Fatal(err)
	}
	running, err := s.Running(ctx, "env-a")
	if err != nil || len(running) != 1 {
		t.Fatalf("Running = %+v, %v, want the one run", running, err)
	}
	got := running[0]
	if got.ID != "run-1" || got.State != updates.RunRunning || len(got.Items) != 2 || got.Items[1].Service != "sidecar" ||
		got.BulkID != "bulk-1" || got.Actor != "ann" || got.JobID != "job-1" || !got.FinishedAt.IsZero() || got.StartedAt.IsZero() {
		t.Errorf("Running[0] = %+v, want the run as recorded and not finished", got)
	}

	ok, err := s.Finish(ctx, "run-1", updates.RunDone, "", 7, 0)
	if err != nil || !ok {
		t.Fatalf("Finish(done) = %v, %v, want true", ok, err)
	}
	ok, err = s.Finish(ctx, "run-1", updates.RunFailed, "late", 9, 9)
	if err != nil || ok {
		t.Fatalf("second Finish = %v, %v, want false", ok, err)
	}
	done, err := s.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != updates.RunDone || done.Reason != "" || done.RevisionSeq != 7 || done.RollbackSeq != 0 || done.FinishedAt.IsZero() {
		t.Errorf("Get = %+v, want the first Finish kept", done)
	}
	if running, err = s.Running(ctx, "env-a"); err != nil || len(running) != 0 {
		t.Errorf("Running after Finish = %+v, %v, want none", running, err)
	}
}
