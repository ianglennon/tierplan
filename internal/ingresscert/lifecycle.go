// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import "time"

// Grace is how long a certificate nobody refers to is kept, and how long a request-ahead holds its row
// (D-06, D-08). It is measured from the request for a requested row and from the release for a released one.
const Grace = 30 * 24 * time.Hour

// RequestHeld reports whether c is held by a request-ahead at now: an administrator asked for the
// certificate ahead of any claim (RequestedAt is set) and no more than Grace has passed since the request.
// While it holds, no other environment may take the row over (D-08, D-21). It is pure: now is the caller's
// clock.
func RequestHeld(c Cert, now time.Time) bool {
	if c.RequestedAt.IsZero() {
		return false
	}
	return !now.After(c.RequestedAt.Add(Grace))
}
