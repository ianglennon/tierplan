// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/server"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

const (
	adminPassword = "correct horse battery"
	caPlaintext   = "the CA private key of env-a"
)

// instanceA is a source instance: an admin, an agent environment with its CA and
// an enrolled agent, a pending join token, two sessions and audit entries.
type instanceA struct {
	dir   string
	st    *store.Store
	vault *vault.Vault
	file  string // the encrypted backup of it
}

func newInstanceA(t *testing.T) instanceA {
	t.Helper()
	ctx := context.Background()
	dir, st := seedDataDir(t, 2)
	v, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := st.Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "u-admin", Username: "admin", PasswordHash: hash, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, st.Q, "u-admin"); err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal(vault.ContextAgentCA+"env-a", []byte(caPlaintext))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type, name_auto)
			VALUES ('env-a', 'agent', 'Production', '', 1, 1, 1, 'standalone', 0)`, nil},
		{`INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-a', 'cert-pem', ?, 1)`, []any{sealed}},
		{`INSERT INTO agents (id, env_id, name, role, engine_id, public_key, cert_serial, cert_fingerprint, cert_not_after, agent_version, status, created_at)
			VALUES ('agent-1', 'env-a', 'node-1', 'standalone', 'engine-1', x'0102', 'serial-1', 'fp-1', 99, 'v0.1.0', 'active', 1)`, nil},
		{`INSERT INTO env_join_tokens (id, env_id, token_hash, generation, created_at, created_by) VALUES ('jt-1', 'env-a', x'aa', 1, 1, 'u-admin')`, nil},
		{`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent, public_id) VALUES (x'01', 'u-admin', 1, 1, 0, '', '', 'p1')`, nil},
		{`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent, public_id) VALUES (x'02', 'u-admin', 1, 1, 0, '', '', 'p2')`, nil},
		{`INSERT INTO user_links (id, user_id, kind, token_hash, created_at, expires_at, created_by) VALUES ('l1', 'u-admin', 'reset', x'bb', 1, 99, 'u-admin')`, nil},
		{`INSERT INTO settings (key, value, updated_at) VALUES ('key_backup_ack', 'yes', 1)`, nil},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "alpha.bayplan")
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	_, err = backup.Export(ctx, st, v, dir, f, []byte(cliPass), backup.ExportOptions{
		AppVersion: "v0.1.0-alpha", InstanceName: "alpha", PublicURL: "https://alpha.example",
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("export instance A: %v", err)
	}
	return instanceA{dir: dir, st: st, vault: v, file: file}
}

