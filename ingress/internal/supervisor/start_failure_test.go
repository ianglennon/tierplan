// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package supervisor

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/cache"
	"github.com/ianglennon/tierplan/ingress/internal/channel"
	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/ingress/internal/identity"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// WR-C-06: the paths on which the node cannot start, or starts without Traefik. The apply-then-start failure
// of a bundle that arrives on the channel is covered by apply_failure_test.go (WR-A-03:
// TestFailedStartIsRetriedAndNotReportedApplied, TestCommitFailureStillStartsTraefik and the "traefik start"
// subtest of TestApplyFailureCauseIsLogged), so only the retry on the next frame is added here.

// mkdirFailFS is the real filesystem whose MkdirAll fails with err.
type mkdirFailFS struct {
	fsx.FS
	err error
}

func (f mkdirFailFS) MkdirAll(string, fs.FileMode) error { return f.err }

// readFailFS is the real filesystem whose reads of a path with the given suffix fail with err (not ErrNotExist).
type readFailFS struct {
	fsx.FS
	suffix string
	err    error
}

func (f readFailFS) ReadFile(name string) ([]byte, error) {
	if strings.HasSuffix(name, f.suffix) {
		return nil, f.err
	}
	return f.FS.ReadFile(name)
}

// mustFailToStart runs the supervisor, which must return an error without entering the channel loop, and
// returns the error. It then checks what every failed start owes the caller: the readiness listener is
// closed, Traefik was never asked to stop (it never ran), and no TierPlan dial was attempted.
func mustFailToStart(t *testing.T, h *harness, wantListener bool) error {
	t.Helper()
	entered := false
	h.deps.RunChannel = func(context.Context, channel.LoopOptions) error {
		entered = true
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := awaitRun(t, h.run(ctx))
	if err == nil {
		t.Fatal("Run returned nil, want the start-up error")
	}
	if entered {
		t.Error("the channel loop was entered although the node could not start")
	}
	if wantListener {
		if code, serr := h.status(); serr == nil {
			t.Errorf("the readiness listener still answers (%d) after Run failed to start", code)
		}
	}
	if got := h.traefik.stops.Load(); got != 0 {
		t.Errorf("Traefik.Stop was called %d times for a node that failed before the channel", got)
	}
	return err
}

// TestGoodCacheTraefikWontStart: a cache that verifies but a Traefik that cannot start is a start-up error,
// not a node that waits for TierPlan with nothing served, and it leaves no listener behind.
func TestGoodCacheTraefikWontStart(t *testing.T) {
	b := validBundle(t, 1, "app.example.test")
	h := newHarness(t, cachedVolume(t, b))
	h.traefik.failStarts.Store(1)

	err := mustFailToStart(t, h, true)
	if !errors.Is(err, errTraefikStart) {
		t.Errorf("Run error = %v, want the Traefik start error wrapped", err)
	}
	if !strings.Contains(err.Error(), "cached bundle") {
		t.Errorf("Run error = %q, want it to name the cached bundle", err)
	}
	if h.state.Applied() {
		t.Error("readiness is marked applied although Traefik never started")
	}
	if got := h.traefik.starts.Load(); got != 1 {
		t.Errorf("Traefik Start was called %d times, want 1", got)
	}
}

// TestListenFailureIsAStartError: with no readiness listener the node does not go on to touch the volume.
func TestListenFailureIsAStartError(t *testing.T) {
	h := newHarness(t, "")
	errListen := errors.New("address already in use")
	h.deps.Listen = func(string, string) (net.Listener, error) { return nil, errListen }

	err := mustFailToStart(t, h, false)
	if !errors.Is(err, errListen) || !strings.Contains(err.Error(), "listen for readiness") {
		t.Errorf("Run error = %v, want the listen error named as the readiness listener", err)
	}
	for _, ev := range h.events.snapshot() {
		if ev == "prepare" || ev == "static" || ev == "key" || ev == "load" {
			t.Errorf("the node did %q although it could not listen", ev)
		}
	}
	if got := h.traefik.starts.Load(); got != 0 {
		t.Errorf("Traefik started %d times without a listener", got)
	}
}

// TestVolumeFailureIsAStartError: a node that cannot write its own volume cannot serve, so each of the three
// preparation steps ends Run with an error that says which, and closes the listener it had opened.
func TestVolumeFailureIsAStartError(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *harness)
		want string
	}{
		{"layout", func(h *harness) {
			h.deps.Store = cache.NewStore(mkdirFailFS{FS: fsx.OS(), err: syscall.EROFS}, h.root)
		}, "prepare the volume"},
		{"static configuration", func(h *harness) {
			h.deps.Store = cache.NewStore(renameFailFS{FS: fsx.OS(), suffix: "/static/traefik.yml", err: syscall.EROFS}, h.root)
		}, "Traefik's static configuration"},
		{"node key", func(h *harness) {
			h.deps.Identity = identity.New(renameFailFS{FS: fsx.OS(), suffix: "/state/node.key", err: syscall.EROFS}, h.root)
		}, "the node key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			tc.set(h)
			err := mustFailToStart(t, h, true)
			if !errors.Is(err, syscall.EROFS) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Run error = %v, want the read-only error naming %q", err, tc.want)
			}
			if got := h.traefik.starts.Load(); got != 0 {
				t.Errorf("Traefik started %d times on a volume that could not be prepared", got)
			}
		})
	}
}

