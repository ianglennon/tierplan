// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package store_test

import (
	"context"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ianglennon/coaming/internal/store"
)

// migrationsUpTo returns the real embedded migrations with a version no higher
// than max, so a test can stage a database as an older release left it.
func migrationsUpTo(t *testing.T, max int64) fstest.MapFS {
	t.Helper()
	dir := os.DirFS("migrations")
	names, err := fs.Glob(dir, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("no embedded migrations: %v", err)
	}
	m := fstest.MapFS{}
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %s has no numeric prefix", name)
		}
		if v > max {
			continue
		}
		b, err := fs.ReadFile(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		m[name] = &fstest.MapFile{Data: b}
	}
	return m
}

// TestAccessMigrationTurnsTheFormerGlobalAdminIntoAnInstanceAdmin runs migration
// 00012 over a version-11 database shaped like a Phase 1 install: the admin role
// with its permissions, one user holding it globally, one holding it on a single
// environment, one holding nothing, and a live session (D-01, D-03, PLAT-02).
func TestAccessMigrationTurnsTheFormerGlobalAdminIntoAnInstanceAdmin(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.1", DataDir: dir, Migrations: migrationsUpTo(t, 11)}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-admin','admin','x',1,1,1)`,
		`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-env','envadmin','x',1,1,1)`,
		`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-none','nobody','x',1,1,1)`,
		`INSERT INTO roles (id, name, builtin) VALUES ('admin','admin',1)`,
		`INSERT INTO role_permissions (role_id, permission) VALUES ('admin','environments.read')`,
		`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-1','user','u-admin','admin',NULL,1)`,
		`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-2','user','u-env','admin','env-x',1)`,
		`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent) VALUES (x'01','u-admin',1,1,0,'','')`,
		`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent) VALUES (x'02','u-admin',1,1,0,'','')`,
	} {
		if _, err := s.Write.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2", DataDir: dir})
	if err != nil {
		t.Fatalf("Migrate to the embedded set: %v", err)
	}
	if res.From != 11 || res.To < 12 {
		t.Fatalf("migrated %d -> %d, want from 11 to at least 12", res.From, res.To)
	}

	flag := func(id string) int {
		var n int
		if err := s.Read.QueryRowContext(ctx, `SELECT instance_admin FROM users WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if flag("u-admin") != 1 || flag("u-env") != 0 || flag("u-none") != 0 {
		t.Errorf("instance_admin = admin %d, env %d, none %d; want 1, 0, 0", flag("u-admin"), flag("u-env"), flag("u-none"))
	}
	var ids []string
	rows, err := s.Read.QueryContext(ctx, `SELECT id FROM role_assignments ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != "ra-2" {
		t.Errorf("assignments left = %v, want only the environment-scoped ra-2", ids)
	}
	if _, err := s.Read.ExecContext(ctx, `SELECT 1 FROM role_permissions`); err == nil {
		t.Error("role_permissions still exists; the registry is the only definition of a role")
	}
	var nRoles int
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM roles WHERE id IN ('admin','operator','viewer') AND builtin = 1`).Scan(&nRoles); err != nil || nRoles != 3 {
		t.Errorf("builtin roles = %d (%v), want 3", nRoles, err)
	}
	var nPublic, nDistinct int
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(public_id), COUNT(DISTINCT public_id) FROM sessions`).Scan(&nPublic, &nDistinct); err != nil || nPublic != 2 || nDistinct != 2 {
		t.Errorf("session public ids = %d filled, %d distinct (%v), want 2 and 2", nPublic, nDistinct, err)
	}
	// A subject now holds one assignment per scope.
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-3','user','u-env','viewer','env-x',2)`); err == nil {
		t.Error("a second role for the same subject and environment was accepted")
	}
}

// TestAccessMigrationOnAFreshDatabaseSeedsTheThreeRoles covers an install that
// never ran the admin-role seeding: the migration itself provides the roles.
func TestAccessMigrationOnAFreshDatabaseSeedsTheThreeRoles(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM roles`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("roles = %d (%v), want 3", n, err)
	}
}
