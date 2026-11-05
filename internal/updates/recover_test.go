// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/stackops"
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

// A restart during the pull leaves the service as it was: it still runs the previous digest and has no earlier spec. Recovery has
// nothing to put back, so the run reads failed by the restart, nothing is written, no rollback is audited and the digest is not
// remembered as one that failed (CR-02). A user's "Update anyway" for a digest that could not be pulled is a different path.
func TestRecoveryOfAnUpdateThatNeverWroteTheService(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	run := r.interrupted(t, "service", "web", "svc-web", "registry.test/team/web:1", dg('a'), dg('b'))

	r.recoverNow(t, nil)

	got := r.runOf(t, run.ID)
	if got.State != updates.RunFailed || got.Reason != updates.ReasonServerRestarted || got.FinishedAt.IsZero() {
		t.Fatalf("run = %s %q, want failed by the restart", got.State, got.Reason)
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want none: the update never changed the service", n)
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 0 {
		t.Errorf("rollback audit rows = %+v, want none: nothing was rolled back", rows)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != updates.ReasonServerRestarted {
		t.Errorf("failure audit rows = %+v, want one naming the restart", rows)
	}
	if f, ok := r.failureOf(t, "web"); ok {
		t.Errorf("failure = %+v, want nothing remembered: the new digest was never tried", f)
	}
	if v := r.verdictOf(t, "web"); v.State == updates.StateRolledBack || v.State == updates.StateRollbackFailed {
		t.Errorf("verdict = %s, want the update still on offer", v.State)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after recovery")
	}
}

// The swarm may have rolled the service back by its own failure action before the restart: it runs the previous digest again, but
// its previous spec names the new one, so the update did write it and the run still ends rolled back (CR-02).
func TestRecoveryOfAServiceTheSwarmRolledBack(t *testing.T) {
	t.Parallel()
	svc := swarmService("web", 'a')
	prev := swarmSpec("web", 'b')
	svc.PreviousSpec = &prev
	svc.UpdateState, svc.UpdateMessage = "rollback_completed", "rollback completed"
	r := newApplyRig(t, nil, svc)
	run := r.interrupted(t, "service", "web", "svc-web", "registry.test/team/web:1", dg('a'), dg('b'))

	r.recoverNow(t, nil)

	if got := r.runOf(t, run.ID); got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted {
		t.Errorf("run = %s %q, want rolled_back by the restart", got.State, got.Reason)
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want none: the swarm had already put the previous version back", n)
	}
}

// recordingClock is the rig's clock that notes every wait recovery itself asks for.
type recordingClock struct {
	*waitClock
	mu    sync.Mutex
	waits []time.Duration
	// onWait, when set, is called with the number of waits asked for so far, before the wait is answered.
	onWait func(n int)
}

func (c *recordingClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	n, hook := len(c.waits), c.onWait
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return c.waitClock.After(d)
}

func (c *recordingClock) asked() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

// oldContainerAs is the container of appContainer as an interrupted update leaves it: stopped at finished, under the name an update
// gives it to make way, so the replacement can take the real one.
func oldContainerAs(finished time.Time) dockertest.FakeContainer {
	c := appContainer("exited")
	c.Names = []string{"/" + appOldName}
	c.Created = t0.Add(-time.Hour).Unix()
	c.Inspect.FinishedAt = finished.UTC().Format(time.RFC3339Nano)
	return c
}

// replacementAs is the container an update created for the new digest, named name, in state.
func replacementAs(name, state string) dockertest.FakeContainer {
	return dockertest.FakeContainer{
		ID: strings.Repeat("d", 64), Names: []string{"/" + name}, Image: pinnedApp(dg('b')), ImageID: imgID('2'), State: state,
		Created: t0.Unix(), Inspect: dockertest.FakeInspect{ImageID: imgID('2')},
	}
}

// markStopped records the old container the way the update does before it stops it.
func (r *applyRig) markStopped(t *testing.T, run updates.Run) {
	t.Helper()
	if ok, err := r.runs.SetOldContainer(context.Background(), run.ID, appID); err != nil || !ok {
		t.Fatalf("record the old container: %v %v", ok, err)
	}
}

// An update of a container cut off after the swap: the replacement holds the name, the old container is stopped under its temporary
// name. Recovery removes the replacement, gives the old container its name back and starts it, because the update had stopped it
// (plan 12-24, D-12, T-12-72).
func TestRecoveryRestoresContainer(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, oldContainerAs(t0.Add(2*time.Minute)), replacementAs("app", "running"))
	run := r.interrupted(t, "container", "app", appID, appImage, dg('a'), dg('b'))
	r.markStopped(t, run)

	r.recoverNow(t, nil)

	if got, want := writeKinds(r.fake.Writes()), []string{"remove", "rename", "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the engine was asked %v, want %v: the replacement removed, the old one renamed back and started", got, want)
	}
	if now := r.containersNow(t); !reflect.DeepEqual(now, map[string]string{"app": "running " + appID}) {
		t.Errorf("containers = %v, want only the old container, back under its name and running", now)
	}
	got := r.runOf(t, run.ID)
	if got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted {
		t.Errorf("run = %s %q, want rolled_back by the restart", got.State, got.Reason)
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 || rows[0]["_result"] != "success" || rows[0]["reason"] != updates.ReasonServerRestarted {
		t.Errorf("rollback audit rows = %+v, want one success naming the restart", rows)
	}
	all, err := r.failures.ForEnv(context.Background(), r.envID)
	if f := all[updates.FailureKey("container", "app")]; err != nil || f.Digest != dg('b') || f.Reason != updates.ReasonServerRestarted {
		t.Errorf("failure = %+v %v, want the new digest remembered with the restart as the reason", f, err)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after recovery")
	}
}

// An update cut off before the old container was stopped (nothing recorded): the old one was never touched. The temporary
// replacement is removed and the run is failed by the restart, with nothing to restore.
func TestRecoveryBeforeStop(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("running"), replacementAs(appNewName, "created"))
	run := r.interrupted(t, "container", "app", appID, appImage, dg('a'), dg('b'))

	r.recoverNow(t, nil)

	if got, want := writeKinds(r.fake.Writes()), []string{"remove"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the engine was asked %v, want only the removal of the temporary replacement", got)
	}
	if now := r.containersNow(t); !reflect.DeepEqual(now, map[string]string{"app": "running " + appID}) {
		t.Errorf("containers = %v, want only the old container, still running", now)
	}
	got := r.runOf(t, run.ID)
	if got.State != updates.RunFailed || got.Reason != updates.ReasonServerRestarted {
		t.Errorf("run = %s %q, want failed by the restart", got.State, got.Reason)
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 0 {
		t.Errorf("rollback audit rows = %+v, want none: nothing was rolled back", rows)
	}
}

// An update of a stack goes through the seam of the api package, once; what the seam leaves open is ended, what it ended is left, and
// a lock that is held is waited for (plan 12-24).
func TestRecoveryStackSeam(t *testing.T) {
	t.Parallel()
	stackRun := func(r *applyRig, id string) updates.Run {
		run := updates.Run{
			ID: id, EnvID: r.envID, Kind: "stack", Workload: "shop", WorkloadID: "stack-1", StackID: "stack-1",
			Items:       []updates.RunItem{{Service: "shop_web", Image: "registry.test/team/web:1", FromDigest: dg('a'), ToDigest: dg('b')}},
			State:       updates.RunRunning,
			Actor:       "user-1",
			RevisionSeq: 2, StartedAt: t0,
		}
		if err := r.runs.Insert(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return run
	}

	t.Run("the seam ends the run", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil)
		run := stackRun(r, "run-stack-a")
		calls := 0
		r.recoverNow(t, func(ctx context.Context, got updates.Run) error {
			calls++
			if got.ID != run.ID {
				t.Errorf("the seam was given run %s, want %s", got.ID, run.ID)
			}
			_, err := r.runs.Finish(ctx, got.ID, updates.RunRolledBack, updates.ReasonServerRestarted, 2, 3)
			return err
		})
		if calls != 1 {
			t.Errorf("the seam was called %d times, want once", calls)
		}
		if got := r.runOf(t, run.ID); got.State != updates.RunRolledBack || got.RollbackSeq != 3 || got.RevisionSeq != 2 {
			t.Errorf("run = %s revision %d rollback %d, want the seam's end kept", got.State, got.RevisionSeq, got.RollbackSeq)
		}
		// Recovery asks for nothing more of a run that ended.
		r.recoverNow(t, func(context.Context, updates.Run) error {
			t.Error("the seam was called for a run that ended")
			return nil
		})
	})

	t.Run("the seam fails", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil)
		run := stackRun(r, "run-stack-b")
		r.recoverNow(t, func(context.Context, updates.Run) error { return errors.New("the rollback could not be made") })
		got := r.runOf(t, run.ID)
		if got.State != updates.RunRollbackFailed || got.Reason != updates.ReasonServerRestarted || got.RevisionSeq != 2 {
			t.Errorf("run = %s %q revision %d, want rollback_failed by the restart, its revision kept", got.State, got.Reason, got.RevisionSeq)
		}
		if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 || rows[0]["_result"] != "failure" || rows[0]["workload"] != "shop_web" {
			t.Errorf("rollback audit rows = %+v, want one failure for shop_web", rows)
		}
	})

	t.Run("the lock is held", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil)
		run := stackRun(r, "run-stack-c")
		calls := 0
		r.recoverNow(t, func(ctx context.Context, got updates.Run) error {
			if calls++; calls == 1 {
				return &updates.LockedError{Holder: stackops.Holder{Kind: "deploy"}}
			}
			_, err := r.runs.Finish(ctx, got.ID, updates.RunRolledBack, updates.ReasonServerRestarted, 2, 3)
			return err
		})
		if calls != 2 {
			t.Errorf("the seam was called %d times, want it asked again after the lock", calls)
		}
		if got := r.runOf(t, run.ID); got.State != updates.RunRolledBack {
			t.Errorf("run = %s, want rolled_back", got.State)
		}
	})
}

