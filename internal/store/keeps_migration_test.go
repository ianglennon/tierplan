// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package store_test

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/ianglennon/tierplan/internal/store"
)

// networkKeepRow is a network keep in either table shape: ref is the network's name.
type networkKeepRow struct {
	EnvID, NodeID, Name, KeptBy string
	KeptAt                      int64
}

// seedNetworkKeeps stages a version-28 database with two environments and the keeps a Phase 10
// install leaves: one swarm-wide network (node ”) and one node-local network.
func seedNetworkKeeps(t *testing.T, s *store.Store) []networkKeepRow {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'prod', '', 1, 30, 30)`,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-2', 'agent', 'lab', '', 1, 30, 30)`,
	} {
		if _, err := s.Write.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed environment: %v", err)
		}
	}
	want := []networkKeepRow{
		{EnvID: "env-1", NodeID: "", Name: "frontend", KeptBy: "user-1", KeptAt: 1_700_000_000_000_000_001},
		{EnvID: "env-1", NodeID: "node-a", Name: "bridge-cache", KeptBy: "user-2", KeptAt: 1_700_000_000_000_000_002},
		{EnvID: "env-2", NodeID: "", Name: "frontend", KeptBy: "user-1", KeptAt: 1_700_000_000_000_000_003},
	}
	for _, r := range want {
		if _, err := s.Write.ExecContext(ctx, `INSERT INTO network_keeps (env_id, node_id, name, kept_by, kept_at) VALUES (?, ?, ?, ?, ?)`,
			r.EnvID, r.NodeID, r.Name, r.KeptBy, r.KeptAt); err != nil {
			t.Fatalf("seed network keep: %v", err)
		}
	}
	return want
}

