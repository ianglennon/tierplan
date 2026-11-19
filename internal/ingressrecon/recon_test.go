// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/ingressrender"
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

// D-09 / SC 1 / PROH-21-01 over the real stores: only a production (or uploaded) certificate that is ready
// puts a hostname in the bundle. A staging certificate is not ready and a hostname with no row has none.
func TestRealStoresSkipStagingAndMissing(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("a.example.com", "/"), decl("b.example.com", "/"), decl("c.example.com", "/"))

	nb, na := fixedNow.Add(-time.Hour), fixedNow.Add(60*24*time.Hour)
	dom := rg.domainID()
	insertCert(t, rg.st, rg.v, "cert-a", "a.example.com", rigEnv, dom, ingresscert.SourceACME, ingresscert.IssuerProduction, nb, na)
	insertCert(t, rg.st, rg.v, "cert-b", "b.example.com", rigEnv, dom, ingresscert.SourceACME, ingresscert.IssuerStaging, nb, na)

	certs := ingresscert.NewStore(ingresscert.StoreDeps{St: rg.st, Vault: rg.v, Now: func() time.Time { return fixedNow }, Notifier: rg.notifier})
	d := rg.deps(nil)
	d.Certs = certs
	r := New(d)
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	b, ok := r.Current(rigEnv)
	if !ok {
		t.Fatal("Current has no bundle")
	}
	if err := b.Verify(); err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	rules := routerRules(t, b)
	if len(rules) != 1 || !hasRouterFor(rules, "a.example.com") {
		t.Fatalf("routers = %v, want one, for a.example.com only", rules)
	}
	got := r.Skipped(rigEnv)
	want := map[string]ingressrender.SkipReason{
		"b.example.com": ingressrender.SkipCertificateNotReady,
		"c.example.com": ingressrender.SkipNoCertificate,
	}
	if len(got) != len(want) {
		t.Fatalf("Skipped = %+v, want b certificate_not_ready and c no_certificate", got)
	}
	for _, s := range got {
		if want[s.Hostname] != s.Reason {
			t.Errorf("Skipped %s = %q, want %q", s.Hostname, s.Reason, want[s.Hostname])
		}
	}
	var version int64
	if err := rg.st.Read.QueryRow(`SELECT certs_version FROM ingress_env WHERE env_id = ?`, rigEnv).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version == 0 || b.Version.Certs != version {
		t.Errorf("bundle Version.Certs = %d, want the environment's certs_version %d (non-zero)", b.Version.Certs, version)
	}
}

// settled starts a reconciler over a rig that holds stack shop with app.example.com (servable) and waits for the
// start render, so a test begins with a published bundle and nothing pending.
func settled(t *testing.T, rg *rig, extra ...ingress.Declaration) *started {
	t.Helper()
	rg.certs.setServable("app.example.com")
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", append([]ingress.Declaration{decl("app.example.com", "/")}, extra...)...)
	s := rg.start()
	if ev := s.awaitRender(t); !ev.changed {
		t.Fatalf("the start render reported an unchanged bundle %s, want a new one", ev.id)
	}
	s.awaitChange(t)
	return s
}

// advanceOneWindow waits for the reconciler to arm its debounce window, moves the clock across it and waits for
// the render it starts.
func advanceOneWindow(t *testing.T, rg *rig, s *started) renderEvent {
	t.Helper()
	rg.clock.AwaitTimer(t, Debounce)
	rg.clock.Advance(Debounce)
	return s.awaitRender(t)
}

// assertNothingAnnounced fails when the Changes subscription has been woken.
func assertNothingAnnounced(t *testing.T, s *started) {
	t.Helper()
	select {
	case <-s.changes.C():
		t.Fatalf("Changes was notified for %v, want nobody told", s.changes.Pending())
	default:
	}
}

