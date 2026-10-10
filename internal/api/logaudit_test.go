// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"fmt"
	"testing"
	"time"
)

// The memory that audits a viewer of a log once (plan 04-19, D-24).

type memClock struct{ t time.Time }

func (c *memClock) now() time.Time { return c.t }

func TestLogsAuditMemoryReportsAViewerAsNewOnceAndSeenAfterwards(t *testing.T) {
	clk := &memClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := newLogAuditMemory(time.Hour, 10, clk.now)
	k := logAuditKey{user: "user:a", container: "c1", sid: "s1"}
	if m.seen(k) {
		t.Fatal("a viewer never seen before was reported as seen")
	}
	if !m.seen(k) {
		t.Fatal("the same viewer was reported as new twice")
	}
	for _, other := range []logAuditKey{{"user:b", "c1", "s1"}, {"user:a", "c2", "s1"}, {"user:a", "c1", "s2"}} {
		if m.seen(other) {
			t.Errorf("%+v shares a viewer with the first", other)
		}
	}
}

func TestLogsAuditMemoryForgetsAViewerAfterTheTTLUnlessItKeepsComingBack(t *testing.T) {
	clk := &memClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := newLogAuditMemory(time.Hour, 10, clk.now)
	idle := logAuditKey{user: "u", container: "c", sid: "idle"}
	busy := logAuditKey{user: "u", container: "c", sid: "busy"}
	m.seen(idle)
	m.seen(busy)
	// The busy page reconnects every 40 minutes; the idle one is gone.
	clk.t = clk.t.Add(40 * time.Minute)
	if !m.seen(busy) {
		t.Fatal("a reconnect within the hour was audited again")
	}
	clk.t = clk.t.Add(40 * time.Minute)
	if !m.seen(busy) {
		t.Error("a page that keeps reconnecting was audited again: the TTL should slide")
	}
	if m.seen(idle) {
		t.Error("a viewer unseen for 80 minutes was still remembered")
	}
}

func TestLogsAuditMemoryEvictsExpiredEntriesAndReportsFullAtTheCapSoTheRowIsWritten(t *testing.T) {
	clk := &memClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := newLogAuditMemory(time.Hour, 3, clk.now)
	for i := range 3 {
		if m.seen(logAuditKey{user: "u", container: "c", sid: fmt.Sprint("s", i)}) {
			t.Fatalf("viewer %d was new", i)
		}
	}
	if m.size() != 3 {
		t.Fatalf("size = %d, want 3", m.size())
	}
	// Full: a fourth viewer is new, and stays new, because there was no room to
	// remember it. The row gets written every time.
	overflow := logAuditKey{user: "u", container: "c", sid: "overflow"}
	for range 2 {
		if m.seen(overflow) {
			t.Error("a viewer that did not fit was reported as seen: its row would be lost")
		}
	}
	if m.size() != 3 {
		t.Errorf("size = %d after an overflow, want it to stay at the cap", m.size())
	}
	// An hour on, the old entries are swept to make room.
	clk.t = clk.t.Add(time.Hour + time.Second)
	if m.seen(overflow) {
		t.Error("the overflow viewer was seen before it was ever remembered")
	}
	if !m.seen(overflow) {
		t.Error("once there was room the viewer was not remembered")
	}
	if m.size() != 1 {
		t.Errorf("size = %d, want the expired entries gone and one left", m.size())
	}
}
