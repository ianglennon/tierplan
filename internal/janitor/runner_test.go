// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The runner (plan 11-04, JAN-09, D-08, D-14): the engine is a fake behind the port, the lock and
// the jobs hub are the real ones.

// fixedClock is a clock that stands still at t0, so the containers of these tests are two days old.
type fixedClock struct{}

func (fixedClock) Now() time.Time                       { return t0 }
func (fixedClock) After(time.Duration) <-chan time.Time { return nil }

// fakeEngine serves a snapshot of one slot of exited containers and records what is removed.
type fakeEngine struct {
	snap Snapshot
	// gate, when set, holds Snapshot until it is closed or the context ends; entered is closed
	// when Snapshot is first called.
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once

	mu        sync.Mutex
	removeErr map[string]error
	removed   []string
}

func newFakeEngine(n int) *fakeEngine {
	var cs []Container
	for i := range n {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), "1", i))
	}
	return &fakeEngine{snap: swarmOf(cs...), entered: make(chan struct{}), removeErr: map[string]error{}}
}

func (e *fakeEngine) Snapshot(ctx context.Context, _ string) (Snapshot, error) {
	e.once.Do(func() { close(e.entered) })
	if e.gate != nil {
		select {
		case <-e.gate:
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
	return e.snap, nil
}

func (e *fakeEngine) Inspect(_ context.Context, _, _ string, ids []string) (map[string]ContainerEnd, error) {
	out := map[string]ContainerEnd{}
	for _, id := range ids {
		out[id] = ContainerEnd{FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1}
	}
	return out, nil
}

func (e *fakeEngine) RemoveContainer(_ context.Context, _, _, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.removeErr[id]; err != nil {
		return err
	}
	e.removed = append(e.removed, id)
	return nil
}

func (e *fakeEngine) removedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.removed)
}

func newRunner(t *testing.T, eng Engine) (*Runner, *stackops.Locks, *jobs.Hub) {
	t.Helper()
	locks := stackops.NewLocks()
	hub := jobs.New(jobs.Options{})
	t.Cleanup(hub.Close)
	return &Runner{Engine: eng, Locks: locks, Jobs: hub, Clock: fixedClock{}}, locks, hub
}

func ticked(ids ...string) []ItemRef {
	var out []ItemRef
	for _, id := range ids {
		out = append(out, ItemRef{Type: KindContainer, NodeID: "n1-id", Ref: id})
	}
	return out
}

func TestRunnerHoldsLockOnlyWhileRunning(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5) // c1..c3 are beyond the two newest
	eng.gate = make(chan struct{})
	run, locks, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)

	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	testsupport.Await(t, "the run to read the environment", eng.entered)
	held, ok := locks.Held("env-1")
	if !ok || held.Kind != stackops.HolderKindJanitor || held.By != "ada" || held.JobID != started.JobID || held.StackID != "" || held.StackName != "" {
		t.Fatalf("held = %+v %v, want a janitor holder by ada with the run's job and no stack", held, ok)
	}
	// A second run, or a deploy, finds the environment taken.
	_, err = run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "bob", Owner: "user:u2", Ticked: ticked("c2")})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.Holder.JobID != started.JobID {
		t.Fatalf("a second Start = %v, want a LockedError naming the first run's job", err)
	}
	if _, _, ok := locks.TryAcquire("env-2", stackops.Holder{Kind: "deploy"}); !ok {
		t.Fatal("a run on env-1 held the lock of env-2")
	}

	close(eng.gate)
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if job.State != jobs.JobDone || len(job.Items) != 1 || job.Items[0].State != jobs.ItemDone {
		t.Fatalf("job = %s items %+v, want done with one removal", job.State, job.Items)
	}
	if held, ok := locks.Held("env-1"); ok {
		t.Fatalf("the job reads finished and the lock is still held by %+v", held)
	}
	if got := eng.removedIDs(); !slices.Equal(got, []string{"c1"}) {
		t.Fatalf("removed = %v, want c1 only", got)
	}
}

