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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/ingress/internal/config"
	"github.com/ianglennon/tierplan/ingress/internal/version"
	"github.com/ianglennon/tierplan/ingress/internal/vip"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
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
		// A path that never exists, so no test reads the real address-in-use flag of the machine it runs on.
		vipFlag: "/nonexistent-tierplan-test/address-in-use",
		args:    args,
		getenv:  testEnv,
		stdout:  &h.stdout,
		stderr:  &h.stderr,
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

// TestProbeHoldsWhileAddressInUse is D-11: while the helper's flag file exists the probe fails whatever the
// proxy says, so keepalived keeps the node in FAULT and it never claims an address another machine holds.
func TestProbeHoldsWhileAddressInUse(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "address-in-use")
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	probeWith := func(flagPath string) (int, *harness) {
		var h harness
		d := h.deps("probe")
		d.readyURL = ts.URL + "/readyz"
		d.vipFlag = flagPath
		d.getenv = func(k string) string {
			if k == ingressbundle.EnvVIPProbePort {
				return "7483"
			}
			return testEnv(k)
		}
		return run(d), &h
	}

	// No flag: the proxy answers 200 and the probe passes, silently, as before.
	if got, h := probeWith(flag); got != 0 || h.stdout.Len() != 0 || h.stderr.Len() != 0 {
		t.Fatalf("without the flag: run = %d, stdout %q stderr %q, want 0 and silence", got, h.stdout.String(), h.stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("the proxy was asked %d times without the flag, want 1", hits.Load())
	}

	// The flag: exit 1 and one short line, and the proxy is not even asked.
	if err := os.WriteFile(flag, []byte("address in use\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, h := probeWith(flag)
	if got != 1 {
		t.Fatalf("with the flag: run = %d, want 1 (stderr %q)", got, h.stderr.String())
	}
	if lines := strings.Split(strings.TrimSuffix(h.stderr.String(), "\n"), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "address in use") {
		t.Fatalf("stderr = %q, want one short line", h.stderr.String())
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("probe wrote to stdout: %q", h.stdout.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("the proxy was asked with the flag present (%d hits)", hits.Load())
	}

	// The flag gone again: the node may claim the address.
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	if got, _ := probeWith(flag); got != 0 {
		t.Fatalf("after the flag was removed: run = %d, want 0", got)
	}

	// A flag that cannot be looked up holds the node too: it must never claim on a guess.
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, h = probeWith(filepath.Join(file, "address-in-use"))
	if got != 1 || h.stdout.Len() != 0 || strings.Count(h.stderr.String(), "\n") != 1 {
		t.Fatalf("an unreadable flag: run = %d, stdout %q stderr %q, want 1 and one line", got, h.stdout.String(), h.stderr.String())
	}

	// The default path is the helper's, which is what the image uses.
	if vip.AddressInUseFlag != "/run/tierplan-vip/address-in-use" {
		t.Fatalf("AddressInUseFlag = %q", vip.AddressInUseFlag)
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

func TestProbeSubcommand(t *testing.T) {
	const port = "7483"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		closed  bool
		env     string
		setEnv  bool
		want    int
	}{
		{"ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, false, port, true, 0},
		{"not ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, false, port, true, 1},
		{"closed port", nil, true, port, true, 1},
		{"slower than the timeout", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(5 * time.Second): // wall-clock: a stall the probe's own deadline must cut short; released early by the request context
			case <-r.Context().Done():
			}
		}, false, port, true, 1},
		{"no port", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, false, "", false, 2},
		{"port is not a number", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, false, "x", true, 2},
		{"port 0", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, false, "0", true, 2},
		{"port above 65535", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, false, "65536", true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.handler
			if handler == nil {
				handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
			}
			ts := httptest.NewServer(handler)
			url := ts.URL + "/readyz"
			if tc.closed {
				ts.Close()
			} else {
				defer ts.Close()
			}
			var h harness
			d := h.deps("probe")
			d.readyURL = url
			d.getenv = func(k string) string {
				if k == ingressbundle.EnvVIPProbePort && tc.setEnv {
					return tc.env
				}
				return testEnv(k)
			}
			start := time.Now()
			got := run(d)
			if got != tc.want {
				t.Fatalf("run = %d, want %d (stderr %q)", got, tc.want, h.stderr.String())
			}
			// The upper bound only has to tell a working deadline from a missing one (the stall above
			// is 5 s), so it is set well clear of the 800 ms deadline to stay steady under -race load.
			if took := time.Since(start); took > 3*time.Second { // wall-clock: upper bound that catches a missing deadline; the stall is 5 s
				t.Fatalf("probe took %s, want its deadline to cut the 5 s stall short", took)
			}
			if tc.want == 0 && (h.stdout.Len() != 0 || h.stderr.Len() != 0) {
				t.Fatalf("a ready probe printed stdout %q stderr %q", h.stdout.String(), h.stderr.String())
			}
			if h.stdout.Len() != 0 {
				t.Fatalf("probe wrote to stdout: %q", h.stdout.String())
			}
			if h.nnpCalls != 0 || h.serveCalls != 0 {
				t.Fatalf("probe touched the node: nnp=%d serve=%d", h.nnpCalls, h.serveCalls)
			}
		})
	}
}

func TestProbeUsesThePublishedPortOnLoopback(t *testing.T) {
	// With no readyURL override the probe builds the URL from the port; a closed port on loopback is a 1,
	// and that the connection was to 127.0.0.1 is pinned by ready.PublishedReadyURL's own test.
	var h harness
	d := h.deps("probe")
	d.getenv = func(k string) string {
		if k == ingressbundle.EnvVIPProbePort {
			return "1"
		}
		return testEnv(k)
	}
	if got := run(d); got != 1 {
		t.Fatalf("run = %d, want 1 (stderr %q)", got, h.stderr.String())
	}
}

func TestProbeWithAnArgumentIsUnknown(t *testing.T) {
	var h harness
	if got := run(h.deps("probe", "extra")); got != 2 {
		t.Fatalf("run = %d, want 2", got)
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

func TestVIPEntrypointSubcommand(t *testing.T) {
	var h harness
	var got vip.RunDeps
	var ctxEnded bool
	d := h.deps("vip-entrypoint")
	d.vipRun = func(ctx context.Context, rd vip.RunDeps) int {
		got = rd
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("signal self: %v", err)
			return 99
		}
		<-ctx.Done()
		ctxEnded = true
		return 7
	}
	if code := run(d); code != 7 {
		t.Fatalf("run = %d, want the entrypoint's own status 7", code)
	}
	if !ctxEnded {
		t.Fatal("SIGTERM did not end the entrypoint's context")
	}
	if got.Getenv == nil || got.Stdout != &h.stdout || got.Stderr != &h.stderr {
		t.Fatalf("the entrypoint was not given the process's env, stdout and stderr: %+v", got)
	}
	if h.nnpCalls != 0 || h.serveCalls != 0 {
		t.Fatalf("vip-entrypoint touched the node: nnp=%d serve=%d (keepalived must keep its capabilities)", h.nnpCalls, h.serveCalls)
	}
}

func TestVIPEntrypointWithAnArgumentIsUnknown(t *testing.T) {
	var h harness
	called := false
	d := h.deps("vip-entrypoint", "extra")
	d.vipRun = func(context.Context, vip.RunDeps) int { called = true; return 0 }
	if got := run(d); got != 2 {
		t.Fatalf("run = %d, want 2", got)
	}
	if called {
		t.Fatal("the entrypoint ran with an extra argument")
	}
	if !strings.Contains(h.stderr.String(), "usage: tierplan-ingress") {
		t.Fatalf("stderr = %q, want a usage line", h.stderr.String())
	}
}
