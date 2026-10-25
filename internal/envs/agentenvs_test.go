// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// An agent environment has no monitor of its own: the monitor holds one target for each connected agent, keyed
// "agent:<agent id>", so a worker that wants to follow an environment (the background drift check of stacks, which reads the
// stacks of an environment by its ID) is never told of one by the monitor. The service tells it instead, once when the
// environment gets its first agent and once when the last one leaves (STK-14, found by the real-engine drift case of
// plan 08-27: the worker ran for "agent:..." keys, found no stack under them, and never checked a swarm).

func TestServiceAnnouncesAnAgentEnvironmentOnItsFirstAgentAndWhenItsLastOneLeaves(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	started, stopped := make(chan string, 8), make(chan string, 8)
	r.svc.WatchAgentEnvironments(func(id string) { started <- id }, func(id string) { stopped <- id })

	a1 := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)
	if got := announced(t, started, "the start"); got != env.ID {
		t.Fatalf("started = %q, want the environment %s and not the agent's monitor key", got, env.ID)
	}

	// A second agent of the same environment is not a new environment.
	a2 := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-2"}, nil)
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
