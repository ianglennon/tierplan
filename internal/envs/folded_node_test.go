// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// What a folded node reads as after the fold, whatever becomes of it (SWM-02, D-08, D-09): a
// node that left the swarm or vanished from its node list makes the environment degraded with a
// reason and stays folded, and its role follows its socket's probe and never defaults to manager.

// foldedIssue returns the folded_node_gone issue of the environment's health, if it has one.
func foldedIssue(h Health) (Issue, bool) {
	for _, is := range h.Issues {
		if is.Code == IssueFoldedNodeGone {
			return is, true
		}
	}
	return Issue{}, false
}

// awaitSocket waits, with no sleep, until the folded socket's status satisfies cond.
func (f *foldRig) awaitSocket(t *testing.T, what string, cond func(Status) bool) Status {
	t.Helper()
	st, err := f.mon.AwaitStatus(testsupport.Context(t), SocketTarget(f.env.ID), cond)
	if err != nil {
		t.Fatalf("timed out waiting for %s: last status %+v: %v", what, st, err)
	}
	return st
}

// foldedLocalNode is the node ID the rig's folded host has, read from the swarm row.
func (f *foldRig) foldedLocalNode(t *testing.T) string {
	t.Helper()
	var node string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(local_node_id, '') FROM environments WHERE id = ?`, f.env.ID).Scan(&node); err != nil {
		t.Fatal(err)
	}
	return node
}

func TestTracerAFoldedNodeThatLeftTheSwarmIsDegradedAndStaysFolded(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)
	if e := f.health(t, f.env.ID); e.Health.State != StateOnline {
		t.Fatalf("health after the fold = %+v, want online", e.Health)
	}

	// The host leaves the swarm: its engine reports no swarm, no node ID and no control.
	f.local.Reconfigure(dockertest.FakeConfig{
		Hostname: "local-1", EngineID: "ENG:local", SwarmState: "inactive",
		Containers: []dockertest.FakeContainer{fakeContainer("local-c", "on-the-local-host")},
	})
	f.mon.Kick(SocketTarget(f.env.ID))
	f.awaitSocket(t, "the socket to probe as standalone", func(s Status) bool { return s.Probe.Type == dockerx.TypeStandalone })

	e := f.health(t, f.env.ID)
	if e.Health.State != StateDegraded || e.Health.Reason != IssueFoldedNodeGone {
		t.Fatalf("health = %+v, want degraded with reason %s", e.Health, IssueFoldedNodeGone)
	}
	is, ok := foldedIssue(e.Health)
	if !ok || is.Node != "local-1" {
		t.Fatalf("issues = %+v, want folded_node_gone naming local-1", e.Health.Issues)
	}
	if got := f.foldedLocalNode(t); got != "node-local" {
		t.Errorf("local_node_id = %q after the node left, want node-local (nothing is unfolded)", got)
	}
}

func TestAFoldedNodeAbsentFromTheNodeListIsDegraded(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)
	ctx := context.Background()

	// The swarm's node list no longer has the host, whichever engine answers the read.
	rest := []dockertest.FakeNode{f.nodes[0]}
	lc := dockertest.FakeConfig{
		Hostname: "local-1", EngineID: "ENG:local", SwarmState: "active", SwarmControl: true,
		SwarmNodeID: "node-local", SwarmClusterID: foldCluster, NodeRows: rest,
		SwarmRemoteManagers: []string{"node-m1", "node-local"},
		Containers:          []dockertest.FakeContainer{fakeContainer("local-c", "on-the-local-host")},
	}
	f.local.Reconfigure(lc)
	mc := managerConfig(1, rest, nil)
	mc.Containers = []dockertest.FakeContainer{fakeContainer("m1-c", "on-mgr-1")}
	f.m.fake.Reconfigure(mc)
	if err := f.c.Refresh(ctx, f.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	e := f.health(t, f.env.ID)
	is, ok := foldedIssue(e.Health)
	if e.Health.State != StateDegraded || !ok || is.Node != "local-1" {
		t.Fatalf("health = %+v, want degraded with folded_node_gone naming local-1", e.Health)
	}
	if got := f.foldedLocalNode(t); got != "node-local" {
		t.Errorf("local_node_id = %q, want node-local still", got)
	}
}

func TestNoFoldedNodeGoneIsRaisedBeforeTheFirstReadOrWhileTheSocketIsUnreachable(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	socket := AgentView{
		ID: "socket:e1", Node: "local-1", Role: roleManager, SwarmNodeID: "node-local", Socket: true,
		Connected: true, ConnectedAt: now, LastSeen: now, EngineReachable: true,
	}
	in := func(target Status, c *ClusterView) RollupInput {
		s := socket
		s.Target = target
		return RollupInput{
			EnvID: "e1", Kind: KindAgent, EnvType: envTypeSwarm, Now: now, ServerStart: now.Add(-time.Hour),
			OfflineAfter: time.Minute, Agents: []AgentView{s}, Expected: 1, Cluster: c,
		}
	}
	online := Status{State: StateOnline, Since: now, Probe: dockerx.ProbeResult{Type: dockerx.TypeSwarmManager, SwarmNodeID: "node-local"}}
	gone := &ClusterView{Known: true, FoldedNodeGone: &NodeSince{Node: "local-1", Since: now}}

	// The cluster has not been read: its view says nothing, and the socket still is the node.
	if _, ok := foldedIssue(Rollup(in(online, &ClusterView{}))); ok {
		t.Error("folded_node_gone raised before the first cluster read")
	}
	// A view that names the node as gone raises it.
	if _, ok := foldedIssue(Rollup(in(online, gone))); !ok {
		t.Error("a ClusterView with FoldedNodeGone raised nothing")
	}
	// An engine that does not answer is reported as that, and no probe says nothing about the node.
	unreachable := Status{State: StateOffline, Since: now, Reason: "engine_unreachable", Probe: dockerx.ProbeResult{Type: dockerx.TypeSwarmManager}}
	if _, ok := foldedIssue(Rollup(in(unreachable, &ClusterView{}))); ok {
		t.Error("folded_node_gone raised while the socket's engine is unreachable")
	}
	if _, ok := foldedIssue(Rollup(in(Status{State: StateUnknown}, &ClusterView{}))); ok {
		t.Error("folded_node_gone raised before the socket's first probe")
	}
}

func TestFoldedNodeGoneIsDecidedOnlyFromAKnownListAndAnAnsweringSocket(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	listed := ClusterState{FetchedAt: now, Read: dockerx.ClusterRead{Nodes: []dockerx.NodeRow{{ID: "node-m1"}, {ID: "node-local"}}}}
	missing := ClusterState{FetchedAt: now, Read: dockerx.ClusterRead{Nodes: []dockerx.NodeRow{{ID: "node-m1"}}}}
	in := func(state State) RollupInput {
		return RollupInput{Agents: []AgentView{
			{ID: "agent:a1", Node: "mgr-1", SwarmNodeID: "node-m1"},
			{ID: "socket:e1", Node: "local-1", SwarmNodeID: "node-local", Socket: true, Target: Status{State: state}},
		}}
	}
	if g := foldedNodeGone(in(StateOnline), listed); g != nil {
		t.Errorf("a listed node is gone: %+v", g)
	}
	if g := foldedNodeGone(in(StateOnline), missing); g == nil || g.Node != "local-1" || !g.Since.Equal(now) {
		t.Errorf("an unlisted node = %+v, want local-1 since the read", g)
	}
	if g := foldedNodeGone(in(StateOffline), missing); g != nil {
		t.Errorf("an unreachable socket's node is gone: %+v", g)
	}
	if g := foldedNodeGone(in(StateUnknown), missing); g != nil {
		t.Errorf("an unprobed socket's node is gone: %+v", g)
	}
	if g := foldedNodeGone(in(StateOnline), ClusterState{}); g != nil {
		t.Errorf("a node is gone before any list is read: %+v", g)
	}
	// An environment with no folded node has no socket view.
	if g := foldedNodeGone(RollupInput{Agents: []AgentView{{ID: "agent:a1", SwarmNodeID: "node-m1"}}}, missing); g != nil {
		t.Errorf("an environment with no folded node reports one gone: %+v", g)
	}
}

// foldedRole is the role the folded node has now.
func (f *foldRig) foldedRole(t *testing.T) string {
	t.Helper()
	n, ok := f.svc.FoldedNode(context.Background(), f.env.ID)
	if !ok {
		t.Fatal("the environment has no folded node")
	}
	return n.Role
}

// localConfig is the rig's folded host as a daemon that reports the role and swarm it is given.
func localConfig(control bool, rows []dockertest.FakeNode) dockertest.FakeConfig {
	c := dockertest.FakeConfig{
		Hostname: "local-1", EngineID: "ENG:local", SwarmState: "active", SwarmControl: control,
		SwarmNodeID: "node-local", NodeRows: rows, SwarmRemoteManagers: []string{"node-m1"},
		Containers: []dockertest.FakeContainer{fakeContainer("local-c", "on-the-local-host")},
	}
	if control {
		c.SwarmClusterID = foldCluster
	}
	return c
}

func TestSocketViewRoleIsNeverManagerBeforeItsFirstProbe(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// listed is the role the swarm's node list gives the folded node; none drops its row.
		listed string
		// noClusters runs the service with no cluster reader at all.
		noClusters bool
		want       string
	}{
		{name: "the list calls it a manager", listed: roleManager, want: roleManager},
		{name: "the list calls it a worker", listed: roleWorker, want: roleWorker},
		{name: "the list does not have it", listed: "", want: roleWorker},
		{name: "there is no cluster reader", listed: roleManager, noClusters: true, want: roleWorker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFoldRig(t, nil)
			f.fold(t)
			// The swarm's managers list the node as the case says.
			rows := []dockertest.FakeNode{f.nodes[0]}
			if tc.listed != "" {
				n := f.nodes[1]
				n.Role = tc.listed
				rows = append(rows, n)
			}
			f.local.Reconfigure(localConfig(true, rows))
			mc := managerConfig(1, rows, nil)
			f.m.fake.Reconfigure(mc)
			if err := f.c.Refresh(ctx, f.env.ID); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if got := f.foldedRole(t); got != roleManager {
				t.Fatalf("role of a probed manager socket = %q, want manager", got)
			}
			// No probe: the monitor of the socket is gone, as before its first result.
			f.mon.Stop(SocketTarget(f.env.ID))
			if st := f.mon.Status(SocketTarget(f.env.ID)); st.Probe.Type != "" {
				t.Fatalf("the socket still has a probe: %+v", st)
			}
			if tc.noClusters {
				f.svc.WithClusters(nil)
			}
			if got := f.foldedRole(t); got != tc.want {
				t.Errorf("role before the first probe = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSocketViewRoleFollowsTheProbeAfterPromotionAndDemotion(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)
	key := SocketTarget(f.env.ID)
	probed := func(typ string) {
		t.Helper()
		f.mon.Kick(key)
		f.awaitSocket(t, "the socket to probe as "+typ, func(s Status) bool { return s.Probe.Type == typ })
	}
	if got := f.foldedRole(t); got != roleManager {
		t.Fatalf("role after the fold = %q, want manager", got)
	}

	// Demoted: still in the swarm, no longer in control of it.
	f.local.Reconfigure(localConfig(false, f.nodes))
	probed(dockerx.TypeSwarmWorker)
	if got := f.foldedRole(t); got != roleWorker {
		t.Errorf("role after a demotion = %q, want worker", got)
	}

	// Promoted again.
	f.local.Reconfigure(localConfig(true, f.nodes))
	probed(dockerx.TypeSwarmManager)
	if got := f.foldedRole(t); got != roleManager {
		t.Errorf("role after a promotion = %q, want manager", got)
	}

	// Outside the swarm altogether is a worker's role too, never a manager's.
	f.local.Reconfigure(dockertest.FakeConfig{Hostname: "local-1", EngineID: "ENG:local", SwarmState: "inactive"})
	probed(dockerx.TypeStandalone)
	if got := f.foldedRole(t); got != roleWorker {
		t.Errorf("role of a standalone engine = %q, want worker", got)
	}
	if got := f.foldedLocalNode(t); got != "node-local" {
		t.Errorf("local_node_id = %q, want node-local: no re-fold happened and none is needed", got)
	}
}
