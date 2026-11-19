// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/ingressrender"
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

// assertNoRender fails when the reconciler rendered since the last event was read.
func assertNoRender(t *testing.T, s *started) {
	t.Helper()
	select {
	case ev := <-s.renders:
		t.Fatalf("the reconciler rendered %s (changed %v), want no render", ev.envID, ev.changed)
	default:
	}
}

// clearServable makes host unservable again, as a certificate that stopped being ready.
func (c *fakeCerts) clearServable(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.servable, host)
}

// raiseCertsVersion bumps the environment's certs_version, as ingresscert's refreshReady does when a readiness flag
// flips by time alone (Phase 20 D-41).
func raiseCertsVersion(t *testing.T, rg *rig, envID string) {
	t.Helper()
	if _, err := rg.st.Write.Exec(`UPDATE ingress_env SET certs_version = certs_version + 1 WHERE env_id = ?`, envID); err != nil {
		t.Fatalf("raise the certificate version of %s: %v", envID, err)
	}
}

// D-20 / SC 4: a cascade followed by Notify (Phase 24's delete path) reaches the bundle within the 250 ms
// debounce, before the poll could have run.
func TestCascadeWithNotifyWithinTheDebounce(t *testing.T) {
	rg := newRig(t)
	s := settled(t, rg)

	if _, err := rg.st.Write.Exec(`DELETE FROM stacks WHERE id = 'stack-shop'`); err != nil {
		t.Fatalf("delete the stack: %v", err)
	}
	rg.notifier.Notify(rigEnv)

	// One debounce window, which is shorter than the poll: only the notification can have caused this render.
	if ev := advanceOneWindow(t, rg, s); !ev.changed {
		t.Fatal("the render after the cascade reported an unchanged bundle")
	}
	b, _ := s.r.Current(rigEnv)
	if rules := routerRules(t, b); len(rules) != 0 {
		t.Fatalf("routers = %v, want none: the stack was deleted", rules)
	}
	s.awaitChange(t)
}

// D-26: when no environment's version pair moved, a poll renders nothing and arms itself again.
func TestUnchangedVersionsRenderNothing(t *testing.T) {
	rg := newRig(t)
	s := settled(t, rg)
	rg.clock.AwaitTimer(t, PollInterval)

	rg.clock.Advance(PollInterval)
	// The poll arms its next timer after it has looked, so seeing that timer means the pass is over.
	rg.clock.AwaitTimer(t, PollInterval)

	assertNoRender(t, s)
	assertNothingAnnounced(t, s)
}

// D-26 / Phase 20 D-41 / T-21-36: a certificate that stops being ready by time alone raises certs_version, and the
// next poll takes its hostname out of the bundle and says why.
func TestReadinessByTimeLeavesAtThePoll(t *testing.T) {
	rg := newRig(t)
	s := settled(t, rg)
	rg.clock.AwaitTimer(t, PollInterval)

	rg.certs.clearServable("app.example.com")
	rg.certs.setReason("app.example.com", ingresscert.ReasonNotReady)
	raiseCertsVersion(t, rg, rigEnv)
	rg.clock.Advance(PollInterval)

	if ev := s.awaitRender(t); !ev.changed {
		t.Fatal("the render after the readiness flip reported an unchanged bundle")
	}
	b, _ := s.r.Current(rigEnv)
	if rules := routerRules(t, b); hasRouterFor(rules, "app.example.com") {
		t.Fatalf("routers = %v, want none for app.example.com once its certificate is not ready", rules)
	}
	got := s.r.Skipped(rigEnv)
	if len(got) != 1 || got[0].Hostname != "app.example.com" || got[0].Reason != ingressrender.SkipCertificateNotReady {
		t.Fatalf("Skipped = %+v, want app.example.com certificate_not_ready", got)
	}
	s.awaitChange(t)
}

// seedSecondEnv adds env-2 with the domain example.org and a swarm stack blog that claims blog.example.org.
func seedSecondEnv(t *testing.T, rg *rig) {
	t.Helper()
	ctx := context.Background()
	rg.seedEnv("env-2", "Two")
	if _, err := rg.model.CreateDomain(ctx,
		ingressstore.CreateDomainInput{EnvID: "env-2", Suffix: "example.org", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil); err != nil {
		t.Fatalf("register example.org: %v", err)
	}
	if _, err := rg.st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-blog', 'env-2', 'blog', 'swarm', 'tester', 1)`); err != nil {
		t.Fatalf("seed the blog stack: %v", err)
	}
	if _, err := rg.model.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
		EnvID: "env-2", StackID: "stack-blog", Actor: "alice",
		Decls: []ingress.Declaration{{Service: "web", Host: "blog.example.org", Label: "tierplan.ingress.host", PathPrefix: "/", Port: 8080}},
	}); err != nil {
		t.Fatalf("store the blog routes: %v", err)
	}
}

// D-19: the safety tick renders every environment, which catches a change no version recorded. The certificate
// source here changes for two environments with no version bump and no Notify, so the poll leaves both alone.
func TestSafetyTickRendersEveryEnvironment(t *testing.T) {
	rg := newRig(t)
	seedSecondEnv(t, rg)
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"))
	s := rg.start()
	for range 2 {
		if ev := s.awaitRender(t); !ev.changed {
			t.Fatalf("the start render of %s reported an unchanged bundle", ev.envID)
		}
	}
	s.awaitChange(t)
	rg.clock.AwaitTimer(t, PollInterval)
	rg.clock.AwaitTimer(t, SafetyTick)
	for _, env := range []string{rigEnv, "env-2"} {
		if b, _ := s.r.Current(env); len(routerRules(t, b)) != 0 {
			t.Fatalf("%s serves %v before it has a certificate", env, routerRules(t, b))
		}
	}

	rg.certs.setServable("app.example.com")
	rg.certs.setServable("blog.example.org")

	// A poll sees no version move and renders nothing.
	rg.clock.Advance(PollInterval)
	rg.clock.AwaitTimer(t, PollInterval)
	assertNoRender(t, s)

	// The tick, a minute after the start, renders both environments.
	rg.clock.Advance(SafetyTick - PollInterval)
	renders := map[string]bool{}
	for range 2 {
		ev := s.awaitRender(t)
		if !ev.changed {
			t.Errorf("the tick's render of %s reported an unchanged bundle", ev.envID)
		}
		renders[ev.envID] = true
	}
	if !renders[rigEnv] || !renders["env-2"] {
		t.Fatalf("the tick rendered %v, want env-1 and env-2", renders)
	}
	if b, _ := s.r.Current(rigEnv); !hasRouterFor(routerRules(t, b), "app.example.com") {
		t.Errorf("env-1 routers = %v, want one for app.example.com", routerRules(t, b))
	}
	if b, _ := s.r.Current("env-2"); !hasRouterFor(routerRules(t, b), "blog.example.org") {
		t.Errorf("env-2 routers = %v, want one for blog.example.org", routerRules(t, b))
	}
}
