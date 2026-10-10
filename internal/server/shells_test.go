// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// The server's shutdown ends every open browser shell (plan 04-22): http.Server.Shutdown
// does not track hijacked connections, so without the hook a WebSocket would outlive it.
func TestBuildShutdownEndsAnOpenShellTellsItsBrowserWhyAndAuditsTheEnd(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "node-1", Containers: []dockertest.FakeContainer{
		{ID: "aaaaaaaaaaaa11111111111111111111111111111111111111111111111111", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
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
	added, err := srv.Environments().AddLocal(ctx, audit.SystemCLI, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); srv.monitor.Status(added.ID).State != envs.StateOnline; {
		if time.Now().After(deadline) {
			t.Fatal("the environment never came online")
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

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(client.CloseIdleConnections)
	loginReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("Origin", base)
	loginResp, err := client.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		CSRFToken string `json:"csrfToken"`
	}
	err = json.NewDecoder(loginResp.Body).Decode(&login)
	_ = loginResp.Body.Close()
	if err != nil || loginResp.StatusCode != http.StatusOK || len(loginResp.Cookies()) != 1 || login.CSRFToken == "" {
		t.Fatalf("login = %d, %v", loginResp.StatusCode, err)
	}
	cookie := loginResp.Cookies()[0]

	openReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/env/"+added.ID+"/containers/web/shell", strings.NewReader(`{"shell":"auto","cols":80,"rows":24}`))
	openReq.Header.Set("Content-Type", "application/json")
	openReq.Header.Set("Origin", base)
	openReq.Header.Set("X-CSRF-Token", login.CSRFToken)
	openReq.AddCookie(cookie)
	openResp, err := client.Do(openReq)
	if err != nil {
		t.Fatal(err)
	}
	var opened struct {
		SessionID string `json:"sessionId"`
		WSPath    string `json:"wsPath"`
	}
	err = json.NewDecoder(openResp.Body).Decode(&opened)
	_ = openResp.Body.Close()
	if err != nil || openResp.StatusCode != http.StatusCreated || opened.WSPath == "" {
		t.Fatalf("open shell = %d, %v", openResp.StatusCode, err)
	}

	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	conn, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(base, "http")+opened.WSPath, &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPHeader: http.Header{"Cookie": {cookie.String()}, "Origin": {base}},
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if err := conn.Write(dctx, websocket.MessageBinary, []byte("echo up\n")); err != nil {
		t.Fatal(err)
	}
	for got := ""; !strings.Contains(got, "echo up"); {
		typ, data, err := conn.Read(dctx)
		if err != nil {
			t.Fatalf("waiting for the shell's echo: %v", err)
		}
		if typ == websocket.MessageBinary {
			got += string(data)
		}
	}

	stop()
	var reason string
	for reason == "" {
		typ, data, err := conn.Read(dctx)
		if err != nil {
			t.Fatalf("the socket ended without telling why: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var c struct{ Type, Reason string }
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		if c.Type == "closed" {
			reason = c.Reason
		}
	}
	if reason != "server_shutdown" {
		t.Errorf("closed frame reason = %q, want server_shutdown", reason)
	}
	// Drain, so the client answers the server's close frame as a browser does.
	for {
		if _, _, err := conn.Read(dctx); err != nil {
			break
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return")
	}

	var closed, details string
	if err := srv.Store().Read.QueryRow(`SELECT action, COALESCE(details, '{}') FROM audit_log WHERE action = 'shell.closed'`).Scan(&closed, &details); err != nil {
		t.Fatalf("no shell.closed row: %v", err)
	}
	if !strings.Contains(details, `"reason":"server_shutdown"`) || !strings.Contains(details, opened.SessionID) {
		t.Errorf("shell.closed details = %s, want reason server_shutdown for session %s", details, opened.SessionID)
	}
}
