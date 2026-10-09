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

// OnNode runs fn once against the engine of the node nodeID of environment envID,
// and then drops that node's cached snapshot, whatever fn returned, so the next
// list read is fresh: a write whose answer was lost may still have happened.
//
// A node is named by its ID, never by its display name, because two hosts can share
// a name (T-02-73) and a write routed by name could reach the wrong one. The ID is
// the agent's, the environment's own ID for the local socket, and socket:<envId>
// for the host folded into a swarm environment; the rows of every list carry it as
// NodeID. A node that is not a connected, unrevoked node of this environment is
// ErrNodeUnavailable and nothing is sent.
//
// This is the one write path from an environment to one engine, as Snapshot is the
// one read path. It never retries: unlike Clusters.ClusterRead, which may repeat a
// read on another manager after a lost tunnel, a repeated write could do its
// change twice (P2 D-42). The caller sees the error of the single attempt.
func (a *Aggregator) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	in, err := a.svc.Input(ctx, envID, a.now())
	if err != nil {
		return err
	}
	key, ok := nodeTarget(in, nodeID)
	if !ok {
		return ErrNodeUnavailable
	}
	cli, _, err := a.svc.mon.Client(key)
	if err != nil {
		// Nothing was sent, so there is nothing to invalidate.
		return fmt.Errorf("node %s: %w", nodeID, err)
	}
	err = fn(cli)
	a.snaps.Invalidate(key)
	return err
}

// nodeTarget is the monitor key of the node nodeID of the environment, or false when
// there is no such node that can be written to now.
func nodeTarget(in RollupInput, nodeID string) (string, bool) {
	if nodeID == "" {
		return "", false
	}
	if in.Kind == KindLocal {
		// The local socket is one node and its ID is the environment's.
		if nodeID == in.EnvID {
			return in.EnvID, true
		}
		return "", false
	}
	for _, ag := range in.Agents {
		if ag.ID == nodeID && ag.Connected && !ag.Revoked {
			return ag.TargetKey(), true
		}
	}
	return "", false
}
