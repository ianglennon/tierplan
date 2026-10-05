// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"math/rand/v2"
	"time"
)

const (
	defaultBackoffMin = time.Second
	defaultBackoffMax = time.Minute
)

// backoff is the delay after the nth consecutive failure: lo, 2*lo, 4*lo and so
// on up to hi, then jittered into the upper half of that step so the delay never
// exceeds hi. It has the shape of the server's own retry backoff (internal/envs),
// and the jitter is what keeps a whole swarm that lost the server at once from
// reconnecting in the same instant (RESEARCH Pitfall 7). rnd returns a value in
// [0, 1); nil means the process's random source.
func backoff(failures int, lo, hi time.Duration, rnd func() float64) time.Duration {
	if rnd == nil {
		rnd = rand.Float64 //nolint:gosec // jitter, not a security decision
	}
	if lo <= 0 {
		lo = defaultBackoffMin
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
