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

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
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

// seedFoldRemovals gives the Local environment what a fold removes: two stacks, one Git
// repository, no registry login and janitor settings.
func (f *foldRig) seedFoldRemovals(t *testing.T) {
	t.Helper()
	for _, n := range []string{"a", "b"} {
		f.mustExec(t, `INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, ?, 'swarm', 'u1', 1)`, "stack-"+n, f.localEnv.ID, n)
	}
	f.mustExec(t, `INSERT INTO git_repos (id, env_id, name, url, created_at, updated_at, created_by) VALUES ('repo-1', ?, 'r', 'https://example.org/r.git', 1, 1, 'u1')`, f.localEnv.ID)
	f.mustExec(t, `INSERT INTO janitor_env_policy (env_id) VALUES (?)`, f.localEnv.ID)
}

func TestFoldOfferDetail(t *testing.T) {
	const domain = `INSERT INTO ingress_domains (id, env_id, suffix, created_by, created_at, updated_at) VALUES ('dom-1', ?, 'example.org', 'u1', 1, 1)`
	elsewhere := func(c *dockertest.FakeConfig) { c.SwarmClusterID = "cluster-elsewhere" }

	// noProbe rows run on a rig whose own host socket nobody watches, so that a probe of it shows
	// as an /info request.
	type rig struct {
		askID    func(f *foldRig) string
		noProbe  bool
		seed     func(t *testing.T, f *foldRig)
		wantErr  error
		want     FoldOfferDetail
		wantRem  *FoldRemovals
		wantIngr *IngressActiveError
	}
	swarm := func(f *foldRig) string { return f.env.ID }
	cases := []struct {
		name  string
		build func(t *testing.T) *foldRig
		rig
	}{
		{
			name:  "a Local-environment manager with things to lose",
			build: func(t *testing.T) *foldRig { return newFoldRig(t, nil) },
			rig: rig{
				askID:   swarm,
				seed:    func(t *testing.T, f *foldRig) { f.seedFoldRemovals(t) },
				want:    FoldOfferDetail{Offered: true, Source: foldSourceLocal, Role: roleManager},
				wantRem: &FoldRemovals{Stacks: 2, GitRepos: 1, Janitor: true},
			},
		},
		{
			name:  "a Local-environment manager holding ingress state",
			build: func(t *testing.T) *foldRig { return newFoldRig(t, nil) },
			rig: rig{
				askID: swarm,
				seed: func(t *testing.T, f *foldRig) {
					f.seedFoldRemovals(t)
					f.mustExec(t, domain, f.localEnv.ID)
				},
				want:     FoldOfferDetail{Source: foldSourceLocal, Role: roleManager},
				wantRem:  &FoldRemovals{Stacks: 2, GitRepos: 1, Janitor: true},
				wantIngr: &IngressActiveError{Domains: 1},
			},
		},
		{
			name:  "a socket-only worker the managers list",
			build: func(t *testing.T) *foldRig { return newSocketFoldRig(t) },
			rig:   rig{askID: swarm, want: FoldOfferDetail{Offered: true, Source: foldSourceSocket, Role: roleWorker}},
		},
		{
			name:  "a socket-only manager of this cluster that is listed and shares a manager",
			build: func(t *testing.T) *foldRig { return newSocketManagerFoldRig(t) },
			rig:   rig{askID: swarm, want: FoldOfferDetail{Offered: true, Source: foldSourceSocket, Role: roleManager}},
		},
		{
			name:  "a socket-only manager of another cluster",
			build: func(t *testing.T) *foldRig { return newSocketManagerFoldRig(t, elsewhere) },
			rig:   rig{askID: swarm},
		},
		{
			name: "a socket-only worker of this swarm that the list lacks",
			build: func(t *testing.T) *foldRig {
				return newSocketFoldRig(t, func(c *dockertest.FakeConfig) { c.SwarmNodeID = "node-removed" })
			},
			rig: rig{askID: swarm, wantErr: ErrFoldNodeNotListed},
		},
		{
			name: "a socket-only worker whose managers share none with the list",
			build: func(t *testing.T) *foldRig {
				return newSocketFoldRig(t, func(c *dockertest.FakeConfig) { c.SwarmRemoteManagers = []string{"node-elsewhere"} })
			},
			rig: rig{askID: swarm},
		},
		{
			name:  "an environment that is folded already",
			build: func(t *testing.T) *foldRig { f := newSocketFoldRig(t); f.fold(t); return f },
			rig:   rig{askID: swarm, noProbe: true},
		},
		{
			name:  "a standalone agent environment",
			build: func(t *testing.T) *foldRig { return newSocketFoldRig(t) },
			rig: rig{
				askID: swarm, noProbe: true,
				seed: func(t *testing.T, f *foldRig) {
					f.mustExec(t, `UPDATE environments SET env_type = 'standalone' WHERE id = ?`, f.env.ID)
				},
			},
		},
		{
			name:  "a Local row that is not the environment asked, with the socket left alone",
			build: func(t *testing.T) *foldRig { return newSocketFoldRig(t) },
			rig: rig{
				askID: swarm, noProbe: true,
				seed: func(t *testing.T, f *foldRig) {
					f.mustExec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('local-x', 'local', 'Local', '/nowhere.sock', 1, 1, 1)`)
				},
			},
		},
		{
			name:  "the Local environment itself",
			build: func(t *testing.T) *foldRig { return newSocketFoldRig(t) },
			rig: rig{
				askID: func(*foldRig) string { return "local-x" }, noProbe: true,
				seed: func(t *testing.T, f *foldRig) {
					f.mustExec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('local-x', 'local', 'Local', '/nowhere.sock', 1, 1, 1)`)
				},
			},
		},
		{
			name:  "an environment that does not exist",
			build: func(t *testing.T) *foldRig { return newSocketFoldRig(t) },
			rig:   rig{askID: func(*foldRig) string { return "no-such-environment" }, noProbe: true, wantErr: ErrNotFound},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.build(t)
			if c.seed != nil {
				c.seed(t, f)
			}
			id := c.askID(f)
			before := f.localInfoRequests()
			got, err := f.svc.FoldOfferDetail(context.Background(), id)
			if c.noProbe {
				if after := f.localInfoRequests(); after != before {
					t.Errorf("the socket was probed %d times, want none", after-before)
				}
			}
			if errors.Is(c.wantErr, ErrNotFound) {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("FoldOfferDetail = %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FoldOfferDetail: %v", err)
			}
			want := c.want
			if want.Source == foldSourceLocal {
				want.LocalEnvID = f.localEnv.ID
			}
			if got.Offered != want.Offered || got.Source != want.Source || got.Role != want.Role || got.LocalEnvID != want.LocalEnvID {
				t.Errorf("detail = %+v, want offered %v source %q role %q local %q", got, want.Offered, want.Source, want.Role, want.LocalEnvID)
			}
			switch {
			case c.wantErr != nil:
				if !errors.Is(got.Refusal, c.wantErr) {
					t.Errorf("Refusal = %v, want %v", got.Refusal, c.wantErr)
				}
			case c.wantIngr != nil:
				var ia *IngressActiveError
				if !errors.Is(got.Refusal, ErrFoldIngressActive) || !errors.As(got.Refusal, &ia) || *ia != *c.wantIngr || got.Ingress == nil || *got.Ingress != *c.wantIngr {
					t.Errorf("Refusal = %v, Ingress = %+v, want the ingress refusal %+v", got.Refusal, got.Ingress, c.wantIngr)
				}
			default:
				if got.Refusal != nil || got.Ingress != nil {
					t.Errorf("Refusal = %v, Ingress = %+v, want none", got.Refusal, got.Ingress)
				}
			}
			switch {
			case c.wantRem == nil:
				if got.Removes != nil {
					t.Errorf("Removes = %+v, want none", got.Removes)
				}
			case got.Removes == nil || *got.Removes != *c.wantRem:
				t.Errorf("Removes = %+v, want %+v", got.Removes, c.wantRem)
			}
		})
	}
}

