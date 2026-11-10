// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	tests := []struct {
		name     string
		failures int
		lifetime time.Duration
		jitter   float64
		want     time.Duration
	}{
		{"failure 1 waits a minute", 1, 90 * day, 0, time.Minute},
		{"failure 2 waits 10 minutes", 2, 90 * day, 0, 10 * time.Minute},
		{"failure 3 waits 100 minutes", 3, 90 * day, 0, 100 * time.Minute},
		{"failure 4 waits 24 hours", 4, 90 * day, 0, 24 * time.Hour},
		{"failure 5 waits 24 hours", 5, 90 * day, 0, 24 * time.Hour},
		{"failure 50 waits 24 hours", 50, 90 * day, 0, 24 * time.Hour},
		{"zero failures count as the first", 0, 90 * day, 0, time.Minute},
		{"jitter +10% lengthens it", 3, 90 * day, 0.1, 110 * time.Minute},
		{"jitter -10% shortens it", 3, 90 * day, -0.1, 90 * time.Minute},
		{"jitter above +10% is clamped", 3, 90 * day, 0.5, 110 * time.Minute},
		{"jitter below -10% is clamped", 3, 90 * day, -0.5, 90 * time.Minute},
		{"a short lifetime caps the 24 h wait at 6 h", 4, 6 * day, 0, 6 * time.Hour},
		{"a short lifetime does not touch a wait below the cap", 3, 6 * day, 0, 100 * time.Minute},
		{"a short lifetime never goes over 6 h even with jitter", 4, 6 * day, 0.1, 6 * time.Hour},
		{"a short lifetime with negative jitter stays below the cap", 4, 6 * day, -0.1, 5*time.Hour + 24*time.Minute},
		{"exactly 10 days is not short", 4, 10 * day, 0, 24 * time.Hour},
		{"just under 10 days is short", 4, 10*day - time.Second, 0, 6 * time.Hour},
		{"an unknown lifetime has no cap", 4, 0, 0, 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BackoffDelay(tc.failures, tc.lifetime, tc.jitter); got != tc.want {
				t.Errorf("delay = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecordFailureHonoursRetryAfter(t *testing.T) {
	now := t0
	t.Run("an empty ledger and no Retry-After sets only the soft schedule", func(t *testing.T) {
		got := RecordFailure(Ledger{}, now, 0, 90*day, 0)
		want := Ledger{ConsecutiveFailures: 1, LastAttemptAt: now, NextAttemptAt: now.Add(time.Minute)}
		if got != want {
			t.Errorf("ledger = %+v, want %+v", got, want)
		}
		if !got.RetryAfterUntil.IsZero() {
			t.Errorf("retry_after_until = %v, want zero", got.RetryAfterUntil)
		}
	})
	t.Run("the count climbs along the schedule", func(t *testing.T) {
		l := Ledger{}
		for i, want := range []time.Duration{time.Minute, 10 * time.Minute, 100 * time.Minute, 24 * time.Hour, 24 * time.Hour} {
			l = RecordFailure(l, now, 0, 90*day, 0)
			if l.ConsecutiveFailures != i+1 || !l.NextAttemptAt.Equal(now.Add(want)) {
				t.Errorf("failure %d: %+v, want next %v", i+1, l, now.Add(want))
			}
		}
	})
	t.Run("a Retry-After is stored as an absolute instant", func(t *testing.T) {
		got := RecordFailure(Ledger{}, now, 30*time.Minute, 90*day, 0)
		if want := now.Add(30 * time.Minute); !got.RetryAfterUntil.Equal(want) {
			t.Errorf("retry_after_until = %v, want %v", got.RetryAfterUntil, want)
		}
	})
	t.Run("a Retry-After of exactly 7 days is honoured", func(t *testing.T) {
		got := RecordFailure(Ledger{}, now, MaxRetryAfter, 90*day, 0)
		if want := now.Add(MaxRetryAfter); !got.RetryAfterUntil.Equal(want) {
			t.Errorf("retry_after_until = %v, want %v", got.RetryAfterUntil, want)
		}
	})
	t.Run("a Retry-After of 30 days is cut to 7", func(t *testing.T) {
		got := RecordFailure(Ledger{}, now, 30*day, 90*day, 0)
		if want := now.Add(7 * day); !got.RetryAfterUntil.Equal(want) {
			t.Errorf("retry_after_until = %v, want %v", got.RetryAfterUntil, want)
		}
	})
	t.Run("a later Retry-After replaces an earlier one", func(t *testing.T) {
		l := Ledger{RetryAfterUntil: now.Add(time.Hour)}
		got := RecordFailure(l, now, 2*time.Hour, 90*day, 0)
		if want := now.Add(2 * time.Hour); !got.RetryAfterUntil.Equal(want) {
			t.Errorf("retry_after_until = %v, want %v", got.RetryAfterUntil, want)
		}
	})
	t.Run("an earlier Retry-After never replaces a later one", func(t *testing.T) {
		l := Ledger{RetryAfterUntil: now.Add(5 * time.Hour)}
		got := RecordFailure(l, now, time.Hour, 90*day, 0)
		if want := now.Add(5 * time.Hour); !got.RetryAfterUntil.Equal(want) {
			t.Errorf("retry_after_until = %v, want %v", got.RetryAfterUntil, want)
		}
	})
	t.Run("a failure without Retry-After keeps the one in force", func(t *testing.T) {
		l := Ledger{RetryAfterUntil: now.Add(5 * time.Hour)}
		got := RecordFailure(l, now, 0, 90*day, 0)
		if want := now.Add(5 * time.Hour); !got.RetryAfterUntil.Equal(want) {
			t.Errorf("retry_after_until = %v, want %v", got.RetryAfterUntil, want)
		}
	})
	t.Run("a short lifetime caps the soft schedule but not the Retry-After", func(t *testing.T) {
		l := Ledger{ConsecutiveFailures: 3}
		got := RecordFailure(l, now, 2*day, 6*day, 0)
		if want := now.Add(6 * time.Hour); !got.NextAttemptAt.Equal(want) {
			t.Errorf("next_attempt_at = %v, want %v", got.NextAttemptAt, want)
		}
		if want := now.Add(2 * day); !got.RetryAfterUntil.Equal(want) {
			t.Errorf("retry_after_until = %v, want %v", got.RetryAfterUntil, want)
		}
	})
	t.Run("jitter reaches the soft schedule", func(t *testing.T) {
		got := RecordFailure(Ledger{ConsecutiveFailures: 2}, now, 0, 90*day, 0.1)
		if want := now.Add(110 * time.Minute); !got.NextAttemptAt.Equal(want) {
			t.Errorf("next_attempt_at = %v, want %v", got.NextAttemptAt, want)
		}
	})
}

func TestAutomaticAllowedBoundary(t *testing.T) {
	now := t0
	tests := []struct {
		name string
		l    Ledger
		now  time.Time
		want bool
	}{
		{"the empty ledger allows every attempt", Ledger{}, now, true},
		{"before the soft schedule", Ledger{NextAttemptAt: now.Add(time.Second)}, now, false},
		{"at exactly the soft schedule", Ledger{NextAttemptAt: now}, now, true},
		{"after the soft schedule", Ledger{NextAttemptAt: now.Add(-time.Second)}, now, true},
		{"before the Retry-After", Ledger{RetryAfterUntil: now.Add(time.Second)}, now, false},
		{"at exactly the Retry-After", Ledger{RetryAfterUntil: now}, now, true},
		{"the Retry-After wins over an earlier soft schedule", Ledger{NextAttemptAt: now.Add(-time.Hour), RetryAfterUntil: now.Add(time.Minute)}, now, false},
		{"the soft schedule wins over an earlier Retry-After", Ledger{NextAttemptAt: now.Add(time.Minute), RetryAfterUntil: now.Add(-time.Hour)}, now, false},
		{"both passed", Ledger{NextAttemptAt: now.Add(-time.Minute), RetryAfterUntil: now.Add(-time.Hour)}, now, true},
		{"allowed at the later of the two", Ledger{NextAttemptAt: now.Add(-time.Minute), RetryAfterUntil: now}, now, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AutomaticAllowed(tc.l, tc.now); got != tc.want {
				t.Errorf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManualBlock(t *testing.T) {
	now := t0
	ago := func(ds ...time.Duration) []time.Time {
		out := make([]time.Time, 0, len(ds))
		for _, d := range ds {
			out = append(out, now.Add(-d))
		}
		return out
	}
	tests := []struct {
		name   string
		l      Ledger
		runs   []time.Time
		until  time.Time
		reason string
	}{
		{"nothing recorded is allowed", Ledger{}, nil, time.Time{}, ""},
		{"a Retry-After in the future blocks with no failures", Ledger{RetryAfterUntil: now.Add(time.Hour)}, nil, now.Add(time.Hour), BlockToldToWait},
		{"a Retry-After in the future blocks whatever the failure count", Ledger{ConsecutiveFailures: 9, RetryAfterUntil: now.Add(time.Hour)}, ago(50*time.Minute, 30*time.Minute, 10*time.Minute), now.Add(time.Hour), BlockToldToWait},
		{"told_to_wait comes before failure_guard", Ledger{RetryAfterUntil: now.Add(time.Minute)}, ago(50*time.Minute, 30*time.Minute, 10*time.Minute), now.Add(time.Minute), BlockToldToWait},
		{"a Retry-After that has passed does not block", Ledger{RetryAfterUntil: now.Add(-time.Second)}, nil, time.Time{}, ""},
		{"a Retry-After at exactly now does not block", Ledger{RetryAfterUntil: now}, nil, time.Time{}, ""},
		{"three failures in the hour block until an hour after the oldest", Ledger{}, ago(50*time.Minute, 30*time.Minute, 10*time.Minute), now.Add(10 * time.Minute), BlockFailureGuard},
		{"the order of the runs does not matter", Ledger{}, ago(10*time.Minute, 50*time.Minute, 30*time.Minute), now.Add(10 * time.Minute), BlockFailureGuard},
		{"an oldest failure 61 minutes ago is outside the hour", Ledger{}, ago(61*time.Minute, 30*time.Minute, 10*time.Minute), time.Time{}, ""},
		{"an oldest failure 59 minutes ago is inside the hour", Ledger{}, ago(59*time.Minute, 30*time.Minute, 10*time.Minute), now.Add(time.Minute), BlockFailureGuard},
		{"exactly one hour after the oldest is allowed again", Ledger{}, ago(time.Hour, 30*time.Minute, 10*time.Minute), time.Time{}, ""},
		{"one second before the hour is still blocked", Ledger{}, ago(time.Hour-time.Second, 30*time.Minute, 10*time.Minute), now.Add(time.Second), BlockFailureGuard},
		{"two failures in the hour are allowed", Ledger{}, ago(30*time.Minute, 10*time.Minute), time.Time{}, ""},
		{"the three latest of four decide", Ledger{}, ago(70*time.Minute, 50*time.Minute, 30*time.Minute, 10*time.Minute), now.Add(10 * time.Minute), BlockFailureGuard},
		{"old failures do not count", Ledger{}, ago(5*time.Hour, 4*time.Hour, 3*time.Hour), time.Time{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var before []time.Time
			before = append(before, tc.runs...)
			until, reason := ManualBlock(tc.l, tc.runs, now)
			if !until.Equal(tc.until) || reason != tc.reason {
				t.Errorf("block = (%v, %q), want (%v, %q)", until, reason, tc.until, tc.reason)
			}
			for i := range before {
				if !tc.runs[i].Equal(before[i]) {
					t.Errorf("the caller's failedRuns slice was reordered")
					break
				}
			}
		})
	}
}

func TestRecordSuccessKeepsRetryAfter(t *testing.T) {
	now := t0
	l := Ledger{ConsecutiveFailures: 4, LastAttemptAt: now.Add(-time.Hour), NextAttemptAt: now.Add(time.Hour), RetryAfterUntil: now.Add(3 * time.Hour)}
	got := RecordSuccess(l)
	if got.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d, want 0", got.ConsecutiveFailures)
	}
	if !got.NextAttemptAt.IsZero() {
		t.Errorf("next_attempt_at = %v, want zero", got.NextAttemptAt)
	}
	if !got.RetryAfterUntil.Equal(l.RetryAfterUntil) {
		t.Errorf("retry_after_until = %v, want %v still in force", got.RetryAfterUntil, l.RetryAfterUntil)
	}
	if !got.LastAttemptAt.Equal(l.LastAttemptAt) {
		t.Errorf("last_attempt_at moved to %v", got.LastAttemptAt)
	}
	if AutomaticAllowed(got, now) {
		t.Errorf("a success must not let an attempt through a Retry-After still in force")
	}
}
