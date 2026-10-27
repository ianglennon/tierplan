// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
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

// awaitAll waits until the worker has stored a result for every stack named, in whatever order. Stacks whose
// timers ran out at the same clock move are checked in one pass or in two, depending on how fast the timers' goroutines
// run, so a test must not depend on which.
func (r *monitorRig) awaitAll(t *testing.T, stackIDs ...string) []seenCheck {
	t.Helper()
	want := map[string]bool{}
	for _, id := range stackIDs {
		want[id] = true
	}
	var others []seenCheck
	for len(want) > 0 {
		got := testsupport.Await(t, "the worker to store a result for each of its stacks", r.seen)
		if got.outcome == stackops.DriftOutcomeStored && want[got.stack] {
			delete(want, got.stack)
			continue
		}
		others = append(others, got)
	}
	return others
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

// monitorEnv is the environment every stand-in test works on.
const monitorEnv = "env-monitor"

// monitorStack is a deployed stack of the target, with a good revision 1.
func monitorStack(src *fakeDriftSource, id, name string, target stacks.Target) stackops.Stack {
	st := stackops.Stack{ID: id, Name: name, Target: target}
	src.add(monitorEnv, st, stackops.Snapshot{ID: "rev-" + id, StackID: id, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood}, map[string]string{})
	st.EnvID = monitorEnv
	return st
}

// driftCalls stands in for the check of a stack: it counts the calls and answers that nothing differs.
type driftCalls struct {
	mu      sync.Mutex
	byStack map[string]int
	// hook, when set, runs inside each call before it answers.
	hook func(in stackops.DriftInput)
}

func newDriftCalls() *driftCalls { return &driftCalls{byStack: map[string]int{}} }

func (c *driftCalls) check(_ context.Context, in stackops.DriftInput) (stackops.DriftResult, error) {
	c.mu.Lock()
	c.byStack[in.Stack.ID]++
	hook := c.hook
	c.mu.Unlock()
	if hook != nil {
		hook(in)
	}
	return stackops.DriftResult{State: stackops.DriftNone, Revision: in.Deployed.Seq, CheckedAt: time.Unix(1, 0).UTC()}, nil
}

func (c *driftCalls) count(stackID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byStack[stackID]
}

// fakeHub stands in for the events hub: each Subscribe hands out a channel the test writes to or closes.
type fakeHub struct {
	mu     sync.Mutex
	subs   []chan dockerx.EventRow
	opened chan struct{}
}

func newFakeHub() *fakeHub { return &fakeHub{opened: make(chan struct{}, 16)} }

func (h *fakeHub) Subscribe(context.Context, string) ([]dockerx.EventRow, <-chan dockerx.EventRow, func()) {
	ch := make(chan dockerx.EventRow, 64)
	h.mu.Lock()
	h.subs = append(h.subs, ch)
	h.mu.Unlock()
	h.opened <- struct{}{}
	return nil, ch, func() {}
}

// sub is the channel of the n-th subscription, counting from zero. The worker subscribes before it arms its first
// timer, so a test that has seen that timer has seen the subscription too.
func (h *fakeHub) sub(t *testing.T, n int) chan dockerx.EventRow {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if n >= len(h.subs) {
		t.Fatalf("the worker has made %d subscriptions, want at least %d", len(h.subs), n+1)
	}
	return h.subs[n]
}

// serviceEvent and containerEvent are the Docker events the worker reads.
func serviceEvent(action, name string, attrs map[string]string) dockerx.EventRow {
	return dockerx.EventRow{Type: "service", Action: action, Name: name, Attributes: attrs}
}

func containerEvent(action string, attrs map[string]string) dockerx.EventRow {
	return dockerx.EventRow{Type: "container", Action: action, Name: "c1", Attributes: attrs}
}

// settle moves the clock to the worker's first look, waits until every stack named was checked and until the worker
// has armed its next timer, so the test starts from a worker at rest.
func (r *monitorRig) settle(t *testing.T, stackIDs ...string) {
	t.Helper()
	r.clk.Advance(3 * time.Second)
	for _, id := range stackIDs {
		r.await(t, id, stackops.DriftOutcomeStored)
	}
	testsupport.Await(t, "the worker to arm its next timed look", r.clk.armed)
}

// quiet says the worker holds no timer it was not asked for: every arming the test counted was consumed.
func (r *monitorRig) quiet(t *testing.T) {
	t.Helper()
	if n := len(r.clk.armed); n != 0 {
		t.Errorf("the worker armed %d more timers than the test expected", n)
	}
}

func TestDriftMonitorDebouncesEvents(t *testing.T) {
	t.Parallel()
	src, calls, hub := &fakeDriftSource{}, newDriftCalls(), newFakeHub()
	web := monitorStack(src, "web-id", "web", stacks.TargetSwarm)
	monitorStack(src, "db-id", "db", stacks.TargetSwarm)
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check, Subscribe: hub.Subscribe})
	m.settle(t, "db-id", "web-id")
	ns := map[string]string{stacks.LabelSwarmNamespace: web.Name}

	// Three events for one stack arm one timer; an event for another stack arms its own. The second timer is how the test
	// knows the first three were all read.
	for range 3 {
		hub.sub(t, 0) <- serviceEvent("update", "web_web", ns)
	}
	hub.sub(t, 0) <- serviceEvent("update", "db_db", map[string]string{stacks.LabelSwarmNamespace: "db"})
	testsupport.Await(t, "the timer of web", m.clk.armed)
	testsupport.Await(t, "the timer of db", m.clk.armed)
	m.quiet(t)
	if calls.count("web-id") != 1 {
		t.Fatalf("web was checked %d times before its quiet time ran out, want only the first look", calls.count("web-id"))
	}

	m.clk.Advance(3 * time.Second)
	m.awaitAll(t, "db-id", "web-id")
	if calls.count("web-id") != 2 || calls.count("db-id") != 2 {
		t.Errorf("checks of web and db = %d and %d, want 2 and 2: one look and one check for the three events", calls.count("web-id"), calls.count("db-id"))
	}
}

