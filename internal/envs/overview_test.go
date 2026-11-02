// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

// A cluster read is only as current as the manager that gave it. When no manager is
// connected to answer, the cached node counts (read while the managers were up) are
// not shown as if they were current: the overview falls back to what the agents
// themselves say, the same "unknown, not wrong" the summary route gives for nodes.

// gone is an agent whose session ended long before now.
func gone(a AgentView) AgentView {
	a.Connected = false
	return a
}

func TestOverviewDoesNotClaimTheCachedNodeCountsWhenNoManagerIsConnected(t *testing.T) {
	now := t0.Add(time.Hour)
	in := swarm(now, gone(mgr("a", "m1")), gone(mgr("b", "m2")), gone(mgr("c", "m3")), func() AgentView {
		w := wrk("d", "w1")
		w.LastSeen = now
		return w
	}())
	in.Cluster = &ClusterView{Known: true, NodesReady: 4, NodesTotal: 4, ExpectedAgents: 4, NotAManager: true}
	in.Expected = 4

	o := overview(in, Status{}, "")
	if o.Nodes != nil && o.Nodes.Total == 4 && o.Nodes.Ready == 4 {
		t.Fatalf("nodes = %+v: the cached 4 of 4 was claimed with no manager connected", *o.Nodes)
	}
	if o.Nodes == nil || *o.Nodes != (NodeCounts{Ready: 1, Total: 4}) {
		t.Errorf("nodes = %+v, want the agents' own count, 1 present of 4 expected", o.Nodes)
	}
	if o.Agents == nil || *o.Agents != (AgentCounts{Connected: 1, Expected: 4}) {
		t.Errorf("agents = %+v, want 1 connected of 4 expected", o.Agents)
	}

	// With a manager connected the cluster's counts stand (the existing rule).
	in.Agents[0].Connected = true
	in.Agents[0].LastSeen = now
	in.Cluster.NotAManager = false
	o = overview(in, Status{}, "")
	if o.Nodes == nil || *o.Nodes != (NodeCounts{Ready: 4, Total: 4}) {
		t.Errorf("nodes = %+v, want 4 of 4 from the cluster once a manager answers", o.Nodes)
	}
}

func TestOverviewOfALocalSwarmDoesNotClaimCachedNodeCountsFromAWorkerOrAnOfflineSocket(t *testing.T) {
	cluster := &ClusterView{Known: true, NodesReady: 3, NodesTotal: 3}
	local := func(typ string, state State) (RollupInput, Status) {
		return RollupInput{Kind: KindLocal, Cluster: cluster}, Status{State: state, Probe: dockerx.ProbeResult{Type: typ}}
	}

	in, st := local(dockerx.TypeSwarmManager, StateOnline)
	if o := overview(in, st, ""); o.Nodes == nil || *o.Nodes != (NodeCounts{Ready: 3, Total: 3}) {
		t.Errorf("a local manager: nodes = %+v, want 3 of 3 from the cluster", o.Nodes)
	}
	in, st = local(dockerx.TypeSwarmWorker, StateOnline)
	if o := overview(in, st, ""); o.Nodes != nil {
		t.Errorf("a local worker: nodes = %+v, want unknown, a worker cannot answer a cluster question", *o.Nodes)
	}
	in, st = local(dockerx.TypeSwarmManager, StateOffline)
	if o := overview(in, st, ""); o.Nodes != nil {
		t.Errorf("an offline local manager: nodes = %+v, want unknown, the cached read is stale", *o.Nodes)
	}
}

// The address space summary reaches the overview with the cluster view (plan 10-17, D-06 items 1 and 2).
func TestOverviewAddressSpace(t *testing.T) {
	now := t0.Add(time.Hour)
	sum := &AddressSummary{
		Worst:    &WorstMeter{Kind: MeterOverlay, Name: "backend", Used: 200, Capacity: 256, Percent: 78, State: "getting_full", Unit: UnitAddresses},
		Orphaned: 2, Measured: true,
	}

	// A swarm with a manager present shows it.
	in := swarm(now, mgr("a", "m1"), wrk("b", "w1"))
	in.Cluster = &ClusterView{AddressSpace: sum}
	if o := overview(in, Status{}, ""); o.AddressSpace != sum {
		t.Errorf("swarm: address space = %+v, want the summary of the view", o.AddressSpace)
	}
	// No summary, or no cluster view, is nil: nothing was ever measured.
	in.Cluster = &ClusterView{}
	if o := overview(in, Status{}, ""); o.AddressSpace != nil {
		t.Errorf("no summary: address space = %+v, want nil", o.AddressSpace)
	}
	in.Cluster = nil
	if o := overview(in, Status{}, ""); o.AddressSpace != nil {
		t.Errorf("no view: address space = %+v, want nil", o.AddressSpace)
	}
	// Every agent gone: the numbers would be out of date, so none are shown.
	in = swarm(now, gone(mgr("a", "m1")), gone(mgr("b", "m2")))
	in.Cluster = &ClusterView{AddressSpace: sum}
	in.Agents[0].LastSeen, in.Agents[1].LastSeen = t0, t0
	if o := overview(in, Status{}, ""); o.AddressSpace != nil {
		t.Errorf("offline: address space = %+v, want nil", o.AddressSpace)
	}
	// A standalone host shows its estimate.
	host := standalone(now, up("a", "host"))
	host.Cluster = &ClusterView{AddressSpace: sum}
	if o := overview(host, Status{}, ""); o.AddressSpace != sum {
		t.Errorf("standalone: address space = %+v, want the summary", o.AddressSpace)
	}

	// The server's own socket: shown while it answers, not while it does not.
	local := RollupInput{EnvID: "local", Kind: KindLocal, Now: now, Cluster: &ClusterView{AddressSpace: sum}}
	if o := overview(local, Status{State: StateOnline, Probe: dockerx.ProbeResult{Type: dockerx.TypeSwarmManager}}, ""); o.AddressSpace != sum {
		t.Errorf("local online: address space = %+v, want the summary", o.AddressSpace)
	}
	if o := overview(local, Status{State: StateOffline}, ""); o.AddressSpace != nil {
		t.Errorf("local offline: address space = %+v, want nil", o.AddressSpace)
	}
}
