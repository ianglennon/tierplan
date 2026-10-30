// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package auth_test

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// Plan 03-10, D-20: the first refusal of a kind is written in full, repeats
// inside a window become one summary entry.

func userRefusal(userID, target, reason, env string) audit.Entry {
	return audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: userID, Name: "name-" + userID},
		Action: audit.ActionPermissionDenied, Result: audit.ResultDenied, Reason: reason,
		TargetType: "route", TargetID: target, EnvID: env,
		IP: "198.51.100.4", UserAgent: "coalesce-test/1.0",
	}
}

func newCoalescer(next *recorder, c *clock, opts ...auth.CoalescerOption) *auth.RefusalCoalescer {
	return auth.NewRefusalCoalescer(next, slog.New(slog.DiscardHandler), c.now, opts...)
}

func TestCoalescerWritesTheFirstRefusalNowAndOneSummaryWhenTheWindowCloses(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	ctx := context.Background()
	e := userRefusal("u1", "GET /api/audit", "forbidden", "env-1")
	for range 5 {
		if err := co.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Second)
	}
	if got := next.all(); len(got) != 1 || got[0].Details["coalesced"] != "" {
		t.Fatalf("rows before the window closes = %+v, want the one full refusal", got)
	}
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(next.all()); got != 1 {
		t.Fatalf("a sweep inside the window wrote %d more rows, want none", got-1)
	}
	clk.advance(time.Minute)
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	rows := next.all()
	if len(rows) != 2 {
		t.Fatalf("rows after the window = %d, want 2", len(rows))
	}
	s := rows[1]
	if s.Details["count"] != "4" || s.Details["coalesced"] != "true" {
		t.Fatalf("summary details = %v, want count 4 and coalesced=true", s.Details)
	}
	for _, k := range []string{"first", "last"} {
		if _, err := time.Parse(time.RFC3339, s.Details[k]); err != nil {
			t.Fatalf("summary %s = %q: %v", k, s.Details[k], err)
		}
	}
	if s.Actor != e.Actor || s.Action != e.Action || s.Result != e.Result || s.Reason != e.Reason ||
		s.TargetType != e.TargetType || s.TargetID != e.TargetID || s.EnvID != e.EnvID || s.IP != e.IP {
		t.Fatalf("summary = %+v, want the first refusal's own fields", s)
	}
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(next.all()); got != 2 {
		t.Fatalf("a second sweep wrote %d more rows, want none", got-2)
	}
}

