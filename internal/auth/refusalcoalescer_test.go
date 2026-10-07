// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
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
