// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"database/sql"
	"encoding/json"
	"errors"
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

func TestRunFinishIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := runStore(r)
	if err := s.Insert(ctx, sampleRun("run-1", "env-a")); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Finish(ctx, "run-1", updates.RunRolledBack, "health check failed", 3, 2); err != nil || !ok {
		t.Fatalf("Finish = %v, %v", ok, err)
	}
	ok, err := s.SetState(ctx, "run-1", updates.RunRunning)
	if err != nil || ok {
		t.Fatalf("SetState on a finished run = %v, %v, want false", ok, err)
	}
	got, err := s.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != updates.RunRolledBack || got.Reason != "health check failed" || got.RevisionSeq != 3 || got.RollbackSeq != 2 {
		t.Errorf("Get = %+v, want the terminal state unchanged", got)
	}
	// A run that has not ended can move: queued to running.
	q := sampleRun("run-2", "env-a")
	q.State = updates.RunQueued
	if err := s.Insert(ctx, q); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetState(ctx, "run-2", updates.RunRunning); err != nil || !ok {
		t.Fatalf("SetState on a queued run = %v, %v, want true", ok, err)
	}
	if g, _ := s.Get(ctx, "run-2"); g.State != updates.RunRunning {
		t.Errorf("run-2 state = %q, want running", g.State)
	}
}

func TestRunOldContainerRecorded(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := runStore(r)
	run := sampleRun("run-1", "env-a")
	run.Kind = "container"
	if err := s.Insert(ctx, run); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetOldContainer(ctx, "run-1", "c0ffee"); err != nil || !ok {
		t.Fatalf("SetOldContainer = %v, %v, want true", ok, err)
	}
	got, err := s.Get(ctx, "run-1")
	if err != nil || got.OldContainer != "c0ffee" {
		t.Fatalf("Get = %+v, %v, want the old container", got, err)
	}
	if _, err := s.Finish(ctx, "run-1", updates.RunDone, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetOldContainer(ctx, "run-1", "other"); err != nil || ok {
		t.Errorf("SetOldContainer on a finished run = %v, %v, want false", ok, err)
	}
	if got, _ = s.Get(ctx, "run-1"); got.OldContainer != "c0ffee" {
		t.Errorf("OldContainer = %q, want c0ffee kept", got.OldContainer)
	}
}

func TestRunItemsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := runStore(r)
	run := sampleRun("run-1", "env-a")
	if err := s.Insert(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != len(run.Items) {
		t.Fatalf("Items = %+v, want %+v", got.Items, run.Items)
	}
	for i := range run.Items {
		if got.Items[i] != run.Items[i] {
			t.Errorf("Items[%d] = %+v, want %+v", i, got.Items[i], run.Items[i])
		}
	}
	// The stored JSON holds those four keys and nothing else.
	var raw string
	if err := r.st.Write.QueryRow(`SELECT items FROM image_update_runs WHERE id = 'run-1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored []map[string]any
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	for _, m := range stored {
		if len(m) != 4 {
			t.Errorf("stored item = %v, want exactly service, image, from_digest, to_digest", m)
		}
		for _, k := range []string{"service", "image", "from_digest", "to_digest"} {
			if _, ok := m[k]; !ok {
				t.Errorf("stored item = %v, missing %q", m, k)
			}
		}
	}
	// A run with no items reads back as an empty list, not an error.
	empty := sampleRun("run-2", "env-a")
	empty.Items = nil
	if err := s.Insert(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if g, err := s.Get(ctx, "run-2"); err != nil || len(g.Items) != 0 {
		t.Errorf("Get(empty) = %+v, %v, want no items", g, err)
	}
}

func TestRunningIsPerEnvironment(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	r.addEnvironment(t, "env-b")
	s := runStore(r)
	queued := sampleRun("run-q", "env-a")
	queued.State = updates.RunQueued
	refused := sampleRun("run-r", "env-a")
	refused.State = updates.RunRefused
	other := sampleRun("run-b", "env-b")
	for _, run := range []updates.Run{queued, refused, other, sampleRun("run-a", "env-a")} {
		if err := s.Insert(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Running(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, run := range got {
		ids[run.ID] = true
		if run.EnvID != "env-a" {
			t.Errorf("Running(env-a) returned %+v of another environment", run)
		}
	}
	if len(got) != 2 || !ids["run-q"] || !ids["run-a"] || ids["run-r"] || ids["run-b"] {
		t.Errorf("Running(env-a) = %v, want the queued and the running run only", ids)
	}
}

func TestRunsCascadeWithEnvironment(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := runStore(r)
	if err := s.Insert(ctx, sampleRun("run-1", "env-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Write.Exec(`DELETE FROM environments WHERE id = 'env-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "run-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get after the environment is deleted = %v, want no rows", err)
	}
}

func TestRunUnknownStateRefused(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := runStore(r)
	bad := sampleRun("run-bad", "env-a")
	bad.State = updates.RunState("exploded")
	if err := s.Insert(ctx, bad); err == nil {
		t.Fatal("Insert with an unknown state succeeded, want the table's CHECK to refuse it")
	}
	if _, err := s.Get(ctx, "run-bad"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get = %v, want no row", err)
	}
	ok := sampleRun("run-ok", "env-a")
	if err := s.Insert(ctx, ok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetState(ctx, "run-ok", updates.RunState("exploded")); err == nil {
		t.Error("SetState to an unknown state succeeded, want the CHECK to refuse it")
	}
	if g, _ := s.Get(ctx, "run-ok"); g.State != updates.RunRunning {
		t.Errorf("state = %q, want running kept", g.State)
	}
}