func TestASocketOnlyManagerFoldsByClusterAndNodeID(t *testing.T) {
	f := newSocketManagerFoldRig(t)
	ctx := context.Background()
	if f.offered(t, f.env.ID) {
		t.Fatal("foldOffered is true on a socket-only install")
	}
	f.fold(t)

	if n := f.localEnvironments(t); n != 0 {
		t.Fatalf("%d Local environments after the fold, want none", n)
	}
	rows := f.audits(t, "environment.local_folded")
	if len(rows) != 1 {
		t.Fatalf("audit = %+v, want one row", rows)
	}
	for _, want := range []string{`"source":"socket"`, `"role":"manager"`, `"grants":"0"`, "node-local"} {
		if !strings.Contains(rows[0].Details, want) {
			t.Errorf("audit details %q lack %q", rows[0].Details, want)
		}
	}
	// With the manager agent gone the folded socket serves cluster reads, as a manager.
	f.kick(t, f.m)
	var host string
	if err := f.c.ClusterRead(ctx, f.env.ID, func(cli *dockerx.Client) error {
		res, err := cli.Probe(ctx)
		host = res.Hostname
		return err
	}); err != nil {
		t.Fatalf("ClusterRead with no agent connected: %v", err)
	}
	if host != "local-1" {
		t.Errorf("the read went to %q, want the folded host local-1", host)
	}

	// A socket-only manager of another swarm is neither offered nor folded.
	g := newSocketManagerFoldRig(t, func(c *dockertest.FakeConfig) { c.SwarmClusterID = "cluster-elsewhere" })
	if err := g.svc.FoldLocal(ctx, admin, g.env.ID, "", ""); !errors.Is(err, ErrFoldNotOffered) {
		t.Fatalf("FoldLocal of a manager of another cluster = %v, want ErrFoldNotOffered", err)
	}
	if n := len(g.audits(t, "environment.local_folded")); n != 0 {
		t.Errorf("%d audit rows for a refused fold", n)
	}
	var node string
	if err := g.st.Read.QueryRow(`SELECT COALESCE(local_node_id, '') FROM environments WHERE id = ?`, g.env.ID).Scan(&node); err != nil || node != "" {
		t.Errorf("local_node_id = %q, %v after a refused fold, want none", node, err)
	}
}

