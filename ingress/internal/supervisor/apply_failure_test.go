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
		<-done
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
		<-done
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
		<-done
	})
}
