// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package store_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// phase2Fixture names what seedPhase2Database wrote, so the assertions after the
// upgrade compare against the seed rather than against literals.
type phase2Fixture struct {
	adminID, aliceID string
	envID, agentID   string
	caPlaintext      []byte
	auditEntries     int
}

// TestPhase2DatabaseUpgradesToHead stages a database exactly as a Phase 2 install
// leaves it (schema version 11: a first admin holding the global admin
// assignment, a second user, an agent environment with a sealed CA key, an agent,
// two sessions, settings and an audit chain from the real writer), upgrades it
// with the embedded migrations, and checks that nothing was lost and that the
// former admin is now an instance admin (PLAT-02, D-01, D-34).
func TestPhase2DatabaseUpgradesToHead(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)

	// Bring the database to schema version 11 with only the migrations a Phase 2
	// release shipped.
	res, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "phase2-fixture", DataDir: dir, SkipBackup: true, Migrations: migrationsUpTo(t, 11),
	})
	if err != nil {
		t.Fatalf("migrate to version 11: %v", err)
	}
	if res.To != 11 {
		t.Fatalf("fixture schema version = %d, want 11", res.To)
	}

	v, err := vault.LoadOrCreate("", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fx := seedPhase2Database(t, s, v)

	// The upgrade itself: the embedded set, an existing database, so a snapshot
	// is taken first (P1 D-83).
	up, err := store.Migrate(ctx, s, store.MigrateOptions{DataDir: dir, AppVersion: "phase2-fixture"})
	if err != nil {
		t.Fatalf("upgrade to head: %v", err)
	}
	if up.From != 11 || up.To <= 11 {
		t.Fatalf("upgrade went %d -> %d, want from 11 to a higher version", up.From, up.To)
	}
	assertSnapshotTaken(t, dir, up.SnapshotPath)
	assertAuthorityConverted(t, s, fx)
	assertSessionsHavePublicIDs(t, s, 2)
	assertCASurvived(t, s, dir, fx)
	assertEnvironmentDataUnchanged(t, s, fx)
	assertAuditChainIntact(t, s, v, fx)
	assertFormerAdminIsInstanceAdmin(t, s, fx)
	assertShellPrefsTableWorks(t, s, fx.adminID)
}

