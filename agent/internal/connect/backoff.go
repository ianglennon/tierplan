// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import "time"

// backoff is the delay after the nth consecutive failure. Stub.
func backoff(failures int, lo, hi time.Duration, rnd func() float64) time.Duration { return 0 }
