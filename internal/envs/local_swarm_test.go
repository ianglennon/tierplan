// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// RefuseLocalOnSwarmNode is the server half of D-02 (SWM-01, D-21): a socket that is a node of a swarm is never
// added as a Local environment, whatever the swarm state, unless that node is already folded.

func TestRefuseLocalOnSwarmNode(t *testing.T) {
	t.Parallel()
	swarm := func(state string, control bool) dockertest.FakeConfig {
		return dockertest.FakeConfig{
			Hostname: "node-1", SwarmState: state, SwarmControl: control, SwarmNodeID: "node-local", SwarmClusterID: "cluster-1",
		}
	}
	tests := []struct {
		name string
		cfg  dockertest.FakeConfig
		// prep runs against the rig before the rule is asked.
		prep func(t *testing.T, r *rig)
		want error
	}{
		{name: "an active manager is refused", cfg: swarm("active", true), want: ErrLocalIsSwarmNode},
		{name: "an active worker is refused", cfg: swarm("active", false), want: ErrLocalIsSwarmNode},
		{name: "a pending node is refused", cfg: swarm("pending", false), want: ErrLocalIsSwarmNode},
		{name: "a node in error is refused", cfg: swarm("error", false), want: ErrLocalIsSwarmNode},
		{name: "a locked node is refused", cfg: swarm("locked", false), want: ErrLocalIsSwarmNode},
		{name: "a standalone engine (inactive) may be added", cfg: swarm("inactive", false)},
		{name: "a standalone engine (no swarm state) may be added", cfg: dockertest.FakeConfig{Hostname: "node-1"}},
		{
			name: "no socket at the configured path is left to AddLocal",
			cfg:  swarm("active", true),
			prep: func(t *testing.T, r *rig) {
				r.svc = NewService(r.st, r.aud, r.mon, "unix:///nonexistent/dir/docker.sock", "", r.clk.now)
			},
		},
		{
			name: "an engine that does not answer is left to AddLocal",
			cfg:  dockertest.FakeConfig{Hostname: "node-1", SwarmState: "active", SwarmControl: true, SwarmNodeID: "node-local", FailInfo: true},
		},
		{
			name: "a stopped daemon is left to AddLocal",
			cfg:  swarm("active", true),
			prep: func(t *testing.T, r *rig) { r.fake.Stop() },
		},
		{
			name: "a local environment that exists is left to AddLocal (ErrLocalExists)",
			cfg:  swarm("active", true),
			prep: func(t *testing.T, r *rig) {
				now := r.clk.now().UnixNano()
				if _, err := r.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, created_at, updated_at) VALUES ('local-1', 'local', 'Local', '/var/run/docker.sock', ?, ?)`, now, now); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a node already folded into a swarm environment may be added again",
			cfg:  swarm("active", true),
			prep: func(t *testing.T, r *rig) {
				insertAgentEnv(t, r, "swarm-env", "Swarm")
				if _, err := r.st.Write.Exec(`UPDATE environments SET local_node_id = 'node-local' WHERE id = 'swarm-env'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a swarm environment folded with another node does not let this one in",
			cfg:  swarm("active", true),
			prep: func(t *testing.T, r *rig) {
				insertAgentEnv(t, r, "swarm-env", "Swarm")
				if _, err := r.st.Write.Exec(`UPDATE environments SET local_node_id = 'node-other' WHERE id = 'swarm-env'`); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrLocalIsSwarmNode,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.cfg)
			if tc.prep != nil {
				tc.prep(t, r)
			}
			err := r.svc.RefuseLocalOnSwarmNode(context.Background())
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("RefuseLocalOnSwarmNode = %v, want %v", err, tc.want)
			}
		})
	}
}
