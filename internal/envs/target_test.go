// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// A write is routed by node ID through the one write path, runs once, and always
// leaves the node's snapshot to be read fresh (plan 04-01, P2 D-42, T-02-73).

var errBoom = errors.New("boom")

func stopWeb() func(*dockerx.Client) error {
	return func(c *dockerx.Client) error {
		return c.ContainerAction(context.Background(), "web", dockerx.VerbStop, dockerx.ActionOptions{})
	}
}

func webContainers() []dockertest.FakeContainer {
	return []dockertest.FakeContainer{{ID: "web", Names: []string{"/web"}, Image: "nginx", State: "running", Status: "Up"}}
}

func TestWriteOnNodeRoutesTheLocalSocketByTheEnvironmentID(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host", Containers: webContainers()})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := r.agg.Snapshot(ctx, e.ID); err != nil { // warm the cache
		t.Fatal(err)
	}

	// The node ID of the local socket is the environment's own ID, and the rows say so.
	agg, _, _ := r.agg.Snapshot(ctx, e.ID)
	if agg.Containers[0].NodeID != e.ID {
		t.Fatalf("row NodeID = %q, want the environment ID %q", agg.Containers[0].NodeID, e.ID)
	}

	if err := r.agg.WriteOnNode(ctx, e.ID, "a-display-name", stopWeb()); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("a display name routed somewhere: err = %v", err)
	}
	if got := local.Writes(); len(got) != 0 {
		t.Fatalf("an unknown node received %v", got)
	}

	if err := r.agg.WriteOnNode(ctx, e.ID, e.ID, stopWeb()); err != nil {
		t.Fatalf("WriteOnNode(local): %v", err)
	}
	if got := local.Writes(); !slices.Equal(got, []string{"POST /containers/web/stop"}) {
		t.Fatalf("writes = %v", got)
	}
	after, _, err := r.agg.Snapshot(ctx, e.ID)
	if err != nil || after.Containers[0].State != "exited" {
		t.Fatalf("the list after the write = %+v, %v; want a fresh read showing exited", after.Containers, err)
	}
}

func TestWriteOnNodeRoutesAnAgentEnvironmentByAgentID(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Containers: webContainers()}, nil)
	r.whenOnline(t, env.ID)
	ctx := context.Background()

	agg, _, err := r.agg.Snapshot(ctx, env.ID)
	if err != nil || len(agg.Containers) != 1 || agg.Containers[0].NodeID != a.id {
		t.Fatalf("rows = %+v, %v; want NodeID to be the agent ID %q", agg.Containers, err, a.id)
	}
	if err := r.agg.WriteOnNode(ctx, env.ID, a.id, stopWeb()); err != nil {
		t.Fatalf("WriteOnNode(agent): %v", err)
	}
	if got := a.fake.Writes(); !slices.Equal(got, []string{"POST /containers/web/stop"}) {
		t.Fatalf("the agent's daemon saw %v", got)
	}
	// The host name is a display name only.
	if err := r.agg.WriteOnNode(ctx, env.ID, "edge-1", stopWeb()); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("routing by host name: err = %v, want ErrNodeUnavailable", err)
	}
	if err := r.agg.WriteOnNode(ctx, "no-such-environment", a.id, stopWeb()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown environment: err = %v, want ErrNotFound", err)
	}
}

func TestWriteOnNodeRefusesARevokedAndADisconnectedAgentWithoutTouchingDocker(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	ctx := context.Background()
	goneEnv := r.srv.NewEnvironment(t, "Gone")
	gone := r.connect(t, goneEnv, dockertest.FakeConfig{Hostname: "edge-1", Containers: webContainers()}, nil)
	liveEnv := r.srv.NewEnvironment(t, "Live")
	live := r.connect(t, liveEnv, dockertest.FakeConfig{Hostname: "edge-2", Containers: webContainers()}, nil)
	r.whenOnline(t, goneEnv.ID)
	r.whenOnline(t, liveEnv.ID)

	_ = gone.conn.WS.CloseNow()
	eventually(t, "the first agent to be seen disconnected", func() bool {
		in, err := r.svc.Input(ctx, goneEnv.ID, r.clk.Now())
		if err != nil {
			return false
		}
		for _, ag := range in.Agents {
			if ag.ID == gone.id {
				return !ag.Connected
			}
		}
		return false
	})
	if err := r.agg.WriteOnNode(ctx, goneEnv.ID, gone.id, stopWeb()); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("disconnected agent: err = %v, want ErrNodeUnavailable", err)
	}

	r.srv.Exec(t, `UPDATE agents SET status = 'revoked', revoked_at = ? WHERE id = ?`, r.clk.Now().UnixNano(), live.id)
	if err := r.agg.WriteOnNode(ctx, liveEnv.ID, live.id, stopWeb()); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("revoked agent: err = %v, want ErrNodeUnavailable", err)
	}
	// An agent of another environment is not a node of this one.
	if err := r.agg.WriteOnNode(ctx, goneEnv.ID, live.id, stopWeb()); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("another environment's agent: err = %v, want ErrNodeUnavailable", err)
	}
	if len(gone.fake.Writes())+len(live.fake.Writes()) != 0 {
		t.Fatalf("a refused node received writes: %v %v", gone.fake.Writes(), live.fake.Writes())
	}
}

