// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import "context"

// SetRoundHook sets a function Run calls after every round, so a test waits on a signal instead of a poll.
func (s *Service) SetRoundHook(fn func()) { s.roundHook = fn }

// Round takes one round over every enabled environment, as Run does, and returns when it is done.
func (s *Service) Round(ctx context.Context) { s.round(ctx) }

// WakePending reports whether a wake is waiting for the loop, and takes it.
func (s *Service) WakePending() bool {
	select {
	case <-s.wake:
		return true
	default:
		return false
	}
}