// A lock held by a deploy at startup is waited for on the clock, not taken over and not skipped: nothing is written until it is free,
// and then the update is rolled back (plan 12-24).
func TestRecoveryWaitsForLock(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'b'))
	run := r.interrupted(t, "service", "web", "svc-web", "registry.test/team/web:1", dg('a'), dg('b'))
	release, _, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", By: "someone", Started: t0})
	if !ok {
		t.Fatal("could not take the lock")
	}
	r.clock.mu.Lock()
	r.clock.hold = true
	r.clock.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.recoverNow(t, nil)
	}()
	select {
	case <-r.clock.armed:
	case <-testsupport.Context(t).Done():
		t.Fatal("recovery did not wait for the lock")
	}
	if n := len(r.fake.Writes()); n != 0 {
		t.Errorf("the engine was written %d times while the lock was held, want none", n)
	}
	if got := r.runOf(t, run.ID); got.State != updates.RunRunning {
		t.Errorf("run = %s while the lock was held, want it left running", got.State)
	}

	release()
	r.clock.mu.Lock()
	r.clock.hold = false
	r.clock.mu.Unlock()
	r.clock.fire(updates.LockPoll)
	select {
	case <-done:
	case <-testsupport.Context(t).Done():
		t.Fatal("recovery did not go on once the lock was free")
	}
	if got := r.runOf(t, run.ID); got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted {
		t.Errorf("run = %s %q, want rolled_back by the restart", got.State, got.Reason)
	}
}

