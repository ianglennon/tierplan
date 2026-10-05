// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/web"
)

// TestBasePathInvariant runs the same assertions at the root and under a base
// path, over http and https, through the real router (D-50). The address in the
// agent install commands is built from the same base path (plan 02-09).
func TestBasePathInvariant(t *testing.T) {
	cases := []struct {
		name, publicURL, base string
		wantCookie, wantPath  string
	}{
		{"root over http", "http://bayplan.test", "", "bayplan_session", "/"},
		{"root over https", "https://bayplan.test", "", "__Host-bayplan_session", "/"},
		{"base over http", "http://bayplan.test/bayplan", "/bayplan", "bayplan_session", "/bayplan"},
		{"base over https", "https://bayplan.test/bayplan", "/bayplan", "__Secure-bayplan_session", "/bayplan"},
	}
	_, err := fs.Stat(web.FS(), "index.html")
	spaBuilt := err == nil
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"BAYPLAN_DATA_DIR":   t.TempDir() + "/data",
				"BAYPLAN_PUBLIC_URL": tc.publicURL,
			}
			if strings.HasPrefix(tc.publicURL, "http://") {
				// An http public URL for another host is a plain agent address that
				// only the development switch hands out (G-02-R4-3), and this test is
				// about the base path, so the http cases turn the switch on.
				env["BAYPLAN_AGENT_ALLOW_INSECURE"] = "true"
			}
			srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			if got := srv.Instance().BasePath(); got != tc.base {
				t.Fatalf("BasePath = %q, want %q", got, tc.base)
			}

			const password = "correct horse battery"
			addAdmin(t, srv, password)

			do := func(method, target, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, target, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", srv.Instance().PublicOrigin())
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				return rec
			}

			// The index checks need the built SPA embedded from web/dist. A checkout
			// without `make web` (the integration jobs) embeds only .gitkeep, so they
			// are skipped there; the base path of the API and cookies is still checked.
			if spaBuilt {
				// A proxy that does not strip: the index is served under the base.
				rec := do(http.MethodGet, tc.base+"/", "")
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s/ = %d", tc.base, rec.Code)
				}
				if want := `<meta name="bayplan-base" content="` + tc.base + `">`; !strings.Contains(rec.Body.String(), want) {
					t.Errorf("index lacks %s:\n%s", want, rec.Body)
				}
				// A proxy that strips: the same index at the root.
				if rec := do(http.MethodGet, "/", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bayplan-base") {
					t.Errorf("GET / = %d", rec.Code)
				}
				// The base with no trailing slash.
				if tc.base != "" {
					if rec := do(http.MethodGet, tc.base, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bayplan-base") {
						t.Errorf("GET %s = %d", tc.base, rec.Code)
					}
				}
			} else {
				t.Log("web/dist is not built (run make web); the SPA index checks are skipped")
			}

			// The API answers with and without the base.
			for _, p := range []string{tc.base + "/api/version", "/api/version"} {
				if rec := do(http.MethodGet, p, ""); rec.Code != http.StatusOK {
					t.Errorf("GET %s = %d, want 200", p, rec.Code)
				}
			}
			// The probes answer unprefixed.
			if rec := do(http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
				t.Errorf("GET /healthz = %d, want 200", rec.Code)
			}

			// Sign-in sets the cookie of the scope, scoped to the base.
			rec := do(http.MethodPost, tc.base+"/api/auth/login", `{"username":"admin","password":"`+password+`"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("login = %d %s", rec.Code, rec.Body)
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("login set %d cookies, want 1", len(cookies))
			}
			c := cookies[0]
			var session struct {
				CSRFToken string `json:"csrfToken"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil || session.CSRFToken == "" {
				t.Fatalf("login body has no csrfToken: %s (%v)", rec.Body, err)
			}
			if c.Name != tc.wantCookie || c.Path != tc.wantPath || c.Secure != strings.HasPrefix(tc.publicURL, "https://") || !c.HttpOnly {
				t.Fatalf("cookie = %+v, want name %q path %q", c, tc.wantCookie, tc.wantPath)
			}
			// ...and it authenticates, with the base and without it.
			for _, p := range []string{tc.base + "/api/me", "/api/me"} {
				req := httptest.NewRequest(http.MethodGet, p, nil)
				req.AddCookie(c)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("GET %s with the cookie = %d %s", p, rec.Code, rec.Body)
				}
			}

			// The address agents are told to dial carries the same base path, with
			// wss for https and ws for http, and the commands are built from it.
			req := httptest.NewRequest(http.MethodPost, tc.base+"/api/environments", strings.NewReader(`{"kind":"agent","name":"edge"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", srv.Instance().PublicOrigin())
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			req.AddCookie(c)
			rec = httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("add agent environment = %d %s", rec.Code, rec.Body)
			}
			var added struct {
				Install struct {
					AgentURL string `json:"agentUrl"`
					Swarm    struct {
						Command string `json:"command"`
					} `json:"swarm"`
				} `json:"install"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
				t.Fatal(err)
			}
			wantScheme := "wss://"
			if strings.HasPrefix(tc.publicURL, "http://") {
				wantScheme = "ws://"
			}
			if want := wantScheme + "bayplan.test" + tc.base + "/agent/connect"; added.Install.AgentURL != want {
				t.Errorf("install.agentUrl = %q, want %q", added.Install.AgentURL, want)
			}
			if !strings.HasSuffix(added.Install.AgentURL, tc.base+"/agent/connect") {
				t.Errorf("install.agentUrl = %q does not end in %q", added.Install.AgentURL, tc.base+"/agent/connect")
			}
			if !strings.Contains(added.Install.Swarm.Command, "BAYPLAN_AGENT_URL="+added.Install.AgentURL) {
				t.Errorf("the swarm command does not dial %q:\n%s", added.Install.AgentURL, added.Install.Swarm.Command)
			}
		})
	}
}

// addAdmin inserts an admin user the way the other server tests do.
func addAdmin(t *testing.T, srv *Server, password string) {
	t.Helper()
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
	if err := rbac.AssignGlobalRole(ctx, tx, srv.Store().Q, rbac.PrincipalUser, "u1", rbac.AdminRoleID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
