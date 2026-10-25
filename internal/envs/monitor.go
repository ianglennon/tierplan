// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package envs keeps the managed environments: which Docker hosts an admin
// opted in (the registry), and whether each is reachable right now (the
// monitor).
package envs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/engine"
)

// State is what the monitor currently believes about an environment.
type State string

// States.
const (
	// StateUnknown is an environment whose monitor has not probed yet, or is
	// not running.
	StateUnknown State = "unknown"
	// StateOnline is an environment whose engine answered and is supported.
	StateOnline State = "online"
	// StateOffline is an environment whose engine did not answer.
	StateOffline State = "offline"
	// StateUnsupported is an environment whose engine answered but is not one
	// Bayplan manages (too old, Podman, rootless, Docker Desktop).
	StateUnsupported State = "unsupported"
)

// Status is the monitor's view of one environment.
type Status struct {
	State State
	// Since is when the environment entered State. For an offline environment
	// it is the first failed probe, not the latest, so the UI can say
	// "Offline since 14:03" (D-26).
	Since time.Time
	// Reason is the dockerx reason code for offline and unsupported states.
	Reason string
	// LastAttempt is when the most recent probe finished.
	LastAttempt time.Time
	// Probe is the result of the most recent probe.
	Probe dockerx.ProbeResult
}

// Errors from Monitor.Client. A handler that gets one answers with the
// full-page offline or unsupported state instead of an error.
var (
	ErrOffline      = errors.New("envs: the environment is offline")
	ErrUnsupported  = errors.New("envs: the environment's engine is not supported")
	errNotMonitored = errors.New("envs: the environment is not monitored")
)

// Probe timing (D-26). While an engine answers it is probed every 30s with a
// 5s timeout; after a failure the retry delay starts at 1s, doubles, and is
// capped at 30s, with jitter so several environments do not retry in step.
const (
	defaultInterval     = 30 * time.Second
	defaultProbeTimeout = 5 * time.Second
	backoffStart        = time.Second
	backoffMax          = 30 * time.Second
)

// entry is one monitored environment.
type entry struct {
	client *dockerx.Client
	cancel context.CancelFunc
	done   chan struct{}
	// kick asks the loop for an immediate probe. Capacity one: a second kick
	// while one is pending adds nothing.
	kick chan struct{}
	// probed is closed once the first probe has finished, whatever it found.
	probed     chan struct{}
	probedOnce sync.Once

	mu sync.RWMutex
	st Status
}

// Monitor probes each started environment from one goroutine per environment.
// It is safe for concurrent use.
type Monitor struct {
	log   *slog.Logger
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// interval and probeTimeout are the online probe period and the per-probe
	// timeout. They are fields so tests can shorten them.
	interval, probeTimeout time.Duration

	mu      sync.Mutex
	entries map[string]*entry
	wg      sync.WaitGroup
	live    atomic.Int64
	// changed is closed and replaced whenever a status may have changed: an
	// environment starts, a probe result is stored, or one stops. AwaitStatus
	// waits on it instead of polling. Guarded by mu.
	changed chan struct{}
	// watchers are told when an environment starts or stops being monitored
	// (the events hub, plan 21). They are called after the monitor's lock is
	// released, so a callback may call back into the monitor.
	watchers []watcher
}

// watcher is a pair of callbacks for the lifecycle of monitored environments.
type watcher struct {
	started, stopped func(envID string)
}

// NewMonitor returns an empty Monitor. now and sleep default to the real
// clock; tests inject them to observe backoff without waiting. sleep must
// return early with the context's error when the context ends.
func NewMonitor(log *slog.Logger, now func() time.Time, sleep func(context.Context, time.Duration) error) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = realSleep
	}
	return &Monitor{
		log: log, now: now, sleep: sleep,
		interval: defaultInterval, probeTimeout: defaultProbeTimeout,
		entries: map[string]*entry{},
		changed: make(chan struct{}),
	}
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// watch registers callbacks for environments that start and stop being
// monitored. started runs after a Start that began a new loop, stopped after
// Stop or StopAll ended one.
func (m *Monitor) watch(started, stopped func(envID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watchers = append(m.watchers, watcher{started: started, stopped: stopped})
}

// Watch is watch for a worker outside this package (the background drift check of stacks). Register it before
// the first environment starts: an environment that is already monitored is not announced.
func (m *Monitor) Watch(started, stopped func(envID string)) { m.watch(started, stopped) }

func (m *Monitor) currentWatchers() []watcher {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]watcher(nil), m.watchers...)
}