// An environment that cannot be reached is tried again after 1 minute, doubling to 15, and given up after 2 hours as rollback_failed
// with the reason environment_offline (plan 12-24, RESEARCH Pitfall 13).
func TestRecoveryOfflineBackoff(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("running"))
	clk := &recordingClock{waitClock: r.clock}
	run := r.interrupted(t, "container", "app", appID, appImage, dg('a'), dg('b'))
	// The host goes away: every read of it fails, however often it is tried.
	r.fake.Stop()

	(&updates.Recovery{Runs: &r.runs, Applier: r.applier, Clock: clk}).Run(testsupport.Context(t))

	waits := clk.asked()
	if len(waits) < 5 {
		t.Fatalf("recovery waited %v, want a backoff", waits)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute}
	if !reflect.DeepEqual(waits[:5], want) {
		t.Errorf("first waits = %v, want %v", waits[:5], want)
	}
	var total time.Duration
	for _, w := range waits[5:] {
		if w != 15*time.Minute {
			t.Errorf("a later wait = %v, want it held at 15 minutes", w)
		}
	}
	for _, w := range waits {
		total += w
	}
	if total < updates.OfflineGiveUp || total > updates.OfflineGiveUp+updates.OfflineBackoffMax {
		t.Errorf("recovery waited %v in all, want it to give up after about %v", total, updates.OfflineGiveUp)
	}
	got := r.runOf(t, run.ID)
	if got.State != updates.RunRollbackFailed || got.Reason != updates.ReasonEnvironmentOffline {
		t.Errorf("run = %s %q, want rollback_failed with environment_offline", got.State, got.Reason)
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 || rows[0]["_result"] != "failure" || rows[0]["reason"] != updates.ReasonServerRestarted {
		t.Errorf("rollback audit rows = %+v, want one failure naming the restart", rows)
	}
}

