// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store"
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
