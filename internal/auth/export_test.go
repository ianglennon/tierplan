// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
