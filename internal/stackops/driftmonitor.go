// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The timing of the background drift check (D-22). A stack is checked about every five minutes, with jitter so the
// environments of one server do not all read at the same moment, a few seconds after the last Docker event that names
// it, and when an operation on it ends.
const (
	// DefaultDriftInterval is the period of the timed check of every stack of an environment.
	DefaultDriftInterval = 5 * time.Minute
	// DefaultDriftDebounce is how long the worker waits after an event for more of them before it checks the stack.
	DefaultDriftDebounce = 3 * time.Second

	// driftRetryAfterFailure is how soon a timed check is tried again when a stack could not be read (an environment
	// that is offline, a swarm without a manager). It is held to the interval when that is shorter.
	driftRetryAfterFailure = 30 * time.Second
)

// The kinds of key a pending check is held under: a stack by its ID.
const keyID = "id:"

// DriftSource is what the drift worker reads about the stacks of an environment: Repo, or a stand-in for it in a test.
type DriftSource interface {
	Stacks(ctx context.Context, envID string) ([]Stack, error)
	GoodRevisionInfo(ctx context.Context, stackID string) (DeployedRevision, bool, error)
	LatestGoodRevision(ctx context.Context, stackID string) (Snapshot, bool, error)
	RevisionPins(ctx context.Context, snapshotID string) (map[string]string, error)
}

var _ DriftSource = Repo{}

// DriftMonitorDeps are what a DriftMonitor works with.
type DriftMonitorDeps struct {
	// Ops reads the engines: Drift compares a stack with its deployed revision and Cached reads what an environment holds.
	Ops Deps
	// Repo is where the stacks and their revisions are read from.
	Repo DriftSource
	// Store holds the newest check of each stack, which the lists read.
	Store *DriftStore
	// Locks is the operation lock. No stack of an environment is checked while it is held (a deploy under way is not
	// drift).
	Locks OperationLock
	// Subscribe opens the events of an environment, as the events hub does (Hub.Subscribe). Nil means the worker
	// checks on its timer and when asked only.
	Subscribe func(ctx context.Context, envID string) (backlog []dockerx.EventRow, ch <-chan dockerx.EventRow, cancel func())
	// Clock is the time the timer, the debounce and the re-subscription run on. Nil is the wall clock.
	Clock Clock
	// Interval and Debounce are the period of the timed check and the quiet time after an event. Zero is
	// DefaultDriftInterval and DefaultDriftDebounce.
	Interval, Debounce time.Duration
	// Log receives what the worker could not do. Nil means slog.Default().
	Log *slog.Logger

	// DriftFunc, when set, is the check of one stack in place of Ops.Drift. A test counts the checks with it.
	DriftFunc func(ctx context.Context, in DriftInput) (DriftResult, error)
	// Checked, when set, is told what became of each stack the worker looked at (the DriftOutcome values), and, with
	// an empty stack ID, that a pass held back for an operation. A test waits on it instead of on a length of time.
	Checked func(envID, stackID, outcome string)
}

// What became of a stack the worker looked at.
const (
	// DriftOutcomeStored is a check whose result was stored.
	DriftOutcomeStored = "stored"
	// DriftOutcomeSkipped is a timed look that found the engine as it was at the last stored check, so it made none.
	DriftOutcomeSkipped = "skipped"
	// DriftOutcomeDropped is a check whose result was thrown away because an operation took the lock, or finished,
	// while it ran.
	DriftOutcomeDropped = "dropped"
	// DriftOutcomeFailed is a stack that could not be read.
	DriftOutcomeFailed = "failed"
	// DriftOutcomeIgnored is a stack with nothing to compare: stopped, or never deployed.
	DriftOutcomeIgnored = "ignored"
	// DriftOutcomeHeld is a pass that checked nothing because the environment's operation lock was held.
	DriftOutcomeHeld = "held"
)

// DriftMonitor checks the stacks of every environment it is told about for drift, in the background, and keeps the
// result in the DriftStore for the lists to show (D-22). It has one light worker for each environment, which checks the
// stacks of that environment one at a time, never while the environment's operation lock is held, and never writes to
// an engine (D-23).
type DriftMonitor struct {
	d DriftMonitorDeps

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	workers map[string]*driftWorker
	// early are environments announced before Start.
	early []string
	wg    sync.WaitGroup
}