func TestWriteOnNodeRunsOnceAndStillInvalidatesWhenTheCallFails(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host", Containers: webContainers()})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := r.agg.Snapshot(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	cached := func() bool {
		r.snaps.mu.Lock()
		defer r.snaps.mu.Unlock()
		en := r.snaps.entries[e.ID]
		return en != nil && en.ok
	}
	if !cached() {
		t.Fatal("the snapshot was not cached before the write")
	}

	calls := 0
	err = r.agg.WriteOnNode(ctx, e.ID, e.ID, func(*dockerx.Client) error {
		calls++
		return errBoom
	})
	if !errors.Is(err, errBoom) || calls != 1 {
		t.Fatalf("err = %v after %d calls, want errBoom after exactly one", err, calls)
	}
	if cached() {
		t.Fatal("a failed write left the snapshot cached: its outcome is unknown, so the next read must be fresh")
	}
}

func TestNodeTargetTable(t *testing.T) {
	agents := []AgentView{
		{ID: "a1", Connected: true},
		{ID: "a4", Connected: true, ReadOnly: true},
		{ID: "a2", Connected: true, Revoked: true},
		{ID: "a3"},
		{ID: "socket:env-s", Connected: true, Socket: true},
	}
	cases := []struct {
		name string
		in   RollupInput
		node string
		key  string
		ok   bool
		ro   bool
	}{
		{"local hit", RollupInput{EnvID: "env-l", Kind: KindLocal}, "env-l", "env-l", true, false},
		{"local wrong node", RollupInput{EnvID: "env-l", Kind: KindLocal}, "other", "", false, false},
		{"agent hit", RollupInput{EnvID: "env-a", Kind: KindAgent, Agents: agents}, "a1", AgentTarget("a1"), true, false},
		{"revoked agent", RollupInput{EnvID: "env-a", Kind: KindAgent, Agents: agents}, "a2", "", false, false},
		{"disconnected agent", RollupInput{EnvID: "env-a", Kind: KindAgent, Agents: agents}, "a3", "", false, false},
		{"folded socket", RollupInput{EnvID: "env-s", Kind: KindAgent, Agents: agents}, "socket:env-s", "socket:env-s", true, false},
		{"read-only agent", RollupInput{EnvID: "env-a", Kind: KindAgent, Agents: agents}, "a4", AgentTarget("a4"), true, true},
		{"unknown node", RollupInput{EnvID: "env-a", Kind: KindAgent, Agents: agents}, "zz", "", false, false},
		{"empty node", RollupInput{EnvID: "env-a", Kind: KindAgent, Agents: agents}, "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, ro, ok := nodeTarget(tc.in, tc.node)
			if key != tc.key || ok != tc.ok || ro != tc.ro {
				t.Fatalf("nodeTarget = %q, %v, %v; want %q, %v, %v", key, ro, ok, tc.key, tc.ok, tc.ro)
			}
		})
	}
}

func TestWriteOnNodeRefusesAReadOnlyHostBeforeAnyCallButReadsStillRun(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Locked")
	facts := &wire.Facts{EngineID: "ENG:ro", Hostname: "ro-1", EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: true}}
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "ro-1", Containers: webContainers()}, facts)
	r.whenOnline(t, env.ID)
	ctx := context.Background()

	called := false
	err := r.agg.WriteOnNode(ctx, env.ID, a.id, func(*dockerx.Client) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrHostReadOnly) || called {
		t.Fatalf("err = %v, called = %v; want ErrHostReadOnly and fn never run", err, called)
	}
	if got := a.fake.Writes(); len(got) != 0 {
		t.Fatalf("a read-only host received %v", got)
	}

	// A read on the same node still runs.
	read := false
	if err := r.agg.OnNode(ctx, env.ID, a.id, func(c *dockerx.Client) error {
		read = true
		_, e := c.ContainerOwnership(ctx, "web")
		return e
	}); err != nil || !read {
		t.Fatalf("OnNode on a read-only host: err = %v, ran = %v; reads must still work", err, read)
	}
}