// oldKeeps reads the keeps of one environment the way the version-28 queries did, ordered by node
// and name.
func oldKeeps(t *testing.T, s *store.Store, envID string) []networkKeepRow {
	t.Helper()
	rows, err := s.Read.Query(`SELECT env_id, node_id, name, kept_by, kept_at FROM network_keeps WHERE env_id = ? ORDER BY node_id, name`, envID)
	if err != nil {
		t.Fatalf("read network_keeps: %v", err)
	}
	defer rows.Close()
	var out []networkKeepRow
	for rows.Next() {
		var r networkKeepRow
		if err := rows.Scan(&r.EnvID, &r.NodeID, &r.Name, &r.KeptBy, &r.KeptAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableExists(t *testing.T, s *store.Store, name string) bool {
	t.Helper()
	var n int
	if err := s.Read.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// TestVersion28DatabaseUpgradesNetworkKeeps stages a database as the release before the janitor
// leaves it (schema version 28) with network keeps in it, upgrades it, and checks that every keep
// is now a janitor_keeps row of kind network and that the Address space queries return the same
// rows as before (plan 11-20, D-12, JAN-07: no network a person chose Keep on is lost).
func TestVersion28DatabaseUpgradesNetworkKeeps(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	res, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "v28-fixture", DataDir: dir, SkipBackup: true, Migrations: migrationsUpTo(t, 28),
	})
	if err != nil || res.To != 28 {
		t.Fatalf("migrate to version 28: %v (at %d)", err, res.To)
	}
	if tableExists(t, s, "janitor_keeps") || !tableExists(t, s, "network_keeps") {
		t.Fatal("version 28 must have network_keeps and no janitor_keeps")
	}
	seeded := seedNetworkKeeps(t, s)
	before := map[string][]networkKeepRow{"env-1": oldKeeps(t, s, "env-1"), "env-2": oldKeeps(t, s, "env-2")}

	up, err := store.Migrate(ctx, s, store.MigrateOptions{DataDir: dir, AppVersion: "v28-fixture"})
	if err != nil {
		t.Fatalf("upgrade to head: %v", err)
	}
	if up.From != 28 || up.To < 29 {
		t.Fatalf("upgrade went %d -> %d, want from 28 to at least 29", up.From, up.To)
	}
	if tableExists(t, s, "network_keeps") || !tableExists(t, s, "janitor_keeps") {
		t.Fatal("after the upgrade network_keeps must be gone and janitor_keeps present")
	}

	// Every keep arrived as kind network, with the network's name as both ref and name.
	var count int
	if err := s.Read.QueryRow(`SELECT COUNT(*) FROM janitor_keeps`).Scan(&count); err != nil || count != len(seeded) {
		t.Fatalf("janitor_keeps rows = %d (%v), want %d", count, err, len(seeded))
	}
	for _, r := range seeded {
		var kind, ref, name, by string
		var at int64
		err := s.Read.QueryRow(`SELECT kind, ref, name, kept_by, kept_at FROM janitor_keeps WHERE env_id = ? AND node_id = ? AND ref = ?`,
			r.EnvID, r.NodeID, r.Name).Scan(&kind, &ref, &name, &by, &at)
		if err != nil {
			t.Fatalf("keep %+v missing after the upgrade: %v", r, err)
		}
		if kind != "network" || ref != r.Name || name != r.Name || by != r.KeptBy || at != r.KeptAt {
			t.Errorf("keep %+v arrived as kind %q ref %q name %q by %q at %d", r, kind, ref, name, by, at)
		}
	}

	// The Address space queries answer with the same rows they did before.
	for env, was := range before {
		got, err := s.Q.ListNetworkKeeps(ctx, env)
		if err != nil {
			t.Fatalf("ListNetworkKeeps %s: %v", env, err)
		}
		if len(got) != len(was) {
			t.Fatalf("%s: %d keeps after, %d before", env, len(got), len(was))
		}
		for i, g := range got {
			if (networkKeepRow{EnvID: g.EnvID, NodeID: g.NodeID, Name: g.Name, KeptBy: g.KeptBy, KeptAt: g.KeptAt}) != was[i] {
				t.Errorf("%s keep %d = %+v, was %+v", env, i, g, was[i])
			}
		}
	}

	// The environment's keeps go with it, as they did in network_keeps.
	if _, err := s.Write.ExecContext(ctx, `DELETE FROM environments WHERE id = 'env-2'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Q.ListNetworkKeeps(ctx, "env-2"); err != nil || len(got) != 0 {
		t.Fatalf("keeps of a deleted environment = %+v (%v), want none", got, err)
	}
}

// TestJanitorKeepsDownMigration migrates a version-29 database with network keeps back to
// version 28 and checks that every network keep is restored, that a container or image keep
// (which the older shape cannot hold) is dropped without harming them, and that migrating up
// again brings them all back (plan 11-20, D-12).
func TestJanitorKeepsDownMigration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "v28-fixture", DataDir: dir, SkipBackup: true, Migrations: migrationsUpTo(t, 28),
	}); err != nil {
		t.Fatalf("migrate to version 28: %v", err)
	}
	seeded := seedNetworkKeeps(t, s)
	before := map[string][]networkKeepRow{"env-1": oldKeeps(t, s, "env-1"), "env-2": oldKeeps(t, s, "env-2")}

	migrations := migrationsUpTo(t, 29)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "v29-fixture", DataDir: dir, SkipBackup: true, Migrations: migrations,
	}); err != nil {
		t.Fatalf("migrate to version 29: %v", err)
	}
	// Keeps of the other kinds, which only the new table can hold.
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO janitor_keeps (env_id, kind, node_id, ref, name, kept_by, kept_at)
		VALUES ('env-1', 'container', 'node-a', 'c0ffee', 'db', 'user-1', 5), ('env-1', 'image', '', 'sha256:abc', 'nginx:1', 'user-1', 6)`); err != nil {
		t.Fatalf("seed other keeps: %v", err)
	}
	// An unknown kind is refused.
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO janitor_keeps (env_id, kind, ref, kept_by, kept_at) VALUES ('env-1', 'volume', 'v', 'u', 1)`); err == nil {
		t.Fatal("a janitor_keeps row of kind volume was accepted, want the CHECK to refuse it")
	}

	p, err := goose.NewProvider(goose.DialectSQLite3, s.Write, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 28); err != nil {
		t.Fatalf("migrate down to 28: %v", err)
	}
	if tableExists(t, s, "janitor_keeps") || !tableExists(t, s, "network_keeps") {
		t.Fatal("after migrating down janitor_keeps must be gone and network_keeps back")
	}
	for env, was := range before {
		got := oldKeeps(t, s, env)
		if len(got) != len(was) {
			t.Fatalf("%s: %d network keeps after migrating down, want %d", env, len(got), len(was))
		}
		for i := range got {
			if got[i] != was[i] {
				t.Errorf("%s keep %d = %+v, want %+v", env, i, got[i], was[i])
			}
		}
	}
	var total int
	if err := s.Read.QueryRow(`SELECT COUNT(*) FROM network_keeps`).Scan(&total); err != nil || total != len(seeded) {
		t.Fatalf("network_keeps rows = %d (%v), want %d: only network keeps come back", total, err, len(seeded))
	}

	// And up again: the same keeps return to janitor_keeps.
	if _, err := p.UpTo(ctx, 29); err != nil {
		t.Fatalf("migrate up to 29 again: %v", err)
	}
	if err := s.Read.QueryRow(`SELECT COUNT(*) FROM janitor_keeps WHERE kind = 'network'`).Scan(&total); err != nil || total != len(seeded) {
		t.Fatalf("janitor_keeps network rows after the round trip = %d (%v), want %d", total, err, len(seeded))
	}
}
