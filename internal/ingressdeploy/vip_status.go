// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// ErrVIPNotEnabled is the answer for an environment that has no virtual IP row: the status is never an empty one.
var ErrVIPNotEnabled = errors.New("ingressdeploy: the environment has no virtual IP")

// The states of VIPHolder.
const (
	// VIPHolderNone means no node reports MASTER.
	VIPHolderNone = "none"
	// VIPHolderOne is the healthy case: exactly one node reports MASTER.
	VIPHolderOne = "one"
	// VIPHolderMany means more than one node reports MASTER, which the status reports as a problem.
	VIPHolderMany = "many"
)

// VIPHolder says which nodes hold the address, from the lines the nodes' helpers wrote.
type VIPHolder struct {
	// State is VIPHolderNone, VIPHolderOne or VIPHolderMany.
	State string
	// NodeIDs are the swarm node IDs of the nodes reporting MASTER, ordered by host name then ID; empty for none.
	NodeIDs []string
}

// VIPCandidate is one node that can hold the address and how it stands. Every field is a closed-vocabulary value, a
// validated host name or a time: no log text reaches it (T-26-31).
type VIPCandidate struct {
	NodeID, Hostname string
	// VRRPState is MASTER, BACKUP, FAULT or STOP as the node last reported, "" when it has not reported any.
	VRRPState string
	// ProxyReady is true when the node's proxy is enrolled, healthy and on the current bundle, as the ingress status
	// counts a ready node.
	ProxyReady bool
	// TaskState is the state of the node's newest keepalived task in the swarm's task list, "" when it has none or the
	// task list could not be read. TaskStarted is when that task reached the running state, the zero time when it is not
	// running.
	TaskState   string
	TaskStarted time.Time
	// LastFault is the reason of the node's last fault, "" when it has none.
	LastFault string
	// LastHeard is when TierPlan last read the node's log without a failure, the zero time when it never has.
	LastHeard time.Time
}

// VIPStatus is what TierPlan reports of an environment's virtual IP (D-22). It holds no key and no secret.
type VIPStatus struct {
	Enabled bool
	// Address is the virtual address with the prefix length of its network.
	Address netip.Prefix
	// RouterID is the VRRP router ID and RouterDerived says the server chose it.
	RouterID      int
	RouterDerived bool
	KeyGeneration int
	Holder        VIPHolder
	// HolderlessSince is when the address last lost its holder; set only while Holder is none, and the zero time then
	// too when the reader has not seen the environment yet.
	HolderlessSince time.Time
	// Candidates are the nodes that run a keepalived task or have reported a state, ordered by host name then node ID.
	Candidates []VIPCandidate
	// Managers is the privileged-services opt-in of every manager, and ManagersReadable is false when it could not be
	// read, which leaves Managers empty.
	Managers         []envs.ManagerPolicyRow
	ManagersReadable bool
	// ForeignRouter is true when any node reports another router using the router ID, and ForeignSender is that router's
	// validated address, the invalid Addr when none was reported.
	ForeignRouter bool
	ForeignSender netip.Addr
	// ClockSkewNodes are the swarm node IDs of the nodes reporting clock skew, ordered by host name then ID.
	ClockSkewNodes []string
	// EnsureAt and EnsureError are when the last ensure pass ended and the class of its failure, "" when it succeeded.
	EnsureAt    time.Time
	EnsureError string
}

// vipProxyReady says, by swarm node ID, whether each enrolled proxy node is ready, by the rule the ingress status uses.
func (s *Service) vipProxyReady(ctx context.Context, envID string) (map[string]bool, error) {
	ready := map[string]bool{}
	if s.d.Nodes == nil {
		return ready, nil
	}
	rows, err := s.d.Nodes.ListNodes(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list the ingress nodes: %w", err)
	}
	current, have := "", false
	if s.d.CurrentBundle != nil {
		current, have = s.d.CurrentBundle(envID)
	}
	for _, n := range rows {
		if n.SwarmNodeID != "" && s.proxyReady(n, current, have) {
			ready[n.SwarmNodeID] = true
		}
	}
	return ready, nil
}

// vipTasks returns the newest keepalived task of each node, by swarm node ID. A swarm that cannot be read gives none
// and no error: the status then leaves the task states empty and still answers from what the reader knows.
func (s *Service) vipTasks(ctx context.Context, envID string) map[string]dockerx.TaskRow {
	out := map[string]dockerx.TaskRow{}
	if s.d.Engine == nil {
		return out
	}
	err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		rows, err := c.Tasks(ctx, dockerx.TaskFilter{ServiceIDs: []string{ingress.ServiceNameKeepalived}})
		if err != nil {
			return err
		}
		// The rows come newest first, so the first of a node is its current task.
		for _, r := range rows {
			if _, seen := out[r.NodeID]; !seen && r.NodeID != "" {
				out[r.NodeID] = r
			}
		}
		return nil
	})
	if err != nil {
		s.log().Warn("could not read the keepalived tasks for the status", "environment", envID, "error", err)
		return map[string]dockerx.TaskRow{}
	}
	return out
}

