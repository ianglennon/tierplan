// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/webui"
)

const indexHTML = `<!doctype html><div id="root"></div>`

func newRouter(t *testing.T) *httpx.Router {
	t.Helper()
	site := fstest.MapFS{"index.html": {Data: []byte(indexHTML)}}
	r := httpx.New(httpx.Options{Fallback: webui.Handler(site)})
	api.RegisterSystem(r)
	return r
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestVersionIsPublicAndComplete(t *testing.T) {
	rec := get(t, newRouter(t).Handler(), "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "commit", "display", "sourceUrl", "sourceTreeUrl"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing field %q in %s", k, rec.Body)
		}
	}
	if _, ok := body["$schema"]; ok {
		t.Error("response must not carry a $schema property")
	}
}

func TestUnknownAPIPathIsJSONProblem(t *testing.T) {
	rec := get(t, newRouter(t).Handler(), "/api/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", ct)
	}
	if strings.Contains(rec.Body.String(), "root") {
		t.Fatal("API 404 must never return the SPA index")
	}
}

func TestSPAFallback(t *testing.T) {
	rec := get(t, newRouter(t).Handler(), "/login")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("/login = %d %q, want SPA index", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index Cache-Control = %q, want no-cache", cc)
	}
}

func TestNoHumaDefaultRoutes(t *testing.T) {
	h := newRouter(t).Handler()
	for _, p := range []string{"/openapi", "/openapi.json", "/openapi.yaml", "/docs", "/schemas/x"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404; body %s", p, rec.Code, rec.Body)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newRouter(t).Handler()
	for _, p := range []string{"/api/version", "/api/nope", "/login"} {
		rec := get(t, h, p)
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s CSP %q missing %q", p, csp, want)
			}
		}
		for _, dir := range strings.Split(csp, ";") {
			if strings.HasPrefix(strings.TrimSpace(dir), "script-src") && strings.Contains(dir, "unsafe") {
				t.Errorf("%s script-src must not be relaxed: %q", p, csp)
			}
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s missing nosniff", p)
		}
		if rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s missing Referrer-Policy", p)
		}
	}
}

func TestNonPublicRequirementIsDeniedByDefault(t *testing.T) {
	r := newRouter(t)
	httpx.Register(r, huma.Operation{
		OperationID: "secret", Method: http.MethodGet, Path: "/api/secret",
	}, rbac.Requirement{Perm: "x.read", Scope: rbac.ScopeGlobal},
		func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })

	rec := get(t, r.Handler(), "/api/secret")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	found := false
	for _, rt := range r.Routes() {
		if rt.Path == "/api/secret" && rt.Req.Perm == "x.read" {
			found = true
		}
	}
	if !found {
		t.Error("route registry does not list /api/secret")
	}
}

func TestZeroRequirementPanics(t *testing.T) {
	r := newRouter(t)
	defer func() {
		if recover() == nil {
			t.Fatal("registering a zero Requirement must panic")
		}
	}()
	httpx.Register(r, huma.Operation{
		OperationID: "oops", Method: http.MethodGet, Path: "/api/oops",
	}, rbac.Requirement{},
		func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })
}