// An environment that comes back while recovery is backing off is rolled back at the next try, not given up on.
func TestRecoveryOfflineThenBack(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, oldContainerAs(t0.Add(2*time.Minute)), replacementAs("app", "running"))
	run := r.interrupted(t, "container", "app", appID, appImage, dg('a'), dg('b'))
	r.markStopped(t, run)
	r.fake.Stop()
	clk := &recordingClock{waitClock: r.clock}
	clk.onWait = func(n int) {
		if n == 2 {
			r.fake.Restart(t)
			// The monitor notices the host again on its next probe.
			testsupport.WaitFor(t, "the environment to be read again", func() bool { // testsupport: external, the monitor probes the host on its own schedule
				_, _, err := r.applier.Aggregator.FreshSnapshot(testsupport.Context(t), r.envID)
				return err == nil
			})
		}
	}

	(&updates.Recovery{Runs: &r.runs, Applier: r.applier, Clock: clk}).Run(testsupport.Context(t))

	if waits := clk.asked(); len(waits) != 2 {
		t.Errorf("recovery waited %v, want two waits and then the rollback", waits)
	}
	if got := r.runOf(t, run.ID); got.State != updates.RunRolledBack || got.Reason != updates.ReasonServerRestarted {
		t.Errorf("run = %s %q, want rolled_back by the restart", got.State, got.Reason)
	}
}

