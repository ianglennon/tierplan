// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/engine"
)

// The fold proof (SWM-02, D-06, spike S13): the socket's own swarm facts compared with the
// node list a manager agent read. proveNode is pure, so the whole matrix is a table.

// TestTracerAWorkerProbeIsProvenByNodeIDAgainstAManagerReadList is the thin end-to-end slice: a
// fake worker engine reports its node ID and managers through the real probe, and the pure
// proof offers it only while its node ID is in the manager-read list.
func TestTracerAWorkerProbeIsProvenByNodeIDAgainstAManagerReadList(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: false, SwarmNodeID: "node-w1",
		SwarmRemoteManagers: []string{"node-m1", "node-m2"},
	})
	cli, err := dockerx.New(engine.LocalSocketTransport{Path: fake.SocketPath()})
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	p, err := cli.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !slices.Equal(p.SwarmRemoteManagers, []string{"node-m1", "node-m2"}) {
		t.Fatalf("SwarmRemoteManagers = %v, want node-m1 and node-m2", p.SwarmRemoteManagers)
	}
	if len(p.SwarmManagerHosts) != 2 {
		t.Fatalf("SwarmManagerHosts = %v, want two hosts", p.SwarmManagerHosts)
	}
	for _, h := range p.SwarmManagerHosts {
		if strings.Contains(h, ":") {
			t.Errorf("manager host %q carries a port", h)
		}
	}

	m1 := dockerx.NodeRow{ID: "node-m1", Role: roleManager}
	m2 := dockerx.NodeRow{ID: "node-m2", Role: roleManager}
	w1 := dockerx.NodeRow{ID: "node-w1", Role: roleWorker}
	if got := proveNode(p, []dockerx.NodeRow{m1, m2, w1}); got != verdictOffered {
		t.Errorf("listed worker: verdict %v, want verdictOffered", got)
	}
	if got := proveNode(p, []dockerx.NodeRow{m1, m2}); got != verdictNodeNotListed {
		t.Errorf("worker absent from the list: verdict %v, want verdictNodeNotListed", got)
	}
}

// TestProveNodeVerdicts is every row of spike S13 as a table: the verdict follows the node
// list's membership and the managers the node reports, never one of them alone.
func TestProveNodeVerdicts(t *testing.T) {
	row := func(id, role string) dockerx.NodeRow { return dockerx.NodeRow{ID: id, Role: role} }
	list := []dockerx.NodeRow{row("node-m1", roleManager), row("node-m2", roleManager), row("node-w1", roleWorker)}
	probe := func(state, node string, managers ...string) dockerx.ProbeResult {
		return dockerx.ProbeResult{SwarmNodeState: state, SwarmNodeID: node, SwarmRemoteManagers: managers}
	}
	cases := []struct {
		name  string
		p     dockerx.ProbeResult
		nodes []dockerx.NodeRow
		want  foldVerdict
	}{
		{"a manager that reports itself and another manager", probe("active", "node-m1", "node-m1", "node-m2"), list, verdictOffered},
		{"a worker that shares one manager", probe("active", "node-w1", "node-m2", "node-x"), list, verdictOffered},
		{"a worker removed with node rm --force still reports the managers (S13 row 5)", probe("active", "node-gone", "node-m1", "node-m2"), list, verdictNodeNotListed},
		{"a node of another swarm: disjoint managers, id absent (S13 row 6)", probe("active", "node-other", "node-o1"), list, verdictManagersMismatch},
		{"a node id listed by luck but managers disjoint", probe("active", "node-w1", "node-o1"), list, verdictManagersMismatch},
		{"a node reporting no managers at all", probe("active", "node-w1"), list, verdictManagersMismatch},
		{"a reported manager that is only a worker in the list does not count", probe("active", "node-w1", "node-w1"), list, verdictManagersMismatch},
		{"swarm state inactive (S13 row 7)", probe("inactive", "node-w1", "node-m1"), list, verdictNotInSwarm},
		{"swarm state pending", probe("pending", "node-w1", "node-m1"), list, verdictNotInSwarm},
		{"swarm state error", probe("error", "node-w1", "node-m1"), list, verdictNotInSwarm},
		{"swarm state empty", probe("", "node-w1", "node-m1"), list, verdictNotInSwarm},
		{"active but no node id", probe("active", "", "node-m1"), list, verdictNotInSwarm},
		{"a nil node list", probe("active", "node-w1", "node-m1"), nil, verdictNoNodeList},
		{"an empty node list", probe("active", "node-w1", "node-m1"), []dockerx.NodeRow{}, verdictNoNodeList},
		{"not in a swarm outranks a missing list", probe("inactive", "", "node-m1"), nil, verdictNotInSwarm},
		{"promotion keeps the node id: now a manager row (S13 row 3)", probe("active", "node-w1", "node-m1"), []dockerx.NodeRow{row("node-m1", roleManager), row("node-w1", roleManager)}, verdictOffered},
		{"demotion keeps the node id: now a worker row (S13 row 4)", probe("active", "node-m2", "node-m1", "node-m2"), []dockerx.NodeRow{row("node-m1", roleManager), row("node-m2", roleWorker)}, verdictOffered},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := proveNode(c.p, c.nodes); got != c.want {
				t.Errorf("verdict %v, want %v", got, c.want)
			}
		})
	}
}