// NewDriftMonitor is a monitor with no environment yet: Start begins it, and EnvStarted adds an environment.
func NewDriftMonitor(d DriftMonitorDeps) *DriftMonitor {
	if d.Clock == nil {
		d.Clock = RealClock()
	}
	if d.Interval <= 0 {
		d.Interval = DefaultDriftInterval
	}
	if d.Debounce <= 0 {
		d.Debounce = DefaultDriftDebounce
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Store == nil {
		d.Store = NewDriftStore()
	}
	return &DriftMonitor{d: d, workers: map[string]*driftWorker{}}
}

// Start lets the monitor run until ctx ends or Stop is called. The environments announced before it start now.
func (m *DriftMonitor) Start(ctx context.Context) {
	m.mu.Lock()
	if m.ctx != nil || m.stopped {
		m.mu.Unlock()
		return
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	early := m.early
	m.early = nil
	m.mu.Unlock()
	for _, id := range early {
		m.EnvStarted(id)
	}
}

// Stop ends every worker and waits for them. It is safe to call twice, and before Start.
func (m *DriftMonitor) Stop() {
	m.mu.Lock()
	m.stopped = true
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// EnvStarted begins the worker of an environment that is now monitored. It does nothing for one that has a worker.
func (m *DriftMonitor) EnvStarted(envID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	if m.ctx == nil {
		if !slices.Contains(m.early, envID) {
			m.early = append(m.early, envID)
		}
		return
	}
	if _, ok := m.workers[envID]; ok {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	w := &driftWorker{
		m: m, envID: envID, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1),
		dirty: map[string]struct{}{},
	}
	m.workers[envID] = w
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(w.done)
		w.run(ctx)
	}()
}

// EnvStopped ends the worker of an environment that is no longer monitored and waits for it.
func (m *DriftMonitor) EnvStopped(envID string) {
	m.mu.Lock()
	w := m.workers[envID]
	delete(m.workers, envID)
	m.early = slices.DeleteFunc(m.early, func(id string) bool { return id == envID })
	m.mu.Unlock()
	if w != nil {
		w.cancel()
		<-w.done
	}
}

// Recheck asks for a check of a stack as soon as the environment's operation lock is free. The operations on a stack
// call it when they end. It never blocks, and does nothing for an environment with no worker.
func (m *DriftMonitor) Recheck(envID, stackID string) {
	m.mu.Lock()
	w := m.workers[envID]
	m.mu.Unlock()
	if w == nil {
		return
	}
	w.markDirty(keyID + stackID)
}

// jitter spreads d by up to a tenth either way.
func jitter(d time.Duration) time.Duration {
	spread := int64(d) / 10
	if spread <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(2*spread+1)-spread)
}

// driftWorker is the worker of one environment. Only its goroutine reads or writes prints; mu guards what the event
// loop, the debounce timers and Recheck share with it.
type driftWorker struct {
	m      *DriftMonitor
	envID  string
	cancel context.CancelFunc
	done   chan struct{}
	// wake tells the loop that something is ready to check. Capacity one: a second wake while one is pending adds nothing.
	wake chan struct{}

	mu sync.Mutex
	// dirty are the keys ready to be checked.
	dirty   map[string]struct{}
	tickDue bool
}

func (w *driftWorker) clock() Clock { return w.m.d.Clock }

// markDirty makes a key ready to check and wakes the loop.
func (w *driftWorker) markDirty(key string) {
	w.mu.Lock()
	w.dirty[key] = struct{}{}
	w.mu.Unlock()
	w.signal()
}

func (w *driftWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take hands the loop what is ready and clears it.
func (w *driftWorker) take() (keys []string, tick bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.dirty {
		keys = append(keys, k)
	}
	clear(w.dirty)
	tick = w.tickDue
	w.tickDue = false
	return keys, tick
}

// putBack returns keys the pass did not get to.
func (w *driftWorker) putBack(keys []string, tick bool) {
	w.mu.Lock()
	for _, k := range keys {
		w.dirty[k] = struct{}{}
	}
	w.tickDue = w.tickDue || tick
	w.mu.Unlock()
}

// run is the loop of the environment: a timed look at every stack, and a look at a stack that was asked for.
func (w *driftWorker) run(ctx context.Context) {
	d := w.m.d
	// The first look comes a few seconds after the environment starts, when the monitor has probed it.
	tick := w.clock().After(d.Debounce)
	for {
		ticked := false
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-tick:
			ticked = true
			w.mu.Lock()
			w.tickDue = true
			w.mu.Unlock()
		}
		failed := w.pass(ctx)
		if ticked {
			next := jitter(d.Interval)
			if failed {
				next = min(driftRetryAfterFailure, d.Interval)
			}
			tick = w.clock().After(next)
		}
	}
}

func (m *DriftMonitor) drift(ctx context.Context, in DriftInput) (DriftResult, error) {
	if m.d.DriftFunc != nil {
		return m.d.DriftFunc(ctx, in)
	}
	return m.d.Ops.Drift(ctx, in)
}

func (m *DriftMonitor) locked(envID string) bool {
	if m.d.Locks == nil {
		return false
	}
	_, held := m.d.Locks.Held(envID)
	return held
}

// pass checks what is ready, one stack after another. It reports whether a stack could not be read. While the
// environment's operation lock is held it checks nothing and leaves everything for the call that ends the operation.
func (w *driftWorker) pass(ctx context.Context) (failed bool) {
	m := w.m
	if m.locked(w.envID) {
		w.report("", DriftOutcomeHeld)
		return false
	}
	keys, tick := w.take()
	if len(keys) == 0 && !tick {
		return false
	}
	own, err := m.d.Repo.Stacks(ctx, w.envID)
	if err != nil {
		if ctx.Err() == nil {
			m.d.Log.Warn("the drift check could not list the stacks of an environment", "environment", w.envID, "error", err)
		}
		w.putBack(keys, tick)
		return true
	}
	var targets []Stack
	for _, st := range own {
		if tick || slices.Contains(keys, keyID+st.ID) {
			targets = append(targets, st)
		}
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	for i, st := range targets {
		if ctx.Err() != nil || m.locked(w.envID) {
			// An operation took the lock between two stacks: the rest wait for the call that ends it.
			rest := make([]string, 0, len(targets)-i)
			for _, t := range targets[i:] {
				rest = append(rest, keyID+t.ID)
			}
			w.putBack(rest, false)
			if ctx.Err() == nil {
				w.report("", DriftOutcomeHeld)
			}
			return failed
		}
		outcome := w.check(ctx, st)
		if outcome == DriftOutcomeFailed {
			failed = true
		}
		w.report(st.ID, outcome)
	}
	return failed
}

func (w *driftWorker) report(stackID, outcome string) {
	if w.m.d.Checked != nil {
		w.m.d.Checked(w.envID, stackID, outcome)
	}
}

// check is the check of one stack, and says what became of it (the DriftOutcome values).
func (w *driftWorker) check(ctx context.Context, st Stack) string {
	m := w.m
	repo := m.d.Repo
	if _, has, err := repo.GoodRevisionInfo(ctx, st.ID); err != nil {
		w.logFailure(ctx, st, "read the deployed revision", err)
		return DriftOutcomeFailed
	} else if !has || st.Stopped {
		// A stack that was stopped or never deployed has nothing to compare; the list shows no badge for it.
		return DriftOutcomeIgnored
	}
	good, ok, err := repo.LatestGoodRevision(ctx, st.ID)
	if err != nil {
		w.logFailure(ctx, st, "read the deployed revision", err)
		return DriftOutcomeFailed
	}
	if !ok {
		return DriftOutcomeIgnored
	}
	pins, err := repo.RevisionPins(ctx, good.ID)
	if err != nil {
		w.logFailure(ctx, st, "read the images of the deployed revision", err)
		return DriftOutcomeFailed
	}
	cctx, cancel := context.WithTimeout(ctx, driftCheckTimeout)
	defer cancel()
	res, err := m.drift(cctx, DriftInput{EnvID: w.envID, Stack: st, Deployed: &good, Pins: pins})
	if err != nil {
		w.logFailure(ctx, st, "check the stack", err)
		return DriftOutcomeFailed
	}
	m.d.Store.Put(st.ID, res)
	return DriftOutcomeStored
}

func (w *driftWorker) logFailure(ctx context.Context, st Stack, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	// An environment that is offline is the normal reason, and the environment's own banners say so.
	w.m.d.Log.Debug("the drift check could not "+what, "environment", w.envID, "stack", st.ID, "error", err)
}
