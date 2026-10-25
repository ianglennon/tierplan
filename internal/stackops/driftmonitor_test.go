// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The background drift check (plan 08-21, STK-14, D-22): a worker for each environment that checks its stacks on a
// timer, on the events that name a stack and when an operation ends, never during one. Every test drives the worker
// with a clock it moves by hand and waits on what the worker reports (DriftMonitorDeps.Checked), never on a length
// of time.

// fakeDriftSource is the stacks and revisions the worker reads, held in memory.
type fakeDriftSource struct {
	mu     sync.Mutex
	stacks []stackops.Stack
	// revs is the newest good revision of each stack by stack ID; pins the images it ran with, by revision ID.
	revs map[string]stackops.Snapshot
	pins map[string]map[string]string
}

func (f *fakeDriftSource) add(envID string, st stackops.Stack, rev stackops.Snapshot, pins map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.EnvID = envID
	f.stacks = append(f.stacks, st)
	if f.revs == nil {
		f.revs, f.pins = map[string]stackops.Snapshot{}, map[string]map[string]string{}
	}
	f.revs[st.ID] = rev
	f.pins[rev.ID] = pins
}

// setStopped marks a stack stopped or running, as a stop or a start does.
func (f *fakeDriftSource) setStopped(stackID string, stopped bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.stacks {
		if f.stacks[i].ID == stackID {
			f.stacks[i].Stopped = stopped
		}
	}
}

// deploy records a new good revision of a stack, as a deploy does.
func (f *fakeDriftSource) deploy(stackID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rev := f.revs[stackID]
	rev.Seq++
	f.revs[stackID] = rev
}

func (f *fakeDriftSource) Stacks(_ context.Context, envID string) ([]stackops.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []stackops.Stack
	for _, st := range f.stacks {
		if st.EnvID == envID {
			out = append(out, st)
		}
	}
	return out, nil
}

func (f *fakeDriftSource) GoodRevisionInfo(_ context.Context, stackID string) (stackops.DeployedRevision, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rev, ok := f.revs[stackID]
	return stackops.DeployedRevision{Seq: rev.Seq}, ok, nil
}

func (f *fakeDriftSource) LatestGoodRevision(_ context.Context, stackID string) (stackops.Snapshot, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rev, ok := f.revs[stackID]
	return rev, ok, nil
}

func (f *fakeDriftSource) RevisionPins(_ context.Context, snapshotID string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pins[snapshotID], nil
}

// seenCheck is one report of the worker.
type seenCheck struct{ env, stack, outcome string }

// monitorRig is a started DriftMonitor over a fake source with a clock the test moves.
type monitorRig struct {
	mon   *stackops.DriftMonitor
	clk   *stepClock
	store *stackops.DriftStore
	locks *stackops.Locks
	src   *fakeDriftSource
	seen  chan seenCheck
}

// startMonitor builds the monitor over deps (whatever the test leaves empty is filled in), starts it and announces
// envID. The first timer the worker arms is the one the test waits for before it moves the clock.
func startMonitor(t *testing.T, envID string, src *fakeDriftSource, deps stackops.DriftMonitorDeps) *monitorRig {
	t.Helper()
	r := &monitorRig{
		clk: newStepClock(), store: stackops.NewDriftStore(), locks: stackops.NewLocks(), src: src,
		seen: make(chan seenCheck, 256),
	}
	deps.Repo, deps.Store, deps.Locks, deps.Clock = src, r.store, r.locks, r.clk
	if deps.Interval == 0 {
		deps.Interval = 5 * time.Minute
	}
	if deps.Debounce == 0 {
		deps.Debounce = 3 * time.Second
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	deps.Checked = func(env, stack, outcome string) { r.seen <- seenCheck{env, stack, outcome} }
	r.mon = stackops.NewDriftMonitor(deps)
	r.mon.Start(testsupport.Context(t))
	t.Cleanup(r.mon.Stop)
	r.mon.EnvStarted(envID)
	testsupport.Await(t, "the worker to arm its first timer", r.clk.armed)
	return r
}

// await waits for the worker to report outcome for stackID ("" for a pass that held back) and returns the reports
// it read on the way.
func (r *monitorRig) await(t *testing.T, stackID, outcome string) []seenCheck {
	t.Helper()
	var skipped []seenCheck
	for {
		got := testsupport.Await(t, "the worker to report "+outcome+" for stack "+stackID, r.seen)
		if got.stack == stackID && got.outcome == outcome {
			return skipped
		}
		skipped = append(skipped, got)
	}
}

func TestDriftMonitorChecksOnTick(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", func(s *swarm.ServiceSpec) { n := uint64(3); s.Mode.Replicated.Replicas = &n })
	writes := len(rig.fake.Writes())
	src := &fakeDriftSource{}
	src.add(rig.envID, rig.st, rig.store.rev, rig.store.pins)
	var calls int
	var mu sync.Mutex
	m := startMonitor(t, rig.envID, src, stackops.DriftMonitorDeps{
		Ops: rig.deps,
		DriftFunc: func(ctx context.Context, in stackops.DriftInput) (stackops.DriftResult, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return rig.deps.Drift(ctx, in)
		},
	})
	if _, ok := m.store.Get(rig.st.ID); ok {
		t.Fatal("the store holds a result before the first look")
	}

	// The clock moves past the interval: the worker looks once, with nobody having opened the stack.
	m.clk.Advance(6 * time.Minute)
	m.await(t, rig.st.ID, stackops.DriftOutcomeStored)

	res, ok := m.store.Get(rig.st.ID)
	if !ok || res.State != stackops.DriftDrifted || res.Count == 0 {
		t.Fatalf("stored result = %+v, %v, want drifted for the scaled service", res, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("the worker made %d checks, want 1", calls)
	}
	// The worker reads and never writes to an engine (D-23, T-08-77).
	if got := rig.fake.Writes(); len(got) != writes {
		t.Errorf("the background check wrote %v", got[writes:])
	}
}
