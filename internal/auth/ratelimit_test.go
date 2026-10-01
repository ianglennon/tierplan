// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth_test

import (
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/auth"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func TestIPLimiterBurstThenDeny(t *testing.T) {
	c := newClock()
	l := auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, c.now)
	for i := 1; i <= 5; i++ {
		if !l.Allow("203.0.113.9") {
			t.Fatalf("call %d denied, want allowed within the burst", i)
		}
	}
	if l.Allow("203.0.113.9") {
		t.Fatal("6th call allowed, want denied")
	}
	if !l.Allow("198.51.100.1") {
		t.Fatal("a different IP must have its own budget")
	}
}

func TestIPLimiterRefills(t *testing.T) {
	c := newClock()
	l := auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, c.now)
	for range 5 {
		l.Allow("203.0.113.9")
	}
	if l.Allow("203.0.113.9") {
		t.Fatal("exhausted budget still allowed")
	}
	c.advance(11 * time.Second)
	if l.Allow("203.0.113.9") {
		t.Fatal("allowed before one interval has passed")
	}
	c.advance(2 * time.Second)
	if !l.Allow("203.0.113.9") {
		t.Fatal("denied after one interval has passed")
	}
}

func TestIPLimiterEvictsIdleEntries(t *testing.T) {
	c := newClock()
	l := auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, c.now)
	l.Allow("203.0.113.1")
	l.Allow("203.0.113.2")
	if got := l.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	c.advance(11 * time.Minute)
	l.Allow("203.0.113.3")
	if got := l.Len(); got != 1 {
		t.Fatalf("Len after the TTL = %d, want 1 (only the fresh entry)", got)
	}
}

func TestIPLimiterActiveEntryIsNotEvicted(t *testing.T) {
	c := newClock()
	l := auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, c.now)
	for range 5 {
		l.Allow("203.0.113.1")
	}
	// Keep hammering across several TTLs: an attacker must not get a fresh
	// budget by being evicted while still active.
	for range 30 {
		c.advance(time.Minute)
		l.Allow("203.0.113.1")
	}
	if l.Len() != 1 {
		t.Fatalf("Len = %d, want 1", l.Len())
	}
}
