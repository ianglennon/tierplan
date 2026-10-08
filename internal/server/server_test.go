// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
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
	if err := Healthcheck(context.Background(), ":"+port, ""); err != nil {
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

	if err := Healthcheck(context.Background(), ":"+port, ""); err == nil {
		t.Fatal("Healthcheck after shutdown should fail")
	}
}

func TestHealthcheckRejectsNon200AndBadListen(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err := Healthcheck(context.Background(), ":"+port, ""); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("Healthcheck against a 503 = %v, want an error naming 503", err)
	}
	if err := Healthcheck(context.Background(), "no-port", ""); err == nil {
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

func TestBuildRejectsADockerHostThatIsNotAUnixSocketBeforeTouchingTheDataDirectory(t *testing.T) {
	for _, host := range []string{"tcp://1.2.3.4:2375", "ssh://host", "npipe:////./pipe/docker_engine", "/var/run/docker.sock"} {
		dir := t.TempDir() + "/data"
		cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir, "DOCKER_HOST": host})
		_, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
		if !errors.Is(err, engine.ErrUnsupportedDockerHost) || !strings.Contains(err.Error(), "unix") {
			t.Errorf("DOCKER_HOST=%s: Build = %v, want ErrUnsupportedDockerHost naming unix", host, err)
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Errorf("DOCKER_HOST=%s: a refused startup must not create the data directory (D-21)", host)
		}
	}
}

func TestBuildGuardsEveryRoute(t *testing.T) {
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if err := httpx.VerifyAllGuarded(srv.router); err != nil {
		t.Errorf("the assembled router has an unguarded route: %v", err)
	}
	// The openapi subcommand builds the same routes with no dependencies.
	if err := httpx.VerifyAllGuarded(NewRouter(RouterDeps{})); err != nil {
		t.Errorf("the dependency-free router has an unguarded route: %v", err)
	}
}

