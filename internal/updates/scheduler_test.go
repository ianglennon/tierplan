// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/schedule"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The scheduler of the update checks (plan 12-09, IMG-04, D-01): time is a clock the test moves, the
// registry is the fake of the checker's tests and the store is the real one.

// schedClock is a clock the test moves. After arms a timer and announces it, so that a test that moves
// the clock knows the scheduler is waiting on it and never sleeps to find out.
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

// Tick waits for the scheduler to arm its next tick, which it does once its round is done, and then moves
// the clock on by d.
func (c *schedClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	testsupport.Await(t, "the scheduler to arm its next tick", c.armed)
	c.Advance(d)
}

const (
	tickLen  = time.Minute
	imageRef = "ghcr.io/acme/app:2"
)

// schedFix is a scheduler over the rig, on a clock the test moves. Its environments run one container each
// on the image imageRef, from the digest dg('1'), and the registry says the tag has moved to dg('9').
type schedFix struct {
	*rig
	sch   *updates.Scheduler
	clk   *schedClock
	pol   *updates.PolicyStore
	ended chan []string
	ids   []string
	snap  *countingSnap
}

// countingSnap is the aggregate of the environments, and how many times it was read. It has only the read
// of a Snapshotter: nothing the scheduler is given can change an engine.
type countingSnap struct {
	mu    sync.Mutex
	agg   envs.Aggregate
	byEnv map[string]envs.Aggregate
	reads int
}

func (c *countingSnap) Snapshot(_ context.Context, envID string) (envs.Aggregate, envs.Health, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if a, ok := c.byEnv[envID]; ok {
		return a, envs.Health{}, nil
	}
	return c.agg, envs.Health{}, nil
}