// A Local environment added between the probe and the write is seen inside the transaction, and
// the fold that lost changes nothing (D-04, T-19-19). The probe is held on the fake's /_ping so
// the Local row is added at exactly that point.
func TestASocketFoldLosesToALocalEnvironmentAddedInTheMeantime(t *testing.T) {
	hold := make(chan struct{})
	f := newSocketFoldRig(t, func(c *dockertest.FakeConfig) { c.ProbeHold = hold })
	ctx := testsupport.Context(t)
	done := make(chan error, 1)
	go func() { done <- f.svc.FoldLocal(ctx, admin, f.env.ID, "", "") }()
	if err := f.local.Await(ctx, func() bool {
		return slices.ContainsFunc(f.local.Requests(), func(p string) bool { return strings.HasSuffix(p, "/_ping") })
	}); err != nil {
		t.Fatalf("the fold never probed the socket: %v", err)
	}
	f.mustExec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('local-race', 'local', 'Local', '/x.sock', 1, 1, 1)`)
	close(hold)

	if err := testsupport.Await(t, "the fold's answer", done); !errors.Is(err, ErrFoldNotOffered) {
		t.Fatalf("FoldLocal = %v, want ErrFoldNotOffered", err)
	}
	var node string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(local_node_id, '') FROM environments WHERE id = ?`, f.env.ID).Scan(&node); err != nil || node != "" {
		t.Errorf("local_node_id = %q, %v after a lost race, want none", node, err)
	}
	if n := len(f.audits(t, "environment.local_folded")); n != 0 {
		t.Errorf("%d audit rows for a lost race", n)
	}
}