// A run that something else has ended is not rolled back again: recovery of the same run twice writes to the engine once, and a run a
// late writer ended while recovery was at it keeps that end (the terminal guard of the run store).
func TestRecoveryOnce(t *testing.T) {
	t.Parallel()
	t.Run("twice", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil, swarmService("web", 'b'))
		run := r.interrupted(t, "service", "web", "svc-web", "registry.test/team/web:1", dg('a'), dg('b'))
		r.recoverNow(t, nil)
		r.recoverNow(t, nil)
		if n := len(r.fake.ServiceUpdates()); n != 1 {
			t.Errorf("service updates = %d, want one: the second recovery finds nothing to do", n)
		}
		if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 {
			t.Errorf("rollback audit rows = %d, want one", len(rows))
		}
		if got := r.runOf(t, run.ID); got.State != updates.RunRolledBack {
			t.Errorf("run = %s, want rolled_back", got.State)
		}
	})

	t.Run("a late writer", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil)
		run := updates.Run{
			ID: "run-late", EnvID: r.envID, Kind: "stack", Workload: "shop", WorkloadID: "stack-1", StackID: "stack-1",
			Items: []updates.RunItem{{Service: "shop_web", Image: "registry.test/team/web:1", FromDigest: dg('a'), ToDigest: dg('b')}},
			State: updates.RunRunning, Actor: "user-1", StartedAt: t0,
		}
		if err := r.runs.Insert(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		r.recoverNow(t, func(ctx context.Context, got updates.Run) error {
			// The update's own job, which was not gone after all, finishes it first.
			_, err := r.runs.Finish(ctx, got.ID, updates.RunDone, "", 2, 0)
			return err
		})
		if got := r.runOf(t, run.ID); got.State != updates.RunDone {
			t.Errorf("run = %s, want the late writer's done kept", got.State)
		}
		if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 0 {
			t.Errorf("rollback audit rows = %+v, want none: recovery did not end this run", rows)
		}
	})
}

// Recovery never starts or finishes an update (IMG-06, T-12-71): a service is only ever written at the digest it ran, a container is
// only removed, renamed and started, and nothing is pulled or created.
func TestRecoveryOnlyRollsBack(t *testing.T) {
	t.Parallel()
	t.Run("service", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil, swarmService("web", 'b'))
		r.interrupted(t, "service", "web", "svc-web", "registry.test/team/web:1", dg('a'), dg('b'))
		r.offer("web", dg('c')) // the registry has something newer still: recovery must not look at it
		r.recoverNow(t, nil)
		sent := r.fake.ServiceUpdates()
		if len(sent) == 0 {
			t.Fatal("recovery wrote nothing")
		}
		for _, u := range sent {
			if image := u.Spec.TaskTemplate.ContainerSpec.Image; !strings.HasSuffix(image, "@"+dg('a')) {
				t.Errorf("a service was written as %q, want only the digest it ran before", image)
			}
		}
		for _, w := range r.fake.Writes() {
			if strings.Contains(w, "/images/create") {
				t.Errorf("recovery pulled an image: %s", w)
			}
		}
	})

	t.Run("container", func(t *testing.T) {
		t.Parallel()
		r := containerRig(t, nil, oldContainerAs(t0.Add(2*time.Minute)), replacementAs("app", "running"))
		run := r.interrupted(t, "container", "app", appID, appImage, dg('a'), dg('b'))
		r.markStopped(t, run)
		r.offer("app", dg('c'))
		r.recoverNow(t, nil)
		for _, k := range writeKinds(r.fake.Writes()) {
			if k != "remove" && k != "rename" && k != "start" {
				t.Errorf("recovery asked the engine to %s, want only remove, rename and start", k)
			}
		}
		if n := len(r.fake.ContainerCreates()); n != 0 {
			t.Errorf("recovery created %d containers, want none", n)
		}
		for _, w := range r.fake.Writes() {
			if strings.Contains(w, "/images/create") {
				t.Errorf("recovery pulled an image: %s", w)
			}
		}
	})

	t.Run("queued", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil, swarmService("web", 'a'))
		run := r.interrupted(t, "service", "web", "svc-web", "registry.test/team/web:1", dg('a'), dg('b'))
		if _, err := r.runs.SetState(context.Background(), run.ID, updates.RunQueued); err != nil {
			t.Fatal(err)
		}
		r.recoverNow(t, nil)
		if n := len(r.fake.Writes()); n != 0 {
			t.Errorf("the engine was written %d times for a run that never started, want none", n)
		}
		if got := r.runOf(t, run.ID); got.State != updates.RunFailed || got.Reason != updates.ReasonServerRestarted {
			t.Errorf("run = %s %q, want failed by the restart", got.State, got.Reason)
		}
	})
}