func TestBuildWritesASetupTokenWhileNoUserExistsAndServesTheGate(t *testing.T) {
	dir := t.TempDir() + "/data"
	var logs bytes.Buffer
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	token, err := auth.ReadTokenFile(dir)
	if err != nil {
		t.Fatalf("no setup token file after Build: %v", err)
	}
	if !strings.Contains(logs.String(), token) || !strings.Contains(logs.String(), "setup required") {
		t.Error("the setup token was not logged")
	}
	if !srv.SetupGate().NeedsSetup() {
		t.Error("NeedsSetup = false on a fresh data directory")
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	body, err := http.Post(ts.URL+"/api/setup/token", "application/json", strings.NewReader(`{"token":"`+token+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Body.Close()
	if body.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/setup/token = %d, want 200", body.StatusCode)
	}
}

// TestBuildSignsInWithTheSessionAuthenticatorAndCSRFGuard drives the assembled
// handler: the placeholders of earlier plans are gone, so a user can sign in,
// is recognised from the cookie, and a mutation without the CSRF token is
// refused (plan 12).
func TestBuildSignsInWithTheSessionAuthenticatorAndCSRFGuard(t *testing.T) {
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	const password = "correct horse battery"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := srv.Store().Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	if err := srv.Store().Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: hash, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, srv.Store().Q, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	do := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := do(http.MethodGet, "/api/me", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me without a session: %d, want 401", rec.Code)
	}
	rec := do(http.MethodPost, "/api/auth/login", `{"username":"admin","password":"`+password+`"}`, nil, "")
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()[0]
	meRec := do(http.MethodGet, "/api/me", "", cookie, "")
	if meRec.Code != http.StatusOK {
		t.Fatalf("/api/me with the cookie: %d %s", meRec.Code, meRec.Body)
	}
	// The granted user is an instance admin, and /api/me lists every global
	// permission and every environment permission under "*" (D-01, D-03).
	if u, err := srv.Store().RQ.GetUserByID(ctx, "u1"); err != nil || u.InstanceAdmin != 1 {
		t.Fatalf("the first admin: instance_admin = %d (%v), want 1", u.InstanceAdmin, err)
	}
	var me struct {
		Permissions struct {
			Global       []string            `json:"global"`
			Environments map[string][]string `json:"environments"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(meRec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	var wantGlobal, wantEnv []string
	for _, p := range rbac.All() {
		if p.Scope == rbac.ScopeGlobal {
			wantGlobal = append(wantGlobal, string(p.Name))
		} else {
			wantEnv = append(wantEnv, string(p.Name))
		}
	}
	slices.Sort(wantGlobal)
	slices.Sort(wantEnv)
	if !slices.Equal(me.Permissions.Global, wantGlobal) || !slices.Equal(me.Permissions.Environments["*"], wantEnv) {
		t.Errorf("/api/me permissions = %+v, want global %v and * %v", me.Permissions, wantGlobal, wantEnv)
	}
	if rec := do(http.MethodPost, "/api/auth/logout", "", cookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without the CSRF token: %d, want 403", rec.Code)
	}
	var got struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(do(http.MethodGet, "/api/me", "", cookie, "").Body.Bytes(), &got); err != nil || got.CSRFToken == "" {
		t.Fatalf("no CSRF token from /api/me: %v", err)
	}
	if rec := do(http.MethodPost, "/api/auth/logout", "", cookie, got.CSRFToken); rec.Code != http.StatusNoContent {
		t.Fatalf("logout with the CSRF token: %d, want 204", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/me", "", cookie, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me after logout: %d, want 401", rec.Code)
	}
}

// A reverse proxy that is not in BAYPLAN_TRUSTED_PROXIES shows up in the bell, and
// one that is does not (plan 02-25, D-51, D-53).
func TestBuildReportsForwardedHeadersFromAPeerThatIsNotATrustedProxy(t *testing.T) {
	build := func(trusted string) (*Server, *http.Cookie) {
		env := map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}
		if trusted != "" {
			env["BAYPLAN_TRUSTED_PROXIES"] = trusted
		}
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		const password = "correct horse battery"
		hash, err := auth.HashPassword(password)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		tx, err := srv.Store().Write.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		now := time.Now().UnixNano()
		if err := srv.Store().Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
			ID: "u1", Username: "admin", PasswordHash: hash, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := rbac.GrantInstanceAdminTx(ctx, tx, srv.Store().Q, "u1"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
			t.Fatalf("login: %d %s", rec.Code, rec.Body)
		}
		return srv, rec.Result().Cookies()[0]
	}
	warnings := func(srv *Server, cookie *http.Cookie, forwarded bool) []string {
		req := httptest.NewRequest(http.MethodGet, "/api/warnings", nil)
		req.AddCookie(cookie)
		if forwarded {
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var body struct {
			Items []struct {
				ID string `json:"id"`
				IP string `json:"ip"`
			} `json:"items"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("GET /api/warnings: %d %s", rec.Code, rec.Body)
		}
		var out []string
		for _, it := range body.Items {
			if strings.HasPrefix(it.ID, "proxy_") {
				out = append(out, it.ID+"@"+it.IP)
			}
		}
		return out
	}

	srv, cookie := build("")
	if got := warnings(srv, cookie, false); len(got) != 0 {
		t.Fatalf("without any forwarded header: %v, want no proxy warning", got)
	}
	// httptest requests come from 192.0.2.1. The first request carries the header
	// and is recorded as it passes; the second one reads it back.
	warnings(srv, cookie, true)
	if got := warnings(srv, cookie, false); len(got) != 1 || got[0] != "proxy_untrusted_forwarded@192.0.2.1" {
		t.Fatalf("after a request with X-Forwarded-For from an untrusted peer: %v, want proxy_untrusted_forwarded naming 192.0.2.1", got)
	}

	srv, cookie = build("192.0.2.0/24")
	warnings(srv, cookie, true)
	if got := warnings(srv, cookie, false); len(got) != 0 {
		t.Fatalf("from a trusted proxy: %v, want no proxy warning", got)
	}
}

// Environments (plan 16): the local host is managed only after an admin opts
// in, and a restart picks the environment up again and resumes monitoring it.
func TestBuildAddsNoEnvironmentAndNeverContactsDockerUntilAnAdminOptsIn(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "node-1"})
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir, "DOCKER_HOST": "unix://" + fake.SocketPath()})

	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	list, err := srv.Environments().List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("a fresh start lists %v (%v), want no environments (D-19)", list, err)
	}
	if got := fake.Requests(); len(got) != 0 {
		t.Fatalf("startup with no environment talked to Docker: %v", got)
	}

	added, err := srv.Environments().AddLocal(context.Background(), audit.SystemCLI, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	// A restart: the row is in the database, the monitor is started again, and
	// {envId} resolves against the database.
	srv, err = Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := srv.Environments().Get(context.Background(), added.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.State == envs.StateOnline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted server never monitored the environment: %+v", got.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	env, err := srv.Environments().Resolve(context.Background(), added.ID)
	if err != nil || env.Name != "node-1" {
		t.Errorf("Resolve = %+v, %v", env, err)
	}
	if _, err := srv.Environments().Resolve(context.Background(), "nope"); !errors.Is(err, httpx.ErrEnvNotFound) {
		t.Errorf("Resolve(unknown) = %v, want ErrEnvNotFound", err)
	}
}

// Events (plan 21): the hub follows the environments the monitor follows, and an
// open event stream does not hold a graceful shutdown up (Pitfall 8).
func TestBuildFeedsEventsPerEnvironmentAndShutdownDoesNotWaitForAnOpenStream(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "node-1", Events: []dockertest.FakeEvent{
		{Type: "container", Action: "start", ActorID: "c-1", TimeNano: time.Now().Add(-time.Minute).UnixNano(), Attributes: map[string]string{"name": "web"}},
	}})
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data", "DOCKER_HOST": "unix://" + fake.SocketPath()})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	const password = "correct horse battery"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := srv.Store().Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	if err := srv.Store().Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: hash, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, srv.Store().Q, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Adding the environment starts its feed with no extra wiring: the hub
	// watches the monitor.
	added, err := srv.Environments().AddLocal(ctx, audit.SystemCLI, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(srv.Hub().Recent(added.ID, 10)) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the hub never received the daemon's buffered event")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.serve(sctx, ln) }()

	// No keep-alives: a pooled client can dial a spare connection while the
	// login connection is on its way back to the pool, and Shutdown waits up to
	// 5s for a connection that never sent a request, which is not the stream
	// this test is about.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(client.CloseIdleConnections)

	loginReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := client.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK || len(loginResp.Cookies()) != 1 {
		t.Fatalf("login = %d", loginResp.StatusCode)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/env/"+added.ID+"/events/stream", nil)
	req.AddCookie(loginResp.Cookies()[0])
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "event: docker") || !strings.Contains(string(buf[:n]), `"actorId":"c-1"`) {
		t.Fatalf("first read = %q, %v, want the backlog frame", buf[:n], err)
	}

	start := time.Now()
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v with a stream open, want a clean shutdown", err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("shutdown took %v with a stream open, want it not to wait for the stream", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return with a stream open")
	}

	// Closing the hub dropped every feed with its events.
	if got := srv.Hub().Recent(added.ID, 1); len(got) != 0 {
		t.Errorf("the hub still holds events after shutdown: %v", got)
	}
}

