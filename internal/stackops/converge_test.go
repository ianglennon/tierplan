// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// stepClock is a clock a test moves by hand. After registers a timer and announces it on armed, so a
// test that advances the clock knows the code under test is waiting on it and does not sleep to find out.
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []stepTimer
	armed  chan struct{}
}

type stepTimer struct {
	at time.Time
	ch chan time.Time
}

func newStepClock() *stepClock {
	return &stepClock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), armed: make(chan struct{}, 1024)}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, stepTimer{at: c.now.Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// Tick waits for the code under test to arm its next timer and then moves the clock on by d.
func (c *stepClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	testsupport.Await(t, "the wait to arm its next poll", c.armed)
	c.Advance(d)
}

// service builds a service as an inspect reads it, in the mode and with the update status given.
func convService(mode string, replicas uint64, hash string, update *dockerx.ServiceUpdateStatus) dockerx.ServiceDetail {
	return dockerx.ServiceDetail{ID: "svc", Name: "web", Mode: mode, Replicas: replicas, TemplateHash: hash, UpdateStatus: update}
}

func task(id, state, desired, hash, err string) dockerx.TaskRow {
	return dockerx.TaskRow{ID: id, ServiceID: "svc", State: state, DesiredState: desired, SpecHash: hash, Err: err}
}

func TestProgressCountsRunningTasksOnTheCurrentTemplateAgainstWhatTheSwarmWants(t *testing.T) {
	t.Parallel()
	rows := []dockerx.TaskRow{
		task("t4", "running", "running", "new", ""),
		task("t3", "starting", "running", "new", ""),
		task("t2", "running", "running", "old", ""),
		task("t1", "shutdown", "shutdown", "old", ""),
	}
	c := stackops.Progress(convService("replicated", 3, "new", nil), rows)
	if c.Running != 1 || c.Desired != 3 || c.Want != 3 || c.State != stackops.UpdateNone {
		t.Fatalf("progress = %+v, want 1 of 3 running on the new template, wanting 3, no update", c)
	}
	if c.Settled() {
		t.Error("a service with 1 of 3 tasks on its current template reads as settled")
	}
}

func TestProgressOfAGlobalServiceWantsAsManyTasksAsTheSwarmMadeOneForEachEligibleNode(t *testing.T) {
	t.Parallel()
	rows := []dockerx.TaskRow{task("a", "running", "running", "h", ""), task("b", "running", "running", "h", "")}
	c := stackops.Progress(convService("global", 0, "h", nil), rows)
	if c.Running != 2 || c.Desired != 2 || c.Want != 2 || !c.Settled() {
		t.Fatalf("progress = %+v, want 2 of 2 and settled", c)
	}
	if none := stackops.Progress(convService("global", 0, "h", nil), nil); none.Settled() {
		t.Errorf("a global service with no task yet reads as settled: %+v", none)
	}
}

func TestProgressReadsTheUpdateStatusTheNewestTaskErrorAndAnImagePull(t *testing.T) {
	t.Parallel()
	up := &dockerx.ServiceUpdateStatus{State: "updating", Message: "update in progress"}
	rows := []dockerx.TaskRow{
		task("t3", "preparing", "running", "h", ""),
		task("t2", "rejected", "shutdown", "h", "No such image: web:2"),
		task("t1", "failed", "shutdown", "h", "older error"),
	}
	c := stackops.Progress(convService("replicated", 1, "h", up), rows)
	if c.State != "updating" || c.Message != "update in progress" || c.Err != "No such image: web:2" || !c.Pulling || c.Settled() {
		t.Fatalf("progress = %+v, want an update in progress, the newest error and a pull", c)
	}
}

func TestASettledServiceNeedsNoUpdateStillRollingOut(t *testing.T) {
	t.Parallel()
	running := []dockerx.TaskRow{task("t", "running", "running", "h", "")}
	for _, tc := range []struct {
		name    string
		service dockerx.ServiceDetail
		rows    []dockerx.TaskRow
		settled bool
	}{
		{"running and no update status", convService("replicated", 1, "h", nil), running, true},
		{"running and the update completed", convService("replicated", 1, "h", &dockerx.ServiceUpdateStatus{State: "completed"}), running, true},
		{"running while the update still rolls out", convService("replicated", 1, "h", &dockerx.ServiceUpdateStatus{State: "updating"}), running, false},
		{"running while a rollback still rolls out", convService("replicated", 1, "h", &dockerx.ServiceUpdateStatus{State: "rollback_started"}), running, false},
		{"scaled to zero with nothing left", convService("replicated", 0, "h", nil), nil, true},
		{"scaled to zero with a task still going down", convService("replicated", 0, "h", nil), []dockerx.TaskRow{task("t", "running", "running", "h", "")}, false},
		{"no task made yet for a replica", convService("replicated", 2, "h", nil), nil, false},
	} {
		if got := stackops.Progress(tc.service, tc.rows).Settled(); got != tc.settled {
			t.Errorf("%s: settled = %v, want %v", tc.name, got, tc.settled)
		}
	}
}

func TestAPausedOrRolledBackUpdateIsStopped(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"paused", "rollback_paused", "rollback_completed"} {
		c := stackops.Progress(convService("replicated", 1, "h", &dockerx.ServiceUpdateStatus{State: state}), nil)
		if !c.Stopped() || c.Settled() {
			t.Errorf("%s: stopped = %v, settled = %v, want stopped and not settled", state, c.Stopped(), c.Settled())
		}
	}
}

