// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package backoff is the delay between attempts for the channel dial and the
// Traefik restart (D-19, D-20). The agent has the same function; the modules
// cannot share agent/internal, so this is a copy.
package backoff

import (
	"math/rand/v2"
	"time"
)

const defaultMin = time.Second

// Delay is the delay after the nth consecutive failure: lo, 2*lo, 4*lo and so on
// up to hi, then jittered into the upper half of that step so the delay never
// exceeds hi. The jitter is what keeps a swarm that lost TierPlan at once from
// reconnecting in the same instant. rnd returns a value in [0, 1); nil means the
// process's random source. The bounds are the caller's: the channel uses 1 s to
// 60 s and the Traefik restart 1 s to 30 s. A lo of zero or less is 1 s, a hi
// below lo is lo, and fewer than one failure counts as one.
func Delay(failures int, lo, hi time.Duration, rnd func() float64) time.Duration {
	if rnd == nil {
		rnd = rand.Float64 //nolint:gosec // jitter, not a security decision
	}
	if lo <= 0 {
		lo = defaultMin
	}
	if hi < lo {
		hi = lo
	}
	step := lo
	for i := 1; i < failures && step < hi; i++ {
		step *= 2
	}
	step = min(step, hi)
	half := step / 2
	return half + time.Duration(rnd()*float64(half))
}