func TestDriftMonitorMapsServiceEventsByName(t *testing.T) {
	t.Parallel()
	src, calls, hub := &fakeDriftSource{}, newDriftCalls(), newFakeHub()
	shop := monitorStack(src, "shop-id", "shop", stacks.TargetSwarm)
	monitorStack(src, "box-id", "box", stacks.TargetStandalone)
	held := func(_ context.Context, _ string, swarm bool) (stackops.Held, error) {
		if !swarm {
			return stackops.Held{}, nil
		}
		return stackops.Held{Services: []dockerx.ServiceRow{
			{ID: "svc-1", Name: "shop_web", Stack: shop.Name, Labels: map[string]string{stacks.LabelStackID: shop.ID}},
		}}, nil
	}
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check, HeldFunc: held, Subscribe: hub.Subscribe})
	m.settle(t, "box-id", "shop-id")
	ch := hub.sub(t, 0)

	// What cannot change what a check finds is let go without a timer: a container of a swarm task (it names a
	// namespace, not a project), a health check result, an exec, an image.
	ch <- containerEvent("die", map[string]string{stacks.LabelSwarmNamespace: "shop"})
	ch <- dockerx.EventRow{Type: "container", Action: "health_status", InvalidateOnly: true, Attributes: map[string]string{stacks.LabelComposeProject: "box"}}
	ch <- containerEvent("exec_start", map[string]string{stacks.LabelComposeProject: "box"})
	ch <- dockerx.EventRow{Type: "image", Action: "pull", Name: "nginx"}
	// A service event that names no stack is found among the services each stack owns; one no stack owns finds none.
	ch <- serviceEvent("update", "unrelated_svc", nil)
	ch <- serviceEvent("update", "shop_web", nil)
	// A container names its stack by its Compose project.
	ch <- containerEvent("die", map[string]string{stacks.LabelComposeProject: "box"})
	testsupport.Await(t, "the timer of the unrelated service", m.clk.armed)
	testsupport.Await(t, "the timer of shop_web", m.clk.armed)
	testsupport.Await(t, "the timer of the box container", m.clk.armed)
	m.quiet(t)

	m.clk.Advance(3 * time.Second)
	for _, s := range m.awaitAll(t, "box-id", "shop-id") {
		t.Errorf("the worker also reported %+v, want only the box container and the shop service to be checked", s)
	}
	if calls.count("shop-id") != 2 || calls.count("box-id") != 2 {
		t.Errorf("checks of shop and box = %d and %d, want 2 and 2 (one look, one event each)", calls.count("shop-id"), calls.count("box-id"))
	}
}

