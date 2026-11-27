// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// selfNamespaceTimeout bounds the one read of TierPlan's own container at start-up.
const selfNamespaceTimeout = 3 * time.Second

// Labels the engine puts on every container of a swarm service's task (D-03).
const (
	labelSwarmServiceName = "com.docker.swarm.service.name"
	labelSwarmServiceID   = "com.docker.swarm.service.id"
)

// readSelfNamespace is the swarm stack namespace of the container TierPlan runs in, read once at start-up
// through the local engine, so the stack lists can flag the stack that holds the server (D-49). It is empty
// when TierPlan does not run in a container, in no stack, or the engine cannot be asked: the lists then flag
// by the container ID alone, and nothing else changes.
func readSelfNamespace(ctx context.Context, socketPath, selfID string, log *slog.Logger) string {
	facts, err := readSelfFacts(ctx, socketPath, selfID)
	if err != nil {
		log.Debug("could not read TierPlan's own container for its stack", "error", err)
		return ""
	}
	return facts.StackNamespace
}

// readSelfFacts reads TierPlan's own container once through the local socket: its swarm service, stack and the networks it is
// attached to. It reads the local engine's container inspect and nothing of the swarm, so it answers on a worker too (D-05).
// An empty container ID or socket path is an error: TierPlan then does not run in a container the engine can name.
func readSelfFacts(ctx context.Context, socketPath, selfID string) (ingressdeploy.SelfFacts, error) {
	if selfID == "" || socketPath == "" {
		return ingressdeploy.SelfFacts{}, errors.New("TierPlan's own container or the local socket is not known")
	}
	cli, err := dockerx.New(engine.LocalSocketTransport{Path: socketPath})
	if err != nil {
		return ingressdeploy.SelfFacts{}, fmt.Errorf("ask the local engine: %w", err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(ctx, selfNamespaceTimeout)
	defer cancel()
	detail, _, err := cli.ContainerDetail(ctx, selfID)
	if err != nil {
		return ingressdeploy.SelfFacts{}, err
	}
	facts := selfFactsOf(detail)
	if facts.ContainerID == "" {
		facts.ContainerID = selfID
	}
	return facts, nil
}

// selfReader is the deployer's read of TierPlan's own container: readSelfFacts on the server's socket and container ID, the
// same values readSelfNamespace is called with.
func selfReader(socketPath, selfID string) func(ctx context.Context) (ingressdeploy.SelfFacts, error) {
	return func(ctx context.Context) (ingressdeploy.SelfFacts, error) {
		return readSelfFacts(ctx, socketPath, selfID)
	}
}

// selfFactsOf maps a container's inspect to what the deployer needs to know of TierPlan's own container. Swarm is true when
// the container carries a swarm service label, so it is a task of a service and not a plain container on a swarm node.
func selfFactsOf(detail dockerx.ContainerDetail) ingressdeploy.SelfFacts {
	f := ingressdeploy.SelfFacts{
		ContainerID:    detail.ID,
		ServiceName:    detail.Labels[labelSwarmServiceName],
		ServiceID:      detail.Labels[labelSwarmServiceID],
		StackNamespace: detail.Labels[stacks.LabelSwarmNamespace],
	}
	f.Swarm = f.ServiceName != "" || f.ServiceID != ""
	for _, n := range detail.Networks {
		f.Networks = append(f.Networks, n.Name)
	}
	return f
}
