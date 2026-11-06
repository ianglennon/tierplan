// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// An update that is cut off while its wait runs is either the server stopping or a person's Cancel, and the two are told apart
// (CR-01): the swarm has taken the new version in both, and neither is a failure of it.

// startedWaiting starts the update of web to digest b, with the new tasks held in preparing, and returns once its wait has armed its
// first poll, so the swarm has the new version and the update is waiting on it.
func (r *applyRig) startedWaiting(t *testing.T) string {
	t.Helper()
	r.offer("web", dg('b'))
	r.clock.hold = true
	r.fake.SetTaskStateWhileImage("web", dg('b'), "preparing", "")
	t.Cleanup(func() { r.clock.mu.Lock(); r.clock.hold = false; r.clock.mu.Unlock() })
	id, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	testsupport.Await(t, "the wait to arm its first poll", r.clock.armed)
	return id
}

// The server stopping during the wait leaves the run running, with nothing audited or remembered and the lock free, so restart
// recovery finds it and puts the previous version back. The run does not read failed or cancelled, which would hide it from recovery.
func TestShutdownDuringTheWaitLeavesTheRunForRecovery(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	id := r.startedWaiting(t)

	r.hub.Close()

	job, ok := r.hub.Get(id)
	if !ok {
		t.Fatal("the job is gone")
	}
	run := r.runRow(t, job)
	if run.State != updates.RunRunning || !run.FinishedAt.IsZero() {
		t.Fatalf("run = %s %q, want it left running for recovery", run.State, run.Reason)
	}
	for _, action := range []string{"image.update_failed", "image.update_rolled_back", "image.updated"} {
		if rows := r.audits(t, action); len(rows) != 0 {
			t.Errorf("%s audit rows = %+v, want none: the update has not ended", action, rows)
		}
	}
	if f, ok := r.failureOf(t, "web"); ok {
		t.Errorf("failure = %+v, want nothing remembered of a version the shutdown never judged", f)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is still held after the job ended")
	}
	if n := len(r.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want only the update", n)
	}

	// The next start finds the run and puts the previous version back.
	r.applier.Jobs = jobs.New(jobs.Options{})
	t.Cleanup(r.applier.Jobs.Close)
	r.clock.mu.Lock()
	r.clock.hold = false
	r.clock.mu.Unlock()
	r.recoverNow(t, nil)
	if got := r.runOf(t, run.ID); got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted {
		t.Errorf("after recovery run = %s %q, want rolled_back by the restart", got.State, got.Reason)
	}
}

// A shutdown while the image is still being pulled changed nothing, and leaves the run for recovery too, which ends it failed by the
// restart: the person did not cancel it.
func TestShutdownDuringThePullLeavesTheRunForRecovery(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	r := newApplyRig(t, func(c *dockertest.FakeConfig) { c.PullHold = hold }, swarmService("web", 'a'))
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	r.offer("web", dg('b'))
	id, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = r.fake.Await(testsupport.Context(t), func() bool {
		return slices.ContainsFunc(r.fake.Writes(), func(w string) bool { return strings.HasPrefix(w, "POST /images/create") })
	})
	if err != nil {
		t.Fatalf("waiting for the pull to reach the engine: %v", err)
	}

	r.hub.Close()

	job, _ := r.hub.Get(id)
	if run := r.runRow(t, job); run.State != updates.RunRunning {
		t.Errorf("run = %s %q, want it left running for recovery", run.State, run.Reason)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is still held after the job ended")
	}
}

// A person's Cancel during the wait leaves the new version as the swarm has it, and does not report a failure: the run ends done with
// the cancel as its reason, the audit log says the image was updated, nothing is remembered against the version and nothing is put
// back. The step says the version was not confirmed.
func TestCancelDuringTheWaitIsNotAFailure(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	id := r.startedWaiting(t)

	if err := r.hub.Cancel(id); err != nil {
		t.Fatal(err)
	}
	job, err := r.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}

	run := r.runRow(t, job)
	if run.State != updates.RunDone || run.Reason != updates.ReasonCancelled {
		t.Errorf("run = %s %q, want done with the cancel as its reason", run.State, run.Reason)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 0 {
		t.Errorf("failure audit rows = %+v, want none", rows)
	}
	if rows := r.audits(t, "image.updated"); len(rows) != 1 || rows[0]["_result"] != "success" || rows[0]["reason"] != updates.ReasonCancelled {
		t.Errorf("update audit rows = %+v, want one success naming the cancel", rows)
	}
	if f, ok := r.failureOf(t, "web"); ok {
		t.Errorf("failure = %+v, want nothing remembered", f)
	}
	if n := len(r.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want only the update: a cancel puts nothing back", n)
	}
	for _, it := range job.Items {
		if it.ID == updates.ItemWait && (it.State != jobs.ItemCancelled || it.Code != updates.CodeWaitCancelled || it.Message == "") {
			t.Errorf("wait step = %s %q %q, want cancelled with its own code and sentence", it.State, it.Code, it.Message)
		}
		if it.State == jobs.ItemFailed {
			t.Errorf("step %s failed, want none failed", it.ID)
		}
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is still held after the job ended")
	}
}

