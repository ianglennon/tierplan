// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/tunnel"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// An agent environment has no monitor of its own: the monitor holds one target for each connected agent, keyed
// "agent:<agent id>", so a worker that wants to follow an environment (the background drift check of stacks, which reads the
// stacks of an environment by its ID) is never told of one by the monitor. The service tells it instead, once when the
// environment gets its first agent and once when the last one leaves (STK-14, found by the real-engine drift case of
// plan 08-27: the worker ran for "agent:..." keys, found no stack under them, and never checked a swarm).

func TestServiceAnnouncesAnAgentEnvironmentOnItsFirstAgentAndWhenItsLastOneLeaves(t *testing.T) {
	// Two managers of one swarm are two agents of one environment.
	r := newSwarmRigBare(t, managerNodes(), nil)
	env := r.env
	started, stopped := make(chan string, 8), make(chan string, 8)
	r.svc.WatchAgentEnvironments(func(id string) { started <- id }, func(id string) { stopped <- id })

	a1 := r.connect(t, env, managerConfig(1, r.nodes, nil), managerFacts(1))
	r.srv.Events.WaitLen(t, 1)
	if got := announced(t, started, "the start"); got != env.ID {
		t.Fatalf("started = %q, want the environment %s and not the agent's monitor key", got, env.ID)
	}

	// A second agent of the same environment is not a new environment.
	a2 := r.connect(t, env, managerConfig(2, r.nodes, nil), managerFacts(2))
	r.srv.Events.WaitLen(t, 2)
	if len(started) != 0 {
		t.Fatalf("the second agent announced the environment again: %d starts", len(started))
	}

	// One agent leaving leaves the environment watched.
	_ = a1.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 3)
	if len(stopped) != 0 {
		t.Fatalf("the environment was announced stopped while an agent was still connected")
	}

	// The last one leaving ends it.
	_ = a2.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 4)
	if got := announced(t, stopped, "the stop"); got != env.ID {
		t.Fatalf("stopped = %q, want the environment %s", got, env.ID)
	}
	if len(stopped) != 0 || len(started) != 0 {
		t.Errorf("announced %d more stops and %d more starts than the one of each", len(stopped), len(started))
	}
}

// Removing an agent environment kicks its sessions asynchronously and forgets them at once, so the later Disconnected
// of each finds nothing to end. The removal announces the stop itself, once, or the drift worker of the environment
// would keep its timer and its event subscription until the server restarted (WR-05).
func TestRemovingAnAgentEnvironmentAnnouncesItStoppedOnce(t *testing.T) {
	r := newSwarmRigBare(t, managerNodes(), nil)
	env := r.env
	started, stopped := make(chan string, 8), make(chan string, 8)
	r.svc.WatchAgentEnvironments(func(id string) { started <- id }, func(id string) { stopped <- id })

	r.connect(t, env, managerConfig(1, r.nodes, nil), managerFacts(1))
	r.srv.Events.WaitLen(t, 1)
	if got := announced(t, started, "the start"); got != env.ID {
		t.Fatalf("started = %q, want the environment %s", got, env.ID)
	}

	if err := r.svc.Remove(context.Background(), admin, env.ID, "203.0.113.5", "test", false); err != nil {
		t.Fatal(err)
	}
	if got := announced(t, stopped, "the stop"); got != env.ID {
		t.Fatalf("stopped = %q, want the removed environment %s", got, env.ID)
	}
	// The kicked session's own Disconnected, which finds the environment gone, must not announce it again.
	r.srv.Events.WaitLen(t, 2)
	if len(stopped) != 0 || len(started) != 0 {
		t.Errorf("announced %d more stops and %d more starts after the removal", len(stopped), len(started))
	}
}

// A quick reconnect of the only agent of an environment is announced as started, stopped, started: the hub holds its
// lifecycle lock across each Observer call (tunnel.Hub.life), so the service hears Connected and Disconnected in the
// order the sessions began and ended, and a watcher that tracks the announcements (as the drift monitor does) ends with
// a worker for a connected environment and none for a disconnected one (WR-06).
func TestAQuickReconnectIsAnnouncedInOrderAndLeavesOneWorker(t *testing.T) {
	r := newSwarmRigBare(t, managerNodes(), nil)
	env := r.env
	var mu sync.Mutex
	var order []string
	running := map[string]bool{}
	note := func(kind string, on bool) func(string) {
		return func(id string) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, kind)
			running[id] = on
		}
	}
	r.svc.WatchAgentEnvironments(note("start", true), note("stop", false))
	state := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(order, ","), running[env.ID]
	}

	a1 := r.connect(t, env, managerConfig(1, r.nodes, nil), managerFacts(1))
	r.srv.Events.WaitLen(t, 1)
	const cycles = 5
	n := 1
	for range cycles {
		r.srv.Hub.Kick(a1.id, tunnel.DisconnectClosed)
		n++
		r.srv.Events.WaitLen(t, n)
		r.reconnect(t, a1)
		n++
		r.srv.Events.WaitLen(t, n)
	}
	got, up := state()
	if want := strings.Repeat("start,stop,", cycles) + "start"; got != want || !up {
		t.Fatalf("announced %q (running %v), want %q with the environment running", got, up, want)
	}

	r.svc.mu.Lock()
	connected := len(r.svc.connected[env.ID])
	r.svc.mu.Unlock()
	if connected != 1 {
		t.Fatalf("the service holds %d sessions of the environment, want 1", connected)
	}
}

func TestEventRowsOfAnAgentEnvironmentAreItsMergedFeedAsPlainRows(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)

	_, ch, cancel := r.agg.EventRows(t.Context(), env.ID)
	defer cancel()
	a.fake.PublishEvent(dockertest.FakeEvent{Type: "service", Action: "update", ActorID: "svc-1", Attributes: map[string]string{"name": "stack_web"}})

	ctx := testsupport.Context(t)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("the channel closed before the event came")
			}
			if ev.Type == "service" && ev.Name == "stack_web" {
				return
			}
		case <-ctx.Done():
			t.Fatal("the service event of the environment never reached the rows")
		}
	}
}

// announced is the next environment a watcher was told of, failing the test when none comes.
func announced(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-testsupport.Context(t).Done():
		t.Fatalf("%s of the environment was never announced", what)
		return ""
	}
}
