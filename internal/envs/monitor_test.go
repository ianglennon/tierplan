// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

func TestWatchIsCalledForStartAndStop(t *testing.T) {
	m := awaitMonitor(t)
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	started, stopped := make(chan string, 4), make(chan string, 4)
	m.Watch(func(id string) { started <- id }, func(id string) { stopped <- id })

	if err := m.Start(context.Background(), "watched", fake); err != nil {
		t.Fatal(err)
	}
	if got := <-started; got != "watched" {
		t.Fatalf("started = %q, want watched", got)
	}
	// A second Start of a monitored environment begins no loop, so it announces nothing.
	if err := m.Start(context.Background(), "watched", fake); err != nil {
		t.Fatal(err)
	}
	m.Stop("watched")
	if got := <-stopped; got != "watched" {
		t.Fatalf("stopped = %q, want watched", got)
	}
	if len(started) != 0 || len(stopped) != 0 {
		t.Errorf("announced %d starts and %d stops more than once each", len(started), len(stopped))
	}
}

// Watch is for a worker that follows environments by their ID. The monitor also holds the targets of an agent environment
// ("agent:<agent id>") and the socket of a host folded into a swarm ("socket:<environment id>"): they are not environment
// IDs, and a worker handed one finds nothing under it (STK-14, found by the real-engine drift case of plan 08-27).
func TestWatchAnnouncesOnlyEnvironmentIDsNotTheTargetsOfAnAgentEnvironment(t *testing.T) {
	m := awaitMonitor(t)
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	started, stopped := make(chan string, 8), make(chan string, 8)
	m.Watch(func(id string) { started <- id }, func(id string) { stopped <- id })

	for _, key := range []string{AgentTarget("agent-1"), SocketTarget("swarm-env"), "local-env"} {
		if err := m.Start(context.Background(), key, fake); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{AgentTarget("agent-1"), SocketTarget("swarm-env"), "local-env"} {
		m.Stop(key)
	}
	close(started)
	close(stopped)
	var got []string
	for id := range started {
		got = append(got, "start "+id)
	}
	for id := range stopped {
		got = append(got, "stop "+id)
	}
	if len(got) != 2 || got[0] != "start local-env" || got[1] != "stop local-env" {
		t.Errorf("announced %v, want only the start and the stop of local-env", got)
	}
}
