// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// The memory that audits a viewer of a log once (plan 04-19, D-24).

type memClock struct{ t time.Time }

func (c *memClock) now() time.Time { return c.t }

func TestLogsAuditMemoryReportsAViewerAsNewOnceAndSeenAfterwards(t *testing.T) {
	t.Parallel()
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

func TestLogsAuditMemoryForgetMakesTheViewerNewAgain(t *testing.T) {
	t.Parallel()
	m := newLogAuditMemory(time.Hour, 10, nil)
	k := logAuditKey{user: "user:a", container: "c1", sid: "s1"}
	m.seen(k)
	m.forget(k)
	if m.seen(k) {
		t.Fatal("a forgotten viewer was reported as seen")
	}
	m.forget(logAuditKey{user: "nobody"}) // a viewer never remembered is harmless
}

// WR-07: a viewer is marked before its row is written, so a row that failed must not
// leave the viewer "already audited" for an hour. The next stream of it writes the row.
func TestLogsViewedIsWrittenOnTheNextStreamWhenTheFirstRowFailed(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)
	d := LogsDeps{Audit: w, Log: slog.New(slog.DiscardHandler)}
	mem := newLogAuditMemory(time.Hour, 10, nil)
	p := &rbac.Principal{Type: rbac.PrincipalUser, ID: "user-1", Name: "admin"}
	params := logParams{sid: "abcdefghijklmnopqrstuv", tail: "100"}
	rows := func() int {
		var n int
		if err := st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND target_id = ?`,
			audit.ActionLogsViewed, "c1").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The first row cannot be written: the request was cancelled.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	auditLogsViewed(cancelled, d, mem, p, "env-1", "c1", "web", "node-1", params, "agent")
	if n := rows(); n != 0 {
		t.Fatalf("%d logs.viewed rows after a failed write, want 0", n)
	}

	// The viewer reconnects: this is the stream whose row must be written.
	auditLogsViewed(context.Background(), d, mem, p, "env-1", "c1", "web", "node-1", params, "agent")
	if n := rows(); n != 1 {
		t.Fatalf("%d logs.viewed rows after the retry, want 1", n)
	}
	// And only one: the viewer is now audited.
	auditLogsViewed(context.Background(), d, mem, p, "env-1", "c1", "web", "node-1", params, "agent")
	if n := rows(); n != 1 {
		t.Fatalf("%d logs.viewed rows after a reconnect, want 1", n)
	}
}

func TestLogsAuditMemoryForgetsAViewerAfterTheTTLUnlessItKeepsComingBack(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