// TestVersion12DatabaseUpgradesToHeadWithTheShellPreferenceTable stages a database as
// the release before the remembered shell choice leaves it (schema version 12), upgrades
// it, and checks that its users are untouched and that user_shell_prefs now exists
// (plan 04-24, D-26).
func TestVersion12DatabaseUpgradesToHeadWithTheShellPreferenceTable(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := open(t, dir)
	res, err := store.Migrate(ctx, s, store.MigrateOptions{
		AppVersion: "v12-fixture", DataDir: dir, SkipBackup: true, Migrations: migrationsUpTo(t, 12),
	})
	if err != nil || res.To != 12 {
		t.Fatalf("migrate to version 12: %v (at %d)", err, res.To)
	}
	var one int
	if err := s.Read.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'user_shell_prefs'`).Scan(&one); err != nil || one != 0 {
		t.Fatalf("user_shell_prefs exists at version 12 (%d, %v)", one, err)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-1','ann','h',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	up, err := store.Migrate(ctx, s, store.MigrateOptions{DataDir: dir, AppVersion: "v12-fixture"})
	if err != nil {
		t.Fatalf("upgrade to head: %v", err)
	}
	if up.From != 12 || up.To < 13 {
		t.Fatalf("upgrade went %d -> %d, want from 12 to at least 13", up.From, up.To)
	}
	assertShellPrefsTableWorks(t, s, "u-1")
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

// seedPhase2Database writes the Phase 2 rows with raw SQL against the version-11
// columns, never through the head-schema queries, so the fixture cannot drift
// when later migrations change a table.
func seedPhase2Database(t *testing.T, s *store.Store, v *vault.Vault) phase2Fixture {
	t.Helper()
	ctx := context.Background()
	fx := phase2Fixture{
		adminID: "u-admin", aliceID: "u-alice",
		envID: "env-swarm", agentID: "agent-1",
		caPlaintext: []byte("not-a-real-ca-private-key-just-bytes"),
	}
	hash, err := auth.HashPassword("a fixture password")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal("agent-ca/v1/"+fx.envID, fx.caPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.Write.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}

	exec(`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?,?,?,?,?,?)`,
		fx.adminID, "admin", hash, 10, 10, 10)
	exec(`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?,?,?,?,?,?)`,
		fx.aliceID, "alice", hash, 20, 20, 20)

	// The Phase 1 role: one admin row with permission rows, held globally by the
	// first admin.
	exec(`INSERT INTO roles (id, name, builtin) VALUES ('admin','admin',1)`)
	exec(`INSERT INTO role_permissions (role_id, permission) VALUES ('admin','environments.read')`)
	exec(`INSERT INTO role_permissions (role_id, permission) VALUES ('admin','users.read')`)
	exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-admin','user',?,'admin',NULL,10)`, fx.adminID)

	// An agent environment with its sealed CA and one connected agent.
	exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type, swarm_cluster_id, engine_id)
	      VALUES (?, 'agent', 'prod swarm', '', 1, 30, 30, 'swarm', 'cluster-abc', 'engine-1')`, fx.envID)
	exec(`INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES (?, 'PEM', ?, 30)`, fx.envID, sealed)
	exec(`INSERT INTO agents (id, env_id, name, role, engine_id, swarm_node_id, public_key, cert_serial, cert_fingerprint, cert_not_after, agent_version, status, created_at, last_seen_at)
	      VALUES (?, ?, 'node-1', 'manager', 'engine-1', 'node-id-1', x'0102', '01', 'ff:ee', 99, '0.1.0', 'active', 31, 32)`, fx.agentID, fx.envID)

	// Two live sessions for the admin; version 11 has no public_id column.
	exec(`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent) VALUES (x'aa01',?,40,41,0,'10.0.0.1','ua-1')`, fx.adminID)
	exec(`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent) VALUES (x'aa02',?,42,43,1,'10.0.0.2','ua-2')`, fx.adminID)

	exec(`INSERT INTO settings (key, value, updated_at) VALUES ('public_url','https://bayplan.example.org',50)`)

	// An audit chain written by the real writer, one entry scoped to the environment.
	w := audit.NewWriter(s, v, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	admin := audit.Actor{Type: audit.ActorUser, ID: fx.adminID, Name: "admin"}
	for i, e := range []audit.Entry{
		{Actor: admin, Action: audit.ActionSetupCompleted, TargetType: "user", TargetID: fx.adminID, Result: audit.ResultSuccess},
		{Actor: admin, Action: audit.ActionAuthLogin, TargetType: "user", TargetID: fx.adminID, Result: audit.ResultSuccess, IP: "10.0.0.1"},
		{Actor: admin, Action: audit.ActionSettingsUpdated, TargetType: "settings", TargetID: "public_url", Result: audit.ResultSuccess},
		{Actor: admin, Action: audit.ActionEnvironmentAdded, TargetType: "environment", TargetID: fx.envID, EnvID: fx.envID, Result: audit.ResultSuccess},
		{Actor: admin, Action: audit.ActionAuthLogin, TargetType: "user", TargetID: fx.adminID, Result: audit.ResultSuccess, IP: "10.0.0.2"},
	} {
		if err := w.Record(ctx, e); err != nil {
			t.Fatalf("seed audit entry %d: %v", i, err)
		}
	}
	fx.auditEntries = 5

	// The chain must already be intact before the upgrade, or the check after it
	// would prove nothing.
	if r, err := audit.Verify(ctx, s, v); err != nil || r.Broken != nil || r.Entries != fx.auditEntries {
		t.Fatalf("fixture chain before upgrade: %+v, %v", r, err)
	}
	return fx
}

func assertSnapshotTaken(t *testing.T, dir, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("no pre-migration snapshot path reported")
	}
	if filepath.Dir(path) != filepath.Join(dir, "backups") || !strings.HasPrefix(filepath.Base(path), "bayplan-") {
		t.Errorf("snapshot %q is not a bayplan-* file under %s/backups", path, dir)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Errorf("snapshot %q missing or empty: %v", path, err)
	}
}

func assertAuthorityConverted(t *testing.T, s *store.Store, fx phase2Fixture) {
	t.Helper()
	ctx := context.Background()
	flag := func(id string) int {
		t.Helper()
		var n int
		if err := s.Read.QueryRowContext(ctx, `SELECT instance_admin FROM users WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if flag(fx.adminID) != 1 {
		t.Error("the former global admin is not an instance admin")
	}
	if flag(fx.aliceID) != 0 {
		t.Error("the second user became an instance admin")
	}
	var n int
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_assignments WHERE role_id = 'admin' AND env_id IS NULL`).Scan(&n); err != nil || n != 0 {
		t.Errorf("global admin assignments left = %d (%v), want 0", n, err)
	}
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'role_permissions'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("role_permissions tables left = %d (%v), want 0", n, err)
	}
	rows, err := s.Read.QueryContext(ctx, `SELECT id FROM roles ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var roles []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		roles = append(roles, id)
	}
	if got := strings.Join(roles, ","); got != "admin,operator,viewer" {
		t.Errorf("roles = %q, want admin,operator,viewer", got)
	}
	// Users themselves are all still there.
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id IN (?, ?)`, fx.adminID, fx.aliceID).Scan(&n); err != nil || n != 2 {
		t.Errorf("users kept = %d (%v), want 2", n, err)
	}
}

func assertSessionsHavePublicIDs(t *testing.T, s *store.Store, want int) {
	t.Helper()
	var filled, distinct, empty int
	err := s.Read.QueryRowContext(context.Background(),
		`SELECT COUNT(public_id), COUNT(DISTINCT public_id), COUNT(CASE WHEN public_id = '' THEN 1 END) FROM sessions`).
		Scan(&filled, &distinct, &empty)
	if err != nil {
		t.Fatal(err)
	}
	if filled != want || distinct != want || empty != 0 {
		t.Errorf("session public_id: %d filled, %d distinct, %d empty; want %d, %d, 0", filled, distinct, empty, want, want)
	}
}

func assertCASurvived(t *testing.T, s *store.Store, dir string, fx phase2Fixture) {
	t.Helper()
	var sealed string
	if err := s.Read.QueryRowContext(context.Background(), `SELECT key_sealed FROM env_cas WHERE env_id = ?`, fx.envID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	// Reload the vault from the key file, as a restarted server would.
	v, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Open("agent-ca/v1/"+fx.envID, sealed)
	if err != nil {
		t.Fatalf("the sealed CA key no longer opens with the master key: %v", err)
	}
	if !bytes.Equal(got, fx.caPlaintext) {
		t.Error("the CA key opens to different bytes after the upgrade")
	}
}

func assertEnvironmentDataUnchanged(t *testing.T, s *store.Store, fx phase2Fixture) {
	t.Helper()
	ctx := context.Background()
	var name, status, version, url string
	if err := s.Read.QueryRowContext(ctx, `SELECT name FROM environments WHERE id = ?`, fx.envID).Scan(&name); err != nil || name != "prod swarm" {
		t.Errorf("environment name = %q (%v), want prod swarm", name, err)
	}
	if err := s.Read.QueryRowContext(ctx, `SELECT status, agent_version FROM agents WHERE id = ? AND env_id = ?`, fx.agentID, fx.envID).Scan(&status, &version); err != nil || status != "active" || version != "0.1.0" {
		t.Errorf("agent row = status %q version %q (%v), want active 0.1.0", status, version, err)
	}
	if err := s.Read.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'public_url'`).Scan(&url); err != nil || url != "https://bayplan.example.org" {
		t.Errorf("settings public_url = %q (%v)", url, err)
	}
}

func assertAuditChainIntact(t *testing.T, s *store.Store, v *vault.Vault, fx phase2Fixture) {
	t.Helper()
	r, err := audit.Verify(context.Background(), s, v)
	if err != nil {
		t.Fatal(err)
	}
	if r.Broken != nil {
		t.Fatalf("audit chain broken after the upgrade: %+v", *r.Broken)
	}
	if r.Entries != fx.auditEntries {
		t.Errorf("audit entries after the upgrade = %d, want %d (none dropped or added)", r.Entries, fx.auditEntries)
	}
}

func assertFormerAdminIsInstanceAdmin(t *testing.T, s *store.Store, fx phase2Fixture) {
	t.Helper()
	ctx := context.Background()
	c := rbac.NewChecker(s)
	a, err := c.Access(ctx, &rbac.Principal{Type: rbac.PrincipalUser, ID: fx.adminID, Name: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Active || !a.InstanceAdmin {
		t.Errorf("former admin Access = Active %v InstanceAdmin %v, want both true", a.Active, a.InstanceAdmin)
	}
	b, err := c.Access(ctx, &rbac.Principal{Type: rbac.PrincipalUser, ID: fx.aliceID, Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if b.InstanceAdmin {
		t.Error("the second user reads as an instance admin")
	}
}
