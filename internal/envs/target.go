// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"errors"
	"fmt"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// ErrNodeUnavailable means the node a write was aimed at is not a node of the
// environment that Coaming can reach now: unknown, revoked or without a live
// session. Nothing was sent to any engine.
var ErrNodeUnavailable = errors.New("envs: the node is not available")

// ErrHostReadOnly means the agent of the node reported that its host owner set
// COAMING_AGENT_READ_ONLY, so every write to it would be refused. It is named from
// the agent's facts before any call, so a read-only host is never mistaken for an
// agent too old to know a route (D-35).
var ErrHostReadOnly = errors.New("envs: the host is read-only")

// OnNode runs fn once against the engine of the node nodeID of environment envID.
// It is for reads: it leaves the node's cached snapshot alone, so an inspect or a
// long log follow cannot flush the cache that the lists are served from. A write
// goes through WriteOnNode.
//
// A node is named by its ID, never by its display name, because two hosts can share
// a name (T-02-73) and a write routed by name could reach the wrong one. The ID is
// the agent's, the environment's own ID for the local socket, and socket:<envId>
// for the host folded into a swarm environment; the rows of every list carry it as
// NodeID. A node that is not a connected, unrevoked node of this environment is
// ErrNodeUnavailable and nothing is sent.
func (a *Aggregator) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	_, cli, err := a.nodeClient(ctx, envID, nodeID, false)
	if err != nil {
		return err
	}
	return fn(cli)
}

// WriteOnNode runs fn once against the engine of the node nodeID, for a call that
// changes something. It resolves the node as OnNode does, refuses a node whose
// agent reported a read-only host with ErrHostReadOnly before it takes a client,
// and then drops the node's cached snapshot, whatever fn returned, so the next list
// read is fresh: a write whose answer was lost may still have happened.
//
// This is the one write path from an environment to one engine, as Snapshot is the
// one read path. It never retries: unlike Clusters.ClusterRead, which may repeat a
// read on another manager after a lost tunnel, a repeated write could do its
// change twice (P2 D-42). The caller sees the error of the single attempt.
func (a *Aggregator) WriteOnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	key, cli, err := a.nodeClient(ctx, envID, nodeID, true)
	if err != nil {
		return err
	}
	err = fn(cli)
	a.snaps.Invalidate(key)
	return err
}

// WriteOnManager runs fn once against the engine that answers for the whole
// environment, for a call that changes something at the level of the cluster: on a
// swarm the manager the cluster reader would choose, on a standalone host or the local
// socket its single engine. Like WriteOnNode it refuses a read-only host with
// ErrHostReadOnly before it takes a client, never retries (P2 D-42), and drops the
// cached snapshot of the node it used whatever fn returned. It is ErrNodeUnavailable
// when no node of the environment can be reached now.
//
// A swarm-scoped object is created once, by one manager. Every other manager sees it
// through the cluster, so there is nothing to repeat on another node, and a retry on
// another manager after a lost answer could create it twice.
func (a *Aggregator) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	in, err := a.svc.Input(ctx, envID, a.now())
	if err != nil {
		return err
	}
	key, ok := a.svc.primary(in)
	if !ok {
		return ErrNodeUnavailable
	}
	if managerReadOnly(in, key) {
		return fmt.Errorf("environment %s: %w", envID, ErrHostReadOnly)
	}
	cli, _, err := a.svc.mon.Client(key)
	if err != nil {
		// Nothing was sent, so there is nothing to invalidate.
		return fmt.Errorf("environment %s: %w", envID, err)
	}
	err = fn(cli)
	a.snaps.Invalidate(key)
	return err
}

// managerReadOnly reports whether the agent behind the monitor key is on a read-only
// host. The local socket never is.
func managerReadOnly(in RollupInput, key string) bool {
	for _, ag := range in.Agents {
		if ag.TargetKey() == key {
			return ag.ReadOnly
		}
	}
	return false
}

// nodeClient resolves a node to its monitor key and a client for its engine. For a
// write it refuses a read-only host first, before a client is asked for.
func (a *Aggregator) nodeClient(ctx context.Context, envID, nodeID string, write bool) (string, *dockerx.Client, error) {
	in, err := a.svc.Input(ctx, envID, a.now())
	if err != nil {
		return "", nil, err
	}
	key, readOnly, ok := nodeTarget(in, nodeID)
	if !ok {
		return "", nil, ErrNodeUnavailable
	}
	if write && readOnly {
		return "", nil, fmt.Errorf("node %s: %w", nodeID, ErrHostReadOnly)
	}
	cli, _, err := a.svc.mon.Client(key)
	if err != nil {
		// Nothing was sent, so there is nothing to invalidate.
		return "", nil, fmt.Errorf("node %s: %w", nodeID, err)
	}
	return key, cli, nil
}

// nodeTarget is the monitor key of the node nodeID of the environment and whether its
// host is read-only, or false when there is no such node that can be reached now.
func nodeTarget(in RollupInput, nodeID string) (key string, readOnly, ok bool) {
	if nodeID == "" {
		return "", false, false
	}
	if in.Kind == KindLocal {
		// The local socket is one node and its ID is the environment's.
		if nodeID == in.EnvID {
			return in.EnvID, false, true
		}
		return "", false, false
	}
	for _, ag := range in.Agents {
		if ag.ID == nodeID && ag.Connected && !ag.Revoked {
			return ag.TargetKey(), ag.ReadOnly, true
		}
	}
	return "", false, false
}

// SwarmNodeTargets maps each swarm node of the environment that Coaming can reach now to
// the node ID that the container routes take (the ID OnNode and WriteOnNode are given).
// The key is the swarm node ID, as a task or a node row carries it, so a task can be
// followed to its node's engine.
//
// A node is in the map when an agent that runs on it is connected and not revoked: the
// value is that agent's ID. The local socket's own swarm node is the environment's ID,
// and the host folded into a swarm environment is socket:<envId>, which is the ID of its
// view. A node nothing reaches (down, disconnected, revoked, with no agent, or unknown) is
// not in the map; that is how a caller tells that a node cannot be opened. A standalone
// host has no swarm node IDs and the map is empty.
func (a *Aggregator) SwarmNodeTargets(ctx context.Context, envID string) (map[string]NodeRef, error) {
	in, err := a.svc.Input(ctx, envID, a.now())
	if err != nil {
		return nil, err
	}
	out := map[string]NodeRef{}
	if in.Kind == KindLocal {
		if id := in.Local.Probe.SwarmNodeID; id != "" && in.Local.State == StateOnline {
			out[id] = nodeOf(in, in.EnvID, in.Local)
		}
		return out, nil
	}
	for _, ag := range in.Agents {
		if ag.SwarmNodeID == "" || !ag.Connected || ag.Revoked {
			continue
		}
		out[ag.SwarmNodeID] = NodeRef{ID: ag.ID, Name: nodeName(ag), Role: ag.Role, Arch: dockerx.OCIArch(ag.Target.Probe.Arch), ReadOnly: ag.ReadOnly}
	}
	return out, nil
}
