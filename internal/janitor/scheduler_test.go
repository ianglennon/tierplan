// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
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

// round ticks the scheduler once and waits for the round it makes to be over: it has armed its next tick.
func (f *schedFix) round(t *testing.T) {
	t.Helper()
	f.clk.Tick(t, f.sch.tick())
	testsupport.Await(t, "the scheduler to finish its round", f.clk.armed)
	f.clk.armed <- struct{}{}
}

// ended is told when a run the scheduler started has ended and its environment is free again.
func (f *schedFix) endedOn() <-chan string {
	ch := make(chan string, 64)
	f.sch.Ended = func(envID, _ string) { ch <- envID }
	return ch
}

// runIDs are the runs on record, whatever their state, by the ID of their environment.
func (f *schedFix) runsOf(envID string) []Run {
	var out []Run
	for _, r := range recordOf(f.run).runsNow() {
		if r.EnvID == envID {
			out = append(out, r)
		}
	}
	return out
}

// The scheduler never catches up (D-02): started at 05:00 with a 03:00 schedule, the first run is
// tomorrow's slot, and a day's worth of ticks before it start nothing.
func TestSchedulerNoCatchUp(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(5, 0), newFakeEngine(3), "env-1")
	f.start(t)
	next, ok := f.sch.Next("env-1")
	tomorrow := at(3, 0).AddDate(0, 0, 1)
	if !ok || next.Before(tomorrow) || !next.Before(tomorrow.Add(MaxJitter)) {
		t.Fatalf("Next = %v %v, want tomorrow 03:00 plus the offset: today's slot is missed and not made up", next, ok)
	}
	// Twenty hours of ticks, up to tomorrow's slot.
	for f.clk.Now().Before(tomorrow.Add(-time.Hour)) {
		f.round(t)
	}
	if got := f.hub.List(nil); len(got) != 0 || len(f.eng.removeCalls()) != 0 {
		t.Fatalf("jobs = %+v removals = %v before the slot, want none", got, f.eng.removeCalls())
	}
	if runs := f.runsOf("env-1"); len(runs) != 0 {
		t.Fatalf("runs on record = %+v before the slot, want none", runs)
	}
}

// A host that was suspended past its slot runs once when it wakes, not once for every slot it missed.
func TestSchedulerLateRunsOnce(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(2, 0), newFakeEngine(3), "env-1")
	ended := f.endedOn()
	f.start(t)
	f.clk.Tick(t, 5*24*time.Hour) // the machine slept for five days, and the next look is five days on
	testsupport.Await(t, "the late run to end", ended)
	// Its next slot is worked out from now, not from the slot it missed, so nothing follows at once.
	if next, ok := f.sch.Next("env-1"); !ok || !next.After(f.clk.Now()) {
		t.Fatalf("Next = %v %v, want a slot after now (%v)", next, ok, f.clk.Now())
	}
	f.round(t)
	if runs := f.runsOf("env-1"); len(runs) != 1 {
		t.Fatalf("runs = %+v, want one run for the five missed slots", runs)
	}
}

// A disabled environment never runs, and has no next run.
func TestSchedulerDisabled(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(2, 59), newFakeEngine(3), "env-1")
	var mu sync.Mutex
	enabled := false
	f.pol = func(string) Policy {
		mu.Lock()
		defer mu.Unlock()
		p := DefaultPolicy()
		p.Enabled = enabled
		return p
	}
	ended := f.endedOn()
	f.start(t)
	if next, ok := f.sch.Next("env-1"); ok {
		t.Fatalf("Next of a disabled environment = %v, want none", next)
	}
	f.advanceTo(t, at(4, 0))
	if got := f.hub.List(nil); len(got) != 0 {
		t.Fatalf("a disabled environment started %+v", got)
	}

	// Turned on, it has a slot from the next look, and the slot is the next one, not the one it missed.
	mu.Lock()
	enabled = true
	mu.Unlock()
	f.round(t)
	next, ok := f.sch.Next("env-1")
	if !ok || next.Before(at(3, 0).AddDate(0, 0, 1)) {
		t.Fatalf("Next after enabling = %v %v, want tomorrow's slot", next, ok)
	}
	// Turned off again, the slot is dropped.
	mu.Lock()
	enabled = false
	mu.Unlock()
	f.round(t)
	if next, ok := f.sch.Next("env-1"); ok {
		t.Fatalf("Next after disabling = %v, want none", next)
	}
	select {
	case id := <-ended:
		t.Fatalf("a run of %s ended, want none", id)
	default:
	}
}

