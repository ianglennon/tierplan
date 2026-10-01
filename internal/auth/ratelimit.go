// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// IPLimiter is a per-client-address token bucket. Plan 12 reuses it for the
// per-IP login throttle (D-29); the setup gate uses it for D-05.
//
// Idle entries are evicted lazily, at most once per ttl, so the map cannot
// grow without bound. An entry that keeps being used is never evicted, so an
// attacker cannot reset their budget by outlasting the sweep.
type IPLimiter struct {
	every time.Duration
	burst int
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	entries   map[string]*ipEntry
	lastSweep time.Time
}

type ipEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewIPLimiter allows burst calls per address at once and refills one call per
// every. Entries idle for ttl are dropped. now is injected so tests control the
// clock; nil means time.Now.
func NewIPLimiter(every time.Duration, burst int, ttl time.Duration, now func() time.Time) *IPLimiter {
	if now == nil {
		now = time.Now
	}
	return &IPLimiter{
		every: every, burst: burst, ttl: ttl, now: now,
		entries:   make(map[string]*ipEntry),
		lastSweep: now(),
	}
}

// Allow reports whether ip may make another attempt, and counts it if so.
func (l *IPLimiter) Allow(ip string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= l.ttl {
		for k, e := range l.entries {
			if now.Sub(e.seen) >= l.ttl {
				delete(l.entries, k)
			}
		}
		l.lastSweep = now
	}

	e, ok := l.entries[ip]
	if !ok {
		e = &ipEntry{lim: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.entries[ip] = e
	}
	e.seen = now
	return e.lim.AllowN(now, 1)
}

// Len is the number of addresses currently tracked.
func (l *IPLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
