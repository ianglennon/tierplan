// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

// foldNodeListMaxAge is how old a node list may be and still decide a fold. It is the age the
// agents service allows a node list (internal/agents nodeListMaxAge), kept here as its own
// constant because envs must not import agents.
const foldNodeListMaxAge = 2 * time.Minute

// foldVerdict is the answer of the fold proof for one probed socket and one node list: whether
// the socket's host is a node of the swarm the list belongs to, and if not, why not.
type foldVerdict int

const (
	// verdictOffered means the host is a listed node of the swarm and reports a manager of it.
	verdictOffered foldVerdict = iota
	// verdictNotInSwarm means the engine is not an active swarm node, or reported no node ID.
	verdictNotInSwarm
	// verdictNoNodeList means there is no node list to prove against (none believed, stale, or
	// empty).
	verdictNoNodeList
	// verdictManagersMismatch means none of the managers the node reports is a manager in the
	// list: the host belongs to another swarm (or the list is another swarm's).
	verdictManagersMismatch
	// verdictNodeNotListed means the host reports managers of this swarm but its node ID is not
	// in the list: it was removed from the swarm and has not noticed (S13 row 5).
	verdictNodeNotListed
)

// proveNode decides whether the host behind a probed socket may be folded into the swarm whose
// node list (read through a manager agent of that swarm) is nodes. It does no I/O.
//
// Membership of the node ID in a manager-read list is the decider: a node can only be in the
// list of a swarm it is part of. The managers the node reports are a second guard, not a
// substitute, because a worker removed with `docker node rm --force` keeps reporting the same
// managers while no longer being a node (spike S13 row 5); on the reported managers alone it
// would still be offered. A node whose reported managers share no manager with the list is
// refused whether or not its node ID happens to be listed (S13 row 6). Promotion and demotion
// keep a node's ID (S13 rows 3 and 4), so the role of its row does not matter here.
//
// The order is: not an active swarm node, no list, managers disjoint (not this swarm), node
// absent (this swarm, the node is gone), else offered. The last two are told apart so the UI
// can show a refusal for a removed node and nothing for another swarm's host.
func proveNode(p dockerx.ProbeResult, nodes []dockerx.NodeRow) foldVerdict {
	if p.SwarmNodeState != "active" || p.SwarmNodeID == "" {
		return verdictNotInSwarm
	}
	if len(nodes) == 0 {
		return verdictNoNodeList
	}
	managers := make(map[string]bool, len(nodes))
	listed := false
	for _, n := range nodes {
		if n.Role == roleManager {
			managers[n.ID] = true
		}
		if n.ID == p.SwarmNodeID {
			listed = true
		}
	}
	shared := false
	for _, id := range p.SwarmRemoteManagers {
		if managers[id] {
			shared = true
			break
		}
	}
	switch {
	case !shared:
		return verdictManagersMismatch
	case !listed:
		return verdictNodeNotListed
	}
	return verdictOffered
}

// foldNodeList is the node list of swarm environment envID that a fold may be proven against:
// the one Clusters believes (its answerer is in it, the quorum is held) and no older than
// foldNodeListMaxAge. It is nil when the service has no Clusters, nothing is believed, or the
// list is stale; proveNode reads that as verdictNoNodeList. It does no I/O.
func (s *Service) foldNodeList(envID string) []dockerx.NodeRow {
	if s.clusters == nil {
		return nil
	}
	nodes, at, ok := s.clusters.NodeList(envID)
	if !ok || s.now().Sub(at) > foldNodeListMaxAge {
		return nil
	}
	return nodes
}