// A changed schedule moves the next run at the next look.
func TestSchedulerScheduleChange(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(2, 0), newFakeEngine(3), "env-1")
	var mu sync.Mutex
	spec := "0 3 * * *"
	f.pol = func(string) Policy {
		mu.Lock()
		defer mu.Unlock()
		p := DefaultPolicy()
		p.Schedule = spec
		return p
	}
	f.start(t)
	first, _ := f.sch.Next("env-1")
	mu.Lock()
	spec = "0 5 * * *"
	mu.Unlock()
	f.round(t)
	second, ok := f.sch.Next("env-1")
	want, _ := NextRun("0 5 * * *", "env-1", f.clk.Now(), time.UTC)
	if !ok || !second.Equal(want) || !second.After(first) {
		t.Fatalf("Next after the change = %v %v, want %v", second, ok, want)
	}
	// A schedule that cannot be used is not run, and has no next run.
	mu.Lock()
	spec = "* * * * *"
	mu.Unlock()
	f.round(t)
	if next, ok := f.sch.Next("env-1"); ok {
		t.Fatalf("Next of an unusable schedule = %v, want none", next)
	}
}

// gatedEngine holds every Snapshot until the gate is closed and counts the reads in flight, so a test
// sees how many runs are at work at once.
type gatedEngine struct {
	*fakeEngine
	gate chan struct{}
	// in receives once for every read that has begun.
	in chan struct{}

	mu       sync.Mutex
	inFlight int
	most     int
}

func (g *gatedEngine) Snapshot(ctx context.Context, env string) (Snapshot, error) {
	g.mu.Lock()
	g.inFlight++
	g.most = max(g.most, g.inFlight)
	g.mu.Unlock()
	g.in <- struct{}{}
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}()
	select {
	case <-g.gate:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	return g.fakeEngine.Snapshot(ctx, env)
}

func (g *gatedEngine) mostAtOnce() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.most
}

