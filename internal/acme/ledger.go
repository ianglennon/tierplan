// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"slices"
	"time"
)

// The limits on how often TierPlan asks a CA, kept inside what Let's Encrypt publishes.
const (
	// MaxRetryAfter is the longest CA Retry-After that is honoured; a longer one is cut to it so a
	// hostile or buggy value cannot stop renewal for ever.
	MaxRetryAfter = 7 * 24 * time.Hour
	// ShortLifetimeMaxDelay caps a failure wait for a certificate whose lifetime is under
	// ShortLifetime: a 24 hour wait would outlast the certificate's renewal window.
	ShortLifetimeMaxDelay = 6 * time.Hour
	// FailureGuardRuns failed runs inside FailureGuardWindow block a manual Issue or Renew now.
	FailureGuardRuns = 3
	// FailureGuardWindow is the rolling window the failed runs are counted in.
	FailureGuardWindow = time.Hour
)

// Why a manual attempt is blocked (ManualBlock).
const (
	// BlockToldToWait: the CA sent a Retry-After that is still in the future.
	BlockToldToWait = "told_to_wait"
	// BlockFailureGuard: FailureGuardRuns runs failed inside FailureGuardWindow.
	BlockFailureGuard = "failure_guard"
)

// Ledger is the persisted record of failed attempts. NextAttemptAt is the soft schedule (our own
// backoff); RetryAfterUntil is the hard instant a CA told us to wait for. An attempt is allowed
// only once both have passed. Zero instants mean none; the zero Ledger allows everything.
type Ledger struct {
	ConsecutiveFailures int
	LastAttemptAt       time.Time
	NextAttemptAt       time.Time
	RetryAfterUntil     time.Time
}

// backoffSteps is Let's Encrypt's published failure schedule: 1 minute, 10 minutes, 100 minutes,
// then 24 hours for every later failure.
var backoffSteps = [...]time.Duration{time.Minute, 10 * time.Minute, 100 * time.Minute, 24 * time.Hour}

const maxJitter = 0.1

// BackoffDelay is how long to wait after the failures-th consecutive failure (counted from 1; a
// smaller number counts as the first). jitter, in [-0.1, 0.1] (clamped), scales the wait. For a
// certificate whose lifetime is known and under ShortLifetime the result never exceeds
// ShortLifetimeMaxDelay; a lifetime of 0 means unknown and is not capped.
func BackoffDelay(failures int, lifetime time.Duration, jitter float64) time.Duration {
	i := min(max(failures, 1), len(backoffSteps)) - 1
	jitter = min(max(jitter, -maxJitter), maxJitter)
	d := backoffSteps[i] + time.Duration(float64(backoffSteps[i])*jitter)
	if lifetime > 0 && lifetime < ShortLifetime {
		d = min(d, ShortLifetimeMaxDelay)
	}
	return d
}

// RecordFailure is the ledger after a failed attempt at now. The soft schedule moves out by the
// next BackoffDelay. A CA Retry-After (a duration: lego parses delta-seconds or an HTTP date) is
// stored as the absolute instant now + retryAfter, cut to MaxRetryAfter, and replaces an earlier
// instant only when it is later; without one the instant in force is kept.
func RecordFailure(l Ledger, now time.Time, retryAfter, lifetime time.Duration, jitter float64) Ledger {
	l.ConsecutiveFailures++
	l.LastAttemptAt = now
	l.NextAttemptAt = now.Add(BackoffDelay(l.ConsecutiveFailures, lifetime, jitter))
	if retryAfter > 0 {
		until := now.Add(min(retryAfter, MaxRetryAfter))
		if until.After(l.RetryAfterUntil) {
			l.RetryAfterUntil = until
		}
	}
	return l
}

// RecordSuccess is the ledger after a successful attempt: no failures and no soft schedule. A
// Retry-After still in force is kept, because the CA's instruction outlives one success.
func RecordSuccess(l Ledger) Ledger {
	l.ConsecutiveFailures = 0
	l.NextAttemptAt = time.Time{}
	return l
}

// AutomaticAllowed reports whether a scheduled attempt may start at now: only at or after
// max(NextAttemptAt, RetryAfterUntil), so a CA's Retry-After always wins over the soft schedule.
func AutomaticAllowed(l Ledger, now time.Time) bool {
	return !now.Before(l.NextAttemptAt) && !now.Before(l.RetryAfterUntil)
}

// ManualBlock says whether an admin's Issue or Renew now must wait, and until when. A CA
// Retry-After still in the future blocks first (BlockToldToWait), whatever the failure count.
// Otherwise FailureGuardRuns failed runs inside the rolling FailureGuardWindow (failedRuns, in
// any order; the caller's slice is not changed) block until one window after the oldest of the
// latest three; that instant itself is allowed. Allowed is a zero instant and "".
func ManualBlock(l Ledger, failedRuns []time.Time, now time.Time) (until time.Time, reason string) {
	if l.RetryAfterUntil.After(now) {
		return l.RetryAfterUntil, BlockToldToWait
	}
	var past []time.Time
	for _, r := range failedRuns {
		if !r.After(now) {
			past = append(past, r)
		}
	}
	if len(past) < FailureGuardRuns {
		return time.Time{}, ""
	}
	slices.SortFunc(past, func(a, b time.Time) int { return a.Compare(b) })
	oldest := past[len(past)-FailureGuardRuns]
	if end := oldest.Add(FailureGuardWindow); now.Before(end) {
		return end, BlockFailureGuard
	}
	return time.Time{}, ""
}
