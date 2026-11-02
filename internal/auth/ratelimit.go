// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package auth

import (
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// IPLimiter is a per-client-address token bucket. Plan 12 reuses it for the
// per-IP login throttle (D-29); the setup gate uses it for D-05.
//
// Keys are normalised (see limiterKey): every IPv6 address in one /64 shares a
// budget, an IPv4-mapped IPv6 address shares its IPv4 address's budget, and a
// key that is not an address (a user ID) is used unchanged.
//
// Idle entries are evicted lazily, at most once per ttl, so the map cannot
// grow without bound. An entry that keeps being used is never evicted, so an
// attacker cannot reset their budget by outlasting the sweep. The table also
// holds at most maxEntries keys (defaultMaxEntries): when it is full, idle
// entries are swept at once and a new key that still does not fit is refused.
type IPLimiter struct {
	every      time.Duration
	burst      int
	ttl        time.Duration
	now        func() time.Time
	maxEntries int

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
		maxEntries: defaultMaxEntries,
		entries:    make(map[string]*ipEntry),
		lastSweep:  now(),
	}
}

// defaultMaxEntries bounds the limiter's memory (WR-03, T-01-G05).
const defaultMaxEntries = 65536

// limiterKey normalises a limiter key. A text that is not an IP address (a user
// ID, an empty string) is returned unchanged. An IPv4-mapped IPv6 address
// becomes its IPv4 text, any other IPv6 address becomes the text of its /64
// prefix, and an IPv4 address is returned as is. A /64 is the smallest block a
// provider routinely hands to one customer, so rotating addresses inside it
// must not buy a fresh budget (WR-03, T-01-36).
func limiterKey(raw string) string {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return raw
	}
	addr = addr.WithZone("")
	if addr.Is4In6() {
		return addr.Unmap().String()
	}
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return raw
	}
	return p.Masked().String()
}

// AddrKey is the key IPLimiter budgets by, for a caller that bounds something
// else per client address the same way: every address of one IPv6 /64 is one
// key, an IPv4-mapped IPv6 address is its IPv4 address, and a text that is not
// an address comes back unchanged. Without it a second counter would treat
// 2001:db8:1:2::1 and 2001:db8:1:2::2 as two clients while the limiter treats
// them as one.
func AddrKey(raw string) string { return limiterKey(raw) }

// Allow reports whether ip may make another attempt, and counts it if so.
func (l *IPLimiter) Allow(ip string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	key := limiterKey(ip)
	if now.Sub(l.lastSweep) >= l.ttl {
		l.sweep(now)
	}

	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= l.maxEntries {
			l.sweep(now)
			if len(l.entries) >= l.maxEntries {
				// Fail closed. This limiter guards password and setup-token
				// guessing, so a full table must not open a free path; the
				// per-account lock still protects known accounts.
				return false
			}
		}
		e = &ipEntry{lim: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.entries[key] = e
	}
	e.seen = now
	return e.lim.AllowN(now, 1)
}

// sweep drops entries idle for ttl. The caller holds l.mu.
func (l *IPLimiter) sweep(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.seen) >= l.ttl {
			delete(l.entries, k)
		}
	}
	l.lastSweep = now
}

// Len is the number of addresses currently tracked.
func (l *IPLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
