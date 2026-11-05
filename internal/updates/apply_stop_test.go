// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
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
