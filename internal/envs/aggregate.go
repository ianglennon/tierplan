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

// ErrNotOnline is the error of a read of an environment that cannot be read: the
// environment's rolled-up Health says why (state, D-40 reason, since), and Target
// is the status of the target that was tried, which carries the engine of an
// unsupported one. A degraded environment can be read and never gets this error.
//
// errors.Is(err, ErrOffline) and errors.Is(err, ErrUnsupported) hold, so a caller
// that only distinguishes the Phase 1 pair keeps working.
type ErrNotOnline struct {
	Health Health
	Target Status
}

func (e ErrNotOnline) Error() string { return "envs: the environment is " + string(e.Health.State) }

// Is makes an unsupported environment match ErrUnsupported and every other one
// ErrOffline.
func (e ErrNotOnline) Is(target error) bool {
	switch target {
	case ErrUnsupported:
		return e.Health.State == StateUnsupported
	case ErrOffline:
		return e.Health.State != StateUnsupported
	}
	return false
}

// Aggregator is the single read path from an environment to its Docker target or
// targets (D-47). Every handler that needs an environment's Docker data asks it,
// and none asks the monitor or the snapshot cache by environment: an agent
// environment has no monitor of its own. Today it picks one target (the local
// socket, or the primary agent); the manager choice for cluster calls is plan 20's
// and the fan-out over every node of a swarm is plan 21's, inside this type.
type Aggregator struct {
	svc    *Service
	snaps  *Snapshots
	events *Hub
	now    func() time.Time
}

// NewAggregator returns the aggregator over the registry, the snapshot cache and
// the events hub. now defaults to time.Now.
func NewAggregator(svc *Service, snaps *Snapshots, events *Hub, now func() time.Time) *Aggregator {
	if now == nil {
		now = time.Now
	}
	return &Aggregator{svc: svc, snaps: snaps, events: events, now: now}
}

// Snapshot reads the counts and lists of an environment through its target and
// returns them with the environment's health. An environment that is not online or
// degraded answers ErrNotOnline carrying the health; an unknown one ErrNotFound.
// The snapshot cache and its single flight stay per target.
func (a *Aggregator) Snapshot(ctx context.Context, envID string) (Aggregate, Health, error) {
	now := a.now()
	in, err := a.svc.Input(ctx, envID, now)
	if err != nil {
		return Aggregate{}, Health{}, err
	}
	if in.Kind == KindLocal {
		return a.local(ctx, in)
	}

	h := Rollup(in)
	if h.State != StateOnline && h.State != StateDegraded {
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: primaryStatus(a.svc, in)}
	}
	key, ok := a.svc.primary(in)
	if !ok {
		// Present inside the offline window but between sessions: nothing to read
		// through yet.
		return Aggregate{}, h, a.between(in)
	}
	snap, st, err := a.snaps.Get(ctx, key)
	switch {
	case errors.Is(err, ErrUnsupported):
		h := Health{State: StateUnsupported, Reason: ReasonUnsupportedEngine, Since: firstSet(st.Since, now)}
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: st}
	case errors.Is(err, ErrOffline):
		h := Health{State: StateOffline, Reason: ReasonEngineUnreachable, Since: firstSet(st.Since, now)}
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: st}
	case errors.Is(err, errNotMonitored):
		// The session is registered and its monitor has not started yet.
		return Aggregate{}, h, a.between(in)
	case err != nil:
		return Aggregate{}, h, err
	}
	return Aggregate{Snapshot: snap, Target: st, Nodes: nodesOf(in)}, h, nil
}

// local reads the local socket: its health is the monitor's status, kept exactly as
// Phase 1 answered it, including waiting for the first probe of a new environment.
func (a *Aggregator) local(ctx context.Context, in RollupInput) (Aggregate, Health, error) {
	snap, st, err := a.snaps.Get(ctx, in.EnvID)
	in.Local = st
	h := Rollup(in)
	switch {
	case errors.Is(err, ErrOffline), errors.Is(err, ErrUnsupported):
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: st}
	case err != nil:
		return Aggregate{}, h, err
	}
	return Aggregate{
		Snapshot: snap, Target: st,
		Nodes: []NodeRef{{ID: in.EnvID, Name: st.Probe.Hostname, Role: st.Probe.Type}},
	}, h, nil
}

// between is the error for an environment whose agents are present but have no
// live session: reconnecting, since the last session ended.
func (a *Aggregator) between(in RollupInput) error {
	var ended time.Time
	for _, ag := range in.Agents {
		ended = later(ended, ag.Ended)
	}
	h := Health{State: StateReconnecting, Reason: ReasonAgentOffline, Since: firstSet(ended, in.ServerStart)}
	return ErrNotOnline{Health: h}
}

// primaryStatus is the monitor's status of the environment's primary target, or
// unknown when it has none.
func primaryStatus(svc *Service, in RollupInput) Status {
	if key, ok := svc.primary(in); ok {
		return svc.mon.Status(key)
	}
	return Status{State: StateUnknown}
}

// nodesOf names the nodes an aggregate is read from: the connected agents.
func nodesOf(in RollupInput) []NodeRef {
	var out []NodeRef
	for _, a := range connectedAgents(in) {
		out = append(out, NodeRef{ID: a.ID, Name: nodeName(a), Role: a.Role})
	}
	return out
}

// primaryKey is the target the events of an environment come from.
func (a *Aggregator) primaryKey(ctx context.Context, envID string) (string, bool) {
	in, err := a.svc.Input(ctx, envID, a.now())
	if err != nil {
		return "", false
	}
	return a.svc.primary(in)
}

// Events subscribes to the events of an environment's primary target; see
// Hub.Subscribe. An environment with no target to read gets an empty backlog and a
// channel that is already closed.
func (a *Aggregator) Events(ctx context.Context, envID string) (backlog []dockerx.EventRow, ch <-chan dockerx.EventRow, cancel func()) {
	key, ok := a.primaryKey(ctx, envID)
	if !ok {
		closed := make(chan dockerx.EventRow)
		close(closed)
		return nil, closed, func() {}
	}
	return a.events.Subscribe(ctx, key)
}

// Recent returns up to n buffered events of an environment's primary target,
// newest first.
func (a *Aggregator) Recent(ctx context.Context, envID string, n int) []dockerx.EventRow {
	key, ok := a.primaryKey(ctx, envID)
	if !ok {
		return nil
	}
	return a.events.Recent(key, n)
}
