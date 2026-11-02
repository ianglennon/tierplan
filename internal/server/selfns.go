// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// selfNamespaceTimeout bounds the one read of TierPlan's own container at start-up.
const selfNamespaceTimeout = 3 * time.Second

// readSelfNamespace is the swarm stack namespace of the container TierPlan runs in, read once at start-up
// through the local engine, so the stack lists can flag the stack that holds the server (D-49). It is empty
// when TierPlan does not run in a container, in no stack, or the engine cannot be asked: the lists then flag
// by the container ID alone, and nothing else changes.
func readSelfNamespace(ctx context.Context, socketPath, selfID string, log *slog.Logger) string {
	if selfID == "" || socketPath == "" {
		return ""
	}
	cli, err := dockerx.New(engine.LocalSocketTransport{Path: socketPath})
	if err != nil {
		log.Debug("could not ask the local engine for TierPlan's own stack", "error", err)
		return ""
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(ctx, selfNamespaceTimeout)
	defer cancel()
	detail, _, err := cli.ContainerDetail(ctx, selfID)
	if err != nil {
		log.Debug("could not read TierPlan's own container for its stack", "error", err)
		return ""
	}
	return detail.Labels[stacks.LabelSwarmNamespace]
}