// Three environments that are due together run two at a time (T-11-43): the third waits for a free place
// and then runs, and no environment runs twice.
func TestSchedulerConcurrency(t *testing.T) {
	t.Parallel()
	g := &gatedEngine{fakeEngine: newFakeEngine(3), gate: make(chan struct{}), in: make(chan struct{}, 16)}
	f := newSchedFix(t, at(2, 59), g.fakeEngine, "env-a", "env-b", "env-c")
	f.run.Engine = g
	ended := f.endedOn()
	f.start(t)

	var last time.Time
	for _, id := range f.envs {
		next, ok := f.sch.Next(id)
		if !ok {
			t.Fatalf("no next run for %s", id)
		}
		last = laterOf(last, next)
	}
	f.advanceTo(t, last.Add(time.Minute))
	f.round(t) // the look that makes sure every due environment has had its chance
	list := f.hub.List(nil)
	if len(list) != MaxConcurrentRuns {
		t.Fatalf("%d runs started with all three due and the engine held, want %d", len(list), MaxConcurrentRuns)
	}

	for range MaxConcurrentRuns {
		testsupport.Await(t, "a run to be at work", g.in)
	}
	close(g.gate)
	for range MaxConcurrentRuns {
		testsupport.Await(t, "a run to end", ended)
	}
	f.round(t) // a place is free: the third environment, still due, starts
	testsupport.Await(t, "the third run to end", ended)
	if got := g.mostAtOnce(); got != MaxConcurrentRuns {
		t.Fatalf("at most %d runs were at work at once, want exactly %d", got, MaxConcurrentRuns)
	}
	for _, id := range f.envs {
		if runs := f.runsOf(id); len(runs) != 1 || runs[0].State != StateCompleted || runs[0].Trigger != TriggerSchedule {
			t.Errorf("runs of %s = %+v, want one completed scheduled run", id, runs)
		}
	}
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// holdLock takes the environment's lock as a deploy would and answers the function that frees it.
func holdLock(t *testing.T, f *schedFix, envID string) func() {
	t.Helper()
	release, _, ok := f.run.Locks.TryAcquire(envID, stackops.Holder{Kind: "deploy", By: "ada"})
	if !ok {
		t.Fatal("the lock was not free")
	}
	return release
}

// scheduledRequest is the request the scheduler makes for an environment.
func scheduledRequest(envID string) RunRequest {
	return RunRequest{EnvID: envID, Trigger: TriggerSchedule, Actor: "TierPlan", ActorID: SystemActor, Owner: "system:janitor:" + envID, All: true}
}

// waitingOn starts a scheduled run on an environment whose lock is held and waits for the run to poll.
func waitingOn(t *testing.T, f *schedFix, envID string) Started {
	t.Helper()
	started, err := f.run.Start(testsupport.Context(t), scheduledRequest(envID))
	if err != nil {
		t.Fatalf("a scheduled run on a held environment = %v, want it started and waiting", err)
	}
	testsupport.Await(t, "the waiting run to poll the lock", f.clk.armed)
	f.clk.armed <- struct{}{}
	run, _, err := recordOf(f.run).GetRun(testsupport.Context(t), envID, started.RunID)
	if err != nil || run.State != StateWaiting || run.JobID != started.JobID {
		t.Fatalf("run = %+v %v, want it on record as waiting", run, err)
	}
	return started
}

// pollOnce moves the waiting run's clock one poll on and waits for it to have tried the lock again.
func pollOnce(t *testing.T, f *schedFix) {
	t.Helper()
	f.clk.Tick(t, LockPoll)
	testsupport.Await(t, "the waiting run to poll again", f.clk.armed)
	f.clk.armed <- struct{}{}
}

// D-08, T-11-44: a scheduled run polls a held lock every 10 s for 10 minutes and then ends skipped
// with the reason deploy_in_progress, having removed nothing and left the lock to its holder.
func TestScheduledRunSkipsAfterTenMinutes(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(3, 0), newFakeEngine(3))
	release := holdLock(t, f, "env-1")
	defer release()
	started := waitingOn(t, f, "env-1")

	// 59 polls in, 590 s have passed and the run is still waiting.
	for range int(LockWait/LockPoll) - 1 {
		pollOnce(t, f)
	}
	run, _, err := recordOf(f.run).GetRun(testsupport.Context(t), "env-1", started.RunID)
	if err != nil || run.State != StateWaiting || !run.FinishedAt.IsZero() {
		t.Fatalf("run after 590 s = %+v %v, want still waiting", run, err)
	}
	if got := f.eng.removeCalls(); len(got) != 0 {
		t.Fatalf("removals asked while waiting: %v", got)
	}

	// The last poll is at 600 s: the lock is still held and the run gives up.
	testsupport.Await(t, "the last poll", f.clk.armed)
	f.clk.Advance(LockPoll)
	job, err := f.hub.Wait(testsupport.Context(t), started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	run, items, err := recordOf(f.run).GetRun(testsupport.Context(t), "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != StateSkipped || run.Reason != ReasonDeployInProgress || run.FinishedAt.IsZero() || run.Trigger != TriggerSchedule || run.Actor != SystemActor || len(items) != 0 || run.Counts != (Counts{}) {
		t.Fatalf("run = %+v items %+v, want skipped with deploy_in_progress, ended, by the system, with nothing counted", run, items)
	}
	if want := at(3, 10); !run.FinishedAt.Equal(want) {
		t.Errorf("the run ended at %v, want ten minutes after it started: %v", run.FinishedAt, want)
	}
	if job.State != jobs.JobDone || len(job.Items) != 0 {
		t.Errorf("job = %s with %d items, want it ended with none", job.State, len(job.Items))
	}
	select {
	case <-f.eng.entered:
		t.Fatal("the run read the environment under a deploy")
	default:
	}
	if got := f.eng.removeCalls(); len(got) != 0 {
		t.Fatalf("removals asked under a deploy: %v", got)
	}
	// The deploy still holds its lock: the skipped run neither took nor freed it.
	if held, ok := f.run.Locks.Held("env-1"); !ok || held.Kind != "deploy" {
		t.Fatalf("held = %+v %v, want the deploy's lock untouched", held, ok)
	}
	rows := f.audit.rows()
	if len(rows) != 1 || rows[0].Actor.ID != SystemActor || rows[0].Details["outcome"] != StateSkipped || rows[0].Details["reason"] != ReasonDeployInProgress || rows[0].Details["trigger"] != TriggerSchedule {
		t.Fatalf("audit = %+v, want one janitor.cleaned row by system:janitor: skipped, deploy_in_progress", rows)
	}
}

// A lock freed after three minutes lets the waiting run go ahead and clean, as a run that never waited.
func TestScheduledRunProceedsWhenFreed(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(3, 0), newFakeEngine(3))
	release := holdLock(t, f, "env-1")
	started := waitingOn(t, f, "env-1")
	for range int(3*time.Minute/LockPoll) - 1 {
		pollOnce(t, f)
	}
	if got := f.eng.removeCalls(); len(got) != 0 {
		t.Fatalf("removals asked while the deploy held the lock: %v", got)
	}
	release() // the deploy ends at three minutes
	testsupport.Await(t, "the last poll", f.clk.armed)
	f.clk.Advance(LockPoll)
	if _, err := f.hub.Wait(testsupport.Context(t), started.JobID); err != nil {
		t.Fatal(err)
	}
	run, items, err := recordOf(f.run).GetRun(testsupport.Context(t), "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != StateCompleted || run.Reason != "" || run.JobID != started.JobID || len(items) != 1 || run.Counts.RemovedContainers != 1 {
		t.Fatalf("run = %+v items %+v, want a completed run that removed the stale container", run, items)
	}
	if got := f.eng.removedIDs(); !slices.Equal(got, []string{"c1"}) {
		t.Fatalf("removed = %v, want c1", got)
	}
	if held, ok := f.run.Locks.Held("env-1"); ok {
		t.Fatalf("the lock is still held by %+v after the run", held)
	}
}