// Start begins monitoring envID through t, with its first probe at once. The
// loop ends when ctx ends or Stop is called. Starting an environment that is
// already monitored does nothing, so there is exactly one loop per environment.
func (m *Monitor) Start(ctx context.Context, envID string, t engine.Transport) error {
	began, err := m.start(ctx, envID, t)
	if err != nil || !began {
		return err
	}
	for _, w := range m.currentWatchers() {
		w.started(envID)
	}
	return nil
}

// start is Start without the watchers; began reports whether a loop was started.
func (m *Monitor) start(ctx context.Context, envID string, t engine.Transport) (began bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.entries[envID]; ok {
		select {
		case <-old.done:
			// Its context ended and the loop exited; start a fresh one.
			_ = old.client.Close()
			delete(m.entries, envID)
		default:
			return false, nil
		}
	}
	cli, err := dockerx.New(t)
	if err != nil {
		return false, fmt.Errorf("monitor %s: %w", envID, err)
	}
	lctx, cancel := context.WithCancel(ctx)
	e := &entry{
		client: cli, cancel: cancel,
		done:   make(chan struct{}),
		kick:   make(chan struct{}, 1),
		probed: make(chan struct{}),
		st:     Status{State: StateUnknown},
	}
	m.entries[envID] = e
	m.notifyLocked()
	m.live.Add(1)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(e.done)
		// Deferred last, so it runs first: loopCount must already be down when
		// done closes and Stop returns, or a caller that counts right after Stop
		// still sees the loop it just waited for.
		defer m.live.Add(-1)
		m.run(lctx, envID, e)
	}()
	return true, nil
}

// Stop ends monitoring envID and waits for its loop to exit. It does nothing
// for an environment that is not monitored.
func (m *Monitor) Stop(envID string) {
	m.mu.Lock()
	e, ok := m.entries[envID]
	delete(m.entries, envID)
	m.notifyLocked()
	m.mu.Unlock()
	if ok {
		m.stop(e)
		for _, w := range m.currentWatchers() {
			w.stopped(envID)
		}
	}
}

// StopAll ends every monitor and waits for them. The server calls it before it
// closes the database.
func (m *Monitor) StopAll() {
	m.mu.Lock()
	all := make([]*entry, 0, len(m.entries))
	ids := make([]string, 0, len(m.entries))
	for id, e := range m.entries {
		all = append(all, e)
		ids = append(ids, id)
		delete(m.entries, id)
	}
	m.notifyLocked()
	m.mu.Unlock()
	for _, e := range all {
		m.stop(e)
	}
	for _, id := range ids {
		for _, w := range m.currentWatchers() {
			w.stopped(id)
		}
	}
}

func (m *Monitor) stop(e *entry) {
	e.cancel()
	<-e.done
	_ = e.client.Close()
}

func (m *Monitor) entry(envID string) (*entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[envID]
	return e, ok
}

// Status returns the current status of envID: StateUnknown until the first
// probe finishes, and for an environment that is not monitored.
func (m *Monitor) Status(envID string) Status {
	e, ok := m.entry(envID)
	if !ok {
		return Status{State: StateUnknown}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st
}

// AwaitProbe waits until the first probe of envID has finished, or ctx ends, or
// the monitor stops. It returns at once for an environment that is already
// probed or not monitored. Right after an environment is added its state is
// still unknown; a handler waits here instead of reporting it offline.
func (m *Monitor) AwaitProbe(ctx context.Context, envID string) {
	e, ok := m.entry(envID)
	if !ok {
		return
	}
	select {
	case <-e.probed:
	case <-e.done:
	case <-ctx.Done():
	}
}

// notify wakes every AwaitStatus waiter.
func (m *Monitor) notify() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifyLocked()
}

// notifyLocked is notify with m.mu held: it closes the broadcast channel and
// replaces it, so a waiter that took the old one is woken and a later waiter
// gets a fresh one.
func (m *Monitor) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// AwaitStatus returns the first status of envID for which cond is true. It
// evaluates cond now and again after every start, probe result and stop of any
// monitored environment, and never polls. An environment that is not monitored
// yet reads StateUnknown and is waited for like any other (unlike AwaitProbe,
// which returns at once). When ctx ends first it returns the last status
// together with the context's error; the context only guards against a hang.
func (m *Monitor) AwaitStatus(ctx context.Context, envID string, cond func(Status) bool) (Status, error) {
	for {
		// Take the channel before reading the status, so a change between the
		// read and the wait is not missed.
		m.mu.Lock()
		ch := m.changed
		m.mu.Unlock()
		st := m.Status(envID)
		if cond(st) {
			return st, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return st, ctx.Err()
		}
	}
}