// SC 4 / D-19: a claim, a route and a certificate change each reach the bundle within one debounce window.
func TestRendersWithinTheDebounceOfEachChange(t *testing.T) {
	t.Run("claim", func(t *testing.T) {
		rg := newRig(t)
		s := settled(t, rg)
		rg.certs.setServable("new.example.com")
		rg.seedSwarmStack("stack-blog", "blog")
		rg.storeRoutes("stack-blog", decl("new.example.com", "/"))

		if ev := advanceOneWindow(t, rg, s); !ev.changed {
			t.Fatal("the render after a claim reported an unchanged bundle")
		}
		b, _ := s.r.Current(rigEnv)
		if rules := routerRules(t, b); !hasRouterFor(rules, "new.example.com") || !hasRouterFor(rules, "app.example.com") {
			t.Fatalf("routers = %v, want app.example.com and new.example.com", rules)
		}
		s.awaitChange(t)
	})
	t.Run("route", func(t *testing.T) {
		rg := newRig(t)
		s := settled(t, rg)
		rg.storeRoutes("stack-shop", decl("app.example.com", "/"), decl("app.example.com", "/api"))

		if ev := advanceOneWindow(t, rg, s); !ev.changed {
			t.Fatal("the render after a route change reported an unchanged bundle")
		}
		b, _ := s.r.Current(rigEnv)
		if rules := routerRules(t, b); len(rules) != 2 {
			t.Fatalf("routers = %v, want two", rules)
		}
		s.awaitChange(t)
	})
	t.Run("certificate", func(t *testing.T) {
		rg := newRig(t)
		s := settled(t, rg, decl("wait.example.com", "/"))
		if b, _ := s.r.Current(rigEnv); hasRouterFor(routerRules(t, b), "wait.example.com") {
			t.Fatal("wait.example.com is served before it has a certificate")
		}

		// The certificate turns servable and the store tells the notifier, as ingresscert.Store does.
		rg.certs.setServable("wait.example.com")
		rg.notifier.Notify(rigEnv)

		if ev := advanceOneWindow(t, rg, s); !ev.changed {
			t.Fatal("the render after a certificate change reported an unchanged bundle")
		}
		b, _ := s.r.Current(rigEnv)
		if !hasRouterFor(routerRules(t, b), "wait.example.com") {
			t.Fatalf("routers = %v, want one for wait.example.com", routerRules(t, b))
		}
		if got := s.r.Skipped(rigEnv); len(got) != 0 {
			t.Errorf("Skipped = %+v, want none once the certificate is servable", got)
		}
		s.awaitChange(t)
	})
}

// D-19 / D-06 / D-32: a notification with nothing changed renders again, finds the same ID and tells nobody.
func TestNoChangeKeepsTheBundle(t *testing.T) {
	rg := newRig(t)
	s := settled(t, rg)
	before, _ := s.r.Current(rigEnv)

	rg.notifier.Notify(rigEnv)
	ev := advanceOneWindow(t, rg, s)

	if ev.changed {
		t.Error("the render reported a changed bundle, want the same ID to be a no-op")
	}
	if ev.id != before.ID {
		t.Errorf("rendered ID %s, want %s", ev.id, before.ID)
	}
	after, _ := s.r.Current(rigEnv)
	if after.ID != before.ID {
		t.Errorf("Current's ID moved from %s to %s", before.ID, after.ID)
	}
	assertNothingAnnounced(t, s)
}

// Pitfall 11 / D-32: the same content under a newer config version refreshes the version beside the ID, and
// nobody is told.
func TestEqualIDRefreshesMetadataWithoutNotifying(t *testing.T) {
	rg := newRig(t)
	s := settled(t, rg)
	before, _ := s.r.Current(rigEnv)

	if _, err := rg.st.Write.Exec(`UPDATE ingress_env SET config_version = config_version + 1 WHERE env_id = ?`, rigEnv); err != nil {
		t.Fatalf("raise the config version: %v", err)
	}
	rg.notifier.Notify(rigEnv)
	ev := advanceOneWindow(t, rg, s)

	if ev.changed {
		t.Error("the render reported a changed bundle, want the same ID")
	}
	after, _ := s.r.Current(rigEnv)
	if after.ID != before.ID {
		t.Fatalf("Current's ID moved from %s to %s", before.ID, after.ID)
	}
	if after.Version.Config != before.Version.Config+1 {
		t.Errorf("Current().Version.Config = %d, want %d", after.Version.Config, before.Version.Config+1)
	}
	assertNothingAnnounced(t, s)
}

// D-20 via D-07: the bundle lives in memory only, and a new reconciler over the same stores renders the same ID.
func TestRestartRendersTheSameID(t *testing.T) {
	rg := newRig(t)
	first := settled(t, rg)
	want, _ := first.r.Current(rigEnv)

	second := rg.start()
	ev := second.awaitRender(t)

	if ev.id != want.ID {
		t.Errorf("the restarted reconciler rendered %s, want %s", ev.id, want.ID)
	}
	got, ok := second.r.Current(rigEnv)
	if !ok || got.ID != want.ID {
		t.Errorf("the restarted reconciler's Current = %s (have %v), want %s", got.ID, ok, want.ID)
	}
}
