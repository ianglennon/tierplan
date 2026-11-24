// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// LocalSwarmMembership is what the ingress preflight compares with an environment's swarm, to tell a server that runs on a
// node of the very swarm it manages from one that does not, and to say so when it cannot tell (D-09).

func TestLocalSwarmMembership(t *testing.T) {
	t.Parallel()
	swarm := func(state string, control bool, cluster string) dockertest.FakeConfig {
		return dockertest.FakeConfig{
			Hostname: "node-1", SwarmState: state, SwarmControl: control, SwarmNodeID: "node-local", SwarmClusterID: cluster,
		}
	}
	tests := []struct {
		name string
		cfg  dockertest.FakeConfig
		prep func(t *testing.T, r *rig)
		want SwarmMembership
	}{
		{name: "an active manager gives its cluster and its node", cfg: swarm("active", true, "cluster-1"),
			want: SwarmMembership{Known: true, Member: true, ClusterID: "cluster-1", NodeID: "node-local", EngineID: "FAKE:ID"}},
		{name: "an active worker, which does not report the cluster, gives its node only", cfg: swarm("active", false, ""),
			want: SwarmMembership{Known: true, Member: true, NodeID: "node-local", EngineID: "FAKE:ID"}},
		{name: "a standalone engine is known not to be a member", cfg: swarm("inactive", false, ""),
			want: SwarmMembership{Known: true, EngineID: "FAKE:ID"}},
		{name: "an engine with no swarm state is known not to be a member", cfg: dockertest.FakeConfig{Hostname: "node-1"},
			want: SwarmMembership{Known: true, EngineID: "FAKE:ID"}},
		{
			name: "no socket at the configured path is not known",
			cfg:  swarm("active", true, "cluster-1"),
			prep: func(t *testing.T, r *rig) {
				r.svc = NewService(r.st, r.aud, r.mon, "unix:///nonexistent/dir/docker.sock", "", r.clk.now)
			},
		},
		{
			name: "an engine that does not answer is not known",
			cfg:  dockertest.FakeConfig{Hostname: "node-1", SwarmState: "active", SwarmControl: true, SwarmNodeID: "node-local", SwarmClusterID: "cluster-1", FailInfo: true},
		},
		{
			name: "a stopped daemon is not known",
			cfg:  swarm("active", true, "cluster-1"),
			prep: func(t *testing.T, r *rig) { r.fake.Stop() },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.cfg)
			if tc.prep != nil {
				tc.prep(t, r)
			}
			got, err := r.svc.LocalSwarmMembership(context.Background())
			if err != nil {
				t.Fatalf("LocalSwarmMembership: %v, want no error", err)
			}
			if got != tc.want {
				t.Fatalf("LocalSwarmMembership = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A standalone environment is compared with the engine behind the server's own socket to tell whether it is the server's
// own host, so every answer that knows the engine carries its ID.
func TestLocalSwarmMembershipCarriesEngineID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  dockertest.FakeConfig
		prep func(t *testing.T, r *rig)
		want string
	}{
		{name: "a manager", cfg: dockertest.FakeConfig{Hostname: "n", SwarmState: "active", SwarmControl: true, SwarmNodeID: "sn", SwarmClusterID: "cl", EngineID: "ENG-M"}, want: "ENG-M"},
		{name: "a worker", cfg: dockertest.FakeConfig{Hostname: "n", SwarmState: "active", SwarmNodeID: "sn", EngineID: "ENG-W"}, want: "ENG-W"},
		{name: "a standalone engine", cfg: dockertest.FakeConfig{Hostname: "n", EngineID: "ENG-S"}, want: "ENG-S"},
		{
			name: "no socket",
			cfg:  dockertest.FakeConfig{Hostname: "n", EngineID: "ENG-X"},
			prep: func(t *testing.T, r *rig) {
				r.svc = NewService(r.st, r.aud, r.mon, "unix:///nonexistent/dir/docker.sock", "", r.clk.now)
			},
		},
		{name: "an engine that does not answer", cfg: dockertest.FakeConfig{Hostname: "n", EngineID: "ENG-X", FailInfo: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.cfg)
			if tc.prep != nil {
				tc.prep(t, r)
			}
			got, err := r.svc.LocalSwarmMembership(context.Background())
			if err != nil {
				t.Fatalf("LocalSwarmMembership: %v", err)
			}
			if got.EngineID != tc.want {
				t.Errorf("EngineID = %q, want %q", got.EngineID, tc.want)
			}
		})
	}
}
