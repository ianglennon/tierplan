// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"log/slog"
	"testing"
)

// The host policy of the manager a service write goes to (plan 06-18, D-36): the review tells the
// page before an apply that the manager's agent would refuse it.

func policyClusters() *Clusters {
	return NewClusters(nil, nil, ClusterOptions{Log: slog.New(slog.DiscardHandler)})
}

func TestManagerPolicyOfReadsTheAgentOfTheManagerAWriteGoesTo(t *testing.T) {
	c := policyClusters()
	in := RollupInput{
		EnvID: "env-1", Kind: KindAgent, EnvType: envTypeSwarm,
		Agents: []AgentView{
			{ID: "agent-b", Role: roleManager, Connected: true, ReadOnly: true},
			{ID: "agent-a", Role: roleManager, Connected: true, PrivilegedServices: true},
			{ID: "agent-w", Role: "worker", Connected: true, ReadOnly: true},
		},
	}
	got := c.ManagerPolicyOf(in)
	if want := (ManagerPolicy{Known: true, PrivilegedServices: true}); got != want {
		t.Fatalf("ManagerPolicyOf = %+v, want %+v (agent-a, the lowest ID of the connected managers)", got, want)
	}
	// A manager that is not connected is not the one a write goes to.
	in.Agents[1].Connected = false
	if got := c.ManagerPolicyOf(in); got != (ManagerPolicy{Known: true, ReadOnly: true}) {
		t.Fatalf("ManagerPolicyOf = %+v, want agent-b's policy", got)
	}
}

func TestManagerPolicyOfIsUnknownWhereNoAgentIsInTheWay(t *testing.T) {
	c := policyClusters()
	for name, in := range map[string]RollupInput{
		"the server's own socket": {EnvID: "env-1", Kind: KindLocal},
		"no manager connected": {EnvID: "env-1", Kind: KindAgent, EnvType: envTypeSwarm,
			Agents: []AgentView{{ID: "agent-1", Role: roleManager, Connected: false, PrivilegedServices: true}}},
		"no agents at all": {EnvID: "env-1", Kind: KindAgent, EnvType: envTypeSwarm},
	} {
		if got := c.ManagerPolicyOf(in); got != (ManagerPolicy{}) {
			t.Errorf("%s: ManagerPolicyOf = %+v, want the zero (unknown) policy", name, got)
		}
	}
}
