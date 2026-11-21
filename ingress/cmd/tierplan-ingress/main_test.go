// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/config"
	"github.com/ianglennon/tierplan/ingress/internal/version"
)

// testEnv is the smallest environment the supervisor starts with.
func testEnv(k string) string {
	if k == config.EnvURL {
		return "wss://tierplan.example.test/api/ingress"
	}
	return ""
}

// harness records what run did.
type harness struct {
	stdout, stderr bytes.Buffer
	nnpCalls       int
	nnpErr         error
	serveCalls     int
	serveErr       error
	serveCfg       config.Config
	serveFn        func(ctx context.Context, log *slog.Logger)
	order          []string
}

func (h *harness) deps(args ...string) deps {
	return deps{
		args:   args,
		getenv: testEnv,
		stdout: &h.stdout,
		stderr: &h.stderr,
		nnp: func() error {
			h.nnpCalls++
			h.order = append(h.order, "nnp")
			return h.nnpErr
		},
		serve: func(ctx context.Context, cfg config.Config, log *slog.Logger) error {
			h.serveCalls++
			h.order = append(h.order, "serve")
			h.serveCfg = cfg
			if h.serveFn != nil {
				h.serveFn(ctx, log)
			}
			return h.serveErr
		},
	}
}

func TestVersionPrintsTheVersion(t *testing.T) {
	var h harness
	d := h.deps("--version")
	if got := run(d); got != 0 {
		t.Fatalf("run = %d, want 0", got)
	}
	if got, want := h.stdout.String(), version.Version+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if h.nnpCalls != 0 || h.serveCalls != 0 {
		t.Fatalf("--version touched the node: nnp=%d serve=%d", h.nnpCalls, h.serveCalls)
	}
}

func TestHealthcheckExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		closed bool
		want   int
	}{
		{"ready", http.StatusOK, false, 0},
		{"not ready", http.StatusServiceUnavailable, false, 1},
		{"closed port", 0, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			url := ts.URL + "/readyz"
			if tc.closed {
				ts.Close()
			} else {
				defer ts.Close()
			}
			var h harness
			d := h.deps("healthcheck")
			d.readyURL = url
			if got := run(d); got != tc.want {
				t.Fatalf("run = %d, want %d (stderr %q)", got, tc.want, h.stderr.String())
			}
			if tc.want == 0 && (h.stdout.Len() != 0 || h.stderr.Len() != 0) {
				t.Fatalf("a healthy check printed stdout %q stderr %q", h.stdout.String(), h.stderr.String())
			}
			if h.nnpCalls != 0 || h.serveCalls != 0 {
				t.Fatalf("healthcheck touched the node: nnp=%d serve=%d", h.nnpCalls, h.serveCalls)
			}
		})
	}
}

func TestConfigErrorExitsTwo(t *testing.T) {
	var h harness
	d := h.deps()
	d.getenv = func(string) string { return "" }
	if got := run(d); got != 2 {
		t.Fatalf("run = %d, want 2", got)
	}
	if got := h.stderr.String(); !strings.HasPrefix(got, "tierplan-ingress: "+config.EnvURL) {
		t.Fatalf("stderr = %q, want it to start with `tierplan-ingress: %s`", got, config.EnvURL)
	}
	if h.serveCalls != 0 {
		t.Fatalf("serve ran with a configuration error")
	}
}

func TestUnknownSubcommandExitsTwoWithUsage(t *testing.T) {
	var h harness
	if got := run(h.deps("frobnicate")); got != 2 {
		t.Fatalf("run = %d, want 2", got)
	}
	if !strings.Contains(h.stderr.String(), "usage: tierplan-ingress") {
		t.Fatalf("stderr = %q, want a usage line", h.stderr.String())
	}
	if h.nnpCalls != 0 || h.serveCalls != 0 {
		t.Fatalf("an unknown subcommand touched the node")
	}
}

func TestServeRunsAfterNoNewPrivsWithAJSONLogger(t *testing.T) {
	var h harness
	if got := run(h.deps()); got != 0 {
		t.Fatalf("run = %d, want 0 (stderr %q)", got, h.stderr.String())
	}
	if want := []string{"nnp", "serve"}; strings.Join(h.order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", h.order, want)
	}
	if h.serveCfg.URL != "wss://tierplan.example.test/api/ingress" {
		t.Fatalf("serve got URL %q", h.serveCfg.URL)
	}
	first, _, _ := strings.Cut(h.stdout.String(), "\n")
	var line map[string]any
	if err := json.Unmarshal([]byte(first), &line); err != nil {
		t.Fatalf("the first stdout line is not JSON: %q (%v)", first, err)
	}
	if line["msg"] != "starting tierplan-ingress" || line["version"] != version.Version {
		t.Fatalf("starting line = %v, want msg and version", line)
	}
}

func TestServeLoggerWritesJSONToStdout(t *testing.T) {
	var h harness
	h.serveFn = func(_ context.Context, log *slog.Logger) { log.Info("from serve", "k", "v") }
	_ = run(h.deps())
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &line); err != nil || line["msg"] != "from serve" {
		t.Fatalf("serve's logger did not write JSON to stdout: %q", h.stdout.String())
	}
}

func TestNoNewPrivsFailureStopsBeforeServe(t *testing.T) {
	var h harness
	h.nnpErr = errors.New("prctl refused")
	if got := run(h.deps()); got != 1 {
		t.Fatalf("run = %d, want 1", got)
	}
	if h.serveCalls != 0 {
		t.Fatalf("serve ran although no-new-privileges could not be set")
	}
	if !strings.Contains(h.stderr.String(), "no-new-privileges") {
		t.Fatalf("stderr = %q, want it to name no-new-privileges", h.stderr.String())
	}
}

func TestServeErrorExitsOne(t *testing.T) {
	var h harness
	h.serveErr = errors.New("cannot write the volume")
	if got := run(h.deps()); got != 1 {
		t.Fatalf("run = %d, want 1", got)
	}
	if !strings.Contains(h.stdout.String(), "cannot write the volume") {
		t.Fatalf("the serve error is not in the log: %q", h.stdout.String())
	}
}

// TestSIGTERMEndsTheServeContext: as PID 1 the process ignores a signal it has no handler for, so run must
// install one. The test signals its own process once serve is running.
func TestSIGTERMEndsTheServeContext(t *testing.T) {
	var h harness
	h.serveFn = func(ctx context.Context, _ *slog.Logger) {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("signal self: %v", err)
			return
		}
		<-ctx.Done()
	}
	if got := run(h.deps()); got != 0 {
		t.Fatalf("run = %d, want 0 after SIGTERM", got)
	}
}