// TestCachedBundleServesWhenItsRecordCannotBeCompleted: a verified pending record whose rename to
// applied.json fails still serves (the bundle is on disk and verifies), and the supervisor says so.
func TestCachedBundleServesWhenItsRecordCannotBeCompleted(t *testing.T) {
	b := validBundle(t, 1, "app.example.test")
	root := t.TempDir()
	failCommit := renameFailFS{FS: fsx.OS(), suffix: "/state/applied.json", err: syscall.EIO}
	s := cache.NewStore(failCommit, root)
	if err := s.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// The first run's kill between the dynamic rename and the commit: pending.json, no applied.json.
	if _, reason, _ := s.Accept(frameFor(t, testInstance, b), testInstance); reason != ingressbundle.ReasonApplyFailed {
		t.Fatalf("Accept reason = %q, want apply_failed (the commit failed)", reason)
	}

	h := newHarness(t, root)
	h.deps.Store = cache.NewStore(failCommit, root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, done := h.runUntilChannel(ctx)

	if got := h.traefik.starts.Load(); got != 1 {
		t.Errorf("Traefik Start was called %d times, want 1: the cached bundle verifies", got)
	}
	if !h.state.Applied() {
		t.Error("readiness is not marked applied for a cached bundle that verifies")
	}
	line, ok := h.logLine("the cached bundle serves but its record could not be completed")
	if !ok {
		t.Fatalf("no incomplete-record warning in the log:\n%s", h.logs.String())
	}
	if cause, _ := line["error"].(string); !strings.Contains(cause, syscall.EIO.Error()) {
		t.Errorf("the warning's error = %q, want the commit's %q", cause, syscall.EIO.Error())
	}
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestUnreadableCacheServesNothing: a record that cannot be read (not merely absent) leaves Traefik
// unstarted and the node waiting for TierPlan, with the reason in the log.
func TestUnreadableCacheServesNothing(t *testing.T) {
	root := cachedVolume(t, validBundle(t, 1, "app.example.test"))
	h := newHarness(t, root)
	h.deps.Store = cache.NewStore(readFailFS{FS: fsx.OS(), suffix: "/state/applied.json", err: syscall.EIO}, root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, done := h.runUntilChannel(ctx)

	if got := h.traefik.starts.Load(); got != 0 {
		t.Errorf("Traefik started %d times from a cache that could not be read", got)
	}
	if h.state.Applied() {
		t.Error("readiness is marked applied for a cache that could not be read")
	}
	line, ok := h.logLine("the cache could not be read; nothing is served from it")
	if !ok {
		t.Fatalf("no unreadable-cache warning in the log:\n%s", h.logs.String())
	}
	if cause, _ := line["error"].(string); !strings.Contains(cause, syscall.EIO.Error()) {
		t.Errorf("the warning's error = %q, want %q", cause, syscall.EIO.Error())
	}
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestFailedStartIsRetriedOnTheNextFrame: the same bundle sent again after a failed start is answered
// applied, because it is already on the volume, and starts Traefik. (The health tick's retry is
// TestFailedStartIsRetriedAndNotReportedApplied.)
func TestFailedStartIsRetriedOnTheNextFrame(t *testing.T) {
	h := newHarness(t, "")
	h.traefik.failStarts.Store(1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lo, done := h.runUntilChannel(ctx)

	b := validBundle(t, 1, "app.example.test")
	frame := frameFor(t, testInstance, b)
	if _, reason := lo.Session.Handle(ctx, frame, testInstance); reason != ingressbundle.ReasonApplyFailed {
		t.Fatalf("first Handle reason = %q, want apply_failed", reason)
	}
	if h.state.Applied() {
		t.Fatal("readiness is marked applied although Traefik never started")
	}
	if id, reason := lo.Session.Handle(ctx, frame, testInstance); id != b.ID || reason != "" {
		t.Fatalf("second Handle = (%q, %q), want (%q, \"\"): the start is tried again with the next frame", id, reason, b.ID)
	}
	if got := h.traefik.starts.Load(); got != 2 {
		t.Fatalf("Traefik Start was called %d times, want 2 (the failure, then the retry)", got)
	}
	if !h.state.Applied() {
		t.Fatal("readiness is not marked applied after the retried start")
	}
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