func TestDriftMonitorSkipsWhileLocked(t *testing.T) {
	t.Parallel()
	src, calls, hub := &fakeDriftSource{}, newDriftCalls(), newFakeHub()
	web := monitorStack(src, "web-id", "web", stacks.TargetSwarm)
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check, Subscribe: hub.Subscribe})
	m.settle(t, "web-id")

	// A deploy is under way: the event is read, its time runs out, and the worker looks and finds the lock held.
	release, _, ok := m.locks.TryAcquire(monitorEnv, stackops.Holder{StackID: web.ID, StackName: web.Name, Kind: "deploy"})
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer release()
	hub.sub(t, 0) <- serviceEvent("update", "web_web", map[string]string{stacks.LabelSwarmNamespace: web.Name})
	testsupport.Await(t, "the timer of web", m.clk.armed)
	m.clk.Advance(3 * time.Second)
	m.await(t, "", stackops.DriftOutcomeHeld)
	if calls.count("web-id") != 1 {
		t.Errorf("web was checked %d times, want the first look only: a deploy under way is not drift", calls.count("web-id"))
	}
	if _, ok := m.store.Get("web-id"); !ok {
		t.Error("the result of the first look is gone")
	}
}

func TestDriftMonitorRechecksAfterRelease(t *testing.T) {
	t.Parallel()
	src, calls, hub := &fakeDriftSource{}, newDriftCalls(), newFakeHub()
	web := monitorStack(src, "web-id", "web", stacks.TargetSwarm)
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check, Subscribe: hub.Subscribe})
	m.settle(t, "web-id")

	release, _, _ := m.locks.TryAcquire(monitorEnv, stackops.Holder{StackID: web.ID, StackName: web.Name, Kind: "deploy"})
	hub.sub(t, 0) <- serviceEvent("update", "web_web", map[string]string{stacks.LabelSwarmNamespace: web.Name})
	testsupport.Await(t, "the timer of web", m.clk.armed)
	m.clk.Advance(3 * time.Second)
	m.await(t, "", stackops.DriftOutcomeHeld)

	// The deploy ends: the lock is freed and the stack is asked for again. The check the event wanted and the one asked
	// for are one check.
	src.deploy(web.ID)
	release()
	m.mon.Recheck(monitorEnv, web.ID)
	m.await(t, "web-id", stackops.DriftOutcomeStored)
	if calls.count("web-id") != 2 {
		t.Fatalf("web was checked %d times, want the first look and one check after the release", calls.count("web-id"))
	}
	res, ok := m.store.Get("web-id")
	if !ok || res.Revision != 2 {
		t.Errorf("result = %+v, %v, want the one made against the revision the deploy took", res, ok)
	}
}

func TestDriftMonitorDropsAResultComputedUnderALock(t *testing.T) {
	t.Parallel()
	src, calls := &fakeDriftSource{}, newDriftCalls()
	web := monitorStack(src, "web-id", "web", stacks.TargetSwarm)
	entered, proceed := make(chan struct{}, 4), make(chan struct{})
	calls.hook = func(stackops.DriftInput) {
		entered <- struct{}{}
		<-proceed
	}
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check})

	// The check starts, then a deploy takes the lock: what the check found is not stored as drift.
	m.clk.Advance(3 * time.Second)
	testsupport.Await(t, "the check to start", entered)
	release, _, ok := m.locks.TryAcquire(monitorEnv, stackops.Holder{StackID: web.ID, StackName: web.Name, Kind: "deploy"})
	if !ok {
		t.Fatal("could not take the lock")
	}
	close(proceed)
	m.await(t, "web-id", stackops.DriftOutcomeDropped)
	if res, ok := m.store.Get("web-id"); ok {
		t.Fatalf("a result computed while the lock was taken was stored: %+v", res)
	}

	// The deploy ends and asks for a check: the result after it is stored.
	release()
	m.mon.Recheck(monitorEnv, web.ID)
	m.await(t, "web-id", stackops.DriftOutcomeStored)
	if _, ok := m.store.Get("web-id"); !ok {
		t.Error("the check after the deploy stored nothing")
	}
}