func TestAnUpdateStatusFromBeforeTheWriteIsNotThisDeploys(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	old := &dockerx.ServiceUpdateStatus{State: "rollback_completed", Message: "rollback completed", StartedAt: started, CompletedAt: started.Add(time.Minute)}
	same := *old
	c := stackops.Progress(convService("replicated", 1, "h", &same), nil).IgnoringUpdate(old)
	if c.Stopped() || c.State != stackops.UpdateNone {
		t.Errorf("the status read before the write still stops the wait: %+v", c)
	}
	newer := *old
	newer.StartedAt = started.Add(time.Hour)
	if c := stackops.Progress(convService("replicated", 1, "h", &newer), nil).IgnoringUpdate(old); !c.Stopped() {
		t.Errorf("a new status is ignored: %+v", c)
	}
	if c := stackops.Progress(convService("replicated", 1, "h", &same), nil).IgnoringUpdate(nil); !c.Stopped() {
		t.Errorf("a service with no earlier status ignored its status: %+v", c)
	}
}

// reads scripts the polls of a wait: each call answers the next of its steps, the last one forever.
type reads struct {
	mu    sync.Mutex
	steps []readStep
	calls int
}

type readStep struct {
	c    stackops.Convergence
	rows []dockerx.TaskRow
	err  error
}

func (r *reads) poll(context.Context) (stackops.Convergence, []dockerx.TaskRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := min(r.calls, len(r.steps)-1)
	r.calls++
	return r.steps[i].c, r.steps[i].rows, r.steps[i].err
}

func (r *reads) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func converging(running, want int) stackops.Convergence {
	return stackops.Convergence{Mode: "replicated", Running: running, Desired: want, Want: want, State: stackops.UpdateNone}
}

// wait runs WaitConverged in a goroutine and returns what it ends with.
func wait(ctx context.Context, clk stackops.Clock, timeout time.Duration, r *reads, on func(stackops.Convergence, []dockerx.TaskRow)) <-chan error {
	done := make(chan error, 1)
	go func() { done <- stackops.WaitConverged(ctx, clk, timeout, r.poll, on) }()
	return done
}

