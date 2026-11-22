// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// envInfoFrom describes an environment to the ingress deployer: a standalone environment as one node whose AgentID is
// what the stack pipeline routes writes by, a swarm environment as its nodes.

func TestEnvInfoFromStandaloneLocal(t *testing.T) {
	t.Parallel()
	row := sqlc.Environment{ID: "env-1", Name: "Local", Kind: envs.KindLocal}
	in := func(state envs.State) envs.RollupInput {
		return envs.RollupInput{Kind: envs.KindLocal, Local: envs.Status{
			State: state,
			Probe: dockerx.ProbeResult{Type: dockerx.TypeStandalone, Hostname: "h1", EngineID: "E1"},
		}}
	}
	for _, tc := range []struct {
		name      string
		state     envs.State
		connected bool
	}{
		{"online", envs.StateOnline, true},
		{"offline", envs.StateOffline, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := envInfoFrom("env-1", row, in(tc.state))
			want := ingressdeploy.EnvInfo{
				ID: "env-1", Name: "Local", Swarm: false, Local: true, EngineID: "E1",
				Nodes: []ingressdeploy.NodeInfo{{AgentID: "env-1", Name: "h1", Connected: tc.connected}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("envInfoFrom = %+v, want %+v", got, want)
			}
		})
	}
}

func TestEnvInfoFromStandaloneAgent(t *testing.T) {
	t.Parallel()
	row := sqlc.Environment{
		ID: "env-2", Name: "Edge", Kind: envs.KindAgent,
		EngineID: sql.NullString{String: "E2", Valid: true},
	}
	in := envs.RollupInput{Kind: envs.KindAgent, EnvType: "standalone", Agents: []envs.AgentView{
		{ID: "A0", Node: "old", Revoked: true},
		{ID: "A1", Node: "n1", Connected: true},
	}}
	got := envInfoFrom("env-2", row, in)
	want := ingressdeploy.EnvInfo{
		ID: "env-2", Name: "Edge", Swarm: false, Local: false, EngineID: "E2",
		Nodes: []ingressdeploy.NodeInfo{{AgentID: "A1", Name: "n1", Connected: true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envInfoFrom = %+v, want %+v", got, want)
	}
}

func TestEnvInfoFromSwarmUnchanged(t *testing.T) {
	t.Parallel()
	t.Run("a local swarm manager", func(t *testing.T) {
		row := sqlc.Environment{
			ID: "env-3", Name: "Local", Kind: envs.KindLocal,
			SwarmClusterID: sql.NullString{String: "cl-1", Valid: true},
		}
		in := envs.RollupInput{Kind: envs.KindLocal, Local: envs.Status{
			State: envs.StateOnline,
			Probe: dockerx.ProbeResult{Type: dockerx.TypeSwarmManager, Hostname: "m1", SwarmNodeID: "sn-1", EngineID: "E3"},
		}}
		got := envInfoFrom("env-3", row, in)
		want := ingressdeploy.EnvInfo{
			ID: "env-3", Name: "Local", Swarm: true, ClusterID: "cl-1", ManagerConnected: true,
			Nodes: []ingressdeploy.NodeInfo{{SwarmNodeID: "sn-1", AgentID: "env-3", Name: "m1", Role: "manager", Connected: true}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("envInfoFrom = %+v, want %+v", got, want)
		}
	})
	t.Run("an agent swarm", func(t *testing.T) {
		row := sqlc.Environment{
			ID: "env-4", Name: "Cluster", Kind: envs.KindAgent,
			SwarmClusterID: sql.NullString{String: "cl-2", Valid: true},
			EngineID:       sql.NullString{String: "ignored", Valid: true},
		}
		in := envs.RollupInput{Kind: envs.KindAgent, EnvType: "swarm", Agents: []envs.AgentView{
			{ID: "A1", Node: "m1", Role: "manager", SwarmNodeID: "sn-1", Connected: true},
			{ID: "A2", Node: "w1", Role: "worker", SwarmNodeID: "sn-2"},
			{ID: "A3", Node: "gone", Revoked: true},
		}}
		got := envInfoFrom("env-4", row, in)
		want := ingressdeploy.EnvInfo{
			ID: "env-4", Name: "Cluster", Swarm: true, ClusterID: "cl-2", ManagerConnected: true,
			Nodes: []ingressdeploy.NodeInfo{
				{SwarmNodeID: "sn-1", AgentID: "A1", Name: "m1", Role: "manager", Connected: true},
				{SwarmNodeID: "sn-2", AgentID: "A2", Name: "w1", Role: "worker"},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("envInfoFrom = %+v, want %+v", got, want)
		}
	})
}
