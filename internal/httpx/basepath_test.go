// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ianglennon/coaming/internal/httpx"
)

// seen records the path and raw path the wrapped handler received.
type seen struct{ path, raw string }

func serveThrough(t *testing.T, base, target string) seen {
	t.Helper()
	var got seen
	h := httpx.StripBasePath(func() string { return base })(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = seen{path: r.URL.Path, raw: r.URL.RawPath}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	return got
}

func TestStripBasePath(t *testing.T) {
	cases := []struct {
		name, base, target, want string
	}{
		{"exact base", "/coaming", "/coaming", "/"},
		{"base with slash", "/coaming", "/coaming/", "/"},
		{"api under base", "/coaming", "/coaming/api/version", "/api/version"},
		{"api without base", "/coaming", "/api/version", "/api/version"},
		{"sibling prefix is not the base", "/coaming", "/coamingx/api", "/coamingx/api"},
		{"healthz stays unprefixed", "/coaming", "/healthz", "/healthz"},
		{"readyz stays unprefixed", "/coaming", "/readyz", "/readyz"},
		{"no base changes nothing", "", "/api/version", "/api/version"},
		{"no base keeps a coaming segment", "", "/coaming/api", "/coaming/api"},
		{"nested base", "/tools/coaming", "/tools/coaming/assets/app.js", "/assets/app.js"},
		{"nested base exact", "/tools/coaming", "/tools/coaming", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serveThrough(t, tc.base, tc.target); got.path != tc.want {
				t.Fatalf("base %q target %q: path = %q, want %q", tc.base, tc.target, got.path, tc.want)
			}
		})
	}
}

func TestStripBasePathStripsRawPathConsistently(t *testing.T) {
	got := serveThrough(t, "/coaming", "/coaming/api/a%2Fb")
	if got.path != "/api/a/b" || got.raw != "/api/a%2Fb" {
		t.Fatalf("path %q raw %q, want /api/a/b and /api/a%%2Fb", got.path, got.raw)
	}
	got = serveThrough(t, "/coaming", "/coaming")
	if got.path != "/" {
		t.Fatalf("exact base path = %q, want /", got.path)
	}
	got = serveThrough(t, "/coaming", "/other/a%2Fb")
	if got.path != "/other/a/b" || got.raw != "/other/a%2Fb" {
		t.Fatalf("an unprefixed path must be unchanged, got path %q raw %q", got.path, got.raw)
	}
}

func TestStripBasePathDoesNotMutateTheCallersRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/coaming/api/version", nil)
	h := httpx.StripBasePath(func() string { return "/coaming" })(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if req.URL.Path != "/coaming/api/version" {
		t.Fatalf("the caller's request was mutated: %q", req.URL.Path)
	}
}

func TestStripBasePathReadsTheBaseOnEveryRequest(t *testing.T) {
	base := "/one"
	if got := serveThrough(t, base, "/two/x"); got.path != "/two/x" {
		t.Fatalf("with base /one, /two/x = %q", got.path)
	}
	// One handler, a base that changes between requests.
	var seenPath string
	h := httpx.StripBasePath(func() string { return base })(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
	}))
	do := func() string {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/two/x", nil))
		return seenPath
	}
	if got := do(); got != "/two/x" {
		t.Fatalf("with base /one, /two/x = %q", got)
	}
	base = "/two"
	if got := do(); got != "/x" {
		t.Fatalf("after the base changed to /two, /two/x = %q, want /x", got)
	}
}