func TestWaitConvergedPollsEveryTwoSecondsAndReturnsWhenTheServiceSettles(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: converging(0, 3)}, {c: converging(1, 3)}, {c: converging(3, 3)}}}
	var seen []int
	var mu sync.Mutex
	done := wait(testsupport.Context(t), clk, 10*time.Minute, r, func(c stackops.Convergence, _ []dockerx.TaskRow) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, c.Running)
	})
	clk.Tick(t, stackops.PollInterval)
	clk.Tick(t, stackops.PollInterval)
	if err := testsupport.Await(t, "the wait to return", done); err != nil {
		t.Fatalf("WaitConverged = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != 0 || seen[1] != 1 || seen[2] != 3 {
		t.Errorf("the caller saw %v, want every read in order: 0, 1, 3", seen)
	}
	if stackops.PollInterval != 2*time.Second {
		t.Errorf("PollInterval = %v, want 2s", stackops.PollInterval)
	}
}

func TestWaitConvergedReadsOnceAtOnceWithoutWaiting(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: converging(1, 1)}}}
	if err := stackops.WaitConverged(testsupport.Context(t), clk, time.Minute, r.poll, nil); err != nil {
		t.Fatalf("WaitConverged = %v, want nil", err)
	}
	if r.count() != 1 {
		t.Errorf("reads = %d, want 1", r.count())
	}
	select {
	case <-clk.armed:
		t.Error("a service that had settled still waited on the clock")
	default:
	}
}

func TestWaitConvergedReturnsTheTimeoutErrorWhenTheTimeoutPasses(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: converging(0, 1)}}}
	done := wait(testsupport.Context(t), clk, time.Minute, r, nil)
	// 30 polls of 2 s reach the minute exactly; the wait ends at the poll that finds the timeout passed.
	for range 30 {
		clk.Tick(t, stackops.PollInterval)
	}
	if err := testsupport.Await(t, "the wait to time out", done); !errors.Is(err, stackops.ErrConvergeTimeout) {
		t.Fatalf("WaitConverged = %v, want ErrConvergeTimeout", err)
	}
	if got := r.count(); got != 31 {
		t.Errorf("reads = %d, want 31 (one at once and one every 2 s up to the minute)", got)
	}
}

func TestWaitConvergedFailsWhenTheSwarmPausesOrRollsBackTheUpdate(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"paused", "rollback_completed"} {
		c := converging(0, 1)
		c.State = state
		r := &reads{steps: []readStep{{c: converging(0, 1)}, {c: c}}}
		clk := newStepClock()
		done := wait(testsupport.Context(t), clk, time.Hour, r, nil)
		clk.Tick(t, stackops.PollInterval)
		if err := testsupport.Await(t, "the wait to stop", done); !errors.Is(err, stackops.ErrUpdateStopped) {
			t.Errorf("%s: WaitConverged = %v, want ErrUpdateStopped", state, err)
		}
	}
}

func TestWaitConvergedBearsAFewFailedReadsAndGivesUpOnTheThird(t *testing.T) {
	t.Parallel()
	boom := errors.New("no manager answered")
	bear := &reads{steps: []readStep{{err: boom}, {err: boom}, {c: converging(1, 1)}}}
	clk := newStepClock()
	done := wait(testsupport.Context(t), clk, time.Hour, bear, nil)
	clk.Tick(t, stackops.PollInterval)
	clk.Tick(t, stackops.PollInterval)
	if err := testsupport.Await(t, "the wait to return", done); err != nil {
		t.Fatalf("two failed reads then a good one: WaitConverged = %v, want nil", err)
	}

	giveUp := &reads{steps: []readStep{{err: boom}}}
	clk = newStepClock()
	done = wait(testsupport.Context(t), clk, time.Hour, giveUp, nil)
	clk.Tick(t, stackops.PollInterval)
	clk.Tick(t, stackops.PollInterval)
	if err := testsupport.Await(t, "the wait to give up", done); !errors.Is(err, boom) {
		t.Fatalf("three failed reads: WaitConverged = %v, want the read's error", err)
	}
}

func TestWaitConvergedEndsWithTheContextsError(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: converging(0, 1)}}}
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := wait(ctx, clk, time.Hour, r, nil)
	testsupport.Await(t, "the wait to arm its first poll", clk.armed)
	cancel()
	if err := testsupport.Await(t, "the wait to end", done); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitConverged = %v, want context.Canceled", err)
	}
}