// newSchedFix makes the environments in the database and a scheduler over them that has not started. The
// clock starts at start, in UTC.
func newSchedFix(t *testing.T, start time.Time, ids ...string) *schedFix {
	t.Helper()
	r := newRig(t)
	clk := newSchedClock(start)
	r.chk.Now, r.svc.Now = clk.Now, clk.Now
	for _, id := range ids {
		r.addEnvironment(t, id)
	}
	snap := &countingSnap{agg: aggOf([]envs.ImageRow{img(dg('c'), "ghcr.io/acme/app@"+dg('1'))}, ctr("web", imageRef, dg('c'), nil))}
	r.svc.Aggregator = snap
	r.heads.set(imageRef, answer{digest: dg('9')})
	pol := &updates.PolicyStore{Q: r.st.Q, W: r.st.Write}
	f := &schedFix{rig: r, clk: clk, pol: pol, ended: make(chan []string, 64), ids: ids, snap: snap}
	f.sch = &updates.Scheduler{
		Envs:   func(context.Context) ([]string, error) { return slices.Clone(f.ids), nil },
		Policy: pol, Service: r.svc, Clock: clk, Loc: time.UTC, Tick: tickLen,
		Ended: func(ids []string) { f.ended <- ids },
	}
	r.svc.StaleAfter = f.sch.StaleAfter
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

// advanceTo ticks the scheduler a tick at a time until the clock has reached to.
func (f *schedFix) advanceTo(t *testing.T, to time.Time) {
	t.Helper()
	for f.clk.Now().Before(to) {
		f.clk.Tick(t, tickLen)
	}
}

// slotOf is the first scheduled check of an environment after at, by the default schedule.
func slotOf(t *testing.T, id string, after time.Time) time.Time {
	t.Helper()
	at, err := schedule.NextRun(updates.DefaultSchedule, id, after, time.UTC, updates.MinInterval)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// The tracer: with nobody pressing anything, an environment is checked at its 6-hourly slot, and the
// verdict of what it runs changes from not checked to update available.
func TestSchedulerChecksDueEnvironment(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	const env = "env-a"
	day := time.Date(2026, time.November, 3, 5, 0, 0, 0, time.UTC)
	slot := slotOf(t, env, day) // 06:00 and this environment's offset
	f := newSchedFix(t, slot.Add(-time.Minute-time.Second), env)
	f.start(t)

	if v, err := f.svc.ContainerVerdicts(ctx, env, f.snap.agg); err != nil || v["web"].State != updates.StateNotChecked {
		t.Fatalf("before the slot the container reads %+v, %v, want not_checked", v["web"], err)
	}
	if n := f.heads.requests(); n != 0 {
		t.Fatalf("%d registry requests before the slot, want none", n)
	}

	// A tick short of the slot checks nothing.
	f.clk.Tick(t, tickLen)
	if n := f.heads.requests(); n != 0 {
		t.Fatalf("%d registry requests one second before the slot, want none", n)
	}
	if next, ok := f.sch.Next(env); !ok || !next.Equal(slot) {
		t.Fatalf("Next = %v, %v, want %v", next, ok, slot)
	}

	// The tick that passes the slot starts the check, and the Ended seam says it is over.
	f.clk.Tick(t, tickLen)
	if got := testsupport.Await(t, "the scheduled check to end", f.ended); !slices.Equal(got, []string{env}) {
		t.Fatalf("the check covered %v, want [%s]", got, env)
	}
	if n := f.heads.requests(); n != 1 {
		t.Errorf("%d registry requests, want 1", n)
	}
	if got := stored(t, f.rig, "ghcr.io", "acme/app", "2", ""); got.Digest != dg('9') || got.Error != "" {
		t.Errorf("stored check = %+v, want the digest the tag moved to", got)
	}
	v, err := f.svc.ContainerVerdicts(ctx, env, f.snap.agg)
	if err != nil {
		t.Fatal(err)
	}
	if v["web"].State != updates.StateUpdateAvailable || v["web"].RunningDigest != dg('1') || v["web"].RemoteDigest != dg('9') {
		t.Errorf("after the slot the container reads %+v, want update_available", v["web"])
	}
	if next, ok := f.sch.Next(env); !ok || !next.After(slot) || next.Sub(slot) > 7*time.Hour {
		t.Errorf("Next = %v, %v after the check, want the following slot, six hours on", next, ok)
	}
}

// --- Plan 12-09 task 2: no catch-up, no writes, and staleness that follows the schedule ---

// off switches the scheduled checks of an environment off by its own policy.
func (f *schedFix) setEnv(t *testing.T, id string, o updates.Overrides) {
	t.Helper()
	ctx := testsupport.Context(t)
	_, version, err := f.pol.Env(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pol.SetEnv(ctx, f.st.Q, id, o, version, "test", f.clk.Now()); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
}

// roundDone waits for the scheduler to finish the round it is in: it has armed its next tick.
func (f *schedFix) roundDone(t *testing.T) {
	t.Helper()
	testsupport.Await(t, "the scheduler to finish its round", f.clk.armed)
	f.clk.armed <- struct{}{}
}

// A server that starts at 07:00 does not make up the 06:00 check it missed: the first check of an
// environment is its next slot after the start.
func TestSchedulerNoCatchUp(t *testing.T) {
	t.Parallel()
	const env = "env-a"
	start := time.Date(2026, time.November, 3, 7, 0, 0, 0, time.UTC)
	slot := slotOf(t, env, start)
	if want := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC); slot.Before(want) || slot.Sub(want) > schedule.MaxJitter {
		t.Fatalf("the next slot after 07:00 is %v, want 12:00 and an offset", slot)
	}
	f := newSchedFix(t, start, env)
	f.start(t)
	if next, ok := f.sch.Next(env); !ok || !next.Equal(slot) {
		t.Fatalf("Next = %v, %v, want %v", next, ok, slot)
	}
	// An hour on is still before the slot: nothing was made up.
	f.clk.Tick(t, time.Hour)
	f.roundDone(t)
	if n := f.heads.requests(); n != 0 {
		t.Fatalf("%d registry requests before the slot, want none", n)
	}
	// The scheduler was not looked at for longer than a gap (a stopped clock, a busy host): the one tick
	// that comes late finds the slot, and the slot after it, already past.
	f.clk.Tick(t, slot.Sub(f.clk.Now())+7*time.Hour)
	testsupport.Await(t, "the check at the slot", f.ended)
	if n := f.heads.requests(); n != 1 {
		t.Errorf("%d registry requests, want 1: the slot that was passed once is checked once", n)
	}
	// A check that ran late is not followed by a catch-up: the next slot is the one after now.
	if next, ok := f.sch.Next(env); !ok || !next.After(f.clk.Now()) {
		t.Errorf("Next = %v, %v at %v, want a slot still to come", next, ok, f.clk.Now())
	}
}

// An environment whose policy switches the checks off is never checked on schedule, and Next says so; it
// is checked again from the next slot once it is switched on.
func TestSchedulerDisabled(t *testing.T) {
	t.Parallel()
	const env = "env-a"
	start := time.Date(2026, time.November, 3, 5, 0, 0, 0, time.UTC)
	f := newSchedFix(t, start, env)
	off := false
	f.setEnv(t, env, updates.Overrides{Enabled: &off})
	f.start(t)
	if _, ok := f.sch.Next(env); ok {
		t.Fatal("Next answers true for an environment whose checks are off")
	}
	f.advanceTo(t, start.Add(13*time.Hour)) // two slots
	f.roundDone(t)
	if n := f.heads.requests(); n != 0 || f.snap.reads != 0 {
		t.Errorf("%d registry requests and %d reads of the engine for an environment that is off, want none", n, f.snap.reads)
	}
	if got := f.sch.StaleAfter(env); got != updates.DefaultStaleAfter {
		t.Errorf("StaleAfter = %v for an environment that is off, want %v", got, updates.DefaultStaleAfter)
	}

	// Switched on again, it is checked at its next slot.
	f.setEnv(t, env, updates.Overrides{})
	f.clk.Tick(t, tickLen)
	f.roundDone(t)
	next, ok := f.sch.Next(env)
	if !ok {
		t.Fatal("Next answers false after the checks were switched on")
	}
	f.advanceTo(t, next)
	testsupport.Await(t, "the check at the slot", f.ended)
	if n := f.heads.requests(); n != 1 {
		t.Errorf("%d registry requests once it is on, want 1", n)
	}
}

// An environment's own schedule is the one it follows; reset to NULL it follows the default again.
func TestSchedulerFollowsItsPolicy(t *testing.T) {
	t.Parallel()
	const env = "env-a"
	start := time.Date(2026, time.November, 3, 7, 0, 0, 0, time.UTC)
	f := newSchedFix(t, start, env)
	f.setEnv(t, env, updates.Overrides{Schedule: ptr("0 3 * * *"), Preset: ptr(schedule.PresetDaily)})
	f.start(t)
	daily, _ := schedule.NextRun("0 3 * * *", env, start, time.UTC, updates.MinInterval)
	if next, ok := f.sch.Next(env); !ok || !next.Equal(daily) {
		t.Fatalf("Next = %v, %v, want its own daily slot %v", next, ok, daily)
	}
	f.setEnv(t, env, updates.Overrides{})
	f.clk.Tick(t, tickLen)
	f.roundDone(t)
	every6 := slotOf(t, env, f.clk.Now())
	if next, ok := f.sch.Next(env); !ok || !next.Equal(every6) {
		t.Errorf("Next = %v, %v after the reset, want the default's slot %v", next, ok, every6)
	}
	if got := f.sch.StaleAfter(env); got != 18*time.Hour {
		t.Errorf("StaleAfter = %v on the default schedule, want 18h", got)
	}
}

// Everything due in one tick is one pass through the fleet-deduplicated checker, and no more than
// MaxConcurrentChecks passes are going at once.
func TestSchedulerConcurrency(t *testing.T) {
	t.Parallel()
	t.Run("one tick, one pass", func(t *testing.T) {
		t.Parallel()
		start := time.Date(2026, time.November, 3, 5, 0, 0, 0, time.UTC)
		f := newSchedFix(t, start, "env-a", "env-b", "env-c")
		f.start(t)
		// One tick that is long enough to pass every slot: three environments are due together.
		f.clk.Tick(t, 2*time.Hour)
		got := testsupport.Await(t, "the pass to end", f.ended)
		if !slices.Equal(got, []string{"env-a", "env-b", "env-c"}) {
			t.Errorf("the pass covered %v, want the three environments in one", got)
		}
		if n := f.heads.requests(); n != 1 {
			t.Errorf("%d registry requests, want 1: the three environments run one image", n)
		}
	})

	t.Run("two passes at most", func(t *testing.T) {
		t.Parallel()
		start := time.Date(2026, time.November, 3, 5, 0, 0, 0, time.UTC)
		// Three environments whose slots are minutes apart, so that each is due in a tick of its own.
		var ids []string
		var slots []time.Time
		for i := 0; len(ids) < 3 && i < 200; i++ {
			id := fmt.Sprintf("env-%d", i)
			at := slotOf(t, id, start)
			if !slices.ContainsFunc(slots, func(o time.Time) bool { return at.Sub(o).Abs() < 3*time.Minute }) {
				ids, slots = append(ids, id), append(slots, at)
			}
		}
		order := []int{0, 1, 2}
		slices.SortFunc(order, func(a, b int) int { return slots[a].Compare(slots[b]) })
		f := newSchedFix(t, start, ids...)
		byEnv := map[string]envs.Aggregate{}
		f.heads.gate, f.heads.entered = make(chan struct{}), make(chan struct{}, 8)
		for _, id := range ids {
			ref := "ghcr.io/acme/" + id + ":2"
			byEnv[id] = aggOf([]envs.ImageRow{img(dg('c'), "ghcr.io/acme/"+id+"@"+dg('1'))}, ctr("web", ref, dg('c'), nil))
			f.heads.set(ref, answer{digest: dg('9')})
		}
		f.snap.byEnv = byEnv
		f.start(t)

		first, second, third := ids[order[0]], ids[order[1]], ids[order[2]]
		f.advanceTo(t, slots[order[0]])
		testsupport.Await(t, "the first pass to ask the registry", f.heads.entered)
		f.advanceTo(t, slots[order[1]])
		testsupport.Await(t, "the second pass to ask the registry", f.heads.entered)
		f.advanceTo(t, slots[order[2]])
		f.roundDone(t)
		// Two passes are going, so the third environment's slot is still due: it was not started.
		if next, ok := f.sch.Next(third); !ok || !next.Equal(slots[order[2]]) {
			t.Fatalf("Next(%s) = %v, %v with two passes going, want its slot %v left due", third, next, ok, slots[order[2]])
		}
		for _, id := range []string{first, second} {
			if next, _ := f.sch.Next(id); !next.After(slots[order[1]]) {
				t.Errorf("Next(%s) = %v, want it moved on to the following slot", id, next)
			}
		}

		// The registry answers: the two passes end, and the third environment is checked on the next tick.
		close(f.heads.gate)
		testsupport.Await(t, "a pass to end", f.ended)
		testsupport.Await(t, "a pass to end", f.ended)
		f.clk.Tick(t, tickLen)
		if got := testsupport.Await(t, "the third pass to end", f.ended); !slices.Equal(got, []string{third}) {
			t.Errorf("the third pass covered %v, want [%s]", got, third)
		}
	})
}

// recordingStore wraps the store of checks and notes every call, so that a test says what a scheduled
// check may write to.
type recordingStore struct {
	updates.Store
	mu    sync.Mutex
	calls []string
}

func (r *recordingStore) note(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recordingStore) Checks(ctx context.Context, keys []updates.Key) (map[updates.Key]updates.Check, error) {
	r.note("Checks")
	return r.Store.Checks(ctx, keys)
}

func (r *recordingStore) SaveSuccess(ctx context.Context, c updates.Check) error {
	r.note("SaveSuccess")
	return r.Store.SaveSuccess(ctx, c)
}

func (r *recordingStore) SaveFailure(ctx context.Context, k updates.Key, at time.Time, why updates.Reason) error {
	r.note("SaveFailure")
	return r.Store.SaveFailure(ctx, k, at, why)
}

// The scheduler never starts an update or changes anything on an engine (IMG-06, T-12-21). What it is
// given cannot: the engine is reached only through the read of a Snapshotter, which is all this test
// gives it, and the registry only through Heads, which asks. Everything it writes is a check result; and
// the files of the scheduler import nothing that could write to an engine.
func TestSchedulerNeverWritesToEngines(t *testing.T) {
	t.Parallel()
	const env = "env-a"
	start := time.Date(2026, time.November, 3, 5, 0, 0, 0, time.UTC)
	f := newSchedFix(t, start, env)
	rec := &recordingStore{Store: f.sql}
	f.chk.Store = rec
	slot := slotOf(t, env, start)
	f.start(t)
	f.advanceTo(t, slot)
	testsupport.Await(t, "the check at the slot", f.ended)

	rec.mu.Lock()
	calls := slices.Clone(rec.calls)
	rec.mu.Unlock()
	for _, c := range calls {
		if c != "Checks" && c != "SaveSuccess" && c != "SaveFailure" {
			t.Errorf("the scheduler called %s on the store of checks", c)
		}
	}
	if !slices.Contains(calls, "SaveSuccess") {
		t.Errorf("store calls = %v, want the check to have been saved", calls)
	}
	if f.snap.reads == 0 {
		t.Error("the engine was never read, so the check had nothing to ask about")
	}

	// The scheduler and the policy are not able to reach an engine's write side: they import none of the
	// packages that hold one.
	forbidden := []string{"/internal/dockerx", "/internal/stacks", "/internal/janitor", "/internal/agents", "/internal/jobs", "/internal/api"}
	for _, file := range []string{"scheduler.go", "policy.go"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasSuffix(path, bad) {
					t.Errorf("%s imports %s, which holds the write side of an engine", file, path)
				}
			}
		}
	}
}

