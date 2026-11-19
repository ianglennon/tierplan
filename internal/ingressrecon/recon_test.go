// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"testing"
	"time"

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
