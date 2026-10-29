// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The background poller (plan 09-18, GIT-06, D-14): it finds changes on its own, on a clock the test moves, and
// only records what it sees.

// pollClock is the clock of the poller and of the checks it makes. After arms a timer and announces it, so that a test
// that moves the clock knows the poller is waiting on it and does not sleep to find out.
type pollClock struct {
	*checkClock
	tmu    sync.Mutex
	timers []pollTimer
	armed  chan struct{}
}

type pollTimer struct {
	at time.Time
	ch chan time.Time
}

func newPollClock(c *checkClock) *pollClock {
	return &pollClock{checkClock: c, armed: make(chan struct{}, 4096)}
}

func (c *pollClock) Now() time.Time { return c.checkClock.now() }

func (c *pollClock) After(d time.Duration) <-chan time.Time {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, pollTimer{at: c.Now().Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *pollClock) Advance(d time.Duration) {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	c.checkClock.advance(d)
	now := c.Now()
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(now) {
			t.ch <- now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// Tick waits for the poller to arm its next timer, which it does once a round is done, and then moves the clock on by d.
func (c *pollClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	testsupport.Await(t, "the poller to arm its next tick", c.armed)
	c.Advance(d)
}

// polled is what the poller reported of one stack.
type polled struct{ stackID, outcome string }

// pollFixture is a check fixture whose stacks are polled on a clock the test moves.
type pollFixture struct {
	*checkFixture
	pc      *pollClock
	checked chan polled
}

func newPollFixture(t *testing.T, wrap func(gitx.Client) gitx.Client) *pollFixture {
	t.Helper()
	cf := newCheckFixture(t, wrap, nil)
	cf.deploy(t)
	return &pollFixture{checkFixture: cf, pc: newPollClock(cf.clock), checked: make(chan polled, 256)}
}

// start runs the poller until the test ends. It fills in the clock and the seam that reports what was checked.
func (f *pollFixture) start(t *testing.T, d gitsync.PollerDeps) {
	t.Helper()
	d.Clock = f.pc
	d.Checked = func(stackID, outcome string) { f.checked <- polled{stackID, outcome} }
	if d.Rand == nil {
		d.Rand = func() float64 { return 0 }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.RunPoller(ctx, d)
	}()
	t.Cleanup(func() {
		cancel()
		testsupport.Await(t, "the poller to stop", done)
	})
}

// awaitChecked receives what the poller reported of the next n checks, in the order it reported them.
func (f *pollFixture) awaitChecked(t *testing.T, n int) []polled {
	t.Helper()
	out := make([]polled, 0, n)
	for range n {
		out = append(out, testsupport.Await(t, "the poller to check a stack", f.checked))
	}
	return out
}

// constRand is a random source that always answers v.
func constRand(v float64) func() float64 { return func() float64 { return v } }

func TestPollerFindsFolderChange(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	ctx := context.Background()
	rev, ok, err := f.rr.LatestGoodRevision(ctx, f.stackID)
	if err != nil || !ok {
		t.Fatalf("LatestGoodRevision: %v, %v", ok, err)
	}
	draftBefore, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}

	// Somebody pushes to the stack's folder; nobody presses anything.
	head := f.changeWeb(t, "1")
	f.start(t, gitsync.PollerDeps{Rand: constRand(0.5)})
	f.pc.Tick(t, 5*time.Minute)

	got := f.awaitChecked(t, 1)
	if got[0].stackID != f.stackID || got[0].outcome != string(gitsync.OutcomeBehind) {
		t.Fatalf("the poller reported %+v, want the stack behind", got)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusBehind || s.Behind != 1 || s.HeadCommit != head || s.DeployedCommit != f.sha {
		t.Errorf("state = %+v, want behind 1 at %s with the deployed commit %s", s, head, f.sha)
	}

	// A poll only records: no revision, no change to the draft (the poller never deploys).
	if _, err := f.rr.Revision(ctx, f.stackID, rev.Seq+1); err == nil {
		t.Errorf("revision %d exists after a poll, want none", rev.Seq+1)
	}
	draftAfter, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if draftAfter.Version != draftBefore.Version || draftAfter.Git.Commit != draftBefore.Git.Commit {
		t.Errorf("draft = v%d %+v, was v%d %+v: a poll must not change it", draftAfter.Version, draftAfter.Git, draftBefore.Version, draftBefore.Git)
	}
	if good, ok, err := f.rr.LatestGoodRevision(ctx, f.stackID); err != nil || !ok || good.Seq != rev.Seq {
		t.Errorf("latest good revision = %d, %v, %v, want the deployed one %d", good.Seq, ok, err, rev.Seq)
	}
}
