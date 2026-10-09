// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
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
