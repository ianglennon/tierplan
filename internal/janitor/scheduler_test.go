// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The scheduler (plan 11-15, JAN-01, D-01, D-02, D-08): time is a clock the test moves, the engine is
// the runner's fake and the lock and the jobs hub are the real ones.

// schedClock is a clock the test moves. After arms a timer and announces it, so that a test that moves
// the clock knows the scheduler (or a waiting run) is waiting on it and never sleeps to find out.
type schedClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []schedTimer
	armed  chan struct{}
}

type schedTimer struct {
	at time.Time
	ch chan time.Time
}

func newSchedClock(now time.Time) *schedClock {
	return &schedClock{now: now, armed: make(chan struct{}, 4096)}
}

func (c *schedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *schedClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, schedTimer{at: c.now.Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *schedClock) Advance(d time.Duration) {
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

// Tick waits for something to arm a timer, which the scheduler does once its round is done, and then
// moves the clock on by d.
func (c *schedClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	testsupport.Await(t, "the scheduler to arm its next tick", c.armed)
	c.Advance(d)
}

// at is a time of the first of January 2026 in UTC, the day the fake containers of these tests are
// two days past their stop.
func at(h, m int) time.Time { return time.Date(2026, time.January, 1, h, m, 0, 0, time.UTC) }

// schedFix is a scheduler on a runner with a fake engine, started when the test asks.
type schedFix struct {
	sch   *Scheduler
	run   *Runner
	eng   *fakeEngine
	hub   *jobs.Hub
	clk   *schedClock
	audit *auditLog
	envs  []string
	pol   func(envID string) Policy
}

func newSchedFix(t *testing.T, now time.Time, eng *fakeEngine, envs ...string) *schedFix {
	t.Helper()
	run, _, hub := newRunner(t, eng)
	clk := newSchedClock(now)
	run.Clock = clk
	f := &schedFix{run: run, eng: eng, hub: hub, clk: clk, audit: audited(run), envs: envs}
	f.pol = func(string) Policy { return DefaultPolicy() }
	run.Policy = func(_ context.Context, id string) (Policy, error) { return f.pol(id), nil }
	f.sch = &Scheduler{
		Envs:   func(context.Context) ([]string, error) { return slices.Clone(f.envs), nil },
		Runner: run, Clock: clk, Loc: time.UTC, Tick: time.Minute,
	}
	return f
}

// start runs the scheduler until the test ends, and waits for its first round: it has armed its tick.
func (f *schedFix) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.sch.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		testsupport.Await(t, "the scheduler to stop", done)
	})
	testsupport.Await(t, "the scheduler's first round", f.clk.armed)
	f.clk.armed <- struct{}{} // the signal is put back for the first Tick
}

// advanceTo ticks the scheduler a tick at a time until the clock has reached t, which it may pass by
// less than a tick: the scheduler only looks when its timer fires.
func (f *schedFix) advanceTo(t *testing.T, to time.Time) {
	t.Helper()
	for f.clk.Now().Before(to) {
		f.clk.Tick(t, f.sch.tick())
	}
}

// The tracer: an environment is cleaned by itself at its slot, as the system, with the trigger
// schedule, through the same runner as Clean now.
func TestSchedulerRunsDueEnvironment(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(2, 59), newFakeEngine(3), "env-1")
	f.start(t)

	want, err := NextRun(DefaultSchedule, "env-1", f.clk.Now(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if next, ok := f.sch.Next("env-1"); !ok || !next.Equal(want) {
		t.Fatalf("Next = %v %v, want %v: 03:00 and the environment's offset", next, ok, want)
	}
	if want.Before(at(3, 0)) || want.After(at(3, 10)) {
		t.Fatalf("the first slot is %v, want 03:00 to 03:10", want)
	}
	if len(f.eng.removeCalls()) != 0 {
		t.Fatal("the janitor cleaned before its slot")
	}

	f.advanceTo(t, want)
	testsupport.Await(t, "the scheduled run to read the environment", f.eng.entered)
	list := f.hub.List(nil)
	if len(list) != 1 {
		t.Fatalf("jobs = %+v, want the one scheduled run", list)
	}
	job, err := f.hub.Wait(testsupport.Context(t), list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Owner != "system:janitor:env-1" || job.Meta[MetaTrigger] != TriggerSchedule {
		t.Fatalf("job = %+v, want owned by system:janitor:env-1 with the trigger schedule", job)
	}
	if got := f.eng.removedIDs(); !slices.Equal(got, []string{"c1"}) {
		t.Fatalf("removed = %v, want the stale container c1 removed, live", got)
	}
	run, items, err := recordOf(f.run).GetRun(testsupport.Context(t), "env-1", job.Meta[MetaRunID])
	if err != nil {
		t.Fatal(err)
	}
	if run.Trigger != TriggerSchedule || run.Actor != SystemActor || run.State != StateCompleted || len(items) != 1 || run.Counts.RemovedContainers != 1 {
		t.Fatalf("run = %+v items %+v, want a completed scheduled run by system:janitor that removed one container", run, items)
	}
	rows := f.audit.rows()
	if len(rows) != 1 || rows[0].Action != audit.ActionJanitorCleaned || rows[0].Actor.ID != SystemActor || rows[0].Details["trigger"] != TriggerSchedule {
		t.Fatalf("audit = %+v, want one janitor.cleaned row by system:janitor with the trigger schedule", rows)
	}
	// The next slot is tomorrow's.
	if next, ok := f.sch.Next("env-1"); !ok || !next.After(at(3, 0).AddDate(0, 0, 1).Add(-time.Second)) || !next.Before(at(3, 10).AddDate(0, 0, 1).Add(time.Second)) {
		t.Fatalf("Next after the run = %v %v, want tomorrow at 03:00 and the offset", next, ok)
	}
}