// How old a check may be before it reads stale follows the schedule: three gaps of it, and a day when it
// is off.
func TestStaleAfterFollowsSchedule(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	const env = "env-a"
	now := time.Date(2026, time.November, 3, 5, 0, 0, 0, time.UTC)

	every6 := updates.DefaultPolicy()
	daily := updates.Policy{Enabled: true, Schedule: "0 3 * * *", Preset: schedule.PresetDaily}
	off := updates.Policy{Enabled: false, Schedule: updates.DefaultSchedule, Preset: schedule.PresetEvery6h}
	broken := updates.Policy{Enabled: true, Schedule: "not a schedule"}
	for _, tc := range []struct {
		name string
		pol  updates.Policy
		want time.Duration
	}{
		{"every 6 hours", every6, 18 * time.Hour},
		{"daily", daily, 72 * time.Hour},
		{"off", off, 24 * time.Hour},
		{"a schedule that cannot be read", broken, 24 * time.Hour},
	} {
		if got := updates.StaleAfterOf(tc.pol, env, now, time.UTC); got != tc.want {
			t.Errorf("%s: StaleAfterOf = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The scheduler hands the same bound to the service, and the service judges by it.
	f := newSchedFix(t, now, env)
	f.start(t)
	if got := f.sch.StaleAfter(env); got != 18*time.Hour {
		t.Fatalf("the scheduler's bound = %v, want 18h", got)
	}
	f.heads.set(imageRef, answer{digest: dg('1')}) // the tag has not moved
	if _, err := f.chk.CheckEnv(ctx, env, mustWorkloads(t, f, env)); err != nil {
		t.Fatal(err)
	}
	checkedAt := f.clk.Now()
	verdictAt := func(age time.Duration, bound time.Duration) updates.State {
		t.Helper()
		f.svc.Now = func() time.Time { return checkedAt.Add(age) }
		f.svc.StaleAfter = func(string) time.Duration { return bound }
		v, err := f.svc.ContainerVerdicts(ctx, env, f.snap.agg)
		if err != nil {
			t.Fatal(err)
		}
		return v["web"].State
	}
	bound := f.sch.StaleAfter(env)
	if got := verdictAt(17*time.Hour, bound); got != updates.StateUpToDate {
		t.Errorf("a same-digest check 17h old on every 6 hours reads %s, want up_to_date", got)
	}
	if got := verdictAt(20*time.Hour, bound); got != updates.StateCheckStale {
		t.Errorf("a same-digest check 20h old on every 6 hours reads %s, want check_stale", got)
	}
	offBound := updates.StaleAfterOf(off, env, now, time.UTC)
	if got := verdictAt(23*time.Hour, offBound); got != updates.StateUpToDate {
		t.Errorf("a check 23h old with the schedule off reads %s, want up_to_date", got)
	}
	if got := verdictAt(25*time.Hour, offBound); got != updates.StateCheckStale {
		t.Errorf("a check 25h old with the schedule off reads %s, want check_stale", got)
	}
}

func mustWorkloads(t *testing.T, f *schedFix, env string) []updates.Workload {
	t.Helper()
	ws, err := f.svc.Workloads(testsupport.Context(t), env)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// A container that has not been checked says when it will be.
func TestNotCheckedCarriesNextCheck(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	const env = "env-a"
	start := time.Date(2026, time.November, 3, 7, 0, 0, 0, time.UTC)
	slot := slotOf(t, env, start)
	f := newSchedFix(t, start, env)
	f.svc.NextCheck = f.sch.Next
	f.start(t)
	v, err := f.svc.ContainerVerdicts(ctx, env, f.snap.agg)
	if err != nil {
		t.Fatal(err)
	}
	if got := v["web"]; got.State != updates.StateNotChecked || !got.NextCheckAt.Equal(slot) {
		t.Errorf("verdict = %+v, want not_checked and the next check at %v", got, slot)
	}
	// An environment whose checks are off has no next check to name.
	f.svc.NextCheck = func(string) (time.Time, bool) { return time.Time{}, false }
	if v, _ := f.svc.ContainerVerdicts(ctx, env, f.snap.agg); !v["web"].NextCheckAt.IsZero() {
		t.Errorf("NextCheckAt = %v with no schedule, want none", v["web"].NextCheckAt)
	}
}
