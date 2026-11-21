// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/cache"
	"github.com/ianglennon/tierplan/ingress/internal/channel"
	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// renameFailFS is the real filesystem whose renames onto a path with the given suffix fail with err.
type renameFailFS struct {
	fsx.FS
	suffix string
	err    error
}

func (f renameFailFS) Rename(oldpath, newpath string) error {
	if strings.HasSuffix(filepath.ToSlash(newpath), f.suffix) {
		return f.err
	}
	return f.FS.Rename(oldpath, newpath)
}

// logLine is the first line of the harness's JSON log whose message is msg.
func (h *harness) logLine(msg string) (map[string]any, bool) {
	for _, raw := range strings.Split(h.logs.String(), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			h.t.Fatalf("a log line is not JSON: %q", raw)
		}
		if line["msg"] == msg {
			return line, true
		}
	}
	return nil, false
}

// TestApplyFailureCauseIsLogged is WR-A-02: an apply_failed names the step and the OS error in the log, so
// an operator whose node answers it has something to act on (the EXDEV case D-31 calls out), and a Traefik
// that did not start says why. Before the fix both lines carried no cause at all.
func TestApplyFailureCauseIsLogged(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		h := newHarness(t, "")
		h.deps.Store = cache.NewStore(renameFailFS{FS: fsx.OS(), suffix: "/dynamic/tierplan.yml", err: syscall.EXDEV}, h.root)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		lo, done := h.runUntilChannel(ctx)

		b := validBundle(t, 1, "app.example.test")
		if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, b), testInstance); reason != ingressbundle.ReasonApplyFailed {
			t.Fatalf("Handle reason = %q, want apply_failed", reason)
		}
		line, ok := h.logLine("bundle rejected")
		if !ok {
			t.Fatalf("no \"bundle rejected\" line in the log:\n%s", h.logs.String())
		}
		cause := fmt.Sprint(line["error"])
		if !strings.Contains(cause, "step dynamic") || !strings.Contains(cause, syscall.EXDEV.Error()) {
			t.Errorf("the rejection logged error %q, want the apply step and %q", cause, syscall.EXDEV.Error())
		}
		cancel()
		if err := awaitRun(t, done); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("a verify code needs no cause", func(t *testing.T) {
		h := newHarness(t, "")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		lo, done := h.runUntilChannel(ctx)

		bad := refusedBundle(t, 1, "app.example.test")
		if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, bad), testInstance); reason != ingressbundle.ReasonTLSOptions {
			t.Fatalf("Handle reason = %q, want tls_options", reason)
		}
		line, ok := h.logLine("bundle rejected")
		if !ok {
			t.Fatalf("no \"bundle rejected\" line in the log:\n%s", h.logs.String())
		}
		if v, set := line["error"]; set && v != nil {
			t.Errorf("a verify rejection logged error %v, want none", v)
		}
		cancel()
		if err := awaitRun(t, done); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("traefik start", func(t *testing.T) {
		h := newHarness(t, "")
		h.traefik.failStarts.Store(1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		lo, done := h.runUntilChannel(ctx)

		b := validBundle(t, 1, "app.example.test")
		if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, b), testInstance); reason != ingressbundle.ReasonApplyFailed {
			t.Fatalf("Handle reason = %q, want apply_failed", reason)
		}
		line, ok := h.logLine("bundle is on disk but Traefik did not start")
		if !ok {
			t.Fatalf("no start-failure line in the log:\n%s", h.logs.String())
		}
		if cause := fmt.Sprint(line["error"]); !strings.Contains(cause, errTraefikStart.Error()) {
			t.Errorf("the start failure logged error %q, want %q", cause, errTraefikStart)
		}
		cancel()
		if err := awaitRun(t, done); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
}

// dialOptions makes the harness's fake Dial keep the channel options the supervisor hands it, so a test can
// call hello's AppliedID callback as a real Dial would. The returned function is valid after lo.Dial ran.
func dialOptions(h *harness) func() channel.Options {
	var got channel.Options
	h.deps.Dial = func(_ context.Context, o channel.Options, _ channel.Credentials) (*channel.Conn, error) {
		got = o
		return nil, errNoDial
	}
	return func() channel.Options { return got }
}

// TestFailedStartIsRetriedAndNotReportedApplied is WR-A-03, first path: the first bundle of a node with no
// cache is on disk but Traefik does not start. The answer is apply_failed, hello must not claim the bundle
// (the server would then never resend it), and the next health tick starts Traefik, because no frame will
// come to do it. Before the fix hello reported the bundle and nothing retried the start.
func TestFailedStartIsRetriedAndNotReportedApplied(t *testing.T) {
	h := newHarness(t, "")
	h.traefik.failStarts.Store(1)
	opts := dialOptions(h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lo, done := h.runUntilChannel(ctx)

	b := validBundle(t, 1, "app.example.test")
	if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, b), testInstance); reason != ingressbundle.ReasonApplyFailed {
		t.Fatalf("Handle reason = %q, want apply_failed", reason)
	}
	if h.state.Applied() {
		t.Fatal("readiness is marked applied although Traefik never started")
	}
	if _, err := lo.Dial(ctx); err != errNoDial {
		t.Fatalf("lo.Dial = %v, want the fake's error", err)
	}
	if got := opts().AppliedID(); got != "" {
		t.Fatalf("hello's applied ID = %q while Traefik is not running, want none, so the server resends the bundle", got)
	}
	// A health tick tries the start again; the second Start succeeds, and now the node is whole.
	lo.Session.Health(ctx)
	if got := h.traefik.starts.Load(); got != 2 {
		t.Fatalf("Traefik Start was called %d times, want 2 (the failure and the retry on the health tick)", got)
	}
	if !h.state.Applied() {
		t.Fatal("readiness is not marked applied after the retried start")
	}
	if _, id := lo.Session.Health(ctx); id != b.ID {
		t.Fatalf("health's applied ID = %q after the start, want %q", id, b.ID)
	}
	if got := opts().AppliedID(); got != b.ID {
		t.Fatalf("hello's applied ID = %q after the start, want %q", got, b.ID)
	}
	if got := h.traefik.starts.Load(); got != 2 {
		t.Fatalf("Traefik Start was called %d times, want 2: a started Traefik is not started again", got)
	}
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestCommitFailureStillStartsTraefik is WR-A-03, second path: the commit rename fails after the dynamic
// file is live, so Accept answers apply_failed, but the bundle is what Traefik should read. Before the fix
// handle returned before ensureTraefik, so a node with no cache never started Traefik for it.
func TestCommitFailureStillStartsTraefik(t *testing.T) {
	h := newHarness(t, "")
	h.deps.Store = cache.NewStore(renameFailFS{FS: fsx.OS(), suffix: "/state/applied.json", err: syscall.EIO}, h.root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lo, done := h.runUntilChannel(ctx)

	b := validBundle(t, 1, "app.example.test")
	if _, reason := lo.Session.Handle(ctx, frameFor(t, testInstance, b), testInstance); reason != ingressbundle.ReasonApplyFailed {
		t.Fatalf("Handle reason = %q, want apply_failed (the commit failed)", reason)
	}
	if got := h.traefik.starts.Load(); got != 1 {
		t.Fatalf("Traefik Start was called %d times, want 1: the bundle is live on the volume", got)
	}
	if !h.state.Applied() {
		t.Fatal("readiness is not marked applied although the live bundle is being served")
	}
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
