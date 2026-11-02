// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/webui"
)

const indexHTML = `<!doctype html><div id="root"></div>`

func newRouter(t *testing.T) *httpx.Router {
	t.Helper()
	site := fstest.MapFS{"index.html": {Data: []byte(indexHTML)}}
	r := httpx.New(httpx.Options{Fallback: webui.Handler(site, nil)})
	api.RegisterSystem(r, api.SystemDeps{})
	return r
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestVersionIsPublicAndComplete(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	rec := get(t, newRouter(t).Handler(), "/login")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("/login = %d %q, want SPA index", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index Cache-Control = %q, want no-cache", cc)
	}
}

func TestNoHumaDefaultRoutes(t *testing.T) {
	t.Parallel()
	h := newRouter(t).Handler()
	for _, p := range []string{"/openapi", "/openapi.json", "/openapi.yaml", "/docs", "/schemas/x"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404; body %s", p, rec.Code, rec.Body)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
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

func TestNonPublicRequirementIsRefusedWithoutASession(t *testing.T) {
	t.Parallel()
	r := newRouter(t)
	httpx.Register(r, huma.Operation{
		OperationID: "secret", Method: http.MethodGet, Path: "/api/secret",
	}, rbac.Require(rbac.PermAuditRead),
		func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })

	rec := get(t, r.Handler(), "/api/secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: the default authenticator knows nobody", rec.Code)
	}
	found := false
	for _, rt := range r.Routes() {
		if rt.Path == "/api/secret" && rt.Req.Perm == rbac.PermAuditRead {
			found = true
		}
	}
	if !found {
		t.Error("route registry does not list /api/secret")
	}
}

func TestZeroRequirementPanics(t *testing.T) {
	t.Parallel()
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

func routerWithReady(t *testing.T, ready func(context.Context) error) *httpx.Router {
	t.Helper()
	r := httpx.New(httpx.Options{})
	api.RegisterSystem(r, api.SystemDeps{Ready: ready})
	return r
}

func TestHealthzIsAlwaysOK(t *testing.T) {
	t.Parallel()
	// Even with a failing readiness check, the process is alive.
	r := routerWithReady(t, func(context.Context) error { return errors.New("db down") })
	rec := get(t, r.Handler(), "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("/healthz = %d %q, want 200 ok", rec.Code, rec.Body)
	}
}

func TestReadyzFollowsTheReadyFunc(t *testing.T) {
	t.Parallel()
	r := routerWithReady(t, func(context.Context) error { return nil })
	rec := get(t, r.Handler(), "/readyz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ready" {
		t.Fatalf("/readyz = %d %q, want 200 ready", rec.Code, rec.Body)
	}

	r = routerWithReady(t, func(context.Context) error { return errors.New("/data/secret/path: permission denied") })
	rec = get(t, r.Handler(), "/readyz")
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "not ready" {
		t.Fatalf("/readyz = %d %q, want 503 not ready", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "permission") || strings.Contains(rec.Body.String(), "/data") {
		t.Error("/readyz must not leak the failure reason")
	}

	if rec := get(t, routerWithReady(t, nil).Handler(), "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("/readyz with no Ready func = %d, want 200", rec.Code)
	}
}

func TestProbeBodiesCarryNoVersionDetails(t *testing.T) {
	t.Parallel()
	h := routerWithReady(t, nil).Handler()
	for _, p := range []string{"/healthz", "/readyz"} {
		rec := get(t, h, p)
		body := strings.ToLower(rec.Body.String())
		for _, banned := range []string{"version", "commit", "dev", "v0."} {
			if strings.Contains(body, banned) {
				t.Errorf("%s body %q contains %q", p, body, banned)
			}
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s lacks security headers", p)
		}
	}
}

func TestHealthRoutesAreRegisteredAsExempt(t *testing.T) {
	t.Parallel()
	var exempt []string
	for _, rt := range newRouter(t).Routes() {
		if rt.Exempt {
			exempt = append(exempt, rt.Path)
		}
	}
	if len(exempt) != 2 || exempt[0] != "/healthz" || exempt[1] != "/readyz" {
		t.Errorf("exempt routes = %v, want [/healthz /readyz]", exempt)
	}
}

func TestHealthRoutesAreNotInTheOpenAPIContract(t *testing.T) {
	t.Parallel()
	doc, err := json.Marshal(newRouter(t).API().OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if strings.Contains(string(doc), p) {
			t.Errorf("OpenAPI document mentions %s", p)
		}
	}
}
