// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"runtime"
	"testing"
	"weak"

	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/config"
)

// A fixture lets go of its router when its test ends. Go's testing package keeps every
// finished parallel test, with the closures its cleanups leave behind, until the whole test
// binary ends, so whatever a cleanup can reach stays in memory for the rest of the run. The
// fixtures' cleanups reach the router: the broker, the tunnel hub and the agent updater hold
// the environment service, whose cluster reader holds the address reader, whose grace reads
// the fixture's janitor policy; and a shell fixture's cleanup closes an httptest server that
// serves the router. Every test's router, with all of its OpenAPI schemas and validators,
// then lived to the end, and the race run of this package passed 12 GB.
//
// Each case builds a fixture in a subtest and keeps that subtest's *testing.T, as the testing
// package does for a parallel test, so the closures its cleanups left are still reachable. The
// router must still be collectable.
func TestFinishedFixtureLetsGoOfItsRouter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		build func(t *testing.T) http.Handler
	}{
		{"auth", func(t *testing.T) http.Handler { return newAuthFixture(t, config.Config{}).h }},
		{"agent", func(t *testing.T) http.Handler { return newAgentFix(t).h }},
		{"swarm", func(t *testing.T) http.Handler { return newSwarmFix(t).h }},
		{"janitor swarm", func(t *testing.T) http.Handler { return janitorSwarm(t, onWorker(exitedSlot(3)), nil).h }},
		{"shell", func(t *testing.T) http.Handler { return newShellFix(t, nil).srv.Config.Handler }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var finished *testing.T
			var router weak.Pointer[chi.Mux]
			t.Run("fixture", func(t *testing.T) {
				finished = t
				mux, ok := c.build(t).(*chi.Mux)
				if !ok {
					t.Fatalf("the fixture's router is not a *chi.Mux; this test tracks the router by that type")
				}
				router = weak.Make(mux)
			})
			// t.Run has returned, so the fixture's test and every cleanup it registered have run.
			// Two collections: chi's mux pools its routing contexts, each of which points back
			// at the mux, and the runtime keeps a pool's contents through one collection (its
			// victim cache) and drops them at the next.
			runtime.GC()
			runtime.GC()
			if router.Value() != nil {
				t.Errorf("the %s fixture's router is still reachable after its test ended; every test's router would then live until the test binary ends", c.name)
			}
			runtime.KeepAlive(finished)
		})
	}
}