func TestRunnerJobOwner(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	locks := stackops.NewLocks()
	hub := jobs.New(jobs.Options{MaxRunningPerUser: 1})
	t.Cleanup(hub.Close)
	run := &Runner{Engine: eng, Locks: locks, Jobs: hub, Clock: fixedClock{}}
	ctx := testsupport.Context(t)

	// The person already has the most jobs they may have running.
	if _, err := hub.Start(jobs.Spec{Kind: "pull", Owner: "user:u1"}, func(ctx context.Context, _ *jobs.Reporter) { <-ctx.Done() }); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")}); !errors.Is(err, jobs.ErrTooMany) {
		t.Fatalf("Clean now with the cap reached = %v, want jobs.ErrTooMany", err)
	}
	if _, held := locks.Held("env-1"); held {
		t.Fatal("a run the hub refused kept the lock")
	}

	// A scheduled run is owned by the environment, so that cap never blocks it.
	owner := "system:janitor:env-1"
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: owner, All: true})
	if err != nil {
		t.Fatalf("a scheduled run: %v", err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Owner != owner || job.Kind != JobKind || job.EnvID != "env-1" || job.Meta["runId"] != started.RunID || job.Meta["trigger"] != TriggerSchedule {
		t.Fatalf("job = %+v, want kind %s owned by %s with the run's ID and trigger", job, JobKind, owner)
	}
	if got := eng.removedIDs(); !slices.Equal(got, []string{"c1", "c2", "c3"}) {
		t.Fatalf("a run for everything removed %v, want c1 c2 c3", got)
	}
}

func TestRunnerRemovesOnlyTickedAndRemovableEachOnce(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)
	// c1 twice, c3, c5 (one of the two newest: not removable), a ref nobody has, and c2 on the wrong node.
	req := ticked("c1", "c3", "c1", "c5", "nope")
	req = append(req, ItemRef{Type: KindContainer, NodeID: "other", Ref: "c2"}, ItemRef{Type: KindImage, NodeID: "n1-id", Ref: "c2"})
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: req})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.removedIDs(); !slices.Equal(got, []string{"c1", "c3"}) {
		t.Fatalf("removed = %v, want c1 and c3 once each, in the plan's order", got)
	}
	if len(job.Items) != 2 || job.State != jobs.JobDone {
		t.Fatalf("job = %s items %+v, want done with the two removals", job.State, job.Items)
	}
}

func TestRunnerReportsWhatItDidNotRemove(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	eng.removeErr["c1"] = ErrNoLongerStale
	eng.removeErr["c2"] = ErrGone
	eng.removeErr["c3"] = fmt.Errorf("%w: boom", ErrNodeOffline)
	run, locks, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1", "c2", "c3")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"container/n1-id/c1": {jobs.ItemFailed, codeNoLongerStale},
		"container/n1-id/c2": {jobs.ItemDone, codeGone},
		"container/n1-id/c3": {jobs.ItemFailed, codeNodeOffline},
	}
	for _, it := range job.Items {
		if w := want[it.ID]; it.State != w[0] || it.Code != w[1] {
			t.Errorf("item %s = %s/%s, want %s/%s", it.ID, it.State, it.Code, w[0], w[1])
		}
	}
	if len(job.Items) != 3 {
		t.Errorf("items = %+v, want one for each ticked removal", job.Items)
	}
	if _, held := locks.Held("env-1"); held {
		t.Error("the lock is still held after a run that removed nothing")
	}
}

func TestRunnerFreesTheLockWhenItCannotPlan(t *testing.T) {
	t.Parallel()
	run, locks, hub := newRunner(t, newFakeEngine(5))
	run.Policy = func(context.Context, string) (Policy, error) { return Policy{}, errors.New("no policy") }
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != jobs.JobFailed || len(job.Items) != 1 || job.Items[0].Code != codePlanFailed {
		t.Fatalf("job = %s items %+v, want failed with the plan step failed", job.State, job.Items)
	}
	if _, held := locks.Held("env-1"); held {
		t.Fatal("the lock is still held after a run that could not plan")
	}
}
