// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
)

// ErrLocalIsSwarmNode means the socket TierPlan was asked to add as the local environment belongs to a node of a swarm.
// Such a host is reached through the swarm's agents, and adding it as an environment of its own would list every stack
// twice (D-02, D-21).
var ErrLocalIsSwarmNode = errors.New("envs: the local host is a node of a swarm; add the swarm through its agents and fold the host instead")

// RefuseLocalOnSwarmNode is the server-side half of D-02: the web UI never offers the local host on a swarm node
// (D-21), and this makes sure no other client can add it either. It returns ErrLocalIsSwarmNode when the socket's
// swarm node state is active, pending, error or locked (the probe reads a locked node as a swarm worker), and nil
// when the add may go on.
//
// A node that is already folded into a swarm environment (a KindAgent row whose local_node_id is this socket's swarm
// node ID) is let through: adding the Local environment again is how a fold is undone by hand.
//
// Every case that is not a swarm node returns nil so AddLocal gives the answer it always gave: a host name that does
// not parse, a missing socket, an engine that does not answer, a local environment that exists already
// (ErrLocalExists), and a standalone engine. An engine below the Docker 24 floor is never asked for /info, so its swarm
// state is unknown here and it is not refused (D-21 residual): it is stored as unsupported, has no operations, and
// cannot list stacks twice.
//
// The probe is a second one on top of AddLocal's own, bounded by the monitor's probe timeout, and it runs only on an
// Admin's explicit add. A swarm state that changes between the two inside one request is not guarded.
func (s *Service) RefuseLocalOnSwarmNode(ctx context.Context) error {
	if exists, err := s.hasLocalEnvironment(ctx); err != nil || exists {
		return err // an existing Local environment is AddLocal's answer (ErrLocalExists)
	}
	path, err := engine.ParseDockerHost(s.dockerHost)
	if err != nil || !engine.SocketExists(path) {
		return nil
	}
	res, err := s.probe(ctx, engine.LocalSocketTransport{Path: path})
	if err != nil {
		return nil
	}
	switch res.SwarmNodeState {
	case "active", "pending", "error", "locked":
	default:
		return nil
	}
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		return fmt.Errorf("list environments: %w", err)
	}
	for _, r := range rows {
		if r.Kind == KindAgent && r.LocalNodeID.Valid && r.LocalNodeID.String == res.SwarmNodeID {
			return nil
		}
	}
	return ErrLocalIsSwarmNode
}

// SwarmMembership is what the server knows of the swarm its own Docker socket belongs to.
type SwarmMembership struct {
	// Known is false when the server cannot tell: no socket at the configured path, an engine that does not answer or whose
	// info cannot be read. Member, ClusterID and NodeID mean nothing then.
	Known bool
	// Member is true for an active node of a swarm, manager or worker, and false for a standalone engine.
	Member bool
	// ClusterID is the swarm's cluster ID, kept for a manager: a worker does not learn it, so it is empty.
	ClusterID string
	// NodeID is the engine's node ID in the swarm it belongs to, for a manager and a worker alike.
	NodeID string
}

// LocalSwarmMembership reads the swarm membership of the engine behind the server's own Docker socket, with one probe of the socket,
// bounded by the monitor's probe timeout; nothing it learns is kept. A missing socket, an engine that does not answer and
// one whose info cannot be read are not errors: they answer Known false.
//
// The ingress preflight compares it with the environment it is about to write to (D-09): a server that runs on a node of
// that very swarm must not publish its own ports 80 and 443 in a way that collides with the proxy. A manager reports the
// cluster it belongs to, and a worker only its node ID, so the comparison has to use both.
func (s *Service) LocalSwarmMembership(ctx context.Context) (SwarmMembership, error) {
	path, err := engine.ParseDockerHost(s.dockerHost)
	if err != nil || !engine.SocketExists(path) {
		return SwarmMembership{}, nil
	}
	res, err := s.probe(ctx, engine.LocalSocketTransport{Path: path})
	if err != nil {
		return SwarmMembership{}, nil
	}
	switch res.Type {
	case dockerx.TypeSwarmManager:
		return SwarmMembership{Known: true, Member: true, ClusterID: res.SwarmClusterID, NodeID: res.SwarmNodeID}, nil
	case dockerx.TypeSwarmWorker:
		return SwarmMembership{Known: true, Member: true, NodeID: res.SwarmNodeID}, nil
	case dockerx.TypeStandalone:
		return SwarmMembership{Known: true}, nil
	}
	return SwarmMembership{}, nil
}
