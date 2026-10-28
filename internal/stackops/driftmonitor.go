// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
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
	// driftResubscribeStart and driftResubscribeMax bound the wait before the worker asks the hub for its events
	// again after the hub closed the subscription; it doubles up to the maximum and starts again at an event.
	driftResubscribeStart = time.Second
	driftResubscribeMax   = 30 * time.Second
)

// The events the worker acts on. A swarm stack is judged by its services, so only a service event names one; a
// standalone stack by its containers, and the actions that leave a container changed, stopped or gone.
const (
	driftEventService   = "service"
	driftEventContainer = "container"
)

var driftContainerActions = map[string]bool{
	"create": true, "destroy": true, "start": true, "stop": true, "die": true, "kill": true, "update": true, "rename": true,
}

// The kinds of key a pending check is held under: a stack named by an event, a service named by an event that did
// not name its stack, a stack by its ID.
const (
	keyName    = "name:"
	keyService = "svc:"
	keyID      = "id:"
)

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
	// Audit is the store whose audit log names the causes of the drift a check finds (D-24). Nil leaves them out.
	Audit *store.Store
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

	// DriftFunc, when set, is the check of one stack in place of Ops.Drift, and HeldFunc the read of what an
	// environment holds in place of Ops.Cached. A test counts the checks and feeds the fingerprint with them.
	DriftFunc func(ctx context.Context, in DriftInput) (DriftResult, error)
	HeldFunc  func(ctx context.Context, envID string, swarm bool) (Held, error)
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
		timers: map[string]struct{}{}, dirty: map[string]struct{}{}, prints: map[string]string{},
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
	return d + time.Duration(rand.Int64N(2*spread+1)-spread) //nolint:gosec // jitter that spreads reads, not a secret
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
	wg   sync.WaitGroup

	mu sync.Mutex
	// timers are the keys whose debounce has not run out; dirty are the keys ready to be checked.
	timers  map[string]struct{}
	dirty   map[string]struct{}
	tickDue bool

	// prints is the fingerprint of the engine's state at each stack's last stored check.
	prints map[string]string
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

