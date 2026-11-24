// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"testing"

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
