// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// Startup recovery of an update of a stack (plan 12-24, IMG-07, D-12). The update ran to the point the restart cut it off: the services
// are on the new digest, the run row says running and the update's revision was marked failed, as MarkInterrupted leaves them. The
// seam puts the stack back to the revision before it, in a job of its own, and the run ends with the restart as its reason.

// interruptUpdate leaves the stack as a restart in the middle of its update would: the run running again, and the update's revision
// failed.
func (s *shopFix) interruptUpdate(t *testing.T, jobID string) updates.Run {
	t.Helper()
	run := s.f.runByJob(t, jobID)
	if _, err := s.f.st.Write.Exec(`UPDATE image_update_runs SET state = 'running', reason = '', finished_at = 0 WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.f.st.Write.Exec(`UPDATE stack_snapshots SET outcome = 'failed' WHERE stack_id = ? AND kind = 'revision' AND seq = ?`, s.st.ID, run.RevisionSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.f.st.Write.Exec(`DELETE FROM image_update_failures`); err != nil {
		t.Fatal(err)
	}
	run, err := (updates.RunStore{Q: s.f.st.Q, W: s.f.st.Write}).Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// The tracer of the seam: the stack is rolled back to revision 1 in a job holding the lock, and the run, the memory of the failed digest
// and the audit rows all name the restart.
func TestRecoverStackUpdate(t *testing.T) {
	t.Parallel()
	s := shopQuick(t, "web", "api")
	s.move(t, "web", 'b')
	jobID, job := s.startUpdate(t, "stack", "shop")
	if job.State != "done" {
		t.Fatalf("the update = %s %+v, want it to have worked before the restart", job.State, job.Items)
	}
	run := s.interruptUpdate(t, jobID)
	before := len(s.f.fake.ServiceUpdates())

	if err := api.RecoverStackUpdate(s.f.stackDeps)(context.Background(), run); err != nil {
		t.Fatalf("RecoverStackUpdate: %v", err)
	}

	got := s.f.runByJob(t, jobID)
	if got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted || got.RevisionSeq != 2 || got.RollbackSeq != 3 || got.FinishedAt.IsZero() {
		t.Errorf("run = %s %q revision %d rollback %d, want rolled_back by the restart: revision 2 put back by rollback 3", got.State, got.Reason, got.RevisionSeq, got.RollbackSeq)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 3 || revs[2].Outcome != "good" {
		t.Fatalf("revisions = %+v, want one good rollback revision", revs)
	}
	if origin, from := s.f.revisionOrigin(t, s.st.ID, 3); origin != "rollback" || from != 1 {
		t.Errorf("revision 3 = %q restored from %d, want a rollback of #1", origin, from)
	}
	if last := s.lastImages(before); !strings.HasSuffix(last["shop_web"], "@"+updDigest('a')) {
		t.Errorf("web was last written as %q, want the digest it ran before the update", last["shop_web"])
	}
	if f := s.failures(t)["service/shop_web"]; f.Digest != updDigest('b') || f.Reason != updates.ReasonServerRestarted || f.RollbackFailed || f.RunID != run.ID {
		t.Errorf("failure of shop_web = %+v, want the new digest remembered with the restart as its reason", f)
	}
	if rows := s.f.auditRows(t, "image.update_rolled_back"); len(rows) != 1 || rows[0].Result != "success" || rows[0].Details["reason"] != updates.ReasonServerRestarted {
		t.Errorf("image.update_rolled_back rows = %+v, want one success naming the restart", rows)
	}
	if _, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Error("the lock is still held after the recovery")
	}
}

// An environment whose lock is held is not touched: the seam answers a *LockedError for the recovery to wait on, and writes nothing.
func TestRecoverStackUpdateWaitsForTheLock(t *testing.T) {
	t.Parallel()
	s := shopQuick(t, "web")
	s.move(t, "web", 'b')
	jobID, _ := s.startUpdate(t, "stack", "shop")
	run := s.interruptUpdate(t, jobID)
	release, _, ok := s.f.stackLocks.TryAcquire(s.e.ID, stackops.Holder{Kind: "deploy", By: "someone", Started: time.Now()})
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer release()

	err := api.RecoverStackUpdate(s.f.stackDeps)(context.Background(), run)

	var locked *updates.LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("RecoverStackUpdate = %v, want a *LockedError", err)
	}
	if got := s.f.runByJob(t, jobID); got.State != updates.RunRunning {
		t.Errorf("run = %s, want it left running for the next try", got.State)
	}
	if revs := s.f.revisions(t, s.st.ID); len(revs) != 2 {
		t.Errorf("revisions = %+v, want no rollback revision while the lock is held", revs)
	}
}