// run is the loop of the environment. It does no reading of its own besides the pass: an event only notes the stack it
// names, so the hub never finds the subscription behind.
func (w *driftWorker) run(ctx context.Context) {
	d := w.m.d
	var (
		events    <-chan dockerx.EventRow
		unsub     = func() {}
		resub     <-chan time.Time
		backoff   = driftResubscribeStart
		subscribe = func() {
			if d.Subscribe == nil {
				return
			}
			_, ch, cancel := d.Subscribe(ctx, w.envID)
			events, unsub = ch, cancel
		}
	)
	defer func() {
		unsub()
		w.wg.Wait()
	}()
	subscribe()
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
		case ev, ok := <-events:
			if !ok {
				// The hub closes a subscriber that falls behind, and a stream that ends: ask again after a pause.
				unsub()
				events, unsub = nil, func() {}
				resub = w.clock().After(backoff)
				backoff = min(backoff*2, driftResubscribeMax)
				continue
			}
			backoff = driftResubscribeStart
			w.onEvent(ctx, ev)
			continue
		case <-resub:
			resub = nil
			subscribe()
			continue
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

// onEvent notes the stack an event names and starts its debounce. Events that cannot change what a check finds are let
// go: a health check result, another stack's kind of object, a container of a swarm task.
func (w *driftWorker) onEvent(ctx context.Context, ev dockerx.EventRow) {
	if ev.InvalidateOnly {
		return
	}
	var key string
	switch ev.Type {
	case driftEventService:
		if ns := ev.Attributes[stacks.LabelSwarmNamespace]; ns != "" {
			key = keyName + ns
		} else if ev.Name != "" {
			key = keyService + ev.Name
		}
	case driftEventContainer:
		if !driftContainerActions[ev.Action] {
			return
		}
		if p := ev.Attributes[stacks.LabelComposeProject]; p != "" {
			key = keyName + p
		}
	}
	if key == "" {
		return
	}
	w.mu.Lock()
	if _, ok := w.timers[key]; ok {
		w.mu.Unlock()
		return
	}
	w.timers[key] = struct{}{}
	w.mu.Unlock()
	due := w.clock().After(w.m.d.Debounce)
	w.wg.Go(func() {
		select {
		case <-due:
			w.mu.Lock()
			delete(w.timers, key)
			w.dirty[key] = struct{}{}
			w.mu.Unlock()
			w.signal()
		case <-ctx.Done():
		}
	})
}

// heldCache is what the environment holds, read at most once for each kind of target in a pass.
type heldCache struct {
	w    *driftWorker
	read [2]*heldRead
}

type heldRead struct {
	held Held
	err  error
}

func (c *heldCache) get(ctx context.Context, swarm bool) (Held, error) {
	i := 0
	if swarm {
		i = 1
	}
	if c.read[i] == nil {
		h, err := c.w.m.heldOf(ctx, c.w.envID, swarm)
		c.read[i] = &heldRead{held: h, err: err}
	}
	return c.read[i].held, c.read[i].err
}

func (m *DriftMonitor) heldOf(ctx context.Context, envID string, swarm bool) (Held, error) {
	if m.d.HeldFunc != nil {
		return m.d.HeldFunc(ctx, envID, swarm)
	}
	return m.d.Ops.Cached(ctx, envID, swarm)
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
	held := &heldCache{w: w}
	// timed is the stacks the timer asked for; the rest were named by an event or an operation that ended.
	timed := map[string]bool{}
	var targets []Stack
	add := func(st Stack, byTimer bool) {
		if !slices.ContainsFunc(targets, func(t Stack) bool { return t.ID == st.ID }) {
			targets = append(targets, st)
		}
		if byTimer {
			timed[st.ID] = true
		}
	}
	for _, st := range own {
		if tick {
			add(st, true)
		}
	}
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, keyID):
			if i := slices.IndexFunc(own, func(s Stack) bool { return s.ID == strings.TrimPrefix(k, keyID) }); i >= 0 {
				add(own[i], false)
			}
		case strings.HasPrefix(k, keyName):
			if i := slices.IndexFunc(own, func(s Stack) bool { return s.Name == strings.TrimPrefix(k, keyName) }); i >= 0 {
				add(own[i], false)
			}
		case strings.HasPrefix(k, keyService):
			// A service event that did not name its stack: the service is found among what each stack owns.
			svc := strings.TrimPrefix(k, keyService)
			h, err := held.get(ctx, true)
			if err != nil {
				continue
			}
			for _, st := range own {
				if st.Target == stacks.TargetSwarm && slices.ContainsFunc(h.RowsOf(st), func(r ServiceRow) bool { return r.Name == svc }) {
					add(st, false)
				}
			}
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
		outcome := w.check(ctx, st, held, timed[st.ID])
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
func (w *driftWorker) check(ctx context.Context, st Stack, held *heldCache, timed bool) string {
	m := w.m
	repo := m.d.Repo
	info, has, err := repo.GoodRevisionInfo(ctx, st.ID)
	if err != nil {
		w.logFailure(ctx, st, "read the deployed revision", err)
		return DriftOutcomeFailed
	}
	// A stack that was stopped or never deployed has nothing to compare; the list shows no badge for it.
	if !has || st.Stopped {
		return DriftOutcomeIgnored
	}

	// What the engine holds is read from the caches the lists use. When it is the same as at the last stored check, a
	// timed look has nothing to find. An event or an operation that ended always looks: the cache can be a few seconds
	// behind the engine, and the event is the evidence that something moved.
	print := ""
	if h, herr := held.get(ctx, st.Target == stacks.TargetSwarm); herr == nil {
		print = heldFingerprint(h, st, info.Seq)
	}
	if timed && print != "" && w.prints[st.ID] == print {
		// A drift whose cause is not known is looked at again: Bayplan writes the audit row of a scale after the
		// engine has changed, so an early look can miss it, and nothing else would give it a second chance.
		if cur, ok := m.d.Store.Get(st.ID); ok && DriftCurrent(cur, st, info.Seq) && (cur.State != DriftDrifted || cur.Latest != nil) {
			return DriftOutcomeSkipped
		}
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
	res, err := m.drift(cctx, DriftInput{EnvID: w.envID, Stack: st, Deployed: &good, Pins: pins, Audit: m.d.Audit})
	if err != nil {
		w.logFailure(ctx, st, "check the stack", err)
		return DriftOutcomeFailed
	}

	// A check that ran while an operation took the lock, or that an operation finished under, describes a state that
	// is gone. It is dropped; the call that ends the operation asks for the check that counts.
	if m.locked(w.envID) {
		return DriftOutcomeDropped
	}
	now, hasNow, err := repo.GoodRevisionInfo(ctx, st.ID)
	if err != nil || !hasNow || now.Seq != info.Seq {
		return DriftOutcomeDropped
	}
	if fresh, ferr := m.stackOf(ctx, w.envID, st.ID); ferr != nil || fresh.Stopped != st.Stopped {
		return DriftOutcomeDropped
	}
	m.d.Store.Put(st.ID, res)
	w.prints[st.ID] = print
	return DriftOutcomeStored
}

// stackOf is the stack as it is stored now.
func (m *DriftMonitor) stackOf(ctx context.Context, envID, stackID string) (Stack, error) {
	all, err := m.d.Repo.Stacks(ctx, envID)
	if err != nil {
		return Stack{}, err
	}
	for _, s := range all {
		if s.ID == stackID {
			return s, nil
		}
	}
	return Stack{}, ErrStackNotFound
}

func (w *driftWorker) logFailure(ctx context.Context, st Stack, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	// An environment that is offline is the normal reason, and the environment's own banners say so.
	w.m.d.Log.Debug("the drift check could not "+what, "environment", w.envID, "stack", st.ID, "error", err)
}

// heldFingerprint names what the cached state of the environment holds for the stack, with the revision that is
// deployed and whether it is running: a service of a swarm by its ID, when it last changed, how many replicas it wants
// and its image, a container of a host by its ID, state and image. It changes whenever the stack does. A host
// container counts when it is the stack's or was added to one of its services from the host CLI, with no
// bayplan.stack.id (SplitProject), so docker compose up --scale moves it; another stack's container does not.
func heldFingerprint(h Held, st Stack, seq int64) string {
	own := OwnershipOf(st)
	var parts []string
	if st.Target == stacks.TargetSwarm {
		for _, s := range h.Services {
			if own.OwnsService(s) {
				parts = append(parts, fmt.Sprintf("%s:%d:%d:%s", s.ID, s.UpdatedAt.UnixNano(), s.Desired, s.Image))
			}
		}
	} else {
		mine, unlabelled, _ := SplitProject(own, h.Containers, func(c dockerx.ContainerRow) map[string]string { return c.Labels })
		for _, c := range append(mine, unlabelled...) {
			parts = append(parts, c.ID+":"+c.State+":"+c.ImageID)
		}
	}
	sort.Strings(parts)
	return fmt.Sprintf("rev%d,%s", seq, strings.Join(parts, ","))
}