func TestInputCarriesTheAgentsPrivilegedServicesFactAndAMissingFieldReadsFalse(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	i := 0
	for name, tc := range map[string]struct {
		policy wire.PolicyFacts
		want   bool
	}{
		"opted in":                        {wire.PolicyFacts{PrivilegedServices: true}, true},
		"not opted in":                    {wire.PolicyFacts{}, false},
		"read-only and no field at all":   {wire.PolicyFacts{ReadOnly: true}, false},
		"opted in beside the self-update": {wire.PolicyFacts{PrivilegedServices: true, SelfUpdate: true}, true},
	} {
		i++
		env := r.srv.NewEnvironment(t, "Env "+name)
		facts := &wire.Facts{EngineID: fmt.Sprintf("ENG:priv%d", i), Hostname: "host-1", EngineReachable: true, Policy: tc.policy}
		a := r.connect(t, env, dockertest.FakeConfig{Hostname: "host-1", Containers: webContainers()}, facts)
		r.whenOnline(t, env.ID)
		in, err := r.svc.Input(context.Background(), env.ID, r.clk.Now())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var found bool
		for _, v := range in.Agents {
			if v.ID == a.id {
				found = true
				if v.PrivilegedServices != tc.want {
					t.Errorf("%s: PrivilegedServices = %v, want %v", name, v.PrivilegedServices, tc.want)
				}
			}
		}
		if !found {
			t.Errorf("%s: agent %s is not in the rollup input %+v", name, a.id, in.Agents)
		}
	}
}

func TestOnNodeLeavesTheSnapshotCached(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host", Containers: webContainers()})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := r.agg.Snapshot(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.agg.OnNode(ctx, e.ID, e.ID, func(*dockerx.Client) error { return errBoom }); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	r.snaps.mu.Lock()
	en := r.snaps.entries[e.ID]
	cached := en != nil && en.ok
	r.snaps.mu.Unlock()
	if !cached {
		t.Fatal("a read flushed the snapshot cache")
	}
}

// WriteOnManager is the write path for what belongs to a whole environment: the one
// engine of a standalone host or the local socket, and on a swarm the manager the
// cluster reader chooses (plan 04-12, D-39, P2 D-42).

func TestWriteOnManagerOnTheLocalSocketRunsOnceAndFlushesTheSnapshot(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host", Containers: webContainers()})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := r.agg.Snapshot(ctx, e.ID); err != nil { // warm the cache
		t.Fatal(err)
	}
	calls := 0
	err = r.agg.WriteOnManager(ctx, e.ID, func(c *dockerx.Client) error {
		calls++
		return stopWeb()(c)
	})
	if err != nil || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want one run", err, calls)
	}
	if got := local.Writes(); !slices.Equal(got, []string{"POST /containers/web/stop"}) {
		t.Fatalf("writes = %v", got)
	}
	after, _, err := r.agg.Snapshot(ctx, e.ID)
	if err != nil || after.Containers[0].State != "exited" {
		t.Fatalf("the list after the write = %+v, %v; want a fresh read", after.Containers, err)
	}
}

func TestWriteOnManagerUsesTheOneAgentOfAStandaloneHostAndNeverRetries(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Containers: webContainers()}, nil)
	r.whenOnline(t, env.ID)
	ctx := context.Background()

	calls := 0
	err := r.agg.WriteOnManager(ctx, env.ID, func(c *dockerx.Client) error {
		calls++
		if got := probeHost(t, c); got != "edge-1" {
			t.Errorf("the write went to %q, want edge-1", got)
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want the one attempt's error and no retry", err, calls)
	}
}

func TestWriteOnManagerRefusesAReadOnlyHostAndAnEnvironmentWithNoNodeToReach(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	ctx := context.Background()

	env := r.srv.NewEnvironment(t, "Locked")
	facts := &wire.Facts{EngineID: "ENG:ro", Hostname: "ro-1", EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: true}}
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "ro-1", Containers: webContainers()}, facts)
	r.whenOnline(t, env.ID)
	called := false
	err := r.agg.WriteOnManager(ctx, env.ID, func(*dockerx.Client) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrHostReadOnly) || called || len(a.fake.Writes()) != 0 {
		t.Fatalf("err = %v, called = %v; want ErrHostReadOnly and nothing run", err, called)
	}

	empty := r.srv.NewEnvironment(t, "Nobody home")
	if err := r.agg.WriteOnManager(ctx, empty.ID, func(*dockerx.Client) error {
		t.Error("fn ran with no node to reach")
		return nil
	}); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("an environment with no connected agent: err = %v, want ErrNodeUnavailable", err)
	}
	if err := r.agg.WriteOnManager(ctx, "no-such-environment", func(*dockerx.Client) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown environment: err = %v, want ErrNotFound", err)
	}
}

