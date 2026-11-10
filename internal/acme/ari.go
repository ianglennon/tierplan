// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import "time"

// RenewBasis says what a renewal instant was worked out from; it is stored beside the instant so
// the settings tab can say why the certificate renews when it does.
type RenewBasis string

const (
	// BasisARI is an instant picked inside the CA's ACME Renewal Information window.
	BasisARI RenewBasis = "ari"
	// BasisTwoThirds is two thirds through the certificate's own lifetime.
	BasisTwoThirds RenewBasis = "two_thirds"
	// BasisHalf is half way through the certificate's own lifetime, for short-lived certificates.
	BasisHalf RenewBasis = "half"
)

// ShortLifetime is the lifetime below which a certificate renews at half life and the retry
// delay is capped (ShortLifetimeMaxDelay): two thirds of a six day certificate leaves too little
// time to recover from a failed run.
const ShortLifetime = 10 * 24 * time.Hour

// Check intervals for the CA's renewal information: the CA's Retry-After is followed between
// these bounds, and the default is used when it gives none. Twice a day at the least is what
// Let's Encrypt asks of a client.
const (
	defaultARICheck = 6 * time.Hour
	minARICheck     = time.Hour
	maxARICheck     = 12 * time.Hour
)

// FallbackRenewAt is when a certificate is due for renewal when the CA gave no renewal window:
// two thirds through its own lifetime, or half way when the lifetime is under ShortLifetime.
// It is never a fixed number of days, because a CA's lifetimes change. A lifetime of zero or less
// is due at once (the not-before instant).
func FallbackRenewAt(notBefore, notAfter time.Time) (time.Time, RenewBasis) {
	lifetime := notAfter.Sub(notBefore)
	if lifetime <= 0 {
		return notBefore, BasisHalf
	}
	if lifetime < ShortLifetime {
		return notBefore.Add(lifetime / 2), BasisHalf
	}
	// (l/3)*2 plus the remainder's share keeps the integer result within a nanosecond of exact
	// two thirds without overflowing for very long lifetimes.
	return notBefore.Add(lifetime/3*2 + lifetime%3*2/3), BasisTwoThirds
}

// PickInWindow is the renewal instant for an ARI window [start, end]. A previously picked
// instant (kept, zero when none) is returned while it is still inside the window, so a restart
// or a later poll does not move it; otherwise one instant is drawn uniformly from the window
// with rnd, which returns a value in [0, n). A window that is empty or inverted gives start.
func PickInWindow(start, end, kept time.Time, rnd func(n int64) int64) time.Time {
	if !kept.IsZero() && !kept.Before(start) && !kept.After(end) {
		return kept
	}
	width := int64(end.Sub(start))
	if width <= 0 {
		return start
	}
	r := rnd(width)
	if r < 0 {
		r = 0
	}
	if r >= width {
		r = width - 1
	}
	return start.Add(time.Duration(r))
}

// NextARICheck is when the CA's renewal information is asked again: the CA's Retry-After from
// now, clamped to between 1 hour and 12 hours, and 6 hours when it gave none.
func NextARICheck(now time.Time, retryAfter time.Duration) time.Time {
	d := retryAfter
	switch {
	case d <= 0:
		d = defaultARICheck
	case d < minARICheck:
		d = minARICheck
	case d > maxARICheck:
		d = maxARICheck
	}
	return now.Add(d)
}
