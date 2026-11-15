// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/server"
)

// This test lives in the external test package because it needs the production
// router, and internal/server imports httpx: an in-package test would be an
// import cycle.

// TestExemptRoutesOfTheProductionRouterAreExactlyTheSevenKnownPaths pins the set
// of routes outside the policy seam. VerifyAllGuarded only checks that each
// exempt route is on its list; this checks the reverse too, so a route cannot be
// dropped from the router, or an eighth added to it, without this test noticing.
func TestExemptRoutesOfTheProductionRouterAreExactlyTheSevenKnownPaths(t *testing.T) {
	// The dependency-free router is what the openapi subcommand builds; every
	// route exists in it, so the exempt set is complete.
	r := server.NewRouter(server.RouterDeps{})

	seen := map[string]bool{}
	for _, rec := range r.Routes() {
		if rec.Exempt {
			seen[rec.Path] = true
		}
	}
	got := make([]string, 0, len(seen))
	for p := range seen {
		got = append(got, p)
	}
	sort.Strings(got)

	// /ingress/connect is the address the ingress proxy dials (Phase 22, D-25); its handshake is its own authentication.
	// The webhook address is what a git host calls on a push (Phase 9, D-15): it proves itself with the routing
	// token and the webhook's signature, and can only ask for a check of one stack.
	want := []string{"/agent/connect", "/healthz", "/hooks/git/{token}", "/ingress/connect", "/readyz", "/setup", "/setup/*"}
	if !slices.Equal(got, want) {
		t.Errorf("exempt paths = %v, want exactly %v", got, want)
	}
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Errorf("the production router has an unguarded route: %v", err)
	}

	// /ingress/connect is a placeholder until Phase 22 (D-25): with no handler supplied it answers 404.
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, httpx.IngressConnectPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET %s = %d, want 404 until Phase 22", httpx.IngressConnectPath, rec.Code)
	}
}