func TestWriteOnManagerOnASwarmGoesToTheManagerTheClusterReaderChoosesOnceEvenWhenItFails(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	var hosts []string
	err := s.agg.WriteOnManager(ctx, s.env.ID, func(c *dockerx.Client) error {
		hosts = append(hosts, probeHost(t, c))
		return errBoom
	})
	if !errors.Is(err, errBoom) || !slices.Equal(hosts, []string{"mgr-2"}) {
		t.Fatalf("err = %v, hosts = %v; want one attempt on the leader mgr-2 and no failover for a write", err, hosts)
	}
}

// A swarm node is a swarm ID to the cluster and an agent ID (or the environment's own, or
// socket:<envId>) to the routes that reach it. SwarmNodeTargets is the map between them
// (plan 06-07, RES-04), so a task can link to its container and later plans can route a
// node-local read to the node a task ran on.

func nodeIDs(m map[string]NodeRef) map[string]string {
	out := map[string]string{}
	for swarmID, ref := range m {
		out[swarmID] = ref.ID
	}
	return out
}

func TestSwarmNodeTargetsMapsAConnectedAgentsSwarmNodeToTheAgentID(t *testing.T) {
	f := newFoldRig(t, nil)
	got, err := f.agg.SwarmNodeTargets(context.Background(), f.env.ID)
	if err != nil {
		t.Fatalf("SwarmNodeTargets: %v", err)
	}
	if want := map[string]string{"node-m1": f.m.id}; !maps.Equal(nodeIDs(got), want) {
		t.Fatalf("targets = %v, want %v", nodeIDs(got), want)
	}
	if ref := got["node-m1"]; ref.Name != "mgr-1" {
		t.Errorf("the node is named %q, want the agent's host name mgr-1", ref.Name)
	}
}

func TestSwarmNodeTargetsMapsTheFoldedHostToItsSocketTarget(t *testing.T) {
	f := newFoldRig(t, nil)
	f.fold(t)
	got, err := f.agg.SwarmNodeTargets(context.Background(), f.env.ID)
	if err != nil {
		t.Fatalf("SwarmNodeTargets: %v", err)
	}
	want := map[string]string{"node-m1": f.m.id, "node-local": SocketTarget(f.env.ID)}
	if !maps.Equal(nodeIDs(got), want) {
		t.Fatalf("targets = %v, want %v", nodeIDs(got), want)
	}
	// The ID is one that the node routes accept.
	if err := f.agg.OnNode(context.Background(), f.env.ID, got["node-local"].ID, func(*dockerx.Client) error { return nil }); err != nil {
		t.Errorf("OnNode with the folded host's target: %v", err)
	}
}

func TestSwarmNodeTargetsMapsTheLocalSocketsOwnSwarmNodeToTheEnvironmentID(t *testing.T) {
	f := newFoldRig(t, nil)
	got, err := f.agg.SwarmNodeTargets(context.Background(), f.localEnv.ID)
	if err != nil {
		t.Fatalf("SwarmNodeTargets: %v", err)
	}
	if want := map[string]string{"node-local": f.localEnv.ID}; !maps.Equal(nodeIDs(got), want) {
		t.Fatalf("targets = %v, want %v", nodeIDs(got), want)
	}
}

func TestSwarmNodeTargetsLeavesOutAnAgentThatIsGone(t *testing.T) {
	f := newFoldRig(t, nil)
	ctx := context.Background()
	_ = f.m.conn.WS.CloseNow()
	eventually(t, "the manager's agent to be seen disconnected", func() bool {
		got, err := f.agg.SwarmNodeTargets(ctx, f.env.ID)
		return err == nil && len(got) == 0
	})
}

func TestSwarmNodeTargetsOfAnUnknownEnvironmentIsNotFound(t *testing.T) {
	f := newFoldRig(t, nil)
	if _, err := f.agg.SwarmNodeTargets(context.Background(), "no-such-environment"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