func TestCoalescerFailedFirstWriteReturnsTheErrorAndOpensNoWindow(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	ctx := context.Background()
	e := userRefusal("u1", "GET /api/audit", "forbidden", "")
	next.fail(errors.New("disk full"))
	if err := co.Record(ctx, e); err == nil {
		t.Fatal("a failed write must reach the caller so the answer becomes a 500")
	}
	if co.Pending() != 0 {
		t.Fatal("a refusal that was not written opened a window")
	}
	next.fail(nil)
	for range 3 {
		if err := co.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows := next.all()
	if len(rows) != 1 || rows[0].Details["coalesced"] != "" {
		t.Fatalf("rows = %+v, want the retry written in full as the first of its window", rows)
	}
	if co.Pending() != 1 {
		t.Fatalf("pending = %d, want the one window the retry opened", co.Pending())
	}
}

func TestCoalescerCancellingRunWritesEveryPendingCount(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	e := userRefusal("u1", "GET /api/audit", "forbidden", "")
	for range 4 {
		if err := co.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { co.Run(ctx, time.Hour); close(done) }()
	cancel()
	testsupport.Await(t, "Run to return after its context was cancelled", done)
	rows := next.all()
	if len(rows) != 2 || rows[1].Details["count"] != "3" || rows[1].Details["coalesced"] != "true" {
		t.Fatalf("rows = %+v, want the first refusal and a flushed summary of 3 repeats", rows)
	}
}

func TestCoalescerRetriesAFailedSummaryAtTheNextSweepWithTheCountsMerged(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	ctx := context.Background()
	e := userRefusal("u1", "GET /api/audit", "forbidden", "")
	for range 3 {
		_ = co.Record(ctx, e)
	}
	clk.advance(2 * time.Minute)
	next.fail(errors.New("busy"))
	if err := co.Sweep(ctx); err == nil {
		t.Fatal("a failed summary write must be reported")
	}
	next.fail(nil)
	// The same key refuses again: the closed window is back in the table, so this
	// one counts into it and the merged count is written at the next sweep.
	_ = co.Record(ctx, e)
	clk.advance(2 * time.Minute)
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	rows := next.all()
	if len(rows) != 2 || rows[1].Details["count"] != "3" {
		t.Fatalf("rows = %+v, want the first refusal and one summary with the 2 + 1 repeats", rows)
	}
}

func TestCoalescerAtTheTableCapANewKeyIsWrittenThroughAndOldOnesKeepCoalescing(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk, auth.WithMaxKeys(2))
	ctx := context.Background()
	a := userRefusal("u1", "GET /a", "forbidden", "")
	b := userRefusal("u1", "GET /b", "forbidden", "")
	c := userRefusal("u1", "GET /c", "forbidden", "")
	for _, e := range []audit.Entry{a, b, c, c, c, a, a} {
		if err := co.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// a, b and the full table's c, c, c: every one of c is written, a's repeats are counted.
	if got := len(next.all()); got != 5 {
		t.Fatalf("rows = %d, want a, b and three written-through c", got)
	}
	if co.Pending() != 2 {
		t.Fatalf("pending = %d, want the two windows that fit", co.Pending())
	}
	clk.advance(2 * time.Minute)
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	rows := next.all()
	if len(rows) != 6 || rows[5].TargetID != "GET /a" || rows[5].Details["count"] != "2" {
		t.Fatalf("rows = %+v, want a summary of a's 2 repeats", rows)
	}
}

func TestCoalescerPassesAnonymousSystemAndOtherActionsStraightThrough(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	ctx := context.Background()
	anonymous := userRefusal("", "GET /api/me", "unauthenticated", "")
	anonymous.Actor = audit.Actor{Type: audit.ActorAnonymous}
	system := userRefusal("system:cli", "GET /api/x", "forbidden", "")
	system.Actor = audit.SystemCLI
	login := userRefusal("u1", "u1", "", "")
	login.Action = audit.ActionAuthLogin
	for _, e := range []audit.Entry{anonymous, system, login} {
		for range 4 {
			if err := co.Record(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := len(next.all()); got != 12 {
		t.Fatalf("rows = %d, want all 12 passed through unchanged", got)
	}
	if co.Pending() != 0 {
		t.Fatal("an entry that is not a user's refusal was counted")
	}
}

func TestCoalescerRefusalsThatDifferInEnvironmentOrTargetAreSeparateWindows(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	ctx := context.Background()
	for _, e := range []audit.Entry{
		userRefusal("u1", "GET /x", "forbidden", "env-a"),
		userRefusal("u1", "GET /x", "forbidden", "env-b"),
		userRefusal("u1", "GET /y", "forbidden", "env-a"),
		userRefusal("u2", "GET /x", "forbidden", "env-a"),
		userRefusal("u1", "GET /x", "environment_not_visible", "env-a"),
	} {
		if err := co.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(next.all()); got != 5 || co.Pending() != 5 {
		t.Fatalf("rows = %d, windows = %d, want 5 of each", got, co.Pending())
	}
}

func TestCoalescerBurstsExpireAfterAnHourAndNeedTenRefusals(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	ctx := context.Background()
	few := userRefusal("u1", "GET /few", "forbidden", "")
	many := userRefusal("u2", "GET /many", "forbidden", "")
	for range 8 {
		_ = co.Record(ctx, few)
	}
	for range 10 {
		_ = co.Record(ctx, many)
	}
	clk.advance(2 * time.Minute)
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	bursts := co.Bursts(clk.now())
	if len(bursts) != 1 || bursts[0].ActorID != "u2" || bursts[0].ActorName != "name-u2" || bursts[0].Count != 10 {
		t.Fatalf("bursts = %+v, want only u2's 10 refusals", bursts)
	}
	clk.advance(57 * time.Minute) // 59 minutes after the burst began
	if got := len(co.Bursts(clk.now())); got != 1 {
		t.Fatalf("bursts 59 minutes on = %d, want 1", got)
	}
	clk.advance(2 * time.Minute)
	if got := len(co.Bursts(clk.now())); got != 0 {
		t.Fatalf("bursts an hour on = %d, want 0", got)
	}
}

func TestCoalescerBurstThresholdAndWindowAreOptions(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk, auth.WithBurstThreshold(3), auth.WithWindow(10*time.Second))
	ctx := context.Background()
	e := userRefusal("u1", "GET /x", "forbidden", "")
	for range 3 {
		_ = co.Record(ctx, e)
	}
	clk.advance(11 * time.Second)
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(co.Bursts(clk.now())); got != 1 || len(next.all()) != 2 {
		t.Fatalf("bursts = %d, rows = %d, want 1 burst and a summary after an 11 second window", got, len(next.all()))
	}
}

// CR-B-01: the environment ID of a refusal is whatever a caller put in the URL.
// One user flooding fresh, oversized IDs must neither grow the table without
// bound nor turn every request into a full audit write.
func TestCoalescerAFloodOfRandomEnvironmentIDsKeepsMemoryAndWritesBounded(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	ctx := context.Background()
	const perActor = 32 // auth.defaultMaxPerActor
	const flood = 5000
	big := strings.Repeat("x", 100_000)
	for i := range flood {
		e := userRefusal("u1", "GET /api/env/{envId}/summary", "environment_not_visible", strconv.Itoa(i)+big)
		if err := co.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows := next.all()
	if len(rows) > perActor+1 {
		t.Fatalf("a flood of %d distinct IDs wrote %d full rows, want at most %d", flood, len(rows), perActor+1)
	}
	if co.Pending() > perActor+1 {
		t.Fatalf("open windows = %d, want at most %d", co.Pending(), perActor+1)
	}
	for _, r := range rows {
		if len(r.EnvID) > 128 {
			t.Fatalf("a stored entry holds a %d byte environment ID, want at most 128", len(r.EnvID))
		}
	}
	// Refusals are still audited: the overflow window is summarised with the
	// count of everything folded into it.
	clk.advance(2 * time.Minute)
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var summed int
	for _, r := range next.all() {
		if r.Details["overflow"] == "true" && r.Details["coalesced"] == "true" {
			n, err := strconv.Atoi(r.Details["count"])
			if err != nil {
				t.Fatal(err)
			}
			summed += n
		}
	}
	if want := flood - perActor - 1; summed != want {
		t.Fatalf("the overflow summary counted %d refusals, want %d", summed, want)
	}
	if co.Pending() != 0 {
		t.Fatalf("open windows after the sweep = %d, want 0", co.Pending())
	}
	// Another user still gets their own first refusal written in full.
	before := len(next.all())
	if err := co.Record(ctx, userRefusal("u2", "GET /x", "environment_not_visible", "other")); err != nil {
		t.Fatal(err)
	}
	if len(next.all()) != before+1 {
		t.Fatal("one user's flood stopped another user's refusal being written")
	}
}

func TestCoalescerPerActorOverflowIsAnOptionAndFreesItsSlotsWhenWindowsClose(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk, auth.WithMaxPerActor(2))
	ctx := context.Background()
	for i := range 6 {
		if err := co.Record(ctx, userRefusal("u1", "GET /e", "environment_not_visible", strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	// Two own windows, then the first overflow refusal in full, then three counted.
	if got := len(next.all()); got != 3 || co.Pending() != 3 {
		t.Fatalf("rows = %d, windows = %d, want 3 of each", got, co.Pending())
	}
	clk.advance(2 * time.Minute)
	if err := co.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	// The slots are free again: two new IDs are separate windows.
	for i := 10; i < 12; i++ {
		if err := co.Record(ctx, userRefusal("u1", "GET /e", "environment_not_visible", strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	if co.Pending() != 2 {
		t.Fatalf("windows after the sweep = %d, want 2", co.Pending())
	}
}

// The cap is per route and reason: one user refused on many different routes
// is still written in full on each of them.
func TestCoalescerPerActorCapDoesNotFoldDifferentRoutes(t *testing.T) {
	next, clk := &recorder{}, newClock()
	co := newCoalescer(next, clk)
	for i := range 200 {
		if err := co.Record(context.Background(), userRefusal("u1", "GET /route-"+strconv.Itoa(i), "forbidden", "env-1")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(next.all()); got != 200 {
		t.Fatalf("rows = %d, want one full row for each of 200 routes", got)
	}
}
