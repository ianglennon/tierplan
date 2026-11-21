// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// LocalSwarmClusterID is what the ingress preflight compares with an environment's swarm cluster ID, to tell
// a server that runs on a node of the very swarm it manages from one that does not (D-09).

func TestLocalSwarmClusterID(t *testing.T) {
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
		want string
	}{
		{name: "an active manager gives its cluster", cfg: swarm("active", true, "cluster-1"), want: "cluster-1"},
		{name: "an active worker, which does not report the cluster, gives none", cfg: swarm("active", false, ""), want: ""},
		{name: "a standalone engine gives none", cfg: swarm("inactive", false, ""), want: ""},
		{name: "an engine with no swarm state gives none", cfg: dockertest.FakeConfig{Hostname: "node-1"}, want: ""},
		{
			name: "no socket at the configured path gives none",
			cfg:  swarm("active", true, "cluster-1"),
			prep: func(t *testing.T, r *rig) {
				r.svc = NewService(r.st, r.aud, r.mon, "unix:///nonexistent/dir/docker.sock", "", r.clk.now)
			},
		},
		{
			name: "an engine that does not answer gives none",
			cfg:  dockertest.FakeConfig{Hostname: "node-1", SwarmState: "active", SwarmControl: true, SwarmNodeID: "node-local", SwarmClusterID: "cluster-1", FailInfo: true},
		},
		{
			name: "a stopped daemon gives none",
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
			got, err := r.svc.LocalSwarmClusterID(context.Background())
			if err != nil {
				t.Fatalf("LocalSwarmClusterID: %v, want no error", err)
			}
			if got != tc.want {
				t.Fatalf("LocalSwarmClusterID = %q, want %q", got, tc.want)
			}
		})
	}
}
