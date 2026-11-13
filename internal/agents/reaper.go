// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents

import (
	"context"
	"sync"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

const (
	// reaperActor is the principal an automatic revoke is audited under (D-13).
	reaperActor = "system:agent-reaper"
	// reapAfterMisses is how many consecutive successful node lists must lack an
	// agent's node before the agent is revoked (RESEARCH Pitfall 12).
	reapAfterMisses = 2
	// leftSwarmReason is the reason recorded for an agent whose node left the swarm.
	leftSwarmReason = "left_swarm"
	// removedNodeReason is the reason recorded for an agent whose node an administrator removed
	// from its swarm.
	removedNodeReason = "node_removed"
	// replacedReason is the reason recorded for an agent whose host enrolled again into the
	// same environment (Service.Enrol, review 17 CR-02).
	replacedReason = "replaced"
)

// Reaper revokes the agents of nodes that have left their swarm (D-10). A node that
// is removed with docker node rm, or whose host rejoined another swarm, is gone
// from the node list, and its agent should not keep its access.
//
// It is shown each successful node list with the node of the manager that
// answered, and counts per agent how many lists in a row lacked its node. Two make
// a revoke. Nothing short of that is enough: a node that is only down, drained or
// paused is still in the list, and a list that fails is never shown to it. It is
// safe for concurrent use.
//
// It also finishes what an enrolment could not (Service.Enrol): a swarm agent whose node no
// fresh list had shown enrols without retiring the earlier agent of its node, and at the first
// list that shows the node, while that newest agent is connected, the older offline row is
// retired as replaced, so the node does not keep a stale offline duplicate in its agent list
// and health (Service.retireListedDuplicates).
type Reaper struct {
	svc *Service

	// mu serialises Observe, so concurrent lists are counted one after another and
	// an agent is revoked once.
	mu sync.Mutex
	// misses is, by environment and agent, how many successful lists in a row lacked
	// the agent's node.
	misses map[string]map[string]int
}

// NewReaper returns a reaper that revokes through svc.
func NewReaper(svc *Service) *Reaper {
	return &Reaper{svc: svc, misses: map[string]map[string]int{}}
}

// Observe counts one successful node list of a swarm environment, and revokes each
// active swarm agent whose node has now been missing from two in a row.
//
// A list is believed only if it names the manager that answered it: a list that
// leaves out its own answerer, or has no answerer, is not a list of the swarm it
// claims to describe, and counts for nothing, neither as a miss nor as a sighting.
// That bounds what a hostile manager can do (T-02-68): it can only make agents
// disappear from its own list twice, which revokes them without blocking their
// hosts, so they enrol again, and every revoke is audited under
// system:agent-reaper.
func (r *Reaper) Observe(ctx context.Context, envID string, list dockerx.ClusterRead, answeringNodeID string) {
	// The same rule decides whether an enrolment is checked against a list (dockerx.ClusterRead.ListsAnswerer).
	if !list.ListsAnswerer(answeringNodeID) {
		return
	}
	listed := make(map[string]struct{}, len(list.Nodes))
	for _, n := range list.Nodes {
		listed[n.ID] = struct{}{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.svc.st.RQ.ListAgentsByEnv(ctx, envID)
	if err != nil {
		return // nothing was learned about the agents, so nothing is counted
	}
	misses := r.misses[envID]
	if misses == nil {
		misses = map[string]int{}
		r.misses[envID] = misses
	}
	live := make(map[string]struct{}, len(rows))
	for _, a := range rows {
		if a.Status != statusActive || !a.SwarmNodeID.Valid || a.SwarmNodeID.String == "" {
			// Revoked, or a standalone host: not the reaper's.
			continue
		}
		live[a.ID] = struct{}{}
		if _, ok := listed[a.SwarmNodeID.String]; ok {
			delete(misses, a.ID)
			continue
		}
		misses[a.ID]++
		if misses[a.ID] < reapAfterMisses {
			continue
		}
		delete(misses, a.ID)
		// A failed revoke is tried again at the next two lists; ErrAlreadyRevoked means
		// an admin was first.
		_ = r.svc.AutoRevoke(ctx, envID, a.ID)
	}
	for id := range misses {
		if _, ok := live[id]; !ok {
			delete(misses, id)
		}
	}
	// The older rows of a node this list shows, left by enrolments no list had confirmed yet.
	// An error is ignored as AutoRevoke's is: the next list tries again.
	_ = r.svc.retireListedDuplicates(ctx, envID, listed)
}

// Forget drops what is counted for a removed environment.
func (r *Reaper) Forget(envID string) {
	r.mu.Lock()
	delete(r.misses, envID)
	r.mu.Unlock()
}
