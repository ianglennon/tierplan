// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
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