// Clean now keeps its answer: it does not wait for a held environment, only a schedule does.
func TestManualRunDoesNotWait(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(3, 0), newFakeEngine(3))
	release := holdLock(t, f, "env-1")
	defer release()
	_, err := f.run.Start(testsupport.Context(t), RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.Holder.Kind != "deploy" {
		t.Fatalf("Clean now on a held environment = %v, want a LockedError naming the deploy", err)
	}
	if runs := f.runsOf("env-1"); len(runs) != 0 {
		t.Fatalf("a refused Clean now left %+v on record", runs)
	}
}

// A waiting run whose job is cancelled ends on record as failed and frees nothing it never held.
func TestScheduledRunCancelledWhileWaiting(t *testing.T) {
	t.Parallel()
	f := newSchedFix(t, at(3, 0), newFakeEngine(3))
	release := holdLock(t, f, "env-1")
	defer release()
	started := waitingOn(t, f, "env-1")
	if err := f.hub.Cancel(started.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Wait(testsupport.Context(t), started.JobID); err != nil {
		t.Fatal(err)
	}
	run, _, err := recordOf(f.run).GetRun(testsupport.Context(t), "env-1", started.RunID)
	if err != nil || run.State != StateFailed || run.FinishedAt.IsZero() {
		t.Fatalf("run = %+v %v, want ended failed", run, err)
	}
	if held, ok := f.run.Locks.Held("env-1"); !ok || held.Kind != "deploy" {
		t.Fatalf("held = %+v %v, want the deploy's lock untouched", held, ok)
	}
}

// The held-out check through the scheduler: the runs it fires across the autumn change of clock are the
// ones NextRuns shows, the repeated hour's included.
func TestSchedulerFiresWhereNextRunsShows(t *testing.T) {
	t.Parallel()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)
	f := newSchedFix(t, start, newFakeEngine(3), "env-1")
	f.sch.Loc = london
	f.pol = func(string) Policy {
		p := DefaultPolicy()
		p.Schedule = "30 1 * * *"
		return p
	}
	ended := f.endedOn()
	f.start(t)
	shown, err := NextRuns("30 1 * * *", "env-1", start, london, 2)
	if err != nil || len(shown) != 2 {
		t.Fatalf("NextRuns = %v %v", shown, err)
	}
	for i, at := range shown {
		if next, ok := f.sch.Next("env-1"); !ok || !next.Equal(at) {
			t.Fatalf("run %d: Next = %v %v, want what NextRuns showed: %v", i, next, ok, at)
		}
		f.advanceTo(t, at)
		testsupport.Await(t, "the run to end", ended)
		if started := recordOf(f.run).runsNow(); len(started) != i+1 {
			t.Fatalf("after %v there are %d runs on record, want %d", at, len(started), i+1)
		}
	}
}