func TestBuildTakesTheSelfContainerIDFromTheOption(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler), WithSelfContainerID(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if got := srv.Environments().SelfContainerID(); got != id {
		t.Fatalf("SelfContainerID = %q, want the ID passed to WithSelfContainerID", got)
	}

	// Without the option nothing is badged, whatever /proc says: Build no longer probes it.
	bare, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bare.Close() })
	if got := bare.Environments().SelfContainerID(); got != "" {
		t.Fatalf("SelfContainerID = %q without the option, want empty", got)
	}
}

// WR-04: an anonymous flood must not hold the single write connection, and every
// refusal must still be accounted for.
func TestBuildAggregatesAnAnonymousDenialFloodAndKeepsSignInWorking(t *testing.T) {
	dir := t.TempDir() + "/data"
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = srv.Close()
		}
	})

	count := func(s *store.Store, action string) (n int) {
		t.Helper()
		if err := s.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for i := range 200 {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.RemoteAddr = "198.51.100.7:40000"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: /api/me = %d, want 401", i, rec.Code)
		}
	}
	if got := count(srv.Store(), audit.ActionPermissionDenied); got > 20 {
		t.Fatalf("permission.denied rows during the flood = %d, want at most 20", got)
	}

	// A different address signs in while the flood's address is over budget.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"nobody","password":"not a real password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.9:40000"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in from another address = %d, want 401", rec.Code)
	}
	if got := count(srv.Store(), audit.ActionAuthLoginFailed); got != 1 {
		t.Fatalf("auth.login_failed rows = %d, want the failed sign-in written at once", got)
	}

	// Closing flushes the pending count before the database closes.
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var details string
	if err := st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = ? ORDER BY seq DESC LIMIT 1`, audit.ActionPermissionDenied).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var det map[string]string
	if err := json.Unmarshal([]byte(details), &det); err != nil || det["suppressed"] == "" {
		t.Fatalf("last permission.denied details = %q (%v), want a summary with a suppressed count", details, err)
	}
	if n, err := strconv.Atoi(det["suppressed"]); err != nil || n < 150 || n > 180 {
		// 200 refusals less the burst of 20, less at most a few refills on a slow machine.
		t.Fatalf("suppressed = %q, want about 180", det["suppressed"])
	}
}

// hstsOf serves one request through the built server's handler and returns the
// Strict-Transport-Security header, for a request that arrived over TLS or not.
func hstsOf(t *testing.T, env map[string]string, overTLS bool) string {
	t.Helper()
	env["BAYPLAN_DATA_DIR"] = t.TempDir() + "/data"
	srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if overTLS {
		req.TLS = &tls.ConnectionState{}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Header().Get("Strict-Transport-Security")
}

func TestHSTSIsNeverSentWithTheSelfSignedCertificate(t *testing.T) {
	// A browser that pinned the generated certificate could not click through
	// a regenerated one (WR-09), so the header is withheld in that mode.
	if got := hstsOf(t, map[string]string{"BAYPLAN_TLS_SELF_SIGNED": "true"}, true); got != "" {
		t.Errorf("self-signed: Strict-Transport-Security = %q, want none", got)
	}
	if got := hstsOf(t, map[string]string{"BAYPLAN_TLS_SELF_SIGNED": "true", "BAYPLAN_PUBLIC_URL": "https://bayplan.example"}, true); got != "" {
		t.Errorf("self-signed with an https public URL: Strict-Transport-Security = %q, want none", got)
	}

	// A supplied certificate is the operator's own: HSTS is sent (D-37).
	src := t.TempDir()
	if _, err := TLSConfig(config.Config{DataDir: src, TLSSelfSigned: true}, "", "", nil); err != nil {
		t.Fatal(err)
	}
	supplied := map[string]string{
		"BAYPLAN_TLS_CERT_FILE": filepath.Join(src, "tls", "selfsigned.crt"),
		"BAYPLAN_TLS_KEY_FILE":  filepath.Join(src, "tls", "selfsigned.key"),
	}
	if got := hstsOf(t, supplied, true); got != "max-age=31536000" {
		t.Errorf("supplied certificate: Strict-Transport-Security = %q, want max-age=31536000", got)
	}

	// TLS ended in front of Bayplan: the request is plain but the public URL is https.
	if got := hstsOf(t, map[string]string{"BAYPLAN_PUBLIC_URL": "https://bayplan.example"}, false); got != "max-age=31536000" {
		t.Errorf("https public URL behind a proxy: Strict-Transport-Security = %q, want max-age=31536000", got)
	}
}

// healthyOn serves a 200 on ln and returns its port.
func healthyOn(t *testing.T, ln net.Listener) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	_ = ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestHealthcheckDialsTheConfiguredAddress(t *testing.T) {
	// WR-10, D-86: a container bound to one interface answers there and not on
	// 127.0.0.1, and the probe used to dial only 127.0.0.1.
	t.Run("a concrete IPv4 address", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.2:0")
		if err != nil {
			t.Skipf("cannot bind 127.0.0.2: %v", err)
		}
		port := healthyOn(t, ln)
		if err := Healthcheck(context.Background(), "127.0.0.2:"+port, ""); err != nil {
			t.Errorf("Healthcheck on the address the server is bound to: %v", err)
		}
	})
	t.Run("a concrete IPv6 address", func(t *testing.T) {
		ln, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Skipf("no IPv6 loopback: %v", err)
		}
		port := healthyOn(t, ln)
		if err := Healthcheck(context.Background(), "[::1]:"+port, ""); err != nil {
			t.Errorf("Healthcheck on [::1]: %v", err)
		}
	})
	t.Run("wildcard hosts dial loopback", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := healthyOn(t, ln)
		for _, listen := range []string{":" + port, "0.0.0.0:" + port, "[::]:" + port} {
			if err := Healthcheck(context.Background(), listen, ""); err != nil {
				t.Errorf("Healthcheck(%q): %v", listen, err)
			}
		}
	})
}

func shortReadTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := readTimeout
	readTimeout = d
	t.Cleanup(func() { readTimeout = old })
}

func serveForTest(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(h, nil)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestServerCutsOffARequestBodyThatNeverArrives(t *testing.T) {
	// WR-14, T-01-17: headers arrived in time, the promised body never does.
	// ReadHeaderTimeout no longer applies, so only ReadTimeout ends this.
	shortReadTimeout(t, 200*time.Millisecond)
	addr := serveForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusRequestTimeout)
	}))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	if _, err := conn.Read(buf); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("the server was still waiting for the body after 2s; there is no read timeout")
		}
		// The connection being closed is also a cut-off.
	}
}

func TestAStreamingResponseOutlivesTheReadTimeout(t *testing.T) {
	// The events feed is a bodyless GET that stays open for hours. Go clears
	// the read deadline once such a request is being served, so ReadTimeout
	// must not end it.
	shortReadTimeout(t, 200*time.Millisecond)
	const chunks = 10
	addr := serveForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, _ := w.(http.Flusher)
		for range chunks {
			_, _ = w.Write([]byte("x"))
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	resp, err := http.Get("http://" + addr + "/stream") //nolint:noctx // test against a loopback listener
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || len(b) != chunks {
		t.Fatalf("read %d of %d chunks, err %v: the stream was cut off by the read timeout", len(b), chunks, err)
	}
}

// CR-B-01: a request line or header block larger than maxHeaderBytes is refused
// before any handler (or any audit entry that keys on the path) sees it.
func TestServerRefusesAnOversizedRequestLine(t *testing.T) {
	called := false
	addr := serveForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /api/env/%s/summary HTTP/1.1\r\nHost: x\r\n\r\n", strings.Repeat("a", 200_000))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", resp.StatusCode)
	}
	if called {
		t.Fatal("the handler ran for an oversized request line")
	}
}
