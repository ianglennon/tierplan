// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"sync"
	"time"
)

// Limits of the memory that keeps a viewer from being audited again on every
// reconnect (D-24).
const (
	logAuditTTL     = time.Hour
	logAuditMax     = 10_000
	logAuditSweepAt = time.Minute
)

// logAuditKey is one viewer of one container: the user, the container as the route
// named it, and the viewer's own random ID, which the page makes when it opens and
// sends on every stream and page it asks for (D-24).
type logAuditKey struct {
	user, container, sid string
}

// logAuditMemory remembers which viewers have been audited, so a reconnect of the
// same viewer writes no second row while another viewer, or the same person in
// another window, does. It is bounded: entries expire after the TTL since the viewer
// was last seen, and when the memory is full a new viewer is audited every time it
// opens a stream. Forgetting only ever means writing a row that was not needed.
type logAuditMemory struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu        sync.Mutex
	at        map[logAuditKey]time.Time
	lastSweep time.Time
}

func newLogAuditMemory(ttl time.Duration, max int, now func() time.Time) *logAuditMemory {
	if now == nil {
		now = time.Now
	}
	return &logAuditMemory{ttl: ttl, max: max, now: now, at: map[logAuditKey]time.Time{}}
}

// seen reports whether the viewer has already been audited, and remembers it when it
// has not. False means: write the row. That includes the case where there is no room
// to remember it. A viewer that is seen again has its time to live extended, so a
// page that stays open for hours and reconnects now and then is audited once.
func (m *logAuditMemory) seen(k logAuditKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if t, ok := m.at[k]; ok && now.Sub(t) < m.ttl {
		m.at[k] = now
		return true
	}
	if len(m.at) >= m.max || now.Sub(m.lastSweep) >= logAuditSweepAt {
		for key, t := range m.at {
			if now.Sub(t) >= m.ttl {
				delete(m.at, key)
			}
		}
		m.lastSweep = now
	}
	if len(m.at) >= m.max {
		return false
	}
	m.at[k] = now
	return false
}

// size is how many viewers are remembered, expired ones included until the next sweep.
func (m *logAuditMemory) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.at)
}
