// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import "time"

// Clock is the time the deploy waits on. The convergence wait polls on it and judges its timeout by
// it, so a test advances time itself instead of sleeping (docs/testing.md, "Waiting in tests").
type Clock interface {
	// Now is the current time.
	Now() time.Time
	// After delivers the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the wall clock, for the code that builds Deps.
func RealClock() Clock { return realClock{} }
