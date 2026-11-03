// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// gateRun is one caller of the gate, run in its own goroutine.
type gateRun struct {
	body JanitorPreviewBody
	err  error
	done chan struct{}
}

func startGate(ctx context.Context, g *previewGate, envID, inputs string, fn func(context.Context) (JanitorPreviewBody, error)) *gateRun {
	r := &gateRun{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.body, r.err = g.run(ctx, envID, inputs, fn)
	}()
	return r
}

// blockingPreview is a preview that says when it starts and returns once released, counting its runs.
func blockingPreview(runs *atomic.Int32, started chan<- string, release <-chan struct{}, name string) func(context.Context) (JanitorPreviewBody, error) {
	return func(context.Context) (JanitorPreviewBody, error) {
		runs.Add(1)
		started <- name
		<-release
		return JanitorPreviewBody{PreviewedAt: name}, nil
	}
}

// The janitor's preview reads every node of an environment, and any reader may ask for it (WR-08): the gate runs
// one preview of an environment at a time, and a caller asking for the preview already running gets its answer.
func TestPreviewGate(t *testing.T) {
	t.Parallel()

	t.Run("the same preview is run once", func(t *testing.T) {
		t.Parallel()
		ctx := testsupport.Context(t)
		g := newPreviewGate()
		joined := make(chan struct{}, 1)
		g.onJoin = func() { joined <- struct{}{} }
		var runs atomic.Int32
		started, release := make(chan string, 2), make(chan struct{})
		first := startGate(ctx, g, "env-1", "", blockingPreview(&runs, started, release, "first"))
		testsupport.Await(t, "the first preview to start", started)
		second := startGate(ctx, g, "env-1", "", blockingPreview(&runs, started, release, "second"))
		testsupport.Await(t, "the second caller to join the first preview", joined)
		close(release)
		testsupport.Await(t, "the first caller's answer", first.done)
		testsupport.Await(t, "the second caller's answer", second.done)
		if runs.Load() != 1 || first.body.PreviewedAt != "first" || second.body.PreviewedAt != "first" || first.err != nil || second.err != nil {
			t.Fatalf("runs %d, answers %q %v and %q %v; want one run whose answer both get", runs.Load(), first.body.PreviewedAt, first.err, second.body.PreviewedAt, second.err)
		}
	})

	t.Run("another preview of the environment waits", func(t *testing.T) {
		t.Parallel()
		ctx := testsupport.Context(t)
		g := newPreviewGate()
		waiting := make(chan struct{}, 1)
		g.onWait = func() { waiting <- struct{}{} }
		var runs atomic.Int32
		started, release := make(chan string, 2), make(chan struct{})
		first := startGate(ctx, g, "env-1", "", blockingPreview(&runs, started, release, "first"))
		testsupport.Await(t, "the first preview to start", started)
		five := 5
		candidate := previewInputs(&janitor.Overrides{KeepN: &five}, nil)
		second := startGate(ctx, g, "env-1", candidate, blockingPreview(&runs, started, release, "second"))
		select {
		case <-waiting:
		case name := <-started:
			t.Fatalf("the %s preview started while another preview of the environment was running", name)
		case <-ctx.Done():
			t.Fatal("the second preview neither waited nor started")
		}
		// Another environment is not held up.
		var otherRuns atomic.Int32
		otherStarted := make(chan string, 1)
		other := startGate(ctx, g, "env-2", "", blockingPreview(&otherRuns, otherStarted, release, "other"))
		testsupport.Await(t, "the preview of another environment to start", otherStarted)

		close(release)
		testsupport.Await(t, "the first caller's answer", first.done)
		testsupport.Await(t, "the second preview to start", started)
		testsupport.Await(t, "the second caller's answer", second.done)
		testsupport.Await(t, "the other environment's answer", other.done)
		if runs.Load() != 2 || second.body.PreviewedAt != "second" {
			t.Fatalf("runs %d, second answer %q; want the candidate's own preview run after the first", runs.Load(), second.body.PreviewedAt)
		}
	})

	t.Run("a caller whose request ends stops waiting", func(t *testing.T) {
		t.Parallel()
		ctx := testsupport.Context(t)
		g := newPreviewGate()
		waiting := make(chan struct{}, 1)
		g.onWait = func() { waiting <- struct{}{} }
		var runs atomic.Int32
		started, release := make(chan string, 2), make(chan struct{})
		first := startGate(ctx, g, "env-1", "", blockingPreview(&runs, started, release, "first"))
		testsupport.Await(t, "the first preview to start", started)
		gone, cancel := context.WithCancel(ctx)
		second := startGate(gone, g, "env-1", "other", blockingPreview(&runs, started, release, "second"))
		testsupport.Await(t, "the second preview to wait", waiting)
		cancel()
		testsupport.Await(t, "the second caller to give up", second.done)
		if second.err == nil || runs.Load() != 1 {
			t.Fatalf("second = %v after %d runs, want the context's error and no run of its own", second.err, runs.Load())
		}
		close(release)
		testsupport.Await(t, "the first caller's answer", first.done)
	})
}
