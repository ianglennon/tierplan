// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents

import (
	"context"
	"slices"
	"time"
)

// nodeListMaxAge is how old a node list may be and still decide an enrolment. The cluster
// cache re-reads every 15 seconds, so a list older than this means no manager has answered
// for a while (the quorum is lost, the only manager is the one enrolling, or none is
// connected), and a list that stale is no basis for a refusal.
const nodeListMaxAge = 2 * time.Minute

const (
	// nodeListRefreshAfter is how old a fresh list that lacks an enrolling agent's node must be
	// before the enrolment has it read again, once, before it refuses: a node that just joined
	// the swarm is then found at its first try, and enrolments cannot make the managers read
	// their cluster more than once per this time.
	nodeListRefreshAfter = 5 * time.Second
	// nodeListRefreshTimeout bounds that read inside the handshake, which has
	// wire.HandshakeTimeout (10 seconds) in all, so the refusal or the welcome still has time
	// to reach the caller.
	nodeListRefreshTimeout = 3 * time.Second
)

// NodeVerdict is what the node list says about the swarm node an enrolling caller reports.
type NodeVerdict int

const (
	// NodeNotChecked: no NodeLists provider is set.
	NodeNotChecked NodeVerdict = iota
	// NodeUnverified: no fresh list to check against, or the caller reports no node ID.
	NodeUnverified
	// NodeListed: a fresh list holds the node.
	NodeListed
	// NodeNotListed: a fresh list lacks the node, after at most one read of the list again.
	NodeNotListed
)

// CheckSwarmNode says what the node list makes of the swarm node nodeID in the swarm of envID.
// It is shared by the agent enrolment and the ingress enrolment: the agent accepts
// NodeUnverified, the ingress service refuses it (D-22).
//
// A nil nodes is NodeNotChecked. A caller that reports no node ID, and a swarm with no fresh
// list (none held, the quorum lost, or older than nodeListMaxAge), is NodeUnverified. A fresh
// list that holds the node is NodeListed. A fresh list that lacks it is NodeNotListed. The
// check is on the node ID only; a hostname is as visible as a node ID, so comparing it would
// prove nothing and would refuse a renamed host.
//
// A fresh list that lacks the node and is at least nodeListRefreshAfter old is read again
// once, bounded by nodeListRefreshTimeout, before the verdict, so a node that joined since
// the last read is usually found at the caller's first try.
//
// The residual risk is stated in D-04: a listed node proves the node exists in the swarm, not
// that the caller runs on it.
func CheckSwarmNode(ctx context.Context, nodes NodeLists, now func() time.Time, envID, nodeID string) NodeVerdict {
	if nodes == nil {
		return NodeNotChecked
	}
	if nodeID == "" {
		return NodeUnverified
	}
	ids, fetchedAt, ok := nodes.SwarmNodeIDs(envID)
	fresh := func() bool { return ok && now().Sub(fetchedAt) <= nodeListMaxAge }
	if !fresh() {
		return NodeUnverified
	}
	if slices.Contains(ids, nodeID) {
		return NodeListed
	}
	if now().Sub(fetchedAt) >= nodeListRefreshAfter {
		rctx, cancel := context.WithTimeout(ctx, nodeListRefreshTimeout)
		ids, fetchedAt, ok = nodes.RefreshSwarmNodeIDs(rctx, envID, nodeListRefreshAfter)
		cancel()
		if !fresh() {
			return NodeUnverified
		}
		if slices.Contains(ids, nodeID) {
			return NodeListed
		}
	}
	return NodeNotListed
}
