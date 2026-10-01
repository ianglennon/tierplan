// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/bayplan/internal/config"
)

func testConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	fl := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.BindFlags(fl)
	cfg, err := config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // test against a loopback listener
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestBuildLogsRedactedConfigAndPreparesDataDir(t *testing.T) {
	const secret = "never-log-this-key"
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir, "BAYPLAN_ENCRYPTION_KEY": secret})

	var logs bytes.Buffer
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("startup log leaks the encryption key: %s", logs.String())
	}
	for _, want := range []string{"BAYPLAN_ENCRYPTION_KEY", "[redacted]", "BAYPLAN_LISTEN", "version"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("startup log lacks %q: %s", want, logs.String())
		}
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Errorf("data directory not created: %v", err)
	}
	if srv.DataDir().Path != dir {
		t.Errorf("DataDir().Path = %q, want %q", srv.DataDir().Path, dir)
	}
}

func TestBuildFailsWhenDataDirIsNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o400); err != nil { //nolint:gosec // a directory with no write bit
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // a directory needs the search bit to be removed
	_, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected Build to fail")
	}
}

func TestReadyzTurns503WhenTheDataDirDisappears(t *testing.T) {
	dir := t.TempDir() + "/data"
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if code, body := httpGet(t, ts.URL+"/readyz"); code != 200 || body != "ready" {
		t.Fatalf("/readyz = %d %q, want 200 ready", code, body)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if code, body := httpGet(t, ts.URL+"/readyz"); code != 503 || body != "not ready" {
		t.Fatalf("/readyz after removing the data dir = %d %q, want 503 not ready", code, body)
	}
	if code, _ := httpGet(t, ts.URL+"/healthz"); code != 200 {
		t.Fatalf("/healthz = %d, want 200", code)
	}
}

func TestRunServesThenShutsDownOnCancelAndHealthcheckFollows(t *testing.T) {
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	_, port, _ := net.SplitHostPort(listen)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()

	// Healthcheck succeeds while the server is up. The listen address may be
	// ":port" or "host:port"; only the port matters.
	if err := Healthcheck(context.Background(), ":"+port); err != nil {
		t.Fatalf("Healthcheck while up: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down")
	}

	if err := Healthcheck(context.Background(), ":"+port); err == nil {
		t.Fatal("Healthcheck after shutdown should fail")
	}
}

func TestHealthcheckRejectsNon200AndBadListen(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err := Healthcheck(context.Background(), ":"+port); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("Healthcheck against a 503 = %v, want an error naming 503", err)
	}
	if err := Healthcheck(context.Background(), "no-port"); err == nil {
		t.Error("Healthcheck with an unusable listen address should fail")
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsListenErrorsNamingTheEnvVar(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	srv, err := Build(context.Background(), testConfig(t, map[string]string{
		"BAYPLAN_DATA_DIR": t.TempDir() + "/data",
		"BAYPLAN_LISTEN":   busy.Addr().String(),
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	err = srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_LISTEN") {
		t.Fatalf("Run on a busy port = %v, want an error naming BAYPLAN_LISTEN", err)
	}
}
