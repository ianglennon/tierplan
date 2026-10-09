// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"fmt"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// ErrNodeUnavailable means the node a write was aimed at is not a node of the
// environment that Bayplan can reach now: unknown, revoked or without a live
// session. Nothing was sent to any engine.
var ErrNodeUnavailable = errors.New("envs: the node is not available")

// ErrHostReadOnly means the agent of the node reported that its host owner set
// BAYPLAN_AGENT_READ_ONLY, so every write to it would be refused. It is named from
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
