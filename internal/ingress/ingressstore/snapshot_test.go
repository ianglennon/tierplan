// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// seedSwarmStack inserts a swarm stack in an environment.
func seedSwarmStack(t *testing.T, st *store.Store, id, envID, name string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, ?, 'swarm', 'tester', 1)`,
		id, envID, name); err != nil {
		t.Fatalf("seed swarm stack %s: %v", id, err)
	}
}

// replaceRoutes stores a stack's declarations through the policy and fails the test when it is refused.
func replaceRoutes(t *testing.T, s *Store, envID, stackID string, decls ...ingress.Declaration) {
	t.Helper()
	if _, err := s.ReplaceStackRoutes(context.Background(), ReplaceInput{EnvID: envID, StackID: stackID, Actor: "alice", Decls: decls}); err != nil {
		t.Fatalf("replace the routes of %s: %v", stackID, err)
	}
}

// D-12 / LLD-03 tracer: a claimed swarm route reads back with its stack name, target and strip.
func TestSnapshotCarriesStackNameTargetAndStrip(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedSwarmStack(t, st, "stack-shop", "env-a", "shop")
	registerDomain(t, s, "env-a", "example.com")
	replaceRoutes(t, s, "env-a", "stack-shop", ingress.Declaration{
		Service: "web", Host: "app.example.com", Label: "tierplan.ingress.host",
		PathPrefix: "/api", Port: 8080, Scheme: ingress.SchemeHTTP, Strip: true,
	})

	snap, err := s.Snapshot(ctx, "env-a")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snap.Settings.Exists || snap.Settings.ConfigVersion != configVersion(t, st, "env-a") {
		t.Fatalf("Settings = %+v, want Exists with the stored config_version", snap.Settings)
	}
	if len(snap.Domains) != 1 || snap.Domains[0].Suffix != "example.com" {
		t.Fatalf("Domains = %+v, want example.com", snap.Domains)
	}
	if len(snap.Claims) != 1 {
		t.Fatalf("Claims = %+v, want one", snap.Claims)
	}
	c := snap.Claims[0]
	if c.Hostname != "app.example.com" || c.StackName != "shop" || c.Target != ingress.TargetSwarm ||
		c.OwnerKind != ingress.OwnerStack || c.StackID != "stack-shop" {
		t.Fatalf("claim = %+v, want app.example.com owned by swarm stack shop", c)
	}
	if len(snap.Routes) != 1 {
		t.Fatalf("Routes = %+v, want one", snap.Routes)
	}
	r := snap.Routes[0]
	if r.PathPrefix != "/api" || r.Service != "web" || r.Port != 8080 || !r.Strip || r.Scheme != ingress.SchemeHTTP ||
		r.StackName != "shop" || r.Target != ingress.TargetSwarm || r.OwnerKind != ingress.OwnerStack || r.StackID != "stack-shop" {
		t.Fatalf("route = %+v, want /api to web:8080 with strip, owned by swarm stack shop", r)
	}
}

// insertSystemClaim stores a claim held by the system, with one route, by raw SQL, so a test can pick the
// claim's fields freely (SetSystemClaim is the store's own write). The install holds at most one, because of
// the partial unique index of migration 00049.
func insertSystemClaim(t *testing.T, st *store.Store, envID, domainID, hostname, routeID string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES (?, ?, ?, 'system', NULL, 'system', 1)`,
		hostname, envID, domainID); err != nil {
		t.Fatalf("insert the system claim %s: %v", hostname, err)
	}
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, scheme, strip, created_at, updated_at) VALUES (?, ?, ?, '', '/', 'tierplan', 7480, 'http', 0, 1, 1)`,
		routeID, envID, hostname); err != nil {
		t.Fatalf("insert the system route %s: %v", hostname, err)
	}
}

// insertStackClaim stores a claim held by a stack, with one route, by raw SQL, for a test that needs several
// claims in one environment (the install holds at most one system claim).
func insertStackClaim(t *testing.T, st *store.Store, envID, domainID, stackID, hostname, routeID string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES (?, ?, ?, 'stack', ?, 'alice', 1)`,
		hostname, envID, domainID, stackID); err != nil {
		t.Fatalf("insert the stack claim %s: %v", hostname, err)
	}
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, scheme, strip, created_at, updated_at) VALUES (?, ?, ?, '', '/', 'web', 8080, 'http', 0, 1, 1)`,
		routeID, envID, hostname); err != nil {
		t.Fatalf("insert the stack route %s: %v", hostname, err)
	}
}

// insertRawRoute stores a route of an existing claim by raw SQL, so a test can choose the insert order.
func insertRawRoute(t *testing.T, st *store.Store, envID, id, hostname, path string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, scheme, strip, created_at, updated_at) VALUES (?, ?, ?, '', ?, 'web', 8080, 'http', 0, 1, 1)`,
		id, envID, hostname, path); err != nil {
		t.Fatalf("insert the route %s: %v", id, err)
	}
}

