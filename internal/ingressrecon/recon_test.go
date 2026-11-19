// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"testing"
)

// SC 4 / D-19 / D-32 / D-11 tracer: a route written to the store becomes a new bundle one debounce later, on
// the injected clock, and the ingress switch (off) does not stop the render.
func TestRendersWithinTheDebounceOfAClaimChange(t *testing.T) {
	rg := newRig(t)
	rg.certs.setServable("app.example.com")
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"))

	s := rg.start()
	// The start render publishes the first bundle (D-19: Run renders before it waits).
	if got := s.awaitChange(t); len(got) != 1 || got[0] != rigEnv {
		t.Fatalf("the start render announced %v, want [%s]", got, rigEnv)
	}
	first, ok := s.r.Current(rigEnv)
	if !ok {
		t.Fatal("Current has no bundle after the start render")
	}
	if rules := routerRules(t, first); len(rules) != 1 {
		t.Fatalf("the first bundle has routers %v, want one", rules)
	}

	// A second route for the stack: the store notifies, Run arms one debounce window.
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"), decl("app.example.com", "/shop"))
	rg.clock.AwaitTimer(t, Debounce)
	rg.clock.Advance(Debounce)

	if got := s.awaitChange(t); len(got) != 1 || got[0] != rigEnv {
		t.Fatalf("the debounced render announced %v, want [%s]", got, rigEnv)
	}
	second, _ := s.r.Current(rigEnv)
	if second.ID == first.ID {
		t.Fatal("the bundle ID did not change after a route was added")
	}
	if rules := routerRules(t, second); len(rules) != 2 {
		t.Fatalf("the new bundle has routers %v, want two", rules)
	}
	set, err := rg.model.Settings(context.Background(), rigEnv)
	if err != nil {
		t.Fatal(err)
	}
	if set.Enabled {
		t.Error("the ingress switch is on, want off (the default in this phase): rendering does not depend on it (D-11)")
	}
}
