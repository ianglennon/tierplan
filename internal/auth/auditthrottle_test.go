// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
)

// Plan 01-31, WR-04: anonymous refusals are bounded per address and every one
// is still accounted for, individually or in a counted summary.

// recorder is a stand-in audit writer that remembers what it was given.
type recorder struct {
	mu      sync.Mutex
	entries []audit.Entry
	err     error
}

func (r *recorder) Record(_ context.Context, e audit.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}

func (r *recorder) all() []audit.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Entry(nil), r.entries...)
}

func (r *recorder) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func anon(action, reason, target, ip string) audit.Entry {
	return audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorAnonymous},
		Action: action, Result: audit.ResultDenied, Reason: reason,
		TargetType: "route", TargetID: target, IP: ip, UserAgent: "throttle-test/1.0",
		Details: map[string]string{"first": "yes"},
	}
}

func newThrottle(next *recorder, burst int, c *clock) *auth.AuditThrottle {
	lim := auth.NewIPLimiter(time.Hour, burst, 10*time.Minute, c.now)
	return auth.NewAuditThrottle(next, lim, slog.New(slog.DiscardHandler), c.now)
}

func TestAuditThrottlePassesSignedInActorsThroughUncounted(t *testing.T) {
	next := &recorder{}
	th := newThrottle(next, 1, newClock())
	for _, a := range []audit.Actor{
		{Type: audit.ActorUser, ID: "id-admin", Name: "admin"},
		audit.SystemCLI,
	} {
		for range 5 {
			e := anon(audit.ActionPermissionDenied, "forbidden", "GET /api/x", "203.0.113.9")
			e.Actor = a
			if err := th.Record(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := len(next.all()); got != 10 {
		t.Fatalf("rows = %d, want all 10 passed straight through", got)
	}
	if th.Pending() != 0 {
		t.Fatal("a signed-in actor was counted as suppressed")
	}
}

func TestAuditThrottleWritesTheBurstThenOneSummaryWithTheCount(t *testing.T) {
	next := &recorder{}
	th := newThrottle(next, 3, newClock())
	ctx := context.Background()
	for range 50 {
		if err := th.Record(ctx, anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", "198.51.100.7")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(next.all()); got != 3 {
		t.Fatalf("rows before Flush = %d, want the burst of 3", got)
	}
	if err := th.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows := next.all()
	if len(rows) != 4 {
		t.Fatalf("rows after Flush = %d, want 4", len(rows))
	}
	s := rows[3]
	if _, err := time.Parse(time.RFC3339, s.Details["since"]); err != nil {
		t.Fatalf("summary has no usable since detail: %v", s.Details)
	}
	if s.Details["suppressed"] != "47" || s.Details["first"] != "yes" {
		t.Fatalf("summary details = %v, want suppressed 47 plus the first entry's own details", s.Details)
	}
	if s.IP != "198.51.100.7" || s.Action != audit.ActionPermissionDenied || s.Reason != "unauthenticated" ||
		s.TargetID != "GET /api/me" || s.Result != audit.ResultDenied || s.Actor.Type != audit.ActorAnonymous {
		t.Fatalf("summary = %+v, want the suppressed entry's own fields", s)
	}
	if err := th.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(next.all()); got != 4 {
		t.Fatalf("a second Flush wrote %d more rows, want none", got-4)
	}
}

func TestAuditThrottleAddressesInOneSlash64ShareABudget(t *testing.T) {
	next := &recorder{}
	th := newThrottle(next, 1, newClock())
	ctx := context.Background()
	_ = th.Record(ctx, anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", "2001:db8:1:2::a"))
	_ = th.Record(ctx, anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", "2001:db8:1:2::b"))
	if got := len(next.all()); got != 1 {
		t.Fatalf("rows = %d, want 1: both addresses are one /64", got)
	}
	if th.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", th.Pending())
	}
}

func TestAuditThrottleDifferentActionsAreSummarisedSeparately(t *testing.T) {
	next := &recorder{}
	th := newThrottle(next, 1, newClock())
	ctx := context.Background()
	for range 3 {
		_ = th.Record(ctx, anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", "198.51.100.7"))
		_ = th.Record(ctx, anon(audit.ActionSetupAttemptDenied, "setup_complete", "", "198.51.100.7"))
	}
	if err := th.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var summaries int
	for _, e := range next.all() {
		if e.Details["suppressed"] != "" {
			summaries++
		}
	}
	// The one address has one budget, so only the first entry was written, and
	// the five others fall under two actions.
	if summaries != 2 {
		t.Fatalf("summary rows = %d, want 2 (one per action)", summaries)
	}
}

func TestAuditThrottlePendingCapFoldsIntoAnOverflowSummaryPerAction(t *testing.T) {
	next := &recorder{}
	th := newThrottle(next, 1, newClock())
	th.SetMaxPending(2)
	ctx := context.Background()
	for _, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4"} {
		for range 3 { // one written, two suppressed
			_ = th.Record(ctx, anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", ip))
		}
	}
	if err := th.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var named, overflow int
	var overflowCount int
	for _, e := range next.all() {
		n, ok := e.Details["suppressed"]
		if !ok {
			continue
		}
		if e.Details["sources"] == "many" {
			overflow++
			if e.IP != "" {
				t.Fatalf("overflow summary has IP %q, want none", e.IP)
			}
			overflowCount, _ = strconv.Atoi(n)
			continue
		}
		named++
	}
	if named != 2 || overflow != 1 || overflowCount != 4 {
		t.Fatalf("named=%d overflow=%d overflowCount=%d, want 2 named, 1 overflow row counting 4", named, overflow, overflowCount)
	}
}

func TestAuditThrottleFlushFailureKeepsTheCountsForTheNextFlush(t *testing.T) {
	next := &recorder{}
	th := newThrottle(next, 1, newClock())
	ctx := context.Background()
	for range 4 {
		_ = th.Record(ctx, anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", "198.51.100.7"))
	}
	next.fail(errors.New("disk full"))
	if err := th.Flush(ctx); err == nil {
		t.Fatal("Flush hid a write failure")
	}
	if th.Pending() != 1 {
		t.Fatalf("pending after a failed Flush = %d, want the key kept", th.Pending())
	}
	next.fail(nil)
	if err := th.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows := next.all()
	if len(rows) != 2 || rows[1].Details["suppressed"] != "3" {
		t.Fatalf("rows = %+v, want the first entry then a summary of 3", rows)
	}
}

func TestAuditThrottleRunFlushesOnEachTickAndOnceMoreAtShutdown(t *testing.T) {
	// Run is a ticker, so it runs in a synctest bubble and the ticks come on the fake clock.
	synctest.Test(t, func(t *testing.T) {
		flushesOnTicksAndAtShutdown(t)
	})
}

func flushesOnTicksAndAtShutdown(t *testing.T) {
	next := &recorder{}
	th := newThrottle(next, 1, newClock())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { th.Run(ctx, 5*time.Millisecond); close(done) }()

	for range 3 {
		_ = th.Record(context.Background(), anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", "198.51.100.7"))
	}
	synctest.Sleep(10 * time.Millisecond) // past the first 5 ms tick
	if got := len(next.all()); got < 2 {
		t.Fatal("Run did not flush on a tick")
	}

	// A refusal that arrives just before shutdown still reaches the log.
	_ = th.Record(context.Background(), anon(audit.ActionSetupAttemptDenied, "setup_complete", "", "198.51.100.7"))
	_ = th.Record(context.Background(), anon(audit.ActionSetupAttemptDenied, "setup_complete", "", "198.51.100.7"))
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if th.Pending() != 0 {
		t.Fatalf("pending after shutdown = %d, want the final Flush to have emptied it", th.Pending())
	}
}

func TestAuditThrottleOverARealWriterBoundsRowsAndKeepsTheChainIntact(t *testing.T) {
	f := newLoginFixture(t)
	v := newVault(t, 7)
	th := auth.NewAuditThrottle(f.aud, auth.NewIPLimiter(time.Hour, 3, 10*time.Minute, f.clk.now), slog.New(slog.DiscardHandler), f.clk.now)
	ctx := context.Background()
	for range 200 {
		if err := th.Record(ctx, anon(audit.ActionPermissionDenied, "unauthenticated", "GET /api/me", loginIP)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.audits(t, audit.ActionPermissionDenied)); got > 3 {
		t.Fatalf("rows before Flush = %d, want at most 3", got)
	}
	if err := th.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows := f.audits(t, audit.ActionPermissionDenied)
	if len(rows) != 4 || rows[3].Details == "" {
		t.Fatalf("rows after Flush = %d, want 3 plus one summary", len(rows))
	}
	res, err := audit.Verify(ctx, f.st, v)
	if err != nil || res.Broken != nil {
		t.Fatalf("Verify = %+v, %v; want an intact chain", res, err)
	}
}

func TestLoginRateLimitedRefusalsGoThroughTheAnonymousThrottle(t *testing.T) {
	f := newLoginFixture(t)
	v := newVault(t, 7)
	lim := auth.NewIPLimiter(time.Hour, 1, 10*time.Minute, f.clk.now)
	lg := auth.NewLogin(f.st, f.sess, f.aud, maxFailures, lockout, lim, slog.New(slog.DiscardHandler), f.clk.now)
	lg.SetDummyVerify(func(string) {})
	th := auth.NewAuditThrottle(f.aud, auth.NewIPLimiter(time.Hour, 2, 10*time.Minute, f.clk.now), slog.New(slog.DiscardHandler), f.clk.now)
	lg.SetAnonymousAuditor(th)

	for range 30 {
		_, err := lg.Login(context.Background(), auth.LoginInput{Username: "nobody", Password: "x", IP: loginIP, UserAgent: loginUA})
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	}
	rows := f.audits(t, audit.ActionAuthLoginFailed)
	var limited, unknown int
	for _, r := range rows {
		switch r.Reason {
		case "rate_limited":
			limited++
		case "unknown_user":
			unknown++
		}
	}
	if unknown != 1 || limited != 2 {
		t.Fatalf("unknown_user=%d rate_limited=%d, want 1 and 2 before Flush", unknown, limited)
	}
	if err := th.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows = f.audits(t, audit.ActionAuthLoginFailed)
	last := rows[len(rows)-1]
	var det map[string]string
	if err := json.Unmarshal([]byte(last.Details), &det); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || last.Reason != "rate_limited" || det["suppressed"] != "27" {
		t.Fatalf("after Flush rows=%d last=%+v, want a rate_limited summary with suppressed 27", len(rows), last)
	}
	if res, err := audit.Verify(context.Background(), f.st, v); err != nil || res.Broken != nil {
		t.Fatalf("Verify = %+v, %v", res, err)
	}
}
