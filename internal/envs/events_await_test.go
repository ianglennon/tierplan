// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

func TestTheEventsHubAwaitReturnsOnceAnEventHasBeenBuffered(t *testing.T) {
	r := newHubRig(t, dockertest.FakeConfig{})
	ctx := awaitCtx(t)
	// The daemon stream is open once the fake sees it, which is what an event needs.
	if err := r.fake.Await(ctx, func() bool { return r.fake.OpenEventStreams() == 1 }); err != nil {
		t.Fatalf("the daemon stream never opened: %v", err)
	}
	r.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "c-await"})
	if err := r.hub.Await(ctx, func() bool { return len(r.hub.Recent(hubEnv, 10)) >= 1 }); err != nil {
		t.Fatalf("no event was buffered: %v", err)
	}
	if got := r.hub.Recent(hubEnv, 10); len(got) != 1 || got[0].ActorID != "c-await" {
		t.Fatalf("Recent = %+v, want the one event published", got)
	}
}

func TestTheEventsHubAwaitReturnsOnceATargetsFeedHasOpened(t *testing.T) {
	mon := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	t.Cleanup(mon.StopAll)
	hub := NewHub(mon, nil, slog.New(slog.DiscardHandler), nil)
	t.Cleanup(hub.Close)
	row := dockerx.EventRow{ID: "own-1", Type: "tierplan", Action: "note"}
	if hub.Inject("t1", row) {
		t.Fatal("Inject reported success for a target nobody monitors")
	}
	done := make(chan error, 1)
	go func() {
		done <- hub.Await(awaitCtx(t), func() bool { return hub.Inject("t1", row) })
	}()
	if err := mon.Start(context.Background(), "t1", dockertest.NewFake(t, dockertest.FakeConfig{})); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Await: %v", err)
	}
	if got := hub.Recent("t1", 10); len(got) != 1 || got[0].ID != "own-1" {
		t.Fatalf("Recent = %+v, want the injected row", got)
	}
}

func TestTheEventsHubAwaitReturnsTheContextErrorWhenTheConditionNeverHolds(t *testing.T) {
	mon := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	hub := NewHub(mon, nil, slog.New(slog.DiscardHandler), nil)
	t.Cleanup(hub.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hub.Await(ctx, func() bool { return false }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Await = %v, want context.Canceled", err)
	}
}
