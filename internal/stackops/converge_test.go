// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
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

// slotTask is a task of a replicated service's slot.
func slotTask(id string, slot int, state, desired, hash, err string) dockerx.TaskRow {
	r := task(id, state, desired, hash, err)
	r.Slot = slot
	return r
}

// nodeTask is a task of a global service on a node.
func nodeTask(id, node, state, desired, hash, err string) dockerx.TaskRow {
	r := task(id, state, desired, hash, err)
	r.NodeID = node
	return r
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
	go func() { done <- stackops.WaitConverged(ctx, clk, timeout, r.poll, nil, on) }()
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
	if err := stackops.WaitConverged(testsupport.Context(t), clk, time.Minute, r.poll, nil, nil); err != nil {
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

func TestProgressFindsTheSlotsTheSwarmWillNotStartAgain(t *testing.T) {
	t.Parallel()
	const boom = "task: non-zero exit (1)"
	updating := &dockerx.ServiceUpdateStatus{State: "updating", Message: "update in progress"}
	deadRows := []dockerx.TaskRow{slotTask("t2", 1, "failed", "shutdown", "h", boom), slotTask("t1", 1, "failed", "shutdown", "h", boom)}
	for _, tc := range []struct {
		name       string
		detail     dockerx.ServiceDetail
		rows       []dockerx.TaskRow
		live, dead int
		deadErr    string
		exhausted  bool
		settled    bool
	}{
		{name: "restart policy used up", detail: convService("replicated", 1, "h", nil), rows: deadRows,
			live: 0, dead: 1, deadErr: boom, exhausted: true},
		{name: "a restart in its delay", detail: convService("replicated", 1, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t2", 1, "ready", "ready", "h", ""), slotTask("t1", 1, "failed", "shutdown", "h", boom)},
			live: 1, dead: 0},
		{name: "condition none, one of two slots failed", detail: convService("replicated", 2, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t2", 2, "running", "running", "h", ""), slotTask("t1", 1, "failed", "shutdown", "h", "task: non-zero exit (2)")},
			live: 1, dead: 1, deadErr: "task: non-zero exit (2)", exhausted: true},
		{name: "exit 0 under on-failure", detail: convService("replicated", 1, "h", nil),
			rows: []dockerx.TaskRow{{ID: "t1", ServiceID: "svc", Slot: 1, State: "complete", DesiredState: "shutdown", SpecHash: "h", Message: "finished"}},
			live: 0, dead: 1, deadErr: "finished", exhausted: true},
		{name: "every node of a global service", detail: convService("global", 0, "h", nil),
			rows: []dockerx.TaskRow{nodeTask("b", "node-2", "failed", "shutdown", "h", "second"), nodeTask("a", "node-1", "failed", "shutdown", "h", "first")},
			live: 0, dead: 2, deadErr: "second", exhausted: true},
		{name: "one node of a global service", detail: convService("global", 0, "h", nil),
			rows: []dockerx.TaskRow{nodeTask("b", "node-2", "running", "running", "h", ""), nodeTask("a", "node-1", "failed", "shutdown", "h", boom)},
			live: 1, dead: 1, deadErr: boom, settled: true},
		{name: "no task yet", detail: convService("replicated", 1, "h", nil)},
		{name: "only an earlier template ended", detail: convService("replicated", 1, "new", nil),
			rows: []dockerx.TaskRow{slotTask("t1", 1, "failed", "shutdown", "old", boom)}},
		{name: "an update still rolling out", detail: convService("replicated", 1, "h", updating), rows: deadRows,
			live: 0, dead: 1, deadErr: boom},
		{name: "a crash loop the swarm keeps retrying", detail: convService("replicated", 1, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t2", 1, "rejected", "running", "h", "No such image"), slotTask("t1", 1, "rejected", "shutdown", "h", "No such image")},
			live: 1, dead: 0},
		{name: "a slot a scale-down left behind", detail: convService("replicated", 1, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t2", 1, "running", "running", "h", ""), slotTask("t1", 2, "failed", "shutdown", "h", boom)},
			live: 1, dead: 0, settled: true},
		// The swarm fills the lowest free slot number and deletes a dead slot it does not refill when it
		// answers the write, so slot 4 decides nothing (WR-01).
		{name: "a dead slot above the replica count while the swarm makes the missing one", detail: convService("replicated", 3, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t4", 4, "failed", "shutdown", "h", boom), slotTask("t2", 2, "running", "running", "h", ""), slotTask("t1", 1, "running", "running", "h", "")},
			live: 2, dead: 0},
		// Scale-down keeps slots by node balance, so Live counts a live slot above Want, and the dead slot 1
		// is one the swarm is about to delete (a 3 to 2 scale-down before the swarm reconciles).
		{name: "a slot the swarm kept above the replica count beside a dead one it will delete", detail: convService("replicated", 2, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t3", 3, "running", "running", "h", ""), slotTask("t2", 2, "running", "running", "h", ""), slotTask("t1", 1, "failed", "shutdown", "h", boom)},
			live: 2, dead: 1, deadErr: boom, settled: true},
		// The swarm kept slots 1 and 3 after a scale-down and slot 1 then died for good: detection still fires.
		{name: "a dead slot within the replica count beside a live one above it", detail: convService("replicated", 2, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t3", 3, "running", "running", "h", ""), slotTask("t1", 1, "failed", "shutdown", "h", boom)},
			live: 1, dead: 1, deadErr: boom, exhausted: true},
		// A slot being scaled away is desired remove, so it is never live and Live does not over-count.
		{name: "a task a scale-down is taking away", detail: convService("replicated", 1, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t2", 2, "running", "remove", "h", ""), slotTask("t1", 1, "running", "running", "h", "")},
			live: 1, dead: 0, settled: true},
		{name: "shut down by the swarm, not failed", detail: convService("replicated", 1, "h", nil),
			rows: []dockerx.TaskRow{slotTask("t1", 1, "shutdown", "shutdown", "h", "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := stackops.Progress(tc.detail, tc.rows)
			if c.Live != tc.live || c.Dead != tc.dead || c.DeadErr != tc.deadErr {
				t.Errorf("live, dead, dead error = %d, %d, %q; want %d, %d, %q", c.Live, c.Dead, c.DeadErr, tc.live, tc.dead, tc.deadErr)
			}
			if c.Exhausted() != tc.exhausted {
				t.Errorf("Exhausted = %v, want %v (%+v)", c.Exhausted(), tc.exhausted, c)
			}
			if c.Settled() != tc.settled {
				t.Errorf("Settled = %v, want %v (%+v)", c.Settled(), tc.settled, c)
			}
		})
	}
}

func TestProgressOfARestartInItsDelayStillCarriesTheFailedTasksWords(t *testing.T) {
	t.Parallel()
	rows := []dockerx.TaskRow{slotTask("t2", 1, "ready", "ready", "h", ""), slotTask("t1", 1, "failed", "shutdown", "h", "task: non-zero exit (1)")}
	if c := stackops.Progress(convService("replicated", 1, "h", nil), rows); c.Err != "task: non-zero exit (1)" {
		t.Errorf("Err = %q, want the failed task's words, as before", c.Err)
	}
}

var exhaustedRead = stackops.Convergence{Mode: "replicated", Want: 1, Dead: 1, State: stackops.UpdateNone}

func TestWaitConvergedGivesUpOnAServiceTheSwarmWillNotStartAgainAtTheSecondRead(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: exhaustedRead}}}
	done := wait(testsupport.Context(t), clk, 10*time.Minute, r, nil)
	clk.Tick(t, stackops.PollInterval)
	if err := testsupport.Await(t, "the wait to give up", done); !errors.Is(err, stackops.ErrTasksExhausted) {
		t.Fatalf("WaitConverged = %v, want ErrTasksExhausted", err)
	}
	if got := r.count(); got != 2 {
		t.Errorf("reads = %d, want 2", got)
	}
}

func TestWaitConvergedDoesNotTakeOneExhaustedReadForTheEnd(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: exhaustedRead}, {c: converging(0, 1)}, {c: converging(1, 1)}}}
	done := wait(testsupport.Context(t), clk, 10*time.Minute, r, nil)
	clk.Tick(t, stackops.PollInterval)
	clk.Tick(t, stackops.PollInterval)
	if err := testsupport.Await(t, "the wait to return", done); err != nil {
		t.Fatalf("WaitConverged = %v, want nil", err)
	}
	if got := r.count(); got != 3 {
		t.Errorf("reads = %d, want 3", got)
	}
}

func TestWaitConvergedKeepsCountingExhaustedReadsAcrossAFailedRead(t *testing.T) {
	t.Parallel()
	boom := errors.New("no manager answered")
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: exhaustedRead}, {err: boom}, {c: exhaustedRead}}}
	done := wait(testsupport.Context(t), clk, 10*time.Minute, r, nil)
	clk.Tick(t, stackops.PollInterval)
	clk.Tick(t, stackops.PollInterval)
	if err := testsupport.Await(t, "the wait to give up", done); !errors.Is(err, stackops.ErrTasksExhausted) {
		t.Fatalf("WaitConverged = %v, want ErrTasksExhausted", err)
	}
	if got := r.count(); got != 3 {
		t.Errorf("reads = %d, want 3", got)
	}
}

func TestProgressListsTheTasksStillWaitingForAnAddressAndTheOnesStillStopping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		rows     []dockerx.TaskRow
		waiting  []string
		stopping bool
	}{
		{"new and desired running", []dockerx.TaskRow{slotTask("t3", 3, "new", "running", "h", "")}, []string{"t3"}, false},
		{"a new replacement in its restart delay", []dockerx.TaskRow{slotTask("t3", 3, "new", "ready", "h", "")}, []string{"t3"}, false},
		{"new on an earlier template", []dockerx.TaskRow{slotTask("t3", 3, "new", "running", "old", "")}, nil, false},
		{"allocated and pending", []dockerx.TaskRow{slotTask("t3", 3, "pending", "running", "h", "")}, nil, false},
		{"stopping and still running", []dockerx.TaskRow{slotTask("t1", 1, "running", "shutdown", "h", "")}, nil, true},
		{"stopping but still new", []dockerx.TaskRow{slotTask("t1", 1, "new", "shutdown", "h", "")}, nil, false},
		{"rejected and shut down", []dockerx.TaskRow{slotTask("t1", 1, "rejected", "shutdown", "h", "")}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := stackops.Progress(convService("replicated", 3, "h", nil), tc.rows)
			if !slices.Equal(c.Waiting, tc.waiting) || c.Stopping != tc.stopping {
				t.Errorf("waiting, stopping = %v, %v; want %v, %v", c.Waiting, c.Stopping, tc.waiting, tc.stopping)
			}
			if c.Held != 0 {
				t.Errorf("Held = %d, want 0: only the wait times a hold", c.Held)
			}
		})
	}
}

