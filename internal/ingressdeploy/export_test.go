// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"time"
)

// SetRoundHook sets a function Run calls after every round, so a test waits on a signal instead of a poll.
func (s *Service) SetRoundHook(fn func()) { s.roundHook = fn }

// Round takes one round over every enabled environment, as Run does, and returns when it is done.
func (s *Service) Round(ctx context.Context) { s.round(ctx) }

// WakePending reports whether a wake is waiting for the loop, and takes it.
func (s *Service) WakePending() bool {
	select {
	case <-s.wake:
		return true
	default:
		return false
	}
}

// SetWipeHook sets a function RunWipes calls after every pass that found pending wipes, so a test waits on a signal.
func (s *Service) SetWipeHook(fn func()) { s.wipeHook = fn }

// WipeRound takes one pass of the wipe loop and returns when it is done.
func (s *Service) WipeRound(ctx context.Context) { s.wipeRound(ctx) }

// SetVIPHook sets a function RunVIPReader calls after every round, so a test waits on a signal.
func (s *Service) SetVIPHook(fn func()) { s.vipHook = fn }

// ReadVIPOnce takes one pass of the virtual IP reader over one environment and returns when it is done.
func (s *Service) ReadVIPOnce(ctx context.Context, envID string) { s.readVIPOnce(ctx, envID) }

// VIPNodeStates is what the reader knows of the environment's nodes.
func (s *Service) VIPNodeStates(envID string) []vipNode { return s.vipNodeStates(envID) }

// VIPHolders is the nodes whose last state is MASTER.
func (s *Service) VIPHolders(envID string) []vipNode {
	nodes := map[string]vipNode{}
	for _, n := range s.vipNodeStates(envID) {
		nodes[n.NodeID] = n
	}
	return holders(nodes)
}

// VIPHolderlessSince is when the environment's address last lost its holder.
func (s *Service) VIPHolderlessSince(envID string) time.Time { return s.vipHolderlessSince(envID) }

// MarkVIPHandoff and MarkVIPDisabling set the reader's marks.
func (s *Service) MarkVIPHandoff(envID, nodeID string)    { s.markVIPHandoff(envID, nodeID) }
func (s *Service) MarkVIPDisabling(envID string, on bool) { s.markVIPDisabling(envID, on) }

// VIPSkipped is how many lines the reader refused or dropped in the environment.
func (s *Service) VIPSkipped(envID string) uint64 {
	e := s.vip.find(envID)
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.skipped
}

// SeedNode is one node of the reader's memory that a test writes by hand, so a status test need not play a log.
type SeedNode struct {
	NodeID, Hostname, State, Reason string
	Foreign                         bool
	ForeignSender                   string
	ClockSkew                       bool
	LastHeard, ReadSince, ReadAt    time.Time
	// TaskStarted is when the node's helper last started.
	TaskStarted time.Time
}

// SeedVIPState replaces the reader's memory of the environment with the nodes and the instant the address lost its holder.
func (s *Service) SeedVIPState(envID string, nodes []SeedNode, holderlessSince time.Time) {
	e := s.vip.env(envID)
	e.mu.Lock()
	defer e.mu.Unlock()
	view := vipEnvState{Nodes: map[string]vipNode{}, HolderlessSince: holderlessSince}
	for _, n := range nodes {
		view.Nodes[n.NodeID] = vipNode{
			NodeID: n.NodeID, Hostname: n.Hostname, State: n.State, Reason: n.Reason, Faults: map[string]bool{},
			Foreign: n.Foreign, ForeignSender: n.ForeignSender, ClockSkew: n.ClockSkew, LastHeard: n.LastHeard, ReadSince: n.ReadSince, ReadAt: n.ReadAt, TaskStarted: n.TaskStarted,
		}
	}
	e.view = view
}

// VIPIsDisabling reports whether the reader is marked as watching a switch-off of the environment's virtual IP.
func (s *Service) VIPIsDisabling(envID string) bool { return s.vipIsDisabling(envID) }

// SetVIPReadTimeouts shortens the bounds of one node's read and of one environment's pass, so a test of a node that never
// answers need not wait for the real ones.
func (s *Service) SetVIPReadTimeouts(node, pass time.Duration) {
	s.vipNodeTimeout, s.vipPassTimeout = node, pass
}

// SetSelfWaitBound replaces the bound of a self route job's wait for the next observation of the self network: fn is called
// once as the wait starts, and its channel ends the wait. A nil channel never ends it.
func (s *Service) SetSelfWaitBound(fn func() <-chan time.Time) { s.selfWaitBound = fn }

// DirectAddress is directAddress: the address the self-route card offers as the one that keeps working (D-07).
func DirectAddress(urls SelfURLs, host string) string { return directAddress(urls, host) }
