// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Shutdown (review WR-B01): the checks that run apart from their callers, with the automatic deploys that follow them,
// are ended and waited for before the server closes the database and the jobs.

// hangClient holds a fetch until its context ends, and tells the test when one is waiting.
type hangClient struct {
	gitx.Client
	on      atomic.Bool
	entered chan struct{}
}

func (h *hangClient) FetchRef(ctx context.Context, m gitx.MirrorID, r gitx.Remote, kind gitx.RefKind, ref string) (gitx.Commit, error) {
	if !h.on.Load() {
		return h.Client.FetchRef(ctx, m, r, kind, ref)
	}
	h.entered <- struct{}{}
	<-ctx.Done()
	return gitx.Commit{}, ctx.Err()
}

func TestShutdownEndsAndWaitsForADetachedCheck(t *testing.T) {
	t.Parallel()
	hang := &hangClient{entered: make(chan struct{}, 4)}
	var joins atomic.Int32
	f := newCheckFixture(t, func(c gitx.Client) gitx.Client { hang.Client = c; return hang }, func(string, int) { joins.Add(1) })
	f.deploy(t)
	ctx, cancel := context.WithCancel(context.Background())
	hang.on.Store(true)

	// A caller that gives up (the poller whose context ended) leaves the check running.
	asked := make(chan error, 1)
	go func() {
		_, err := f.svc.Check(ctx, "env-1", f.stackID, gitsync.TriggerPoll)
		asked <- err
	}()
	testsupport.Await(t, "the check to reach the fetch", hang.entered)
	cancel()
	if err := testsupport.Await(t, "the caller to give up", asked); !errors.Is(err, context.Canceled) {
		t.Fatalf("Check = %v, want the caller's cancellation", err)
	}
	if !f.svc.InFlight(f.stackID) {
		t.Fatal("the check is not in flight after its caller gave up, want it left running")
	}

	// Shutdown ends it and returns only when it is gone.
	if err := f.svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if f.svc.InFlight(f.stackID) {
		t.Error("a check is still in flight after Shutdown returned")
	}

	// Nothing starts afterwards: a check is refused and a trigger does nothing.
	if _, err := f.svc.Check(context.Background(), "env-1", f.stackID, gitsync.TriggerManual); !errors.Is(err, gitsync.ErrShuttingDown) {
		t.Errorf("Check after Shutdown = %v, want ErrShuttingDown", err)
	}
	before := joins.Load()
	f.svc.Trigger("env-1", f.stackID, gitsync.TriggerWebhook)
	if f.svc.InFlight(f.stackID) || joins.Load() != before {
		t.Error("a trigger after Shutdown started a check")
	}
}

// A server that begins to shut down while an automatic deploy is between its pull and its deploy starts no deploy.
func TestShutdownStopsAnAutomaticDeployBeforeItStarts(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	expired, stop := context.WithCancel(context.Background())
	stop()
	a.dep.set(func(d *fakeDeployer) {
		// Not waited for here: this runs inside the check that Shutdown would wait for.
		d.whileWorkingOut = func() { _ = a.svc.Shutdown(expired) }
	})
	a.changeWeb(t, "1")

	if _, err := a.svc.CheckWith(context.Background(), "env-1", a.stackID, gitsync.CheckOptions{Trigger: gitsync.TriggerPoll}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckWith: %v", err)
	}
	if err := a.svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := a.dep.count(); n != 0 {
		t.Errorf("deploys started = %d, want none once the server was shutting down", n)
	}
}

func TestShutdownWaitsOnlyAsLongAsItIsGiven(t *testing.T) {
	t.Parallel()
	gate := newGateClient() // ignores its context: the fetch ends when the test lets it
	f := newCheckFixture(t, func(c gitx.Client) gitx.Client { gate.Client = c; return gate }, nil)
	f.deploy(t)
	gate.on.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.svc.Check(context.Background(), "env-1", f.stackID, gitsync.TriggerWebhook)
	}()
	testsupport.Await(t, "the check to reach the fetch", gate.entered)

	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := f.svc.Shutdown(expired); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown with no time left = %v, want the context's error while a check still runs", err)
	}

	gate.release <- struct{}{}
	testsupport.Await(t, "the check to end", done)
	if err := f.svc.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown once the check ended = %v, want nil", err)
	}
}
