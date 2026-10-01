// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// DefaultSnapshotTTL is how long a snapshot is served before it is refetched.
const DefaultSnapshotTTL = 5 * time.Second

// fetchTimeout bounds one fetch of the resource lists. The fetch is shared by
// every caller waiting on it, so it is not tied to any one request's context.
const fetchTimeout = 20 * time.Second

// Snapshots caches one dockerx.Snapshot per environment for a short time, so
// the dashboard tiles and the four list views all read the same lists
// (Pitfall 22) and a busy page cannot flood the daemon (T-01-79). The data is
// read live from Docker and never stored anywhere else (D-57). It is safe for
// concurrent use.
type Snapshots struct {
	mon *Monitor
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*snapEntry
}

// snapEntry is the cache slot of one environment.
type snapEntry struct {
	snap dockerx.Snapshot
	at   time.Time
	ok   bool
	// flight is the fetch in progress, if any. Callers that arrive while it
	// runs wait for it instead of starting another.
	flight *snapFlight
}

type snapFlight struct {
	done chan struct{}
	snap dockerx.Snapshot
	err  error
}

// NewSnapshots returns a cache over m. A ttl of zero or less means
// DefaultSnapshotTTL; a nil now means time.Now.
func NewSnapshots(m *Monitor, ttl time.Duration, now func() time.Time) *Snapshots {
	if ttl <= 0 {
		ttl = DefaultSnapshotTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Snapshots{mon: m, ttl: ttl, now: now, entries: map[string]*snapEntry{}}
}

// Get returns the snapshot of envID together with its status. A cached
// snapshot younger than the TTL is returned as is; otherwise the lists are
// fetched once however many callers ask at the same moment. When the
// environment is offline or unsupported it returns the Monitor's ErrOffline or
// ErrUnsupported untouched, with the status, and nothing is cached. A caller
// whose ctx ends stops waiting with that error; the fetch carries on for the
// others.
func (s *Snapshots) Get(ctx context.Context, envID string) (dockerx.Snapshot, Status, error) {
	cli, st, err := s.mon.Client(envID)
	if err != nil {
		return dockerx.Snapshot{}, st, err
	}
	swarmManager := st.Probe.Type == dockerx.TypeSwarmManager

	s.mu.Lock()
	e := s.entries[envID]
	if e == nil {
		e = &snapEntry{}
		s.entries[envID] = e
	}
	if e.ok && s.now().Sub(e.at) < s.ttl {
		snap := e.snap
		s.mu.Unlock()
		return snap, st, nil
	}
	f := e.flight
	if f == nil {
		f = &snapFlight{done: make(chan struct{})}
		e.flight = f
		// Detached from this caller: the fetch is shared, so one caller giving
		// up must not fail the others.
		go s.fetch(context.WithoutCancel(ctx), envID, e, f, cli, swarmManager)
	}
	s.mu.Unlock()

	select {
	case <-f.done:
		return f.snap, st, f.err
	case <-ctx.Done():
		return dockerx.Snapshot{}, st, ctx.Err()
	}
}

// fetch reads the lists once for everyone waiting on f. Its result is cached
// only when the entry it was started for is still the current one, so a fetch
// that Invalidate overtook is delivered to its waiters but never served later.
func (s *Snapshots) fetch(ctx context.Context, envID string, e *snapEntry, f *snapFlight, cli *dockerx.Client, swarmManager bool) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	snap, err := cli.Snapshot(ctx, swarmManager)

	s.mu.Lock()
	if e.flight == f {
		e.flight = nil
	}
	if err == nil && s.entries[envID] == e {
		e.snap, e.at, e.ok = snap, s.now(), true
	}
	s.mu.Unlock()

	f.snap, f.err = snap, err
	close(f.done)
}

// Invalidate drops the cached snapshot of envID, so the next Get fetches. The
// events feed calls it when Docker reports a change, and nothing happens for an
// environment that has no snapshot.
func (s *Snapshots) Invalidate(envID string) {
	s.mu.Lock()
	delete(s.entries, envID)
	s.mu.Unlock()
}
