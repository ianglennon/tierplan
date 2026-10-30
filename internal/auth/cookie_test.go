// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ianglennon/coaming/internal/auth"
)

// scopeCases is the D-50 table: https at the root keeps the __Host- prefix,
// https under a base cannot (the prefix demands Path=/) and uses __Secure-, and
// plain http uses the bare name at either base.
var scopeCases = []struct {
	name     string
	scope    auth.CookieScope
	wantName string
	wantPath string
}{
	{"https at the root", auth.CookieScope{Secure: true}, "__Host-coaming_session", "/"},
	{"https under a base", auth.CookieScope{Secure: true, BasePath: "/coaming"}, "__Secure-coaming_session", "/coaming"},
	{"https under a nested base", auth.CookieScope{Secure: true, BasePath: "/tools/coaming"}, "__Secure-coaming_session", "/tools/coaming"},
	{"http at the root", auth.CookieScope{}, "coaming_session", "/"},
	{"http under a base", auth.CookieScope{BasePath: "/coaming"}, "coaming_session", "/coaming"},
}

func TestCookieScopeNames(t *testing.T) {
	for _, tc := range scopeCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.CookieName(tc.scope); got != tc.wantName {
				t.Fatalf("CookieName(%+v) = %q, want %q", tc.scope, got, tc.wantName)
			}
		})
	}
}

func TestCookieScopePathsAndFlags(t *testing.T) {
	f := newSessionFixture(t)
	for _, tc := range scopeCases {
		t.Run(tc.name, func(t *testing.T) {
			set := f.m.Cookie("rawid", true, tc.scope)
			clear := auth.ClearCookie(tc.scope)
			for label, c := range map[string]http.Cookie{"Cookie": set, "ClearCookie": clear} {
				if c.Name != tc.wantName || c.Path != tc.wantPath {
					t.Errorf("%s = name %q path %q, want %q %q", label, c.Name, c.Path, tc.wantName, tc.wantPath)
				}
				if c.Secure != tc.scope.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" {
					t.Errorf("%s flags = %+v", label, c)
				}
			}
			if clear.MaxAge != -1 || clear.Value != "" {
				t.Errorf("ClearCookie must expire at once: %+v", clear)
			}
			if set.Value != "rawid" {
				t.Errorf("Cookie value = %q", set.Value)
			}
		})
	}
}

func TestCookieScopeHostPrefixNeverHasAPathOtherThanRoot(t *testing.T) {
	// The __Host- prefix makes a browser drop a cookie whose Path is not "/".
	f := newSessionFixture(t)
	for _, tc := range scopeCases {
		c := f.m.Cookie("rawid", false, tc.scope)
		if len(c.Name) > 7 && c.Name[:7] == "__Host-" && c.Path != "/" {
			t.Errorf("%s: __Host- cookie with Path %q", tc.name, c.Path)
		}
	}
}

func TestCookieScopeAuthenticatorIgnoresACookieOfAnotherScope(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", false)

	httpsUnderBase := auth.CookieScope{Secure: true, BasePath: "/coaming"}
	a := auth.NewAuthenticator(f.st, testIdle, func() auth.CookieScope { return httpsUnderBase }, f.clk.now)

	// A valid session ID under the root-https name is not believed under a base.
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	addCookie(req, auth.CookieName(auth.CookieScope{Secure: true}), res.RawSessionID)
	if p, err := a.Authenticate(req); err != nil || p != nil {
		t.Fatalf("a __Host- cookie authenticated under https at /coaming: %+v, %v", p, err)
	}
	// Nor is the bare name.
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	addCookie(req, auth.CookieName(auth.CookieScope{}), res.RawSessionID)
	if p, err := a.Authenticate(req); err != nil || p != nil {
		t.Fatalf("a bare cookie authenticated under https at /coaming: %+v, %v", p, err)
	}
	// The name of the current scope is.
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	addCookie(req, "__Secure-coaming_session", res.RawSessionID)
	if p, err := a.Authenticate(req); err != nil || p == nil || p.Name != "admin" {
		t.Fatalf("the __Secure- cookie was not read: %+v, %v", p, err)
	}
}

func TestCookieScopeAuthenticatorFollowsTheScopeAtRunTime(t *testing.T) {
	f := newLoginFixture(t)
	f.addUser(t, "admin")
	res := f.mustLogin(t, "admin", false)

	scope := auth.CookieScope{}
	a := auth.NewAuthenticator(f.st, testIdle, func() auth.CookieScope { return scope }, f.clk.now)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		addCookie(r, "coaming_session", res.RawSessionID)
		return r
	}
	if p, err := a.Authenticate(req()); err != nil || p == nil {
		t.Fatalf("plain http at the root: %+v, %v", p, err)
	}
	scope = auth.CookieScope{Secure: true, BasePath: "/coaming"}
	if p, err := a.Authenticate(req()); err != nil || p != nil {
		t.Fatalf("after the public URL became https under /coaming the old cookie still authenticated: %+v, %v", p, err)
	}
}