// VIPStatus reads the virtual IP of an environment: the configuration, who holds the address and how each candidate
// stands (D-22). An environment with no virtual IP is ErrVIPNotEnabled. The manager table, the swarm's task list and the
// host names of the nodes are read without failing the status: what cannot be read is left empty or unreadable.
func (s *Service) VIPStatus(ctx context.Context, envID string) (VIPStatus, error) {
	cfg, err := s.d.Store.GetVIP(ctx, envID)
	if errors.Is(err, ingressstore.ErrVIPNotFound) {
		return VIPStatus{}, ErrVIPNotEnabled
	}
	if err != nil {
		return VIPStatus{}, err
	}
	st := VIPStatus{
		Enabled: cfg.Enabled, Address: cfg.Prefix(), RouterID: cfg.RouterID, RouterDerived: cfg.RouterDerived,
		KeyGeneration: cfg.KeyGeneration, EnsureAt: cfg.EnsureAt, EnsureError: cfg.EnsureError,
		Candidates: []VIPCandidate{}, Managers: []envs.ManagerPolicyRow{}, ClockSkewNodes: []string{},
	}

	ready, err := s.vipProxyReady(ctx, envID)
	if err != nil {
		return VIPStatus{}, err
	}
	names := map[string]string{}
	if s.d.Envs != nil {
		if info, err := s.d.Envs.Info(ctx, envID); err != nil {
			s.log().Warn("could not read the environment for the virtual IP status", "environment", envID, "error", err)
		} else {
			for _, n := range info.Nodes {
				names[n.SwarmNodeID] = n.Name
			}
		}
	}
	tasks := s.vipTasks(ctx, envID)
	states := s.vipNodeStates(envID)

	byID := map[string]*VIPCandidate{}
	candidate := func(id string) *VIPCandidate {
		c := byID[id]
		if c == nil {
			c = &VIPCandidate{NodeID: id, Hostname: names[id], ProxyReady: ready[id]}
			byID[id] = c
		}
		return c
	}
	for _, n := range states {
		c := candidate(n.NodeID)
		if n.Hostname != "" {
			c.Hostname = n.Hostname
		}
		c.VRRPState, c.LastFault, c.LastHeard = n.State, n.Reason, n.LastHeard
		if n.State == ingressbundle.VIPStateMaster {
			st.Holder.NodeIDs = append(st.Holder.NodeIDs, n.NodeID)
		}
		if n.Foreign {
			st.ForeignRouter = true
			if sender, err := netip.ParseAddr(n.ForeignSender); err == nil && !st.ForeignSender.IsValid() {
				st.ForeignSender = sender
			}
		}
		if n.ClockSkew {
			st.ClockSkewNodes = append(st.ClockSkewNodes, n.NodeID)
		}
	}
	for id, t := range tasks {
		c := candidate(id)
		c.TaskState = t.State
		if t.State == "running" {
			c.TaskStarted = t.StateSince
		}
	}

	st.Holder.State = VIPHolderNone
	switch len(st.Holder.NodeIDs) {
	case 0:
		st.HolderlessSince = s.vipHolderlessSince(envID)
	case 1:
		st.Holder.State = VIPHolderOne
	default:
		st.Holder.State = VIPHolderMany
	}

	for _, c := range byID {
		st.Candidates = append(st.Candidates, *c)
	}
	slices.SortFunc(st.Candidates, func(a, b VIPCandidate) int {
		return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.NodeID, b.NodeID))
	})

	if s.d.Managers != nil {
		rows, err := s.d.Managers.ManagerPolicies(ctx, envID)
		if err != nil {
			s.log().Warn("could not read the manager policies for the virtual IP status", "environment", envID, "error", err)
		} else {
			st.Managers, st.ManagersReadable = rows, true
		}
	}
	return st, nil
}

// VIPHolds answers the drain hook's question about one node (D-25): whether it holds the address, and whether it holds it
// alone, meaning no other candidate is BACKUP with a ready proxy to take it. An environment with no virtual IP answers
// false, false. It reads only what the reader knows and the proxy nodes' rows, never the swarm.
func (s *Service) VIPHolds(ctx context.Context, envID, swarmNodeID string) (holds, alone bool, err error) {
	if _, err := s.d.Store.GetVIP(ctx, envID); errors.Is(err, ingressstore.ErrVIPNotFound) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	ready, err := s.vipProxyReady(ctx, envID)
	if err != nil {
		return false, false, err
	}
	states := s.vipNodeStates(envID)
	for _, n := range states {
		if n.NodeID == swarmNodeID && n.State == ingressbundle.VIPStateMaster {
			holds = true
		}
	}
	if !holds {
		return false, false, nil
	}
	alone = true
	for _, n := range states {
		if n.NodeID != swarmNodeID && n.State == ingressbundle.VIPStateBackup && ready[n.NodeID] {
			alone = false
		}
	}
	return holds, alone, nil
}

// VIPMoves reads one page of the address's moves, newest first by time and then by insertion order, for the history route
// (D-23). before is nil for the first page, otherwise the cursor the previous page returned, which is nil on the last
// page. The slice is never nil, and an environment with no moves, or no virtual IP, has an empty one.
func (s *Service) VIPMoves(ctx context.Context, envID string, before *ingressstore.VIPMoveCursor, limit int) ([]ingressstore.VIPMove, *ingressstore.VIPMoveCursor, error) {
	return s.d.Store.ListVIPMoves(ctx, envID, before, limit)
}
