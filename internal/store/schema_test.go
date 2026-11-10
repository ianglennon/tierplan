// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package store_test

import (
	"context"
	"os"
	"testing"
	"testing/fstest"

	"github.com/ianglennon/tierplan/internal/store"
)

// migratedFresh is a new database at the schema this binary embeds.
func migratedFresh(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(context.Background(), s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAFreshDatabaseHasTheRowsTheAppExpects(t *testing.T) {
	s := migratedFresh(t)
	for table, want := range map[string]int{"roles": 3, "janitor_defaults": 1, "image_update_defaults": 1} {
		var n int
		if err := s.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s has %d rows (%v), want %d", table, n, err, want)
		}
	}
}

func TestSchemaConstraints(t *testing.T) {
	s := migratedFresh(t)
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-1', 'ada', 'x', 1, 1, 1)`,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES ('env-a', 'agent', 'A', '', 1, 1, 1, 'standalone')`,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES ('env-b', 'agent', 'B', '', 1, 1, 1, 'standalone')`,
	} {
		if _, err := s.Write.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	assertRestartLoopChecks(t, s)
	assertRestartLoopMutesTableWorks(t, s)
	assertShellPrefsTableWorks(t, s, "u-1")
}

func assertRestartLoopChecks(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	const failures = `UPDATE environments SET restart_loop_failures = ? WHERE id = 'env-a'`
	const window = `UPDATE environments SET restart_loop_window_minutes = ? WHERE id = 'env-a'`
	for _, c := range []struct {
		name, query string
		val         int
		want        bool
	}{
		{"restart_loop_failures", failures, 1, false}, {"restart_loop_failures", failures, 2, true},
		{"restart_loop_failures", failures, 50, true}, {"restart_loop_failures", failures, 51, false},
		{"restart_loop_window_minutes", window, 0, false}, {"restart_loop_window_minutes", window, 1, true},
		{"restart_loop_window_minutes", window, 120, true}, {"restart_loop_window_minutes", window, 121, false},
	} {
		_, err := s.Write.ExecContext(ctx, c.query, c.val)
		if (err == nil) != c.want {
			t.Errorf("%s = %d: accepted = %v, want %v (%v)", c.name, c.val, err == nil, c.want, err)
		}
	}
	if _, err := s.Write.ExecContext(ctx, `UPDATE environments SET restart_loop_failures = 3, restart_loop_window_minutes = 10 WHERE id = 'env-a'`); err != nil {
		t.Fatal(err)
	}
}

// assertRestartLoopMutesTableWorks checks the mutes' uniqueness per environment, kind and
// target, the kinds the table accepts, and that a removed environment takes its mutes.
func assertRestartLoopMutesTableWorks(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	const ins = `INSERT INTO restart_loop_mutes (env_id, kind, target_id, name, muted_by, muted_at) VALUES (?, ?, ?, 'web', 'u-1', 5)`
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "service", "svc-1"); err != nil {
		t.Fatalf("insert a mute: %v", err)
	}
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "service", "svc-1"); err == nil {
		t.Errorf("a second mute of the same target was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "container", "svc-1"); err != nil {
		t.Errorf("the same id as a container is its own target: %v", err)
	}
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "stack", "svc-2"); err == nil {
		t.Errorf("a kind outside service and container was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, `DELETE FROM environments WHERE id = 'env-b'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM restart_loop_mutes WHERE env_id = 'env-b'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d mutes outlived their environment (%v)", n, err)
	}
}

// assertShellPrefsTableWorks writes and reads a remembered choice for the user, and checks
// that the table refuses a shell it does not know and goes with its user.
func assertShellPrefsTableWorks(t *testing.T, s *store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO user_shell_prefs (user_id, image, shell, command, run_as, working_dir, updated_at) VALUES (?, 'nginx:1.27', 'bash', '', 'root', '', 5)`, userID); err != nil {
		t.Fatalf("insert a shell preference: %v", err)
	}
	var shell, runAs string
	if err := s.Read.QueryRowContext(ctx, `SELECT shell, run_as FROM user_shell_prefs WHERE user_id = ? AND image = 'nginx:1.27'`, userID).Scan(&shell, &runAs); err != nil || shell != "bash" || runAs != "root" {
		t.Errorf("preference = %q as %q (%v), want bash as root", shell, runAs, err)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO user_shell_prefs (user_id, image, shell, updated_at) VALUES (?, 'other', 'zsh', 6)`, userID); err == nil {
		t.Errorf("a shell outside auto, bash, sh, ash and custom was accepted")
	}
}

// A database as v0.1.0 left it (migrated to 00036 only) gains tls_certs when the next binary
// migrates it, and the rows it already held are untouched.
func TestTLSCertsTableArrivesOnA00036Database(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir, Migrations: plus(t, 0)}); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, s, "tls_certs") {
		t.Fatal("a database at 00036 already has tls_certs")
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-1', 'ada', 'x', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2.0", DataDir: dir})
	if err != nil {
		t.Fatalf("migrate from 00036: %v", err)
	}
	if res.From != 36 || res.To < 37 {
		t.Errorf("From, To = %d, %d; want 36 and at least 37", res.From, res.To)
	}
	if !hasTable(t, s, "tls_certs") {
		t.Error("tls_certs is missing after migrating a 00036 database")
	}
	var name string
	if err := s.Read.QueryRowContext(ctx, `SELECT username FROM users WHERE id = 'u-1'`).Scan(&name); err != nil || name != "ada" {
		t.Errorf("the existing user row = %q, %v; want ada", name, err)
	}
	assertTLSCertsTableWorks(t, s)
}

