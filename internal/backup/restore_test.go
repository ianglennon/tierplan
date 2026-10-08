// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// targetVault is the master key of the instance a backup is restored into: not
// the key of the fixture that made the backup.
func targetVault(t *testing.T) *vault.Vault {
	t.Helper()
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// schemaOfBinary is the schema version this binary embeds.
func schemaOfBinary(t *testing.T) int64 {
	t.Helper()
	n, err := store.TargetSchema()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var restoreOptions = backup.PrepareOptions{Source: "cli", AppVersion: "v9.9.9"}

// restoreInto stages, prepares and swaps file into dataDir.
func restoreInto(t *testing.T, file []byte, dataDir string, target *vault.Vault) (snapshot string, err error) {
	t.Helper()
	return restoreAt(t, file, dataDir, target, time.Now().UTC())
}

// restoreAt is restoreInto with the time the swap stamps its snapshot with.
func restoreAt(t *testing.T, file []byte, dataDir string, target *vault.Vault, now time.Time) (snapshot string, err error) {
	t.Helper()
	ctx := context.Background()
	staged, err := backup.Stage(ctx, bytes.NewReader(file), []byte(exportPass), dataDir, schemaOfBinary(t))
	if err != nil {
		return "", err
	}
	defer staged.Discard()
	if err := staged.Prepare(ctx, target, restoreOptions); err != nil {
		return "", err
	}
	return staged.Swap(ctx, dataDir, now)
}

func TestInspectReadsTheManifestWithoutTheDatabaseEntry(t *testing.T) {
	f := newExportFixture(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	in, err := backup.Inspect(bytes.NewReader(file), []byte(exportPass), schemaOfBinary(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if in.Manifest.InstanceName != "Home lab" || in.Manifest.AppVersion != "v9.9.9" || in.Manifest.Counts.Users != 1 || !in.Manifest.ChainVerified {
		t.Fatalf("manifest = %+v", in.Manifest)
	}
	if in.Header.Schema != in.Manifest.Schema {
		t.Fatalf("header schema %d, manifest schema %d", in.Header.Schema, in.Manifest.Schema)
	}
}

func TestRestoreIntoAFreshDataDirectoryRewrapsSecretsAndVerifiesTheChainUnderTheNewKey(t *testing.T) {
	f := newExportFixture(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	target := targetVault(t)

	snapshot, err := restoreInto(t, file, dataDir, target)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if snapshot != "" {
		t.Fatalf("a restore into an empty directory took a snapshot: %s", snapshot)
	}
	if entries, _ := os.ReadDir(filepath.Join(dataDir, "restore-staging")); len(entries) != 0 {
		t.Fatalf("staging left behind: %v", entries)
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dataDir, store.DatabaseFile+ext)); err == nil {
			t.Errorf("a %s file sits beside the restored database", ext)
		}
	}

	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var sealed string
	if err := st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := target.Open(vault.ContextAgentCA+"env-1", sealed); err != nil || string(plain) != "ca private key" {
		t.Fatalf("CA key under the target key = %q, %v", plain, err)
	}
	if _, err := f.live.Open(vault.ContextAgentCA+"env-1", sealed); err == nil {
		t.Fatal("the CA key still opens under the source key")
	}
	res, err := audit.Verify(context.Background(), st, target)
	if err != nil || res.Broken != nil || res.Entries != 5 {
		t.Fatalf("Verify = %+v, %v; want 5 entries and no break", res, err)
	}
}

// --- validation before any change (D-30) -------------------------------------

// buildOpts shapes a backup written by hand, the way an older or hostile Bayplan
// could have written one.
type buildOpts struct {
	// maxMigration limits the schema to migrations 1..n; 0 is the embedded set.
	maxMigration int64
	// headerSchema overrides the schema the header and manifest claim; 0 keeps the database's.
	headerSchema int64
	appVersion   string
	username     string
	// sealUnder seals the CA key with this vault instead of the backup's export key.
	sealUnder *vault.Vault
}

// migrationsUpTo is the embedded migrations 1..max, so a database can be built
// as an older release left it.
func migrationsUpTo(t *testing.T, max int64) fstest.MapFS {
	t.Helper()
	dir := os.DirFS("../store/migrations")
	names, err := fs.Glob(dir, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("no migrations: %v", err)
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

const builtCA = "the CA key of env-b"

// buildBackup writes a backup file from a database it builds itself: a user, an
// agent environment with a sealed CA key, and three audit entries signed by a
// source key. The format is written directly, not through Export, so the schema
// can be any release's.
func buildBackup(t *testing.T, o buildOpts) []byte {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mo := store.MigrateOptions{AppVersion: "fixture", DataDir: dir, SkipBackup: true}
	if o.maxMigration > 0 {
		mo.Migrations = migrationsUpTo(t, o.maxMigration)
	}
	res, err := store.Migrate(ctx, st, mo)
	if err != nil {
		t.Fatalf("migrate the fixture: %v", err)
	}
	if o.username == "" {
		o.username = "bob"
	}
	if o.appVersion == "" {
		o.appVersion = "v0.2.0"
	}

	exportKey, err := vault.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	exportVault, err := vault.New(exportKey, "backup-export")
	if err != nil {
		t.Fatal(err)
	}
	sealer := exportVault
	if o.sealUnder != nil {
		sealer = o.sealUnder
	}
	sealed, err := sealer.Seal(vault.ContextAgentCA+"env-b", []byte(builtCA))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Write.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-b', ?, 'x', 1, 1, 1)`, o.username)
	exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES ('env-b', 'agent', 'Prod', '', 1, 1, 1, 'standalone')`)
	exec(`INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-b', 'pem', ?, 1)`, sealed)
	if o.maxMigration == 11 {
		// A Phase 2 install: the first admin holds the global admin role.
		exec(`INSERT INTO roles (id, name, builtin) VALUES ('admin','admin',1)`)
		exec(`INSERT INTO role_permissions (role_id, permission) VALUES ('admin','environments.read')`)
		exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-admin','user','u-b','admin',NULL,1)`)
	}
	source, err := vault.New(bytes.Repeat([]byte{3}, 32), "source")
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.NewWriter(st, source, slog.New(slog.DiscardHandler), nil)
	for range 3 {
		if err := aud.Record(ctx, audit.Entry{
			Actor:  audit.Actor{Type: audit.ActorUser, ID: "u-b", Name: o.username},
			Action: audit.ActionAuthLogin, Result: audit.ResultSuccess,
		}); err != nil {
			t.Fatal(err)
		}
	}

	copyPath := filepath.Join(t.TempDir(), "copy.db")
	conn, err := st.Write.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", copyPath); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	dbBytes, err := os.ReadFile(copyPath) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}

	schema := res.To
	if o.headerSchema != 0 {
		schema = o.headerSchema
	}
	created := time.Now().UTC().Truncate(time.Second)
	manifest, err := json.Marshal(backup.Manifest{
		Format: backup.ManifestFormat, Created: created, AppVersion: o.appVersion, Schema: schema,
		InstanceName: "old instance", PublicURL: "https://old.example", ChainVerified: true,
		Counts:    backup.Counts{Users: 1, Environments: 1, AuditEntries: 3},
		ExportKey: base64.StdEncoding.EncodeToString(exportKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	kdf, err := backup.DefaultKDF()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := backup.NewWriter(&buf, backup.Header{KDF: kdf, Created: created, AppVersion: o.appVersion, Schema: schema}, []byte(exportPass))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(backup.EntryManifest, int64(len(manifest)), bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(backup.EntryDB, int64(len(dbBytes)), bytes.NewReader(dbBytes)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// liveInstall is a data directory holding a restored database, with the hash of
// it, to prove a refused restore changed nothing.
func liveInstall(t *testing.T) (dir string, target *vault.Vault, sum [32]byte) {
	t.Helper()
	f := newExportFixture(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	target = targetVault(t)
	if _, err := restoreInto(t, file, dir, target); err != nil {
		t.Fatalf("set up the live install: %v", err)
	}
	return dir, target, hashDatabase(t, dir)
}

func hashDatabase(t *testing.T, dir string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, store.DatabaseFile)) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "restore-staging"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a staging directory remains: %v", entries)
	}
}

func TestRestoreNoMutationWhenTheFileIsRefused(t *testing.T) {
	dir, target, before := liveInstall(t)
	f := newExportFixture(t)
	_, good, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(good)
	flipped[len(flipped)*3/4] ^= 0xff
	newer := buildBackup(t, buildOpts{headerSchema: schemaOfBinary(t) + 1, appVersion: "v99.0.0"})

	for _, tc := range []struct {
		name string
		file []byte
		pass string
		want error
	}{
		{"wrong passphrase", good, "some other long passphrase", backup.ErrUnreadable},
		{"one flipped byte in the payload", flipped, exportPass, backup.ErrUnreadable},
		{"truncated to half", good[:len(good)/2], exportPass, backup.ErrUnreadable},
		{"a text file", []byte("this is not a backup, just some text that is long enough\n"), exportPass, backup.ErrNotBackup},
		{"a newer schema", newer, exportPass, backup.ErrNewerBackup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staged, err := backup.Stage(context.Background(), bytes.NewReader(tc.file), []byte(tc.pass), dir, schemaOfBinary(t))
			if err == nil {
				staged.Discard()
				t.Fatal("Stage accepted it")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Stage = %v, want %v", err, tc.want)
			}
			if got := hashDatabase(t, dir); got != before {
				t.Fatal("the live database changed")
			}
			assertNoStaging(t, dir)
			_ = target
		})
	}
}

func TestRestoreNewerBackupNamesTheVersionBeforeAnyKeyDerivation(t *testing.T) {
	newer := buildBackup(t, buildOpts{headerSchema: schemaOfBinary(t) + 1, appVersion: "v99.0.0"})
	// A wrong passphrase would be ErrUnreadable if the key derivation ran first.
	_, err := backup.Inspect(bytes.NewReader(newer), []byte("not the passphrase at all"), schemaOfBinary(t))
	if !errors.Is(err, backup.ErrNewerBackup) || !strings.Contains(err.Error(), "v99.0.0") {
		t.Fatalf("Inspect = %v, want ErrNewerBackup naming v99.0.0", err)
	}
}

func TestRestoreMigrateForwardFromSchemaVersion11(t *testing.T) {
	file := buildBackup(t, buildOpts{maxMigration: 11})
	dir := t.TempDir()
	target := targetVault(t)
	if _, err := restoreInto(t, file, dir, target); err != nil {
		t.Fatalf("restore a version 11 backup: %v", err)
	}
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	got, err := st.SchemaVersion(context.Background())
	if err != nil || got != schemaOfBinary(t) {
		t.Fatalf("restored schema = %d, %v; want %d", got, err, schemaOfBinary(t))
	}
	var admin int
	if err := st.Read.QueryRow(`SELECT instance_admin FROM users WHERE id = 'u-b'`).Scan(&admin); err != nil || admin != 1 {
		t.Fatalf("the former global admin: instance_admin = %d, %v; want 1", admin, err)
	}
	var left int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM role_assignments`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("role assignments left = %d, %v; want the global admin row gone", left, err)
	}
	var sealed string
	if err := st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-b'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := target.Open(vault.ContextAgentCA+"env-b", sealed); err != nil || string(plain) != builtCA {
		t.Fatalf("CA key = %q, %v", plain, err)
	}
	res, err := audit.Verify(context.Background(), st, target)
	if err != nil || res.Broken != nil || res.Entries != 5 {
		t.Fatalf("Verify = %+v, %v; want 5 entries (3 + reanchored + restored) and no break", res, err)
	}
}

func TestRestorePrepareFailureLeavesTheLiveDatabaseAndNoStagingDirectory(t *testing.T) {
	dir, target, before := liveInstall(t)
	// The CA key is sealed under a key that is not the backup's export key, so it
	// cannot be re-wrapped; the failure comes after the migration has run.
	stranger, err := vault.New(bytes.Repeat([]byte{5}, 32), "stranger")
	if err != nil {
		t.Fatal(err)
	}
	file := buildBackup(t, buildOpts{sealUnder: stranger})

	staged, err := backup.Stage(context.Background(), bytes.NewReader(file), []byte(exportPass), dir, schemaOfBinary(t))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := staged.Prepare(context.Background(), target, restoreOptions); err == nil {
		t.Fatal("Prepare re-wrapped a value that does not open under the export key")
	}
	if _, err := os.Stat(staged.Dir); err == nil {
		t.Fatal("the staging directory survived a failed Prepare")
	}
	assertNoStaging(t, dir)
	if got := hashDatabase(t, dir); got != before {
		t.Fatal("the live database changed")
	}
}

// --- the pre-restore snapshot and the atomic swap ------------------------------

var prerestoreName = regexp.MustCompile(`^prerestore-\d{8}T\d{6}Z\.db$`)

func TestSwapOverAnExistingDatabaseWritesAPreRestoreSnapshotFirst(t *testing.T) {
	dir, target, _ := liveInstall(t)
	other := buildBackup(t, buildOpts{username: "bob"})
	at := time.Date(2026, 10, 8, 3, 4, 5, 0, time.UTC)

	snapshot, err := restoreAt(t, other, dir, target, at)
	if err != nil {
		t.Fatalf("restore over a live database: %v", err)
	}
	want := filepath.Join(dir, "backups", "prerestore-20261008T030405Z.db")
	if snapshot != want || !prerestoreName.MatchString(filepath.Base(snapshot)) {
		t.Fatalf("snapshot = %q, want %q", snapshot, want)
	}
	if info, err := os.Stat(snapshot); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %v, %v; want 0600", info.Mode(), err)
	}

	// The snapshot is the old database (alice, gone) and the live one is the new (bob).
	names := func(path string) string {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var out []string
		rows, err := db.Query(`SELECT username FROM users ORDER BY username`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return strings.Join(out, ",")
	}
	if got := names(snapshot); got != "alice,gone" {
		t.Fatalf("snapshot users = %q, want the old database (alice,gone)", got)
	}
	if got := names(filepath.Join(dir, store.DatabaseFile)); got != "bob" {
		t.Fatalf("live users = %q, want bob", got)
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, store.DatabaseFile+ext)); err == nil {
			t.Errorf("a %s file sits beside the swapped database", ext)
		}
	}
}

func TestMigrationPrunerLeavesPreRestoreSnapshotsAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir, SkipBackup: true, Migrations: migrationsUpTo(t, 11)}); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	var pre []string
	for i := range 5 {
		name := "prerestore-2026010" + strconv.Itoa(i+1) + "T000000Z.db"
		pre = append(pre, name)
		if err := os.WriteFile(filepath.Join(backups, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(backups, "bayplan-v0.1."+strconv.Itoa(i)+"-2026010"+strconv.Itoa(i+1)+"T000000Z.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Migrating to the head takes a snapshot and prunes the migration snapshots.
	if res, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: "v0.2.0", DataDir: dir}); err != nil || res.SnapshotPath == "" {
		t.Fatalf("Migrate = %+v, %v; want a snapshot", res, err)
	}
	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatal(err)
	}
	var gotPre, gotMig int
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "prerestore-"):
			gotPre++
		case strings.HasPrefix(e.Name(), "bayplan-"):
			gotMig++
		}
	}
	if gotPre != len(pre) || gotMig != 3 {
		t.Fatalf("after the migration pruner: %d prerestore (want %d), %d bayplan-* (want 3)", gotPre, len(pre), gotMig)
	}
}
