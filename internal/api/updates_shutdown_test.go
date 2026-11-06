// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
)

// A server that stops while a bulk update is deploying a stack leaves that item's run for restart recovery, and recovery puts the stack
// back: the server stopping is not a person's Cancel, which keeps the stack as it is, so the run is not ended as cancelled. The deploy
// is held on its first service (a task that never settles), the hub is closed as the server's shutdown does, and a fresh hub stands for
// the next start.
func TestBulkCutOffByShutdownLeavesItsStackItemForRecovery(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web", "api")
	s.move(t, "web", 'b')
	s.f.fake.SetTaskState("shop_web", "preparing", "")

	rec := s.f.call(t, authReq{
		method: http.MethodPost, path: "/api/fleet/updates/run", body: bulkBody(bulkItem(s.e.ID, "service", "shop_web", nil)),
		cookie: s.operator.cookie, csrf: s.operator.csrf,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.Code, rec.Body)
	}
	ans := answerOf[bulkAnswer](t, rec)
	jobID := ans.jobOf(t, s.e.ID)
	awaitStackJobItem(t, s.f, jobID, func(it jobs.Item) bool { return it.ID == "i0/service:shop_web" && it.Unit == "tasks" })

	s.f.jobHub.Close() // the server stops: the job's context ends with jobs.ErrHubClosed as its cause

	runs := s.f.runsOfBulk(t, ans.BulkID)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want the one run of the stack", runs)
	}
	if run := runs[0]; run.State != updates.RunRunning || run.Kind != "stack" {
		t.Fatalf("run = %s %q, want the stack's run left running for recovery", run.State, run.Reason)
	}
	if rows := s.f.auditRows(t, "image.update_failed"); len(rows) != 0 {
		t.Errorf("image.update_failed rows = %+v, want none: the update was not judged failed", rows)
	}
	if f := s.failures(t); len(f) != 0 {
		t.Errorf("failures = %+v, want none remembered for a shutdown", f)
	}

	// The next start: a hub of its own, and recovery finds the run running. The engine's tasks settle again.
	s.f.fake.SetTaskState("shop_web", "", "")
	d := s.f.stackDeps
	d.Jobs = jobs.New(jobs.Options{})
	t.Cleanup(d.Jobs.Close)
	before := len(s.f.fake.ServiceUpdates())
	if err := api.RecoverStackUpdate(d)(context.Background(), runs[0]); err != nil {
		t.Fatalf("RecoverStackUpdate: %v", err)
	}

	got := s.f.runsOfBulk(t, ans.BulkID)[0]
	if got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted || got.FinishedAt.IsZero() {
		t.Errorf("run = %s %q, want rolled_back by the restart", got.State, got.Reason)
	}
	if last := s.lastImages(before); !strings.HasSuffix(last["shop_web"], "@"+updDigest('a')) {
		t.Errorf("web was last written as %q, want the digest it ran before the update", last["shop_web"])
	}
	if _, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Error("the lock is still held after the recovery")
	}
}

// The same for the update of a stack asked for on its own: the server stopping is not a person's Cancel, so the run stays running and
// the next start puts the stack back.
func TestStackUpdateCutOffByShutdownLeavesItsRunForRecovery(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	s.move(t, "web", 'b')
	s.f.fake.SetTaskState("shop_web", "preparing", "")
	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	jobID := answerOf[stackRunAnswer](t, rec).JobID
	awaitStackJobItem(t, s.f, jobID, func(it jobs.Item) bool { return it.ID == "service:shop_web" && it.Unit == "tasks" })

	s.f.jobHub.Close()

	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunRunning {
		t.Fatalf("run = %s %q, want it left running for recovery", run.State, run.Reason)
	}
	if rows := s.f.auditRows(t, "image.update_failed"); len(rows) != 0 {
		t.Errorf("image.update_failed rows = %+v, want none", rows)
	}
	s.f.fake.SetTaskState("shop_web", "", "")
	d := s.f.stackDeps
	d.Jobs = jobs.New(jobs.Options{})
	t.Cleanup(d.Jobs.Close)
	if err := api.RecoverStackUpdate(d)(context.Background(), run); err != nil {
		t.Fatalf("RecoverStackUpdate: %v", err)
	}
	if got := s.f.runByJob(t, jobID); got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted {
		t.Errorf("run = %s %q, want rolled_back by the restart", got.State, got.Reason)
	}
}
