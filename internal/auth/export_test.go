// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package auth

// SetDummyVerify replaces the dummy hash check, so a test can count how many
// times a sign-in for an unknown or locked account pays for one (D-30).
func (l *Login) SetDummyVerify(f func(string)) { l.dummy = f }

// SetMaxEntries lowers the limiter's entry cap so a test can fill it.
func (l *IPLimiter) SetMaxEntries(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxEntries = n
}

// MaxEntries reports the limiter's entry cap.
func (l *IPLimiter) MaxEntries() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxEntries
}

// SetMaxPending lowers how many distinct suppressed-refusal keys the throttle
// holds before it folds new ones into a per-action overflow summary.
func (t *AuditThrottle) SetMaxPending(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxPending = n
}

// Pending is how many keys currently hold suppressed refusals.
func (t *AuditThrottle) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}