// The same holds in a bulk update: the item in flight keeps its run running for recovery to end, and the bulk's own clean-up does not
// finish it as failed. An item that never started is refused as it always was (CR-01).
func TestShutdownDuringABulkLeavesItsRunsForRecovery(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'), swarmService("api", 'a'))
	r.offer("web", dg('b'))
	r.offer("api", dg('b'))
	r.clock.hold = true
	r.fake.SetTaskStateWhileImage("web", dg('b'), "preparing", "")
	t.Cleanup(func() { r.clock.mu.Lock(); r.clock.hold = false; r.clock.mu.Unlock() })
	if _, _, err := r.applier.StartBulk(testsupport.Context(t), r.bulkRequest(dg('b'), "web", "api")); err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "web's wait to arm its first poll", r.clock.armed)

	r.hub.Close()

	runs, err := r.runs.Unfinished(testsupport.Context(t))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]updates.RunState{}
	for _, run := range runs {
		states[run.Workload] = run.State
	}
	if len(states) != 1 || states["web"] != updates.RunRunning {
		t.Errorf("unfinished runs = %v, want only web, running, left for recovery", states)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is still held after the job ended")
	}
}

// beforeAcquire is the rig's locks running a function once, just before the first TryAcquire takes the lock: the moment between a
// request being judged and the environment being locked.
type beforeAcquire struct {
	*stackops.Locks
	once sync.Once
	fn   func()
}

func (l *beforeAcquire) TryAcquire(envID string, h stackops.Holder) (func(), stackops.Holder, bool) {
	l.once.Do(l.fn)
	return l.Locks.TryAcquire(envID, h)
}

// A request that was judged while the lock was free, and reaches the lock after another update of the same service has run and
// finished, is refused as having nothing to update: the service already has the image. It does not write the same spec again or
// record an update the service did not make (WR-04).
func TestAnUpdateJudgedBeforeAnotherFinishedIsRefused(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	var first jobs.Job
	r.applier.Locks = &beforeAcquire{Locks: r.locks, fn: func() {
		// The first update runs to its end inside the gap, through the same applier: its own lock is taken from the real locks.
		r.applier.Locks = r.locks
		first = r.run(t, r.request("web", dg('b')))
	}}

	_, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))

	if first.State != jobs.JobDone {
		t.Fatalf("first update = %s, want done", first.State)
	}
	if !errors.Is(err, updates.ErrNothingToUpdate) {
		t.Errorf("second Start = %v, want ErrNothingToUpdate", err)
	}
	if n := len(r.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want only the first update's", n)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is still held after the refusal")
	}
	runs, rerr := r.runs.Running(testsupport.Context(t), r.envID)
	if rerr != nil || len(runs) != 0 {
		t.Errorf("running runs = %+v, %v, want none", runs, rerr)
	}
}

// flakyFinish is the database of a run store whose write of a run's end fails the first n times, as a busy database does.
type flakyFinish struct {
	*sql.DB
	mu       sync.Mutex
	failures int
	tried    int
}

func (f *flakyFinish) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if strings.Contains(q, "UPDATE image_update_runs") && strings.Contains(q, "finished_at = ?") {
		f.mu.Lock()
		f.tried++
		fail := f.tried <= f.failures
		f.mu.Unlock()
		if fail {
			return nil, errors.New("database is locked")
		}
	}
	return f.DB.ExecContext(ctx, q, args...)
}

// The end of a run that cannot be written at once is written again, so a finished update does not stay running on record, where the
// next start would roll it back (WR-05). When every try fails the row stays running, and that is logged, not hidden.
func TestTheEndOfARunIsWrittenAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		failures int
		want     updates.RunState
	}{
		{"once busy", 1, updates.RunDone},
		{"busy twice", 2, updates.RunDone},
		{"busy every time", 3, updates.RunRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newApplyRig(t, nil, swarmService("web", 'a'))
			r.offer("web", dg('b'))
			db := &flakyFinish{DB: r.st.Write, failures: tc.failures}
			r.applier.Runs = &updates.RunStore{Q: sqlc.New(db), W: r.st.Write}

			job := r.run(t, r.request("web", dg('b')))

			run, err := r.runs.Get(testsupport.Context(t), job.Meta[updates.MetaRunID])
			if err != nil {
				t.Fatal(err)
			}
			if run.State != tc.want {
				t.Errorf("run = %s, want %s", run.State, tc.want)
			}
			if _, held := r.locks.Held(r.envID); held {
				t.Error("the lock is still held after the job ended")
			}
		})
	}
}