func TestDriftMonitorResubscribes(t *testing.T) {
	t.Parallel()
	src, calls, hub := &fakeDriftSource{}, newDriftCalls(), newFakeHub()
	web := monitorStack(src, "web-id", "web", stacks.TargetSwarm)
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check, Subscribe: hub.Subscribe})
	testsupport.Await(t, "the first subscription", hub.opened)
	m.settle(t, "web-id")

	// The hub closes a subscriber that falls behind: the worker waits on the clock and asks again.
	close(hub.sub(t, 0))
	testsupport.Await(t, "the worker to arm its wait before it subscribes again", m.clk.armed)
	m.clk.Advance(time.Second)
	testsupport.Await(t, "the second subscription", hub.opened)

	// And it works from the new one.
	hub.sub(t, 1) <- serviceEvent("update", "web_web", map[string]string{stacks.LabelSwarmNamespace: web.Name})
	testsupport.Await(t, "the timer of web", m.clk.armed)
	m.clk.Advance(3 * time.Second)
	m.await(t, "web-id", stackops.DriftOutcomeStored)
	if calls.count("web-id") != 2 {
		t.Errorf("web was checked %d times, want the first look and the check the new subscription's event asked for", calls.count("web-id"))
	}
}

func TestDriftMonitorOneCheckAtATime(t *testing.T) {
	t.Parallel()
	src, calls := &fakeDriftSource{}, newDriftCalls()
	monitorStack(src, "a-id", "a", stacks.TargetSwarm)
	monitorStack(src, "b-id", "b", stacks.TargetSwarm)
	var inFlight, most atomic.Int32
	calls.hook = func(stackops.DriftInput) {
		n := inFlight.Add(1)
		for {
			cur := most.Load()
			if n <= cur || most.CompareAndSwap(cur, n) {
				break
			}
		}
		// Give a second check, if the worker ran one beside this, every chance to start.
		for range 200 {
			runtime.Gosched()
		}
		inFlight.Add(-1)
	}
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check})

	// Both stacks are due at the first look, and again when both are asked for at once.
	m.settle(t, "a-id", "b-id")
	m.mon.Recheck(monitorEnv, "a-id")
	m.mon.Recheck(monitorEnv, "b-id")
	m.await(t, "a-id", stackops.DriftOutcomeStored)
	m.await(t, "b-id", stackops.DriftOutcomeStored)
	if got := most.Load(); got != 1 {
		t.Errorf("%d checks ran at the same moment on one environment, want one at a time", got)
	}
}

func TestDriftMonitorFingerprintSkipsTheDiff(t *testing.T) {
	t.Parallel()
	src, calls, hub := &fakeDriftSource{}, newDriftCalls(), newFakeHub()
	web := monitorStack(src, "web-id", "web", stacks.TargetSwarm)
	var mu sync.Mutex
	updated := time.Unix(100, 0).UTC()
	held := func(context.Context, string, bool) (stackops.Held, error) {
		mu.Lock()
		defer mu.Unlock()
		return stackops.Held{Services: []dockerx.ServiceRow{
			{ID: "svc-1", Name: "web_web", Stack: web.Name, Desired: 1, UpdatedAt: updated, Labels: map[string]string{stacks.LabelStackID: web.ID}},
		}}, nil
	}
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check, HeldFunc: held, Subscribe: hub.Subscribe})
	m.settle(t, "web-id")

	// Nothing moved at the next timed look: the diff is not made.
	m.clk.Advance(6 * time.Minute)
	m.await(t, "web-id", stackops.DriftOutcomeSkipped)
	testsupport.Await(t, "the worker to arm its next timed look", m.clk.armed)
	if calls.count("web-id") != 1 {
		t.Fatalf("web was checked %d times, want the first look only", calls.count("web-id"))
	}

	// A service changed: the next timed look checks again.
	mu.Lock()
	updated = updated.Add(time.Minute)
	mu.Unlock()
	m.clk.Advance(6 * time.Minute)
	m.await(t, "web-id", stackops.DriftOutcomeStored)
	testsupport.Await(t, "the worker to arm its next timed look", m.clk.armed)
	if calls.count("web-id") != 2 {
		t.Fatalf("web was checked %d times, want a second check for the changed service", calls.count("web-id"))
	}

	// An event is the evidence that something moved, and the cache it is judged by can lag the engine: it checks
	// whatever the fingerprint says.
	hub.sub(t, 0) <- serviceEvent("update", "web_web", map[string]string{stacks.LabelSwarmNamespace: web.Name})
	testsupport.Await(t, "the timer of web", m.clk.armed)
	m.clk.Advance(3 * time.Second)
	m.await(t, "web-id", stackops.DriftOutcomeStored)
	if calls.count("web-id") != 3 {
		t.Errorf("web was checked %d times, want a third check for the event", calls.count("web-id"))
	}
}