// hookAnswer is what the full-network hook answers on one call.
type hookAnswer struct {
	nf  *stackops.NetworkFull
	err error
}

// fullAfter is a full-network hook that answers each call with the next of answers, the last one forever,
// and counts its calls.
func fullAfter(calls *atomic.Int32, answers ...hookAnswer) func(context.Context) (*stackops.NetworkFull, error) {
	return func(context.Context) (*stackops.NetworkFull, error) {
		i := min(int(calls.Add(1))-1, len(answers)-1)
		return answers[i].nf, answers[i].err
	}
}

// held is a replicated service short of one task, which is new: the swarm has not given it an address.
var held = stackops.Convergence{Mode: "replicated", Running: 2, Desired: 3, Want: 3, State: stackops.UpdateNone, Waiting: []string{"t3"}}

var fullNetwork = &stackops.NetworkFull{Network: "web_default", Subnet: "10.0.1.0/24"}

// waitFull runs WaitConverged with a full-network hook in a goroutine and returns what it ends with.
func waitFull(ctx context.Context, clk stackops.Clock, timeout time.Duration, r *reads,
	full func(context.Context) (*stackops.NetworkFull, error), on func(stackops.Convergence, []dockerx.TaskRow)) <-chan error {
	done := make(chan error, 1)
	go func() { done <- stackops.WaitConverged(ctx, clk, timeout, r.poll, full, on) }()
	return done
}

