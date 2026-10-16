// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Folding the local host into the swarm environment it manages (ENV-02, D-48, T-02-96,
// T-02-97). The local host is a real monitored local environment on a fake daemon, the
// swarm is enrolled through real agent sessions, and both are the production code.

const foldCluster = "cluster-1"

// foldRig is a Bayplan whose own host (local-1, node-local) manages the same swarm as
// the agent environment it is enrolled in (manager mgr-1 on another node).
type foldRig struct {
	*tunRig
	local    *dockertest.Fake
	localEnv Environment
	env      tunneltest.Environment
	c        *Clusters
	m        *agentConn
	nodes    []dockertest.FakeNode
}

// newFoldRig builds it. cfg may change the local daemon before it starts.
func newFoldRig(t *testing.T, cfg func(*dockertest.FakeConfig)) *foldRig {
	t.Helper()
	nodes := []dockertest.FakeNode{
		{ID: "node-m1", Hostname: "mgr-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux"},
		{ID: "node-local", Hostname: "local-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux", Leader: true},
	}
	lc := dockertest.FakeConfig{
		Hostname: "local-1", EngineID: "ENG:local", SwarmState: "active", SwarmControl: true,
		SwarmNodeID: "node-local", SwarmClusterID: foldCluster, NodeRows: nodes,
		Containers: []dockertest.FakeContainer{fakeContainer("local-c", "on-the-local-host")},
	}
	if cfg != nil {
		cfg(&lc)
	}
	local := dockertest.NewFake(t, lc)
	r := newTunRig(t, local)
	r.pastGrace()
	f := &foldRig{tunRig: r, local: local, nodes: nodes}
	le, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatalf("AddLocal: %v", err)
	}
	r.mon.AwaitProbe(context.Background(), le.ID)
	f.localEnv = le
	f.env = r.srv.NewEnvironment(t, "Swarm")
	f.c = NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs, Grace: 0})
	r.svc.WithClusters(f.c)
	r.agg.WithNodeTimeout(300 * time.Millisecond)
	mc := managerConfig(1, nodes, nil)
	mc.Containers = []dockertest.FakeContainer{fakeContainer("m1-c", "on-mgr-1")}
	f.m = r.connect(t, f.env, mc, managerFacts(1))
	r.whenOnline(t, f.env.ID)
	return f
}

// offered reads the offer as the API does, from the environment itself.
func (f *foldRig) offered(t *testing.T, envID string) bool {
	t.Helper()
	e, err := f.svc.Get(context.Background(), envID)
	if err != nil {
		t.Fatalf("Get %s: %v", envID, err)
	}
	return e.FoldOffered
}

func (f *foldRig) fold(t *testing.T) {
	t.Helper()
	if err := f.svc.FoldLocal(context.Background(), admin, f.env.ID, "10.0.0.9", "test"); err != nil {
		t.Fatalf("FoldLocal: %v", err)
	}
	// FoldLocal starts the folded host's socket target; a read through it needs its first probe.
	awaitProbed(t, f.mon, SocketTarget(f.env.ID))
	if err := f.c.Refresh(context.Background(), f.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
}

func TestFoldIsOfferedToBothEnvironmentsWhenTheLocalHostManagesTheClusterAnAgentEnvironmentIsBoundTo(t *testing.T) {
	f := newFoldRig(t, nil)
	offered, localID, swarmID := f.svc.FoldOffer(context.Background(), f.localEnv.ID)
	if !offered || localID != f.localEnv.ID || swarmID != f.env.ID {
		t.Fatalf("FoldOffer(local) = %v %q %q, want true, the local and the swarm environment", offered, localID, swarmID)
	}
	offered, localID, swarmID = f.svc.FoldOffer(context.Background(), f.env.ID)
	if !offered || localID != f.localEnv.ID || swarmID != f.env.ID {
		t.Fatalf("FoldOffer(swarm) = %v %q %q, want the same pair from the swarm's side", offered, localID, swarmID)
	}
	if !f.offered(t, f.localEnv.ID) || !f.offered(t, f.env.ID) {
		t.Fatal("foldOffered is not true on both environments")
	}
}

func TestFoldIsNotOfferedWithADifferentClusterAWorkerOrAnEngineThatDoesNotAnswer(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*dockertest.FakeConfig)
	}{
		{"another cluster", func(c *dockertest.FakeConfig) { c.SwarmClusterID = "cluster-elsewhere" }},
		{"a worker", func(c *dockertest.FakeConfig) { c.SwarmControl = false }},
		{"no swarm at all", func(c *dockertest.FakeConfig) {
			c.SwarmState = "inactive"
			c.SwarmControl = false
			c.SwarmClusterID = ""
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFoldRig(t, c.cfg)
			if offered, _, _ := f.svc.FoldOffer(context.Background(), f.localEnv.ID); offered {
				t.Error("FoldOffer(local) offered a fold")
			}
			if f.offered(t, f.localEnv.ID) || f.offered(t, f.env.ID) {
				t.Error("an environment reports foldOffered")
			}
			err := f.svc.FoldLocal(context.Background(), admin, f.env.ID, "", "")
			if !errors.Is(err, ErrFoldNotOffered) {
				t.Errorf("FoldLocal = %v, want ErrFoldNotOffered", err)
			}
			if _, err := f.svc.Get(context.Background(), f.localEnv.ID); err != nil {
				t.Errorf("the local environment was touched by a refused fold: %v", err)
			}
			if n := len(f.audits(t, "environment.local_folded")); n != 0 {
				t.Errorf("%d audit rows for a refused fold", n)
			}
		})
	}
}

