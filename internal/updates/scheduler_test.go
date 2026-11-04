// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"slices"
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

// countingSnap is the aggregate of every environment, and how many times it was read.
type countingSnap struct {
	mu    sync.Mutex
	agg   envs.Aggregate
	reads int
}

func (c *countingSnap) Snapshot(context.Context, string) (envs.Aggregate, envs.Health, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
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
