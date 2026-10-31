// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"
)

// Cadence of the background reader (D-09).
const (
	// eventDebounce is how long a network, container or service event waits before the environment
	// is read again, so a burst of events costs one read.
	eventDebounce = 10 * time.Second
	// addressParallel is the most environments read at once.
	addressParallel = 4
	// addressReadTimeout bounds one background read of one environment.
	addressReadTimeout = 2 * time.Minute
	// The pause before a subscription that ended is opened again, doubling up to the larger.
	watchRetryMin = 5 * time.Second
	watchRetryMax = time.Minute
)

// Run reads every enabled environment's address space and orphan list every Interval, and again
// after a network, container or service event of that environment (debounced), until ctx ends.
// The reads keep the orphan check's last-seen times current when nobody opens the page (D-09), and
// the summaries current for the overview, the dashboard and the health of each environment. It
// returns once everything it started has stopped.
func (s *AddressSpaces) Run(ctx context.Context) {
	s.bmu.Lock()
	s.bg = ctx
	s.bmu.Unlock()
	defer func() {
		s.bmu.Lock()
		s.bg = nil
		s.bmu.Unlock()
		s.wg.Wait()
	}()
	ticks := s.ticks
	if ticks == nil {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		ticks = t.C
	}
	for {
		s.refreshAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// refreshAll reads every enabled environment, a few at a time, keeps one event watcher per enabled
// environment, and forgets the environments that are gone or disabled.
func (s *AddressSpaces) refreshAll(ctx context.Context) {
	defer func() {
		if s.passHook != nil {
			s.passHook()
		}
	}()
	rows, err := s.svc.st.RQ.ListEnvironments(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("list environments to read their address space", "error", err)
		}
		return
	}
	var ids []string
	enabled := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.Enabled != 0 {
			ids = append(ids, row.ID)
			enabled[row.ID] = struct{}{}
		}
	}
	s.syncWatches(ctx, enabled)
	var g errgroup.Group
	g.SetLimit(addressParallel)
	for _, id := range ids {
		g.Go(func() error {
			s.readEnv(ctx, id)
			return nil
		})
	}
	_ = g.Wait()
	s.forget(enabled)
}

// readEnv makes one fresh read of the environment: its address space, then its orphan list, which
// stamps the networks it sees in use. An environment that cannot be read is skipped; what went
// wrong is the next page load's to show.
func (s *AddressSpaces) readEnv(ctx context.Context, envID string) {
	if s.readHook != nil {
		defer s.readHook(envID)()
	}
	ctx, cancel := context.WithTimeout(ctx, addressReadTimeout)
	defer cancel()
	e := s.entry(envID)
	e.mu.Lock()
	e.have = false // a read made for a reason is never the cached one
	e.mu.Unlock()
	if _, err := s.Read(ctx, envID); err != nil && ctx.Err() == nil {
		s.log.Debug("read an environment's address space", "environment", envID, "error", err)
	}
	s.ForgetOrphans(envID)
	if _, err := s.Orphans(ctx, envID); err != nil && ctx.Err() == nil {
		s.log.Debug("check an environment's networks for orphans", "environment", envID, "error", err)
	}
}

// forget drops everything held for the environments that are not in keep.
func (s *AddressSpaces) forget(keep map[string]struct{}) {
	s.mu.Lock()
	for id := range s.entries {
		if _, ok := keep[id]; !ok {
			delete(s.entries, id)
		}
	}
	s.mu.Unlock()
}

// Trigger asks for a fresh read of the environment soon: after eventDebounce, however many times
// it is asked for in the meantime. It does nothing while Run is not running.
func (s *AddressSpaces) Trigger(envID string) {
	s.bmu.Lock()
	ctx := s.bg
	if ctx == nil || s.pending[envID] {
		s.bmu.Unlock()
		return
	}
	s.pending[envID] = true
	s.wg.Add(1)
	s.bmu.Unlock()
	wait := s.after(eventDebounce)
	go func() {
		defer s.wg.Done()
		fire := false
		select {
		case <-ctx.Done():
		case <-wait:
			fire = true
		}
		s.bmu.Lock()
		delete(s.pending, envID)
		s.bmu.Unlock()
		if fire {
			s.readEnv(ctx, envID)
		}
	}()
}

// syncWatches starts an event watcher for each enabled environment that has none and stops the
// watcher of each environment that is no longer enabled.
func (s *AddressSpaces) syncWatches(ctx context.Context, enabled map[string]struct{}) {
	if s.subscribe == nil {
		return
	}
	s.bmu.Lock()
	defer s.bmu.Unlock()
	for id, stop := range s.watches {
		if _, ok := enabled[id]; !ok {
			stop()
			delete(s.watches, id)
		}
	}
	for id := range enabled {
		if _, ok := s.watches[id]; ok {
			continue
		}
		wctx, stop := context.WithCancel(ctx)
		s.watches[id] = stop
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.watch(wctx, id)
		}()
	}
}

// watch follows the events of one environment and triggers a read on each network, container or
// service event. A subscription the hub closes (the stream ended, or the watcher fell behind) is
// opened again after a pause that grows while it keeps failing.
func (s *AddressSpaces) watch(ctx context.Context, envID string) {
	backoff := watchRetryMin
	for {
		_, ch, cancel := s.subscribe(ctx, envID)
		got := false
	events:
		for {
			select {
			case <-ctx.Done():
				cancel()
				return
			case ev, ok := <-ch:
				if !ok {
					break events
				}
				got = true
				switch ev.Type {
				case "network", "container", "service":
					s.Trigger(envID)
				}
			}
		}
		cancel()
		if got {
			backoff = watchRetryMin
		}
		select {
		case <-ctx.Done():
			return
		case <-s.after(backoff):
		}
		backoff = min(backoff*2, watchRetryMax)
	}
}
