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
