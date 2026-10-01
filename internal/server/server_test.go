// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
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

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
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
	// Exactly 32 bytes, as BAYPLAN_ENCRYPTION_KEY must decode to.
	const raw = "never-log-this-key-never-log-thi"
	secret := base64.StdEncoding.EncodeToString([]byte(raw))
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir, "BAYPLAN_ENCRYPTION_KEY": secret})

	var logs bytes.Buffer
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), raw) {
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
	t.Cleanup(func() { _ = srv.Close() })
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
	t.Cleanup(func() { _ = srv.Close() })
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

func TestBuildMigratesAndRecordsTheVersion(t *testing.T) {
	dir := t.TempDir() + "/data"
	var logs bytes.Buffer
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if _, err := os.Stat(dir + "/bayplan.db"); err != nil {
		t.Errorf("database file missing: %v", err)
	}
	if v, err := srv.Store().RQ.GetMeta(context.Background(), "app_version"); err != nil || v != "dev" {
		t.Errorf("app_version = %q, %v; want dev", v, err)
	}
	if !strings.Contains(logs.String(), "database ready") {
		t.Errorf("no migration log line: %s", logs.String())
	}
}

func TestBuildRefusesADatabaseNewerThanTheBinary(t *testing.T) {
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir})
	first, err := Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Store().Write.Exec("INSERT INTO goose_db_version (version_id, is_applied) VALUES (999, 1)"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, store.ErrDowngrade) {
		t.Fatalf("Build = %v, want ErrDowngrade", err)
	}
	for _, want := range []string{"migration failed", "version 999"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestBuildGeneratesTheKeyFileAndNeverLogsIt(t *testing.T) {
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir})

	var logs bytes.Buffer
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	keyPath := dir + "/bayplan.key"
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("no key file generated: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600", st.Mode().Perm())
	}
	raw, err := os.ReadFile(keyPath) //nolint:gosec // reading the file the test just generated
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), strings.TrimSpace(string(raw))) {
		t.Fatal("startup log leaks the generated key")
	}
	if !strings.Contains(logs.String(), "back it up") {
		t.Errorf("startup log lacks the key back-up warning: %s", logs.String())
	}
	if srv.Vault().KeySource() != "file" || srv.Audit() == nil {
		t.Errorf("vault source = %q, audit writer = %v", srv.Vault().KeySource(), srv.Audit())
	}
}

func TestBuildRejectsAMalformedKeyBeforeTouchingTheDatabase(t *testing.T) {
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir, "BAYPLAN_ENCRYPTION_KEY": "too-short"})
	_, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_ENCRYPTION_KEY") {
		t.Fatalf("Build = %v, want an error naming BAYPLAN_ENCRYPTION_KEY", err)
	}
	if _, statErr := os.Stat(dir + "/bayplan.db"); statErr == nil {
		t.Error("a malformed key must stop startup before the database is created")
	}
}

func TestBuildSeedsTheAdminRoleAndEveryRouteIsGuarded(t *testing.T) {
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	got, err := srv.Store().RQ.ListRolePermissions(context.Background(), rbac.AdminRoleID)
	if err != nil || len(got) != len(rbac.All()) {
		t.Errorf("admin holds %d permissions (%v), want %d", len(got), err, len(rbac.All()))
	}
	if err := httpx.VerifyAllGuarded(srv.router); err != nil {
		t.Errorf("the assembled router has an unguarded route: %v", err)
	}
	// The openapi subcommand builds the same routes with no dependencies.
	if err := httpx.VerifyAllGuarded(NewRouter(httpx.Options{}, api.SystemDeps{})); err != nil {
		t.Errorf("the dependency-free router has an unguarded route: %v", err)
	}
}
