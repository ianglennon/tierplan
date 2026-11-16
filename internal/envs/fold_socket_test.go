// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// A TierPlan with no Local environment folds the host behind its own socket into the swarm
// environment it belongs to (D-04), reading the socket only when an Admin asks (D-15).

// newSocketFoldRig is newWorkerFoldRig without AddLocal: the service's DOCKER_HOST is the worker
// fake's socket and nothing else knows it. The rig's localEnv is the zero value.
func newSocketFoldRig(t *testing.T, more ...func(*dockertest.FakeConfig)) *foldRig {
	t.Helper()
	return buildSocketFoldRig(t, func(nodes []dockertest.FakeNode) {
		nodes[1].Role = "worker"
		nodes[1].Leader = false
	}, func(c *dockertest.FakeConfig) {
		c.SwarmControl = false
		c.SwarmClusterID = ""
		c.SwarmRemoteManagers = []string{"node-m1"}
		for _, m := range more {
			m(c)
		}
	})
}

// newSocketManagerFoldRig is the same for a manager of the swarm: it reports the cluster ID the
// swarm environment is bound to and lists itself among its managers.
func newSocketManagerFoldRig(t *testing.T, more ...func(*dockertest.FakeConfig)) *foldRig {
	t.Helper()
	return buildSocketFoldRig(t, nil, func(c *dockertest.FakeConfig) {
		for _, m := range more {
			m(c)
		}
	})
}

// buildSocketFoldRig is buildFoldRig with no Local environment added.
func buildSocketFoldRig(t *testing.T, rows func([]dockertest.FakeNode), cfg func(*dockertest.FakeConfig)) *foldRig {
	t.Helper()
	nodes := []dockertest.FakeNode{
		{ID: "node-m1", Hostname: "mgr-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux"},
		{ID: "node-local", Hostname: "local-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux", Leader: true},
	}
	if rows != nil {
		rows(nodes)
	}
	lc := dockertest.FakeConfig{
		Hostname: "local-1", EngineID: "ENG:local", SwarmState: "active", SwarmControl: true,
		SwarmNodeID: "node-local", SwarmClusterID: foldCluster, NodeRows: nodes,
		SwarmRemoteManagers: []string{"node-m1", "node-local"},
		Containers:          []dockertest.FakeContainer{fakeContainer("local-c", "on-the-local-host")},
	}
	if cfg != nil {
		cfg(&lc)
	}
	local := dockertest.NewFake(t, lc)
	r := newTunRig(t, local)
	r.pastGrace()
	f := &foldRig{tunRig: r, local: local, nodes: nodes}
	f.env = r.srv.NewEnvironment(t, "Swarm")
	f.c = NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs, Grace: 0})
	r.svc.WithClusters(f.c)
	r.agg.WithNodeTimeout(300 * time.Millisecond)
	mc := managerConfig(1, nodes, nil)
	mc.Containers = []dockertest.FakeContainer{fakeContainer("m1-c", "on-mgr-1")}
	f.m = r.connect(t, f.env, mc, managerFacts(1))
	r.whenOnline(t, f.env.ID)
	if err := f.c.Refresh(context.Background(), f.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return f
}

// localInfoRequests is how many times the TierPlan host's own engine was asked /info, which is
// what any probe of its socket does.
func (f *foldRig) localInfoRequests() int {
	n := 0
	for _, p := range f.local.Requests() {
		if strings.HasSuffix(p, "/info") {
			n++
		}
	}
	return n
}

// localEnvironments is how many Local environment rows exist.
func (f *foldRig) localEnvironments(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM environments WHERE kind = 'local'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTracerASocketOnlyWorkerFoldsWithNoLocalEnvironment(t *testing.T) {
	f := newSocketFoldRig(t)
	ctx := context.Background()
	if n := f.localEnvironments(t); n != 0 {
		t.Fatalf("%d Local environments before the fold, want none", n)
	}

	// D-15: nothing that reads the environments looks at the socket, so the list says no offer.
	before := f.localInfoRequests()
	if _, err := f.svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if f.offered(t, f.env.ID) {
		t.Fatal("foldOffered is true on a socket-only install: the socket must not be probed for it")
	}
	if got := f.localInfoRequests(); got != before {
		t.Fatalf("the socket was asked /info %d times by List and Get, want none", got-before)
	}

	f.fold(t)

	if n := f.localEnvironments(t); n != 0 {
		t.Fatalf("%d Local environments after the fold, want none", n)
	}
	var node, path string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(local_node_id, ''), COALESCE(local_socket_path, '') FROM environments WHERE id = ?`, f.env.ID).Scan(&node, &path); err != nil {
		t.Fatal(err)
	}
	if node != "node-local" || path != f.local.SocketPath() {
		t.Errorf("local_node_id %q local_socket_path %q, want node-local and %q", node, path, f.local.SocketPath())
	}
	if st := f.mon.Status(SocketTarget(f.env.ID)); st.State != StateOnline {
		t.Errorf("socket target = %v after the fold, want online", st.State)
	}
	agg, _, err := f.agg.Snapshot(ctx, f.env.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := containerNodes(agg.Containers); !slices.Contains(got, "local-1/on-the-local-host") {
		t.Errorf("containers = %v, want the worker's, read through the socket", got)
	}
	rows := f.audits(t, "environment.local_folded")
	if len(rows) != 1 || rows[0].EnvID != f.env.ID || rows[0].TargetID != f.env.ID {
		t.Fatalf("audit = %+v, want one row for the swarm environment", rows)
	}
	for _, want := range []string{`"source":"socket"`, `"role":"worker"`, `"grants":"0"`, `"local":""`, `"local_environment":""`, "node-local", f.local.SocketPath()} {
		if !strings.Contains(rows[0].Details, want) {
			t.Errorf("audit details %q lack %q", rows[0].Details, want)
		}
	}
	if err := f.svc.FoldLocal(ctx, admin, f.env.ID, "", ""); !errors.Is(err, ErrFoldNotOffered) {
		t.Errorf("a second fold = %v, want ErrFoldNotOffered", err)
	}
	if n := len(f.audits(t, "environment.local_folded")); n != 1 {
		t.Errorf("%d audit rows after a second fold, want 1", n)
	}
}
