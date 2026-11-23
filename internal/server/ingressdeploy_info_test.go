// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/testsupport"
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

// The server gives the deployer the system deployer a standalone host's proxy is brought up with (D-11): the stack
// pipeline's own deps on the server's aggregator, address spaces and registry resolver, and no ingress, so the project is
// never attached or claimed.
func TestNewIngressDeployWiresTheSystemDeployer(t *testing.T) {
	ctx := testsupport.Context(t)
	log := slog.New(slog.DiscardHandler)
	srv, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": filepath.Join(t.TempDir(), "data")}), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if _, err := srv.store.Write.ExecContext(ctx,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		 VALUES ('env-sys', 'agent', 'Edge host', '', 1, 1, 1, 'standalone')`); err != nil {
		t.Fatal(err)
	}
	notifier := ingress.NewNotifier()
	spaces := envs.NewAddressSpaces(srv.envs, srv.aggregator, srv.clusters, envs.AddressSpaceOptions{Log: log})
	deps := ingressDeployDeps{
		Log: log, Store: srv.store, Audit: srv.audit, Aggregator: srv.aggregator, Clusters: srv.clusters, Envs: srv.envs,
		Ingress:  ingressstore.New(ingressstore.Deps{St: srv.store, Vault: srv.vault, Notifier: notifier}),
		Notifier: notifier, Tokens: ingresshub.NewService(srv.store, srv.audit, time.Now), Jobs: srv.jobs,
		Locks: stackops.NewLocks(), Resolver: srv.resolver, AddressSpaces: spaces, Instance: srv.inst,
	}

	want := stackops.Deps{Aggregator: srv.aggregator, AddressSpaces: spaces, Resolver: srv.resolver, Log: log}
	if got := ingressSystem(deps); !reflect.DeepEqual(got, want) {
		t.Errorf("ingressSystem = %+v, want %+v", got, want)
	}

	// Through the deployer the server builds, a standalone environment is answered by the standalone plan.
	res, err := newIngressDeploy(deps).Preview(ctx, "env-sys", ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Plan.Standalone {
		t.Errorf("Preview of a standalone environment answered a swarm plan: %+v", res.Plan)
	}
}

// The host read the server gives the ingress stacks adapter reaches a standalone environment's node through the
// aggregator, so a host that is not connected is an error and the read never runs (D-16, D-18).
func TestIngressHostReadNeedsAConnectedHost(t *testing.T) {
	ctx := testsupport.Context(t)
	srv, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": filepath.Join(t.TempDir(), "data")}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if _, err := srv.store.Write.ExecContext(ctx,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		 VALUES ('env-host', 'agent', 'Edge host', '', 1, 1, 1, 'standalone')`); err != nil {
		t.Fatal(err)
	}
	ran := false
	err = ingressHostRead(srv.aggregator)(ctx, "env-host", func(*dockerx.Client) error { ran = true; return nil })
	if err == nil {
		t.Error("host read of a standalone environment with no connected host answered no error")
	}
	if ran {
		t.Error("the read ran on a host that is not connected")
	}
}