func (f *foldRig) audits(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, actor_id, target_id, COALESCE(env_id, ''), result, COALESCE(details, '') FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.Action, &a.ActorID, &a.TargetID, &a.EnvID, &a.Result, &a.Details); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func TestFoldLocalRetiresTheLocalEnvironmentRecordsTheNodeAndTheSocketAndAuditsItOnce(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)

	if _, err := f.svc.Get(context.Background(), f.localEnv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the local environment after a fold: %v, want ErrNotFound", err)
	}
	var node, path string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(local_node_id, ''), COALESCE(local_socket_path, '') FROM environments WHERE id = ?`, f.env.ID).Scan(&node, &path); err != nil {
		t.Fatal(err)
	}
	if node != "node-local" || path != f.local.SocketPath() {
		t.Errorf("local_node_id %q local_socket_path %q, want node-local and %q", node, path, f.local.SocketPath())
	}
	rows := f.audits(t, "environment.local_folded")
	if len(rows) != 1 || rows[0].EnvID != f.env.ID || rows[0].TargetID != f.env.ID || rows[0].ActorID != admin.ID || rows[0].Result != "success" {
		t.Fatalf("audit = %+v, want one success row for the swarm environment by the admin", rows)
	}
	for _, want := range []string{"node-local", f.local.SocketPath()} {
		if !strings.Contains(rows[0].Details, want) {
			t.Errorf("audit details %q lack %q", rows[0].Details, want)
		}
	}
	if st := f.mon.Status(f.localEnv.ID); st.State != StateUnknown {
		t.Errorf("the retired local monitor still reports %v", st.State)
	}
	if f.offered(t, f.env.ID) {
		t.Error("a folded environment still offers a fold")
	}
	if err := f.svc.FoldLocal(context.Background(), admin, f.env.ID, "", ""); !errors.Is(err, ErrFoldNotOffered) {
		t.Errorf("a second fold = %v, want ErrFoldNotOffered", err)
	}
	if n := len(f.audits(t, "environment.local_folded")); n != 1 {
		t.Errorf("%d audit rows after a second fold, want still 1", n)
	}
}

func TestFoldLocalDeletesTheRetiredLocalEnvironmentsGrantsAndOwnedRowsAndKeepsTheSwarmsOwn(t *testing.T) {
	f := newFoldRig(t, nil)
	seedGrantsOn(t, f.st, f.localEnv.ID)
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-swarm', 'user', 'u1', 'admin', ?, 1)`, f.env.ID); err != nil {
		t.Fatal(err)
	}
	f.fold(t)

	if a, x, o := grantRowsOn(t, f.st, f.localEnv.ID); a != 0 || x != 0 || o != 0 {
		t.Fatalf("left behind for the retired local environment: %d assignments, %d extras, %d owned rows", a, x, o)
	}
	if a, _, _ := grantRowsOn(t, f.st, f.env.ID); a != 1 {
		t.Fatalf("the swarm environment's own assignments = %d, want 1 kept", a)
	}
	rows := f.audits(t, "environment.local_folded")
	if len(rows) != 1 || !strings.Contains(rows[0].Details, `"grants":"2"`) {
		t.Fatalf("audit = %+v, want one entry carrying grants 2", rows)
	}
}

func TestFoldLocalOfAnUnknownEnvironmentIsNotFound(t *testing.T) {
	f := newFoldRig(t, nil)
	if err := f.svc.FoldLocal(context.Background(), admin, "no-such-environment", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FoldLocal = %v, want ErrNotFound", err)
	}
}

func TestAfterTheFoldTheSocketIsATargetACoveredNodeAndAManagerThatServesClusterReads(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)
	ctx := context.Background()

	in, err := f.svc.Input(ctx, f.env.ID, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	targets := Targets(in)
	if !slices.Contains(targets, SocketTarget(f.env.ID)) || !slices.Contains(targets, AgentTarget(f.m.id)) || len(targets) != 2 {
		t.Fatalf("targets = %v, want the socket and the one agent", targets)
	}
	// The cluster counts two nodes that need coverage and has both: no agent is
	// expected on the local node, which the socket covers.
	e := f.health(t, f.env.ID)
	if e.Health.State != StateOnline {
		t.Fatalf("health = %+v, want online with the local node covered by its socket", e.Health)
	}
	if o := e.Overview; o.Agents == nil || o.Agents.Expected != 2 || o.Agents.Connected != 2 || o.Nodes == nil || o.Nodes.Total != 2 || o.Nodes.Ready != 2 {
		t.Errorf("overview = %+v / %+v, want 2 of 2 nodes and agents", o.Nodes, o.Agents)
	}

	// With every agent gone the socket still answers cluster reads, as a manager.
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
	agg, _, err := f.agg.Snapshot(ctx, f.env.ID)
	if err != nil {
		t.Fatalf("Snapshot through the socket alone: %v", err)
	}
	if got := containerNodes(agg.Containers); !slices.Equal(got, []string{"local-1/on-the-local-host"}) {
		t.Errorf("containers = %v, want the local host's, read through the socket", got)
	}
}

