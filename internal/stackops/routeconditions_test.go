// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// A route's conditions are the five things that have to hold before it serves, in a fixed order, each ready, not ready
// or unknown with a stable reason (plan 27-02, D-08). They are derived beside the single legacy state by the same
// function, so the stack page and the Ingress page cannot disagree (D-09).

// conditionOrder is the order every route's conditions come in.
var conditionOrder = []string{
	stackops.CondClaimHeld,
	stackops.CondNetworkAttached,
	stackops.CondBackendHealthy,
	stackops.CondCertificateReady,
	stackops.CondApplied,
}

// servingInput is a route that serves: ingress on, the service runs and is attached, the route is in the current bundle
// and both active nodes applied it.
func servingInput() stackops.RouteStateInput {
	key := stackops.RouteKey{Hostname: "app.example.com", Path: "/"}
	return stackops.RouteStateInput{
		Enabled:      true,
		Routes:       []ingress.Route{{Hostname: "app.example.com", PathPrefix: "/", Service: "web"}},
		Running:      map[string]int{"web": 1},
		Attached:     map[string]bool{"web": true},
		InBundle:     map[stackops.RouteKey]bool{key: true},
		NodesApplied: true,
		BundleKnown:  true,
		ActiveNodes:  2,
		AppliedNodes: 2,
	}
}

func TestRouteConditionsServingRoute(t *testing.T) {
	t.Parallel()

	got := stackops.RouteStates(servingInput())

	if len(got) != 1 {
		t.Fatalf("states = %+v, want one", got)
	}
	s := got[0]
	if s.State != stackops.RouteServing {
		t.Errorf("state = %q, want %q", s.State, stackops.RouteServing)
	}
	if len(s.Conditions) != len(conditionOrder) {
		t.Fatalf("conditions = %+v, want %d", s.Conditions, len(conditionOrder))
	}
	for i, c := range s.Conditions {
		if c.Code != conditionOrder[i] {
			t.Errorf("condition %d = %q, want %q", i, c.Code, conditionOrder[i])
		}
		if c.State != stackops.CondReady || c.Reason != "" {
			t.Errorf("condition %q = %q reason %q, want ready with no reason", c.Code, c.State, c.Reason)
		}
	}
	applied := s.Conditions[4]
	if applied.Applied != 2 || applied.Total != 2 {
		t.Errorf("applied = %d of %d, want 2 of 2", applied.Applied, applied.Total)
	}
}