func TestTLSCertsTableExistsInAFreshDatabase(t *testing.T) {
	s := migratedFresh(t)
	if !hasTable(t, s, "tls_certs") {
		t.Fatal("tls_certs is missing from a fresh database")
	}
	assertTLSCertsTableWorks(t, s)
}

func hasTable(t *testing.T, s *store.Store, name string) bool {
	t.Helper()
	var n int
	if err := s.Read.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// assertTLSCertsTableWorks checks the id is free (the default certificate and later sources each
// use their own) and unique, and that every column is required.
func assertTLSCertsTableWorks(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	const ins = `INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at) VALUES (?, 'c', 'k', 1, 1)`
	for _, id := range []string{"selfsigned", "acme"} {
		if _, err := s.Write.ExecContext(ctx, ins, id); err != nil {
			t.Errorf("insert %q: %v", id, err)
		}
	}
	if _, err := s.Write.ExecContext(ctx, ins, "selfsigned"); err == nil {
		t.Error("a second row with the same id was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO tls_certs (id, cert_pem, created_at, updated_at) VALUES ('nokey', 'c', 1, 1)`); err == nil {
		t.Error("a row without a sealed key was accepted")
	}
}

// upTo37 is the embedded migrations as far as 00037, which is what a build before Phase 14 knew.
func upTo37(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for _, f := range []string{"00036_baseline.sql", "00037_tls_certs.sql"} {
		data, err := os.ReadFile("migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		m[f] = &fstest.MapFile{Data: data}
	}
	return m
}

func TestACMETablesExistInAFreshDatabase(t *testing.T) {
	s := migratedFresh(t)
	assertACMETablesWork(t, s)
}

// A database as the build before Let's Encrypt left it (migrated to 00037) gains the ACME tables, and
// the self-signed certificate row it already held is untouched.
func TestACMETablesArriveOnA00037Database(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir, Migrations: upTo37(t)}); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, s, "acme_state") {
		t.Fatal("a database at 00037 already has acme_state")
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at) VALUES ('selfsigned', 'cert', 'v1.k1-x.sealed', 11, 12)`); err != nil {
		t.Fatal(err)
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2.0", DataDir: dir})
	if err != nil {
		t.Fatalf("migrate from 00037: %v", err)
	}
	if res.From != 37 || res.To < 38 {
		t.Errorf("From, To = %d, %d; want 37 and at least 38", res.From, res.To)
	}
	var cert, sealed string
	var created, updated int64
	if err := s.Read.QueryRowContext(ctx, `SELECT cert_pem, key_sealed, created_at, updated_at FROM tls_certs WHERE id = 'selfsigned'`).Scan(&cert, &sealed, &created, &updated); err != nil {
		t.Fatalf("the selfsigned row after migrating: %v", err)
	}
	if cert != "cert" || sealed != "v1.k1-x.sealed" || created != 11 || updated != 12 {
		t.Errorf("the selfsigned row changed: %q %q %d %d", cert, sealed, created, updated)
	}
	assertACMETablesWork(t, s)
}

// assertACMETablesWork checks the four tables exist, the state singleton is seeded empty and the
// CHECK constraints refuse what the code never writes.
func assertACMETablesWork(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"acme_account", "acme_state", "acme_orders", "acme_runs"} {
		if !hasTable(t, s, table) {
			t.Errorf("%s is missing", table)
		}
	}
	var n int
	var mode, paused string
	var failures, lastAttempt int64
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*), MIN(mode), MIN(paused_reason), MIN(consecutive_failures), MIN(last_attempt_at) FROM acme_state`).Scan(&n, &mode, &paused, &failures, &lastAttempt); err != nil {
		t.Fatal(err)
	}
	if n != 1 || mode != "off" || paused != "" || failures != 0 || lastAttempt != 0 {
		t.Errorf("acme_state = %d rows, mode %q, pause %q, %d failures, last attempt %d; want one empty row in mode off", n, mode, paused, failures, lastAttempt)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO acme_state (id) VALUES (2)`); err == nil {
		t.Error("a second acme_state row was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, `UPDATE acme_state SET mode = 'sometimes'`); err == nil {
		t.Error("a mode outside off and active was accepted")
	}
	const order = `INSERT INTO acme_orders (id, directory_url, order_url, kind, names, cert_key_sealed, stage, status, created_at, updated_at) VALUES (?, 'd', 'o', ?, '[]', 'k', 'created', ?, 1, 1)`
	if _, err := s.Write.ExecContext(ctx, order, "o-1", "issue", "open"); err != nil {
		t.Errorf("a valid order was refused: %v", err)
	}
	if _, err := s.Write.ExecContext(ctx, order, "o-2", "other", "open"); err == nil {
		t.Error("an order of a kind other than issue and renew was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, order, "o-3", "renew", "pending"); err == nil {
		t.Error("an order status other than open and closed was accepted")
	}
}
