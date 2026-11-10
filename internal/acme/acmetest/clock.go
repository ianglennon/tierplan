// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acmetest

import (
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Clock is a clock a test moves. It never sleeps: After arms a timer and announces it on a channel,
// so a test that moves the clock knows the code under test is waiting on it, and Advance fires every
// timer that has come due. It satisfies stackops.Clock (Now and After).
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []clockTimer
	armed  chan struct{}
}

type clockTimer struct {
	at time.Time
	ch chan time.Time
}

// NewClock is a clock standing at now.
func NewClock(now time.Time) *Clock {
	return &Clock{now: now, armed: make(chan struct{}, 4096)}
}

// Now is the time the clock stands at.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After arms a timer that fires once the clock has moved on by d, and announces it to WaitArmed and
// Tick. A timer whose time has already come is not fired until the next Advance.
func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, clockTimer{at: c.now.Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *Clock) Advance(d time.Duration) {
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

// WaitArmed waits until something has armed a timer since the last wait, and consumes that
// announcement. It is how a test learns that the code under test is now waiting on the clock.
func (c *Clock) WaitArmed(t testing.TB) {
	t.Helper()
	testsupport.Await(t, "the code under test to arm a timer on the clock", c.armed)
}

// Tick waits for something to arm a timer and then moves the clock on by d.
func (c *Clock) Tick(t testing.TB, d time.Duration) {
	t.Helper()
	c.WaitArmed(t)
	c.Advance(d)
}