func TestSnapshotEdges(t *testing.T) {
	ctx := context.Background()

	t.Run("an environment with no ingress_env row", func(t *testing.T) {
		s, st, _ := newTestStore(t)
		seedEnv(t, st, "env-a", "Alpha")
		snap, err := s.Snapshot(ctx, "env-a")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snap.Settings != (ingress.Settings{EnvID: "env-a"}) {
			t.Fatalf("Settings = %+v, want only the env id", snap.Settings)
		}
		if len(snap.Claims) != 0 || len(snap.Routes) != 0 {
			t.Fatalf("claims %+v and routes %+v, want none", snap.Claims, snap.Routes)
		}
	})

	t.Run("an environment whose only claim was released", func(t *testing.T) {
		s, st, _ := newTestStore(t)
		seedEnv(t, st, "env-a", "Alpha")
		seedStack(t, st, "stack-a", "env-a", "web-a")
		registerDomain(t, s, "env-a", "example.com")
		claim(t, s, "env-a", "stack-a", "app.example.com")
		replaceRoutes(t, s, "env-a", "stack-a") // no declarations releases the claim
		snap, err := s.Snapshot(ctx, "env-a")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if !snap.Settings.Exists || snap.Settings.ConfigVersion == 0 {
			t.Fatalf("Settings = %+v, want Exists with a raised version", snap.Settings)
		}
		if len(snap.Claims) != 0 || len(snap.Routes) != 0 {
			t.Fatalf("claims %+v and routes %+v, want none", snap.Claims, snap.Routes)
		}
	})

	t.Run("a system claim has no stack", func(t *testing.T) {
		s, st, _ := newTestStore(t)
		seedEnv(t, st, "env-a", "Alpha")
		dom := registerDomain(t, s, "env-a", "example.com")
		insertSystemClaim(t, st, "env-a", dom.ID, "tierplan.example.com", "route-sys")
		snap, err := s.Snapshot(ctx, "env-a")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if len(snap.Claims) != 1 || len(snap.Routes) != 1 {
			t.Fatalf("claims %+v and routes %+v, want one of each", snap.Claims, snap.Routes)
		}
		c := snap.Claims[0]
		if c.OwnerKind != ingress.OwnerSystem || c.StackName != "" || c.Target != "" || c.StackID != "" {
			t.Fatalf("claim = %+v, want a system claim with no stack and no target", c)
		}
		r := snap.Routes[0]
		if r.OwnerKind != ingress.OwnerSystem || r.StackID != "" || r.StackName != "" || r.Target != "" {
			t.Fatalf("route = %+v, want a system route with no stack and no target", r)
		}
	})

	t.Run("a standalone stack's claim has the standalone target", func(t *testing.T) {
		s, st, _ := newTestStore(t)
		seedEnv(t, st, "env-a", "Alpha")
		seedStack(t, st, "stack-a", "env-a", "web-a")
		registerDomain(t, s, "env-a", "example.com")
		claim(t, s, "env-a", "stack-a", "app.example.com")
		snap, err := s.Snapshot(ctx, "env-a")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if len(snap.Claims) != 1 || snap.Claims[0].Target != ingress.TargetStandalone || snap.Claims[0].StackName != "web-a" {
			t.Fatalf("Claims = %+v, want one on standalone stack web-a", snap.Claims)
		}
		if len(snap.Routes) != 1 || snap.Routes[0].Target != ingress.TargetStandalone {
			t.Fatalf("Routes = %+v, want one on the standalone target", snap.Routes)
		}
	})

	t.Run("claims and routes come back ordered and compare equal", func(t *testing.T) {
		s, st, _ := newTestStore(t)
		seedEnv(t, st, "env-a", "Alpha")
		seedStack(t, st, "stack-a", "env-a", "web-a")
		dom := registerDomain(t, s, "env-a", "example.com")
		// Inserted out of order, by raw SQL: two hostnames and three routes.
		insertStackClaim(t, st, "env-a", dom.ID, "stack-a", "zeta.example.com", "r-zeta")
		insertSystemClaim(t, st, "env-a", dom.ID, "alpha.example.com", "r-alpha-root")
		insertRawRoute(t, st, "env-a", "r-alpha-z", "alpha.example.com", "/z")
		insertRawRoute(t, st, "env-a", "r-alpha-a", "alpha.example.com", "/a")

		first, err := s.Snapshot(ctx, "env-a")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		var hosts []string
		for _, c := range first.Claims {
			hosts = append(hosts, c.Hostname)
		}
		if !reflect.DeepEqual(hosts, []string{"alpha.example.com", "zeta.example.com"}) {
			t.Fatalf("claim order = %v, want by hostname", hosts)
		}
		var routes []string
		for _, r := range first.Routes {
			routes = append(routes, r.Hostname+r.PathPrefix)
		}
		want := []string{"alpha.example.com/", "alpha.example.com/a", "alpha.example.com/z", "zeta.example.com/"}
		if !reflect.DeepEqual(routes, want) {
			t.Fatalf("route order = %v, want %v", routes, want)
		}
		second, err := s.Snapshot(ctx, "env-a")
		if err != nil {
			t.Fatalf("Snapshot again: %v", err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("two reads with no write between differ:\n%+v\n%+v", first, second)
		}
	})

	t.Run("another environment's claims and routes never appear", func(t *testing.T) {
		s, st, _ := newTestStore(t)
		seedEnv(t, st, "env-a", "Alpha")
		seedEnv(t, st, "env-b", "Beta")
		seedStack(t, st, "stack-a", "env-a", "web-a")
		seedStack(t, st, "stack-b", "env-b", "web-b")
		registerDomain(t, s, "env-a", "a.example.com")
		registerDomain(t, s, "env-b", "b.example.com")
		claim(t, s, "env-a", "stack-a", "app.a.example.com")
		claim(t, s, "env-b", "stack-b", "app.b.example.com")
		for envID, host := range map[string]string{"env-a": "app.a.example.com", "env-b": "app.b.example.com"} {
			snap, err := s.Snapshot(ctx, envID)
			if err != nil {
				t.Fatalf("Snapshot %s: %v", envID, err)
			}
			if len(snap.Domains) != 1 || len(snap.Claims) != 1 || len(snap.Routes) != 1 ||
				snap.Claims[0].Hostname != host || snap.Routes[0].Hostname != host || snap.Routes[0].EnvID != envID {
				t.Fatalf("Snapshot %s = %+v, want only %s", envID, snap, host)
			}
		}
	})
}

// The read-order rule (settings first) is documented on Snapshot, not timed. What a test can hold is
// that the version a Snapshot carries is the stored one after a write, so the rule has something true to
// order against.
func TestSnapshotReadsSettingsFirst(t *testing.T) {
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "example.com")
	claim(t, s, "env-a", "stack-a", "app.example.com")
	snap, err := s.Snapshot(context.Background(), "env-a")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if want := configVersion(t, st, "env-a"); snap.Settings.ConfigVersion != want || want == 0 {
		t.Fatalf("Settings.ConfigVersion = %d, want the stored %d (non-zero)", snap.Settings.ConfigVersion, want)
	}
	set, err := s.Settings(context.Background(), "env-a")
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if set != snap.Settings {
		t.Fatalf("Settings = %+v, differs from the Snapshot's %+v", set, snap.Settings)
	}
}