// A container the person adds to a standalone stack's Compose project from the host CLI carries the project and no
// bayplan.stack.id. The timed look takes it as the stack's and checks again; a container with another stack's ID is not
// the stack's and does not wake the check (G-08-6, D-22, T-08-103).
func TestDriftMonitorTimedLookSeesAContainerAddedFromTheHostCLI(t *testing.T) {
	t.Parallel()
	src, calls, hub := &fakeDriftSource{}, newDriftCalls(), newFakeHub()
	shop := monitorStack(src, "shop-id", "shop", stacks.TargetStandalone)
	row := func(id string, labels map[string]string) dockerx.ContainerRow {
		labels[stacks.LabelComposeProject] = shop.Name
		return dockerx.ContainerRow{ID: id, Name: "shop-web-" + id, State: "running", ImageID: "sha256:img", Labels: labels}
	}
	var mu sync.Mutex
	containers := []dockerx.ContainerRow{row("c1", map[string]string{stacks.LabelStackID: shop.ID})}
	held := func(context.Context, string, bool) (stackops.Held, error) {
		mu.Lock()
		defer mu.Unlock()
		return stackops.Held{Containers: slices.Clone(containers)}, nil
	}
	add := func(c dockerx.ContainerRow) {
		mu.Lock()
		defer mu.Unlock()
		containers = append(containers, c)
	}
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check, HeldFunc: held, Subscribe: hub.Subscribe})
	m.settle(t, "shop-id")

	// Nothing moved at the next timed look: the check is not made.
	m.clk.Advance(6 * time.Minute)
	m.await(t, "shop-id", stackops.DriftOutcomeSkipped)
	testsupport.Await(t, "the worker to arm its next timed look", m.clk.armed)
	if calls.count("shop-id") != 1 {
		t.Fatalf("shop was checked %d times, want the first look only", calls.count("shop-id"))
	}

	// A container of another stack under the same project name is not the stack's: still nothing moved.
	add(row("c-other", map[string]string{stacks.LabelStackID: "another-stack"}))
	m.clk.Advance(6 * time.Minute)
	m.await(t, "shop-id", stackops.DriftOutcomeSkipped)
	testsupport.Await(t, "the worker to arm its next timed look", m.clk.armed)
	if calls.count("shop-id") != 1 {
		t.Fatalf("shop was checked %d times, want a container with another stack's ID to wake nothing", calls.count("shop-id"))
	}

	// docker compose up --scale adds a container with the project and no ID: the next timed look checks.
	add(row("c2", map[string]string{}))
	m.clk.Advance(6 * time.Minute)
	m.await(t, "shop-id", stackops.DriftOutcomeStored)
	testsupport.Await(t, "the worker to arm its next timed look", m.clk.armed)
	if calls.count("shop-id") != 2 {
		t.Fatalf("shop was checked %d times, want a second check for the container the host CLI added", calls.count("shop-id"))
	}
}

