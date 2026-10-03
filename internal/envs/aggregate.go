// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// NodeRef names one node an aggregate was read from.
type NodeRef struct{ ID, Name, Role string }

// Aggregate is what an environment's Docker read answers with.
type Aggregate struct {
	dockerx.Snapshot
	// Target is the monitor's status of the target the read went to.
	Target Status
	// Nodes are the nodes the data was read from.
	Nodes []NodeRef
	// NotReporting names the nodes whose data is missing (swarm aggregation, plan 21).
	NotReporting []string
}

// ErrNotOnline is the error of a read of an environment that cannot be read.
type ErrNotOnline struct {
	Health Health
	Target Status
}

func (e ErrNotOnline) Error() string { return "envs: the environment is " + string(e.Health.State) }

// Aggregator is the one read path from an environment to its Docker target or targets.
type Aggregator struct {
	svc    *Service
	snaps  *Snapshots
	events *Hub
	now    func() time.Time
}

// NewAggregator returns the aggregator over the registry, the snapshot cache and
// the events hub.
func NewAggregator(svc *Service, snaps *Snapshots, events *Hub, now func() time.Time) *Aggregator {
	if now == nil {
		now = time.Now
	}
	return &Aggregator{svc: svc, snaps: snaps, events: events, now: now}
}

// Snapshot reads the counts and lists of an environment.
func (a *Aggregator) Snapshot(ctx context.Context, envID string) (Aggregate, Health, error) {
	_, _ = ctx, envID
	return Aggregate{}, Health{}, errors.New("envs: not implemented")
}

// Events subscribes to the events of an environment's primary target.
func (a *Aggregator) Events(ctx context.Context, envID string) (backlog []dockerx.EventRow, ch <-chan dockerx.EventRow, cancel func()) {
	_, _ = ctx, envID
	closed := make(chan dockerx.EventRow)
	close(closed)
	return nil, closed, func() {}
}

// Recent returns up to n recent events of an environment's primary target, newest first.
func (a *Aggregator) Recent(ctx context.Context, envID string, n int) []dockerx.EventRow {
	_, _, _ = ctx, envID, n
	return nil
}