// Kick asks for an immediate probe of envID, for "Retry now". It returns at
// once and does nothing for an environment that is not monitored.
func (m *Monitor) Kick(envID string) {
	e, ok := m.entry(envID)
	if !ok {
		return
	}
	select {
	case e.kick <- struct{}{}:
	default: // one is already pending
	}
}

// Client returns the Docker client of envID together with its status when the
// environment is online. For any other state it returns ErrOffline or
// ErrUnsupported with the status, so a handler can show the full-page state;
// for an unknown state (no probe finished yet) it returns ErrOffline too.
func (m *Monitor) Client(envID string) (*dockerx.Client, Status, error) {
	e, ok := m.entry(envID)
	if !ok {
		return nil, Status{State: StateUnknown}, errNotMonitored
	}
	e.mu.RLock()
	st := e.st
	e.mu.RUnlock()
	switch st.State {
	case StateOnline:
		return e.client, st, nil
	case StateUnsupported:
		return nil, st, ErrUnsupported
	default:
		return nil, st, ErrOffline
	}
}

// loopCount is the number of monitor goroutines running, for tests.
func (m *Monitor) loopCount() int { return int(m.live.Load()) }

// run is the one loop of an environment: probe, record, wait, repeat.
func (m *Monitor) run(ctx context.Context, envID string, e *entry) {
	failures := 0
	for {
		pctx, cancel := context.WithTimeout(ctx, m.probeTimeout)
		res, err := e.client.Probe(pctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		reached := m.record(envID, e, res, err)
		var wait time.Duration
		if reached {
			failures = 0
			wait = m.interval
		} else {
			failures++
			wait = backoff(failures)
		}
		if !m.wait(ctx, e, wait) {
			return
		}
	}
}

// wait sleeps for d, or less when the environment is kicked. It reports false
// when ctx ended, which ends the loop.
func (m *Monitor) wait(ctx context.Context, e *entry, d time.Duration) bool {
	wctx, cancel := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-e.kick:
			cancel()
		case <-wctx.Done():
		}
	}()
	_ = m.sleep(wctx, d)
	cancel()
	<-watched
	return ctx.Err() == nil
}

// backoff is the retry delay after the nth consecutive failure: 1s, 2s, 4s and
// so on up to 30s, then jittered into the upper half of that step so the delay
// never exceeds the cap.
func backoff(failures int) time.Duration {
	step := backoffStart
	for i := 1; i < failures && step < backoffMax; i++ {
		step *= 2
	}
	step = min(step, backoffMax)
	half := step / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not a security decision
}

// statusFromProbe turns one probe result into a Status stamped now, and
// reports whether the engine answered (online or unsupported). Since is now;
// the monitor keeps the earlier one while the state does not change.
func statusFromProbe(res dockerx.ProbeResult, probeErr error, now time.Time) (st Status, reached bool) {
	st = Status{LastAttempt: now, Since: now, Probe: res}
	switch {
	case probeErr != nil || !res.Reachable:
		st.State = StateOffline
		st.Reason = res.Reason
		if st.Reason == "" {
			st.Reason = dockerx.ReasonUnreachable
		}
	case !res.Supported:
		st.State, st.Reason, reached = StateUnsupported, res.Reason, true
	default:
		st.State, reached = StateOnline, true
	}
	return st, reached
}

// record stores the result of a probe and reports whether the engine answered
// (online or unsupported), as opposed to a failure that starts backoff.
func (m *Monitor) record(envID string, e *entry, res dockerx.ProbeResult, probeErr error) bool {
	now := m.now()
	st, reached := statusFromProbe(res, probeErr, now)

	e.mu.Lock()
	prev := e.st
	if prev.State == st.State {
		st.Since = prev.Since // the state began at the first probe that showed it
	} else {
		st.Since = now
	}
	e.st = st
	e.mu.Unlock()
	e.probedOnce.Do(func() { close(e.probed) })
	m.notify()

	if prev.State != st.State {
		attrs := []any{"environment", envID, "from", prev.State, "to", st.State}
		if st.Reason != "" {
			attrs = append(attrs, "reason", st.Reason)
		}
		if res.Detail != "" {
			attrs = append(attrs, "detail", res.Detail)
		}
		m.log.Info("environment status changed", attrs...)
	}
	return reached
}