func TestAFoldedNodeReachedThroughTheSocketAndThroughAnAgentCountsOnceWithTheSocketWinning(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)
	ctx := context.Background()

	// An agent that runs on the folded node itself, as the global agent service would.
	dupFacts := &wire.Facts{
		EngineID: "ENG:local-agent", Hostname: "local-1-agent", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-local", ClusterID: foldCluster, Control: true},
	}
	dup := f.connect(t, f.env, dockertest.FakeConfig{
		Hostname: "local-1-agent", Containers: []dockertest.FakeContainer{fakeContainer("dup-c", "seen-through-the-agent")},
	}, dupFacts)
	eventually(t, "the second agent to be connected", func() bool { return connectedNow(f.srv.Hub, f.env.ID, dup.id) })

	in, err := f.svc.Input(ctx, f.env.ID, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if targets := Targets(in); len(targets) != 2 || slices.Contains(targets, AgentTarget(dup.id)) {
		t.Fatalf("targets = %v, want the socket and mgr-1 only", targets)
	}
	agg, _, err := f.agg.Snapshot(ctx, f.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := containerNodes(agg.Containers); !slices.Equal(got, []string{"mgr-1/on-mgr-1", "local-1/on-the-local-host"}) && !slices.Equal(got, []string{"local-1/on-the-local-host", "mgr-1/on-mgr-1"}) {
		t.Errorf("containers = %v, want one row set per node, none through the duplicate agent", got)
	}
	if len(agg.Nodes) != 2 || len(agg.ByNode) != 2 || len(agg.NotReporting) != 0 {
		t.Errorf("nodes %+v byNode %+v notReporting %v, want two nodes, the folded one once", agg.Nodes, agg.ByNode, agg.NotReporting)
	}
	if agg.Counts.Containers != 2 {
		t.Errorf("containers counted %d, want 2", agg.Counts.Containers)
	}
	e := f.health(t, f.env.ID)
	if o := e.Overview; o.Agents == nil || o.Agents.Expected != 2 || o.Agents.Connected != 2 {
		t.Errorf("agents = %+v, want 2 of 2: the duplicate agent is not a third", o.Agents)
	}
	// The merged feed attaches one target per node.
	feed := &mergedFeed{}
	if got := feed.targets(in); len(got) != 2 {
		t.Errorf("event targets = %+v, want one per node", got)
	}
}

func TestTheSocketTargetOfAFoldedEnvironmentStartsWithTheServerAndStopsWhenTheEnvironmentIsRemoved(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)
	ctx := context.Background()
	key := SocketTarget(f.env.ID)
	awaitProbed(t, f.mon, key)
	if st := f.mon.Status(key); st.State != StateOnline {
		t.Fatalf("socket target = %v after the fold, want online", st.State)
	}

	// A restart: every monitor is gone and StartAll brings the socket back.
	f.mon.StopAll()
	if st := f.mon.Status(key); st.State != StateUnknown {
		t.Fatalf("after StopAll the socket target is %v", st.State)
	}
	if err := f.svc.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	awaitProbed(t, f.mon, key)
	if st := f.mon.Status(key); st.State != StateOnline {
		t.Fatalf("socket target = %v after StartAll, want online", st.State)
	}

	if err := f.svc.Remove(ctx, admin, f.env.ID, "", "", false); err != nil {
		t.Fatal(err)
	}
	if st := f.mon.Status(key); st.State != StateUnknown {
		t.Errorf("the socket target of a removed environment is %v, want stopped", st.State)
	}
}

func TestAFoldedHostCountsAsManagedButCanStillBeAddedAgainAsItsOwnEnvironment(t *testing.T) {
	f := newFoldRig(t, nil)
	ctx := context.Background()
	if has, err := f.svc.HasLocal(ctx); err != nil || !has {
		t.Fatalf("HasLocal before the fold = %v, %v, want true: the local host is an environment", has, err)
	}
	f.fold(t)
	if has, err := f.svc.HasLocal(ctx); err != nil || !has {
		t.Fatalf("HasLocal after the fold = %v, %v, want true: the swarm environment manages the host, so nothing offers to add it", has, err)
	}
	// Undoing a fold is adding the local host again (the Phase 1 opt-in).
	if _, err := f.svc.AddLocal(ctx, admin, "", false, "", ""); err != nil {
		t.Fatalf("AddLocal after a fold: %v, want the local host to be addable again", err)
	}
}
