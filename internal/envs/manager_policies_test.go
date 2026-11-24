// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// The privileged-services opt-in of every manager of a swarm (VIP-05, D-19, A-12): the gate of the virtual IP reads it
// from the swarm's node list joined to the connected agents, not from the one manager a write goes to.

func TestManagerPoliciesReadsEveryManager(t *testing.T) {
	worker := dockertest.FakeNode{ID: "node-w1", Hostname: "wrk-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"}
	nodes := managerNodes(worker)
	s := newSwarmRigBare(t, nodes, nil)
	facts := func(i int, privileged bool) *wire.Facts {
		f := managerFacts(i)
		f.Policy = wire.PolicyFacts{PrivilegedServices: privileged}
		return f
	}
	// mgr-1 allows, mgr-2 refuses, mgr-3 has no agent.
	s.m[0] = s.connect(t, s.env, managerConfig(1, nodes, nil), facts(1, true))
	s.m[1] = s.connect(t, s.env, managerConfig(2, nodes, nil), facts(2, false))
	s.whenOnline(t, s.env.ID)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	// A worker whose agent allows is no manager and is not listed.
	s.connect(t, s.env, dockertest.FakeConfig{Hostname: "wrk-1", SwarmState: "active"}, &wire.Facts{
		EngineID: "ENG:w1", Hostname: "wrk-1", EngineReachable: true,
		Swarm:  &wire.SwarmFacts{State: "active", NodeID: "node-w1", ClusterID: "cluster-1"},
		Policy: wire.PolicyFacts{PrivilegedServices: true},
	})

	rows, err := s.c.ManagerPolicies(ctx, s.env.ID)
	if err != nil {
		t.Fatalf("ManagerPolicies: %v", err)
	}
	want := []ManagerPolicyRow{
		{NodeID: "node-m1", Hostname: "mgr-1", State: ManagerAllows},
		{NodeID: "node-m2", Hostname: "mgr-2", State: ManagerRefuses},
		{NodeID: "node-m3", Hostname: "mgr-3", State: ManagerUnknown},
	}
	if len(rows) != len(want) {
		t.Fatalf("ManagerPolicies = %+v, want %+v (managers only)", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// nodeRows is a node list of managers and workers by ID and host name; a name ending in "!" is a worker.
func nodeRows(spec ...[2]string) []dockerx.NodeRow {
	var out []dockerx.NodeRow
	for _, s := range spec {
		role := roleManager
		if len(s[1]) > 0 && s[1][len(s[1])-1] == '!' {
			role, s[1] = roleWorker, s[1][:len(s[1])-1]
		}
		out = append(out, dockerx.NodeRow{ID: s[0], Hostname: s[1], Role: role})
	}
	return out
}

func statesOf(rows []ManagerPolicyRow) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		out[r.NodeID] = r.State
	}
	return out
}

func TestManagerPoliciesLocalSocket(t *testing.T) {
	nodes := nodeRows([2]string{"n1", "mgr-1"}, [2]string{"n2", "mgr-2"}, [2]string{"n3", "mgr-3"}, [2]string{"n4", "wrk-1!"})

	// The folded host: its manager is a connected socket view, the others have agents or none.
	in := RollupInput{
		EnvID: "env-1", Kind: KindAgent, EnvType: envTypeSwarm,
		Agents: []AgentView{
			{ID: "socket:env-1", Node: "mgr-1", Role: roleManager, SwarmNodeID: "n1", Socket: true, Connected: true},
			{ID: "agent-2", Role: roleManager, SwarmNodeID: "n2", Connected: true, PrivilegedServices: true},
			{ID: "agent-3", Role: roleManager, SwarmNodeID: "n3", Connected: true},
			{ID: "agent-4", Role: roleWorker, SwarmNodeID: "n4", Connected: true, PrivilegedServices: true},
		},
	}
	rows := managerPolicyRows(in, nodes)
	if want := (ManagerPolicyRow{NodeID: "n1", Hostname: "mgr-1", State: ManagerAllows, LocalSocket: true}); rows[0] != want {
		t.Errorf("the folded socket's manager = %+v, want %+v", rows[0], want)
	}
	if got, want := statesOf(rows), map[string]string{"n1": ManagerAllows, "n2": ManagerAllows, "n3": ManagerRefuses}; !reflect.DeepEqual(got, want) {
		t.Errorf("states = %v, want %v (the others keep their agent's state, the worker is not listed)", got, want)
	}
	for _, r := range rows[1:] {
		if r.LocalSocket {
			t.Errorf("%s is marked as the local socket; only the folded host is", r.NodeID)
		}
	}

	// A Local environment: its own engine's node is the local socket, the other managers have no agent.
	local := RollupInput{EnvID: "env-2", Kind: KindLocal}
	local.Local.Probe = dockerx.ProbeResult{Type: dockerx.TypeSwarmManager, SwarmNodeID: "n2"}
	rows = managerPolicyRows(local, nodes)
	if got, want := statesOf(rows), map[string]string{"n1": ManagerUnknown, "n2": ManagerAllows, "n3": ManagerUnknown}; !reflect.DeepEqual(got, want) {
		t.Errorf("a Local environment's states = %v, want %v", got, want)
	}
	if !rows[1].LocalSocket || rows[0].LocalSocket {
		t.Errorf("local socket flags = %+v, want only n2", rows)
	}

	// A Local engine that is not a swarm manager, or names no node, makes no manager the local socket.
	local.Local.Probe = dockerx.ProbeResult{Type: dockerx.TypeSwarmWorker, SwarmNodeID: "n2"}
	for _, r := range managerPolicyRows(local, nodes) {
		if r.LocalSocket || r.State != ManagerUnknown {
			t.Errorf("a worker's socket made %+v", r)
		}
	}
}

func TestManagerPoliciesNoAgentIsNeverAllowed(t *testing.T) {
	nodes := nodeRows([2]string{"n1", "mgr-1"}, [2]string{"n2", "mgr-2"}, [2]string{"n3", "mgr-3"}, [2]string{"n4", "mgr-4"})
	in := RollupInput{
		EnvID: "env-1", Kind: KindAgent, EnvType: envTypeSwarm,
		Agents: []AgentView{
			// Disconnected, revoked, and an agent that claims no node: none of them speaks for a manager.
			{ID: "a1", SwarmNodeID: "n1", Connected: false, PrivilegedServices: true},
			{ID: "a2", SwarmNodeID: "n2", Connected: true, Revoked: true, PrivilegedServices: true},
			{ID: "a3", SwarmNodeID: "", Connected: true, PrivilegedServices: true},
			// Two agents on one node: one that refuses wins over one that allows.
			{ID: "a4", SwarmNodeID: "n4", Connected: true, PrivilegedServices: true},
			{ID: "a5", SwarmNodeID: "n4", Connected: true},
		},
	}
	want := map[string]string{"n1": ManagerUnknown, "n2": ManagerUnknown, "n3": ManagerUnknown, "n4": ManagerRefuses}
	if got := statesOf(managerPolicyRows(in, nodes)); !reflect.DeepEqual(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}
}

func TestManagerPoliciesOrder(t *testing.T) {
	nodes := nodeRows(
		[2]string{"n-m2", "m2"}, [2]string{"z-id", "a"}, [2]string{"n-m1", "m1"}, [2]string{"b-id", "a"},
	)
	in := RollupInput{EnvID: "env-1", Kind: KindAgent, EnvType: envTypeSwarm}
	var got []string
	for _, r := range managerPolicyRows(in, nodes) {
		got = append(got, r.Hostname+"/"+r.NodeID)
	}
	if want := []string{"a/b-id", "a/z-id", "m1/n-m1", "m2/n-m2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v (host name, then node ID)", got, want)
	}
}

func TestManagerPoliciesFailsWithoutAReadableManagerList(t *testing.T) {
	// No agent has connected, so no node list was ever read: an error, never an empty table.
	s := newSwarmRigBare(t, managerNodes(), nil)
	if rows, err := s.c.ManagerPolicies(context.Background(), s.env.ID); !errors.Is(err, ErrNoManagerList) || rows != nil {
		t.Errorf("ManagerPolicies = %v, %v, want ErrNoManagerList", rows, err)
	}
}