// buildServer starts the server on dir, closed when the test ends.
func buildServer(t *testing.T, dir string) *server.Server {
	t.Helper()
	fl := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.BindFlags(fl)
	env := map[string]string{"BAYPLAN_DATA_DIR": dir}
	cfg, err := config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("server.Build on the restored data directory: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func queryRows(t *testing.T, st *store.Store, q string) []string {
	t.Helper()
	rows, err := st.Read.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestRestoreOntoAFreshVolumeWithItsOwnKeyKeepsSignInSecretsAndTheAuditChain(t *testing.T) {
	a := newInstanceA(t)
	dir := t.TempDir() // a fresh volume: no database, no key

	stdoutB, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "restore", a.file)
	stdout := string(stdoutB)
	if err != nil {
		t.Fatalf("restore = %v, stderr %q", err, errOut)
	}
	for _, want := range []string{"alpha", "https://alpha.example", "v0.1.0-alpha", "1 users", "1 environments", "2 audit entries", "Restored."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout+errOut, cliPass) {
		t.Fatal("the passphrase was printed")
	}

	// The fresh volume got its own master key.
	vb, err := vault.Load("", dir)
	if err != nil {
		t.Fatalf("the restored data directory has no key: %v", err)
	}
	if vb.KeyID() == a.vault.KeyID() {
		t.Fatal("the restored instance uses the source instance's master key")
	}
	if _, err := os.Stat(filepath.Join(dir, "restore-staging")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(dir, "restore-staging"))
		if len(entries) != 0 {
			t.Fatalf("staging left behind: %v", entries)
		}
	}

	// The chain verifies under the new key alone, from the command line too.
	if vout, _, err := runCLI(t, dir, "audit", "verify"); err != nil || strings.TrimSpace(vout) != "audit chain OK: 4 entries" {
		t.Fatalf("audit verify after restore = %q, %v", vout, err)
	}

	srv := buildServer(t, dir)
	st := srv.Store()
	ctx := context.Background()

	if u, err := st.RQ.GetUserByUsername(ctx, "admin"); err != nil || u.InstanceAdmin != 1 {
		t.Fatalf("the restored admin = %+v, %v; want an instance admin", u, err)
	}

	// The CA key opens under the new master key, and not under the old one.
	var sealed string
	if err := st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-a'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := vb.Open(vault.ContextAgentCA+"env-a", sealed); err != nil || string(plain) != caPlaintext {
		t.Fatalf("CA key under the new master key = %q, %v", plain, err)
	}
	if _, err := a.vault.Open(vault.ContextAgentCA+"env-a", sealed); err == nil {
		t.Fatal("the restored CA key still opens under the source instance's key")
	}

	// Agents keep their certificates; nothing that is per-instance travels.
	const agentCols = `SELECT id || '|' || env_id || '|' || name || '|' || cert_serial || '|' || cert_fingerprint || '|' || status FROM agents`
	if got, want := queryRows(t, st, agentCols), queryRows(t, a.st, agentCols); len(want) != 1 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("agents = %v, want %v", got, want)
	}
	for _, table := range []string{"sessions", "user_links", "env_join_tokens"} {
		if n := queryRows(t, st, "SELECT COUNT(*) FROM "+table); n[0] != "0" {
			t.Errorf("%s has %s rows after a restore, want 0", table, n[0])
		}
	}
	if got := queryRows(t, st, `SELECT value FROM settings WHERE key = 'key_backup_ack'`); len(got) != 0 {
		t.Errorf("key_backup_ack survived the restore: %v", got)
	}

	res, err := audit.Verify(ctx, st, vb)
	if err != nil || res.Broken != nil {
		t.Fatalf("audit.Verify with the new key = %+v, %v", res, err)
	}
	last := queryRows(t, st, `SELECT action || '|' || actor_id FROM audit_log ORDER BY seq DESC LIMIT 2`)
	if len(last) != 2 || last[0] != "backup.restored|system:restore" || last[1] != "audit.reanchored|system:restore" {
		t.Fatalf("last two audit entries (newest first) = %v", last)
	}
	var details string
	if err := st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'audit.reanchored'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var re map[string]string
	if err := json.Unmarshal([]byte(details), &re); err != nil {
		t.Fatal(err)
	}
	if re["source_instance"] != "alpha" || re["old_kid"] != a.vault.KeyID() || re["entries"] != "2" || re["chain_verified"] != "true" {
		t.Fatalf("audit.reanchored details = %v", re)
	}
	var restored string
	if err := st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'backup.restored'`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	var rd map[string]string
	if err := json.Unmarshal([]byte(restored), &rd); err != nil {
		t.Fatal(err)
	}
	if rd["source"] != "cli" || rd["source_instance"] != "alpha" || rd["app_version"] != "v0.1.0-alpha" || rd["schema"] == "" || rd["backup_created"] == "" {
		t.Fatalf("backup.restored details = %v", rd)
	}

	// The old admin signs in with the old password.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+adminPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the restored admin could not sign in: %d %s", rec.Code, rec.Body)
	}
}

// liveInstallOn restores a first backup onto a fresh directory, so a second
// restore replaces an existing database.
func liveInstallOn(t *testing.T) (a instanceA, dir string) {
	t.Helper()
	a = newInstanceA(t)
	dir = t.TempDir()
	if _, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "restore", a.file); err != nil {
		t.Fatalf("first restore = %v, stderr %q", err, errOut)
	}
	return a, dir
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestRestoreOverAnExistingDatabaseWithoutYesShowsTheSummaryExitsTwoAndChangesNothing(t *testing.T) {
	a, dir := liveInstallOn(t)
	db := filepath.Join(dir, store.DatabaseFile)
	before := fileSum(t, db)

	stdout, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "restore", a.file)
	if err == nil {
		t.Fatal("restore over an existing database without --yes exited 0")
	}
	if exitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2 (%v)", exitCode(err), err)
	}
	if !strings.Contains(string(stdout), "alpha") || !strings.Contains(string(stdout), "1 users") {
		t.Fatalf("no backup summary on stdout: %q", stdout)
	}
	if !strings.Contains(errOut, "re-run with --yes to replace the current data") {
		t.Fatalf("stderr = %q", errOut)
	}
	if fileSum(t, db) != before {
		t.Fatal("the database changed")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "backups", "prerestore-*")); len(matches) != 0 {
		t.Fatalf("a refused restore wrote a snapshot: %v", matches)
	}
}

func TestRestoreWithYesReplacesTheDatabaseAndSavesThePreviousOne(t *testing.T) {
	a, dir := liveInstallOn(t)

	stdout, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "restore", "--yes", a.file)
	if err != nil {
		t.Fatalf("restore --yes = %v, stderr %q", err, errOut)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "backups", "prerestore-*.db"))
	if len(matches) != 1 || !strings.Contains(string(stdout), matches[0]) {
		t.Fatalf("snapshots = %v; stdout %q", matches, stdout)
	}
	// The master key of the existing install is kept: it is the target.
	v, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if vout, _, err := runCLI(t, dir, "audit", "verify"); err != nil || !strings.HasPrefix(strings.TrimSpace(vout), "audit chain OK") {
		t.Fatalf("audit verify after a second restore = %q, %v (key %s)", vout, err, v.KeyID())
	}
}

func TestRestoreOfAWrongPassphraseOrANonBackupChangesNothingAndSaysWhy(t *testing.T) {
	a, dir := liveInstallOn(t)
	db := filepath.Join(dir, store.DatabaseFile)
	before := fileSum(t, db)

	_, errOut, err := runCLIWith(t, dir, "not the right passphrase\n", "", "restore", "--yes", a.file)
	if err == nil || !strings.Contains(errOut, "passphrase is wrong, or the file is damaged") {
		t.Fatalf("wrong passphrase = %v, stderr %q", err, errOut)
	}
	text := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(text, []byte("just some notes, nothing like a backup file at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, err = runCLIWith(t, dir, cliPass+"\n", "", "restore", "--yes", text)
	if err == nil || !strings.Contains(errOut, "is not a Bayplan backup file") {
		t.Fatalf("a text file = %v, stderr %q", err, errOut)
	}
	if fileSum(t, db) != before {
		t.Fatal("the database changed")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "backups", "prerestore-*")); len(matches) != 0 {
		t.Fatalf("a refused restore wrote a snapshot: %v", matches)
	}
}
