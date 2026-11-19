// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"testing"
)

// assertNothingPending fails when the subscription was woken, that is, when somebody called Notify.
func assertNothingPending(t *testing.T, sub interface{ C() <-chan struct{} }) {
	t.Helper()
	select {
	case <-sub.C():
		t.Fatal("the notifier was told of a change, want a cascade that nobody announced")
	default:
	}
}

// SC 4 / D-26 / Pitfall 4 / PROH-21-05 tracer: a stack deleted by cascade raises config_version and calls no
// Notify, so only the 2 s version poll sees it. One PollInterval on the injected clock later the route is gone.
func TestCascadeWithoutNotifyWithinThePoll(t *testing.T) {
	rg := newRig(t)
	s := settled(t, rg)
	before, _ := s.r.Current(rigEnv)
	if !hasRouterFor(routerRules(t, before), "app.example.com") {
		t.Fatalf("routers = %v, want one for app.example.com before the delete", routerRules(t, before))
	}
	// The poll timer is armed once the start render is done.
	rg.clock.AwaitTimer(t, PollInterval)

	// A subscription of the test's own: it stays empty only if nothing calls Notify.
	sub := rg.notifier.Subscribe()
	t.Cleanup(sub.Close)
	if _, err := rg.st.Write.Exec(`DELETE FROM stacks WHERE id = 'stack-shop'`); err != nil {
		t.Fatalf("delete the stack: %v", err)
	}
	assertNothingPending(t, sub)

	rg.clock.Advance(PollInterval)
	if ev := s.awaitRender(t); !ev.changed {
		t.Fatal("the render after the cascade reported an unchanged bundle")
	}
	s.awaitChange(t)

	after, ok := s.r.Current(rigEnv)
	if !ok {
		t.Fatal("Current has no bundle after the cascade")
	}
	if rules := routerRules(t, after); len(rules) != 0 {
		t.Fatalf("routers = %v, want none: the stack was deleted", rules)
	}
	if got := s.r.Skipped(rigEnv); len(got) != 0 {
		t.Errorf("Skipped = %+v, want none", got)
	}
	assertNothingPending(t, sub)
}
