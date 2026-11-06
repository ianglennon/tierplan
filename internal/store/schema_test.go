// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package store_test

import (
	"context"
	"testing"

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