// repeat is n reads of c.
func repeat(n int, c stackops.Convergence) []readStep {
	out := make([]readStep, n)
	for i := range out {
		out[i] = readStep{c: c}
	}
	return out
}

func wantNetworkFull(t *testing.T, err error) {
	t.Helper()
	var nfe *stackops.NetworkFullError
	if !errors.As(err, &nfe) || nfe.Network != "web_default" || nfe.Subnet != "10.0.1.0/24" || !errors.Is(err, stackops.ErrNetworkFull) {
		t.Fatalf("WaitConverged = %v, want a NetworkFullError for web_default 10.0.1.0/24", err)
	}
}

func TestWaitConvergedAsksAboutTheNetworksOnlyOnceATaskHasBeenNewForTheHold(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: held}}}
	var calls atomic.Int32
	done := waitFull(testsupport.Context(t), clk, 10*time.Minute, r, fullAfter(&calls, hookAnswer{nf: fullNetwork}), nil)
	// Each tick waits for the wait to arm again, so the wait did not end at the read at 14 s, before the hold of
	// 15 s passed. The ninth read, at 16 s, asks.
	for range 8 {
		clk.Tick(t, stackops.PollInterval)
	}
	wantNetworkFull(t, testsupport.Await(t, "the wait to name the full network", done))
	if calls.Load() != 1 || r.count() != 9 {
		t.Errorf("hook calls, reads = %d, %d; want 1, 9: not asked before the hold", calls.Load(), r.count())
	}
}

