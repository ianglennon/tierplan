// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The outcomes of a drain's handoff of the virtual address (D-25). Each is a closed-vocabulary word the audit log and the
// answer to the drain carry.
const (
	// VIPHandoffMoved means another node reported MASTER after the holder's keepalived was stopped.
	VIPHandoffMoved = "moved"
	// VIPHandoffNoCandidate means no other node was ready to take the address, so nothing was stopped.
	VIPHandoffNoCandidate = "no_candidate"
	// VIPHandoffAgentUnreachable means the holder's agent was not connected or the stop failed.
	VIPHandoffAgentUnreachable = "agent_unreachable"
	// VIPHandoffTimeout means no other node reported MASTER within VIPHandoffBound.
	VIPHandoffTimeout = "timeout"
)

const (
	// VIPHandoffBound is the longest a handoff waits for the address to move. A drain is never held for longer (PROH-26-04).
	VIPHandoffBound = 10 * time.Second
	// vipHandoffPoll is how often, on the injected clock, the handoff reads the nodes again while it waits.
	vipHandoffPoll = 500 * time.Millisecond
)

// VIPHandoff is what a handoff did.
type VIPHandoff struct {
	// Outcome is one of the VIPHandoff outcome constants.
	Outcome string
	// Holder is the host name of the node that took the address; empty unless the outcome is moved.
	Holder string
	// HolderNodeID is the swarm node ID of that node.
	HolderNodeID string
}

// HandoffVIP moves the virtual address off a node before the node is drained (D-25). It answers nil, nil when the node does
// not hold the address, so a drain of any other node runs no handoff. For the holder it stops the node's keepalived
// container through the node's agent, so keepalived sends its priority-0 advert on SIGTERM, then reads the nodes until another
// reports MASTER, for at most VIPHandoffBound of the injected clock. Whatever goes wrong is an outcome, never an error: the
// caller goes on to drain (PROH-26-04). The one error is a failure to learn whether the node holds the address, in which case
// nothing was done.
func (s *Service) HandoffVIP(ctx context.Context, envID, swarmNodeID string) (*VIPHandoff, error) {
	holds, alone, err := s.VIPHolds(ctx, envID, swarmNodeID)
	if err != nil {
		return nil, err
	}
	if !holds {
		return nil, nil
	}
	if alone {
		return &VIPHandoff{Outcome: VIPHandoffNoCandidate}, nil
	}

	// The real clock bounds a call that hangs, and the injected one the wait for the address to move.
	ctx, cancel := context.WithTimeout(ctx, VIPHandoffBound)
	defer cancel()

	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		s.log().Warn("the drain handoff could not read the environment", "environment", envID, "error", err)
		return &VIPHandoff{Outcome: VIPHandoffAgentUnreachable}, nil
	}
	var node *NodeInfo
	for i := range info.Nodes {
		if info.Nodes[i].SwarmNodeID == swarmNodeID {
			node = &info.Nodes[i]
		}
	}
	if node == nil || !node.Connected || node.AgentID == "" {
		return &VIPHandoff{Outcome: VIPHandoffAgentUnreachable}, nil
	}

	// From here a move away from the node is a drain_handoff, whatever the stop does (D-23).
	s.markVIPHandoff(envID, swarmNodeID)
	err = s.d.Engine.WriteOnNode(ctx, envID, node.AgentID, func(cli *dockerx.Client) error {
		rows, err := cli.ContainersWithLabel(ctx, ingress.LabelComponent, ingress.ComponentKeepalived)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.State != "running" {
				continue
			}
			if err := cli.ContainerAction(ctx, row.ID, dockerx.VerbStop, dockerx.ActionOptions{}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log().Warn("the drain handoff could not stop the keepalived container", "environment", envID, "node", swarmNodeID, "error", err)
		return &VIPHandoff{Outcome: VIPHandoffAgentUnreachable}, nil
	}

	clock := s.d.Clock
	end := clock.Now().Add(VIPHandoffBound)
	for {
		s.readVIPOnce(ctx, envID)
		for _, n := range s.vipNodeStates(envID) {
			if n.NodeID != swarmNodeID && n.State == ingressbundle.VIPStateMaster {
				return &VIPHandoff{Outcome: VIPHandoffMoved, Holder: n.Hostname, HolderNodeID: n.NodeID}, nil
			}
		}
		if !clock.Now().Before(end) {
			return &VIPHandoff{Outcome: VIPHandoffTimeout}, nil
		}
		select {
		case <-ctx.Done():
			return &VIPHandoff{Outcome: VIPHandoffTimeout}, nil
		case <-clock.After(vipHandoffPoll):
		}
	}
}
