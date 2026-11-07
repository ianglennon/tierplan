// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gateServer builds a server from env with a data directory of its own, closed with the test.
func gateServer(t *testing.T, env map[string]string) *Server {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
	srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// plainFrom serves one request through the server's handler as a peer at remote, over TLS or not.
func plainFrom(srv *Server, method, target, remote string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remote
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

const nonLoopbackPeer = "203.0.113.9:40000"

func TestGateRedirectsAPlainRequestToThePublicURL(t *testing.T) {
	srv := gateServer(t, map[string]string{
		"TIERPLAN_TLS_SELF_SIGNED": "true",
		"TIERPLAN_PUBLIC_URL":      "https://tierplan.example.com:8443/tp",
	})
	if !srv.https.Live() {
		t.Fatal("TLS is not live")
	}
	host := func(r *http.Request) { r.Host = "evil.test" }

	rec := plainFrom(srv, http.MethodGet, "/tp/api/version?x=1&y=%2F", nonLoopbackPeer, host)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "https://tierplan.example.com:8443/tp/api/version?x=1&y=%2F"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want none", got)
	}

	// The unprefixed form lands on the same address below the base path.
	rec = plainFrom(srv, http.MethodGet, "/api/version?x=1", nonLoopbackPeer, host)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("unprefixed status = %d, want 307", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "https://tierplan.example.com:8443/tp/api/version?x=1"; got != want {
		t.Errorf("unprefixed Location = %q, want %q", got, want)
	}

	// A POST is redirected with 307 too, which keeps the method and the body.
	rec = plainFrom(srv, http.MethodPost, "/tp/api/auth/login", nonLoopbackPeer, nil)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("POST status = %d, want 307", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "https://tierplan.example.com:8443/tp/api/auth/login"; got != want {
		t.Errorf("POST Location = %q, want %q", got, want)
	}

	// A request that arrived over TLS is served: following the Location is not redirected again.
	rec = plainFrom(srv, http.MethodGet, "/tp/api/version", nonLoopbackPeer, func(r *http.Request) { r.TLS = &tls.ConnectionState{} })
	if rec.Code == http.StatusTemporaryRedirect || rec.Header().Get("Location") != "" {
		t.Errorf("over TLS: status = %d, Location = %q, want it served", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Body.String(), "version") {
		t.Errorf("over TLS: body = %q, want the version document", rec.Body.String())
	}
}