func TestDriftMonitorLeavesAStoppedOrNeverDeployedStackAlone(t *testing.T) {
	t.Parallel()
	src, calls := &fakeDriftSource{}, newDriftCalls()
	monitorStack(src, "halted-id", "halted", stacks.TargetSwarm)
	src.setStopped("halted-id", true)
	// A stack with no revision that deployed has nothing to compare.
	src.mu.Lock()
	src.stacks = append(src.stacks, stackops.Stack{ID: "fresh-id", EnvID: monitorEnv, Name: "fresh", Target: stacks.TargetSwarm})
	src.mu.Unlock()
	monitorStack(src, "live-id", "live", stacks.TargetSwarm)
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: calls.check})

	m.clk.Advance(3 * time.Second)
	// The stacks are looked at in name order.
	m.await(t, "fresh-id", stackops.DriftOutcomeIgnored)
	m.await(t, "halted-id", stackops.DriftOutcomeIgnored)
	m.await(t, "live-id", stackops.DriftOutcomeStored)
	if calls.count("halted-id") != 0 || calls.count("fresh-id") != 0 {
		t.Errorf("a stopped stack was checked %d times and a stack never deployed %d times, want neither", calls.count("halted-id"), calls.count("fresh-id"))
	}
	for _, id := range []string{"halted-id", "fresh-id"} {
		if res, ok := m.store.Get(id); ok {
			t.Errorf("%s has a stored result %+v", id, res)
		}
	}
}

// A check is given the store whose audit log names the causes (plan 08-22, D-24), and a drift whose cause is not
// known is looked at again by the next timed look, because Bayplan writes the audit row of a scale after the engine has
// changed and an early look can miss it. A drift whose cause is known is let be.
func TestDriftMonitorGivesTheChecksTheAuditLogAndLooksAgainAtAnUnexplainedDrift(t *testing.T) {
	t.Parallel()
	src, hub := &fakeDriftSource{}, newFakeHub()
	web := monitorStack(src, "web-id", "web", stacks.TargetSwarm)
	audit := storetest.New(t)
	var mu sync.Mutex
	var gotAudit []*store.Store
	var latest *stackops.DriftCause
	check := func(_ context.Context, in stackops.DriftInput) (stackops.DriftResult, error) {
		mu.Lock()
		defer mu.Unlock()
		gotAudit = append(gotAudit, in.Audit)
		return stackops.DriftResult{State: stackops.DriftDrifted, Revision: in.Deployed.Seq, CheckedAt: time.Unix(1, 0).UTC(), Latest: latest}, nil
	}
	held := func(context.Context, string, bool) (stackops.Held, error) {
		return stackops.Held{Services: []dockerx.ServiceRow{
			{ID: "svc-1", Name: "web_web", Stack: web.Name, Desired: 1, UpdatedAt: time.Unix(100, 0).UTC(), Labels: map[string]string{stacks.LabelStackID: web.ID}},
		}}, nil
	}
	m := startMonitor(t, monitorEnv, src, stackops.DriftMonitorDeps{DriftFunc: check, HeldFunc: held, Subscribe: hub.Subscribe, Audit: audit})
	m.settle(t, "web-id")

	// Nothing moved, but nobody knows why the stack drifted: the next timed look checks it again.
	m.clk.Advance(6 * time.Minute)
	m.await(t, "web-id", stackops.DriftOutcomeStored)
	testsupport.Await(t, "the worker to arm its next timed look", m.clk.armed)

	// Now the cause is known: nothing moved, so the look makes no check.
	mu.Lock()
	latest = &stackops.DriftCause{Action: stackops.CauseScaled, By: "alice", Service: "web"}
	mu.Unlock()
	m.clk.Advance(6 * time.Minute)
	m.await(t, "web-id", stackops.DriftOutcomeStored)
	testsupport.Await(t, "the worker to arm its next timed look", m.clk.armed)
	m.clk.Advance(6 * time.Minute)
	m.await(t, "web-id", stackops.DriftOutcomeSkipped)

	mu.Lock()
	defer mu.Unlock()
	if len(gotAudit) != 3 {
		t.Fatalf("web was checked %d times, want 3", len(gotAudit))
	}
	for i, a := range gotAudit {
		if a != audit {
			t.Errorf("check %d was given the store %p, want the audit store %p", i, a, audit)
		}
	}
}