func TestVersionsListsEveryEnvironment(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	// Created out of id order, so the order Versions returns is the query's.
	seedEnv(t, st, "env-b", "Beta")
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-c", "Gamma") // never has an ingress change, so no ingress_env row
	seedStack(t, st, "stack-a", "env-a", "web-a")
	seedStack(t, st, "stack-b", "env-b", "web-b")
	registerDomain(t, s, "env-a", "a.example.com")
	registerDomain(t, s, "env-b", "b.example.com")
	claim(t, s, "env-b", "stack-b", "app.b.example.com")
	claim(t, s, "env-a", "stack-a", "app.a.example.com")
	replaceRoutes(t, s, "env-a", "stack-a") // a second change to env-a only
	if _, err := st.Write.Exec(`UPDATE ingress_env SET certs_version = 7 WHERE env_id = 'env-b'`); err != nil {
		t.Fatal(err)
	}

	got, err := s.Versions(ctx)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	want := []ingress.EnvVersion{
		{EnvID: "env-a", ConfigVersion: configVersion(t, st, "env-a"), CertsVersion: 0},
		{EnvID: "env-b", ConfigVersion: configVersion(t, st, "env-b"), CertsVersion: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Versions = %+v, want %+v", got, want)
	}
	if want[0].ConfigVersion <= want[1].ConfigVersion {
		t.Fatalf("env-a has had more changes than env-b, so its version %d should be higher than %d", want[0].ConfigVersion, want[1].ConfigVersion)
	}
}

// Pitfall 4: a claim deleted because its stack row was deleted raises the environment's config_version
// through the ingress_hostnames_cascade trigger, and no Notify is called until a later phase wires one.
func TestVersionsSeesACascadeWithoutNotify(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	n := ingress.NewNotifier()
	s := New(Deps{St: st, Vault: v, Now: func() time.Time { return fixedNow }, Notifier: n})
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "example.com")
	claim(t, s, "env-a", "stack-a", "app.example.com")

	before, err := s.Versions(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("Versions before = %+v, %v; want one environment", before, err)
	}
	sub := n.Subscribe()
	defer sub.Close()

	if _, err := st.Write.Exec(`DELETE FROM stacks WHERE id = 'stack-a'`); err != nil {
		t.Fatalf("delete the stack: %v", err)
	}
	if c := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames`) + count(t, st, `SELECT COUNT(*) FROM ingress_routes`); c != 0 {
		t.Fatalf("%d claim and route rows remain after the stack was deleted, want none", c)
	}
	after, err := s.Versions(ctx)
	if err != nil {
		t.Fatalf("Versions after: %v", err)
	}
	if len(after) != 1 || after[0].ConfigVersion != before[0].ConfigVersion+1 || after[0].CertsVersion != before[0].CertsVersion {
		t.Fatalf("Versions after = %+v, want config_version %d and certs_version unchanged", after, before[0].ConfigVersion+1)
	}
	if p := sub.Pending(); len(p) != 0 {
		t.Fatalf("Pending = %v, want none: a cascade announces nothing", p)
	}
}

// A certificate write moves certs_version alone.
func TestVersionsSeesACertsVersionBump(t *testing.T) {
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	registerDomain(t, s, "env-a", "example.com")
	claim(t, s, "env-a", "stack-a", "app.example.com")
	before, err := s.Versions(context.Background())
	if err != nil || len(before) != 1 {
		t.Fatalf("Versions before = %+v, %v", before, err)
	}
	if _, err := st.Write.Exec(`UPDATE ingress_env SET certs_version = certs_version + 1 WHERE env_id = 'env-a'`); err != nil {
		t.Fatal(err)
	}
	after, err := s.Versions(context.Background())
	if err != nil || len(after) != 1 {
		t.Fatalf("Versions after = %+v, %v", after, err)
	}
	if after[0].CertsVersion != before[0].CertsVersion+1 || after[0].ConfigVersion != before[0].ConfigVersion {
		t.Fatalf("Versions = %+v then %+v, want only certs_version to move", before, after)
	}
}
