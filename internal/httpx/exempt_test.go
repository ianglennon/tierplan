// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package httpx_test

import (
	"slices"
	"sort"
	"testing"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/server"
)

// This test lives in the external test package because it needs the production
// router, and internal/server imports httpx: an in-package test would be an
// import cycle.

// TestExemptRoutesOfTheProductionRouterAreExactlyTheFiveKnownPaths pins the set
// of routes outside the policy seam. VerifyAllGuarded only checks that each
// exempt route is on its list; this checks the reverse too, so a route cannot be
// dropped from the router, or a sixth added to it, without this test noticing.
func TestExemptRoutesOfTheProductionRouterAreExactlyTheFiveKnownPaths(t *testing.T) {
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

	want := []string{"/agent/connect", "/healthz", "/readyz", "/setup", "/setup/*"}
	if !slices.Equal(got, want) {
		t.Errorf("exempt paths = %v, want exactly %v", got, want)
	}
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Errorf("the production router has an unguarded route: %v", err)
	}
}