func TestWaitConvergedDoesNotEndWhileATaskThatHoldsAnAddressIsStopping(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	stopping := held
	stopping.Stopping = true
	r := &reads{steps: append(repeat(10, stopping), readStep{c: held})}
	var calls atomic.Int32
	done := waitFull(testsupport.Context(t), clk, 10*time.Minute, r, fullAfter(&calls, hookAnswer{nf: fullNetwork}), nil)
	for range 10 {
		clk.Tick(t, stackops.PollInterval)
	}
	wantNetworkFull(t, testsupport.Await(t, "the wait to name the full network", done))
	if calls.Load() != 1 || r.count() != 11 {
		t.Errorf("hook calls, reads = %d, %d; want 1, 11: the address of the stopping task may come free", calls.Load(), r.count())
	}
}

func TestWaitConvergedChecksTheNetworksWhileAnUpdateRollsOut(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	updating := held
	updating.State = "updating"
	r := &reads{steps: []readStep{{c: updating}}}
	var calls atomic.Int32
	done := waitFull(testsupport.Context(t), clk, 10*time.Minute, r, fullAfter(&calls, hookAnswer{nf: fullNetwork}), nil)
	for range 8 {
		clk.Tick(t, stackops.PollInterval)
	}
	wantNetworkFull(t, testsupport.Await(t, "the wait to name the full network", done))
}

func TestWaitConvergedRestartsTheHoldOfATaskThatLeftNew(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	left := held
	left.Waiting = nil
	steps := []readStep{{c: held}}
	steps = append(steps, repeat(3, left)...)
	steps = append(steps, readStep{c: held})
	r := &reads{steps: steps}
	var calls atomic.Int32
	done := waitFull(testsupport.Context(t), clk, 10*time.Minute, r, fullAfter(&calls, hookAnswer{nf: fullNetwork}), nil)
	// The task is new again from the read at 8 s, so its fifteen seconds pass at the read at 24 s.
	for range 12 {
		clk.Tick(t, stackops.PollInterval)
	}
	wantNetworkFull(t, testsupport.Await(t, "the wait to name the full network", done))
	if calls.Load() != 1 || r.count() != 13 {
		t.Errorf("hook calls, reads = %d, %d; want 1, 13: the hold started again when the task returned to new", calls.Load(), r.count())
	}
}

func TestWaitConvergedIgnoresAFailedNetworkRead(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: held}}}
	var calls atomic.Int32
	full := fullAfter(&calls, hookAnswer{err: errors.New("no manager answered")}, hookAnswer{nf: fullNetwork})
	done := waitFull(testsupport.Context(t), clk, 10*time.Minute, r, full, nil)
	for range 9 {
		clk.Tick(t, stackops.PollInterval)
	}
	wantNetworkFull(t, testsupport.Await(t, "the wait to name the full network", done))
	if calls.Load() != 2 || r.count() != 10 {
		t.Errorf("hook calls, reads = %d, %d; want 2, 10: a failed read is asked again at the next", calls.Load(), r.count())
	}
}

func TestWaitConvergedKeepsWaitingWhenNoNetworkIsFull(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	r := &reads{steps: []readStep{{c: held}}}
	var calls atomic.Int32
	var mu sync.Mutex
	var lastHeld int
	on := func(c stackops.Convergence, _ []dockerx.TaskRow) {
		mu.Lock()
		defer mu.Unlock()
		lastHeld = c.Held
	}
	done := waitFull(testsupport.Context(t), clk, time.Minute, r, fullAfter(&calls, hookAnswer{}), on)
	for range 30 {
		clk.Tick(t, stackops.PollInterval)
	}
	if err := testsupport.Await(t, "the wait to time out", done); !errors.Is(err, stackops.ErrConvergeTimeout) {
		t.Fatalf("WaitConverged = %v, want ErrConvergeTimeout", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() != 23 || lastHeld != 1 {
		t.Errorf("hook calls, last Held = %d, %d; want 23 (reads at 16 s to 60 s), 1", calls.Load(), lastHeld)
	}
}
