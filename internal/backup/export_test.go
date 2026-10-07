// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

const exportPass = "correct horse battery"

// exportFixture is a migrated store with a user, a team, an environment with a
// sealed CA key, and audit entries signed by a live vault.
type exportFixture struct {
	st   *store.Store
	live *vault.Vault
	aud  *audit.Writer
	dir  string // the work directory
	// caSealed is the CA key's ciphertext under the live key, as stored.
	caSealed string
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	st := storetest.New(t)
	live, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	f := &exportFixture{
		st: st, live: live, dir: t.TempDir(),
		aud: audit.NewWriter(st, live, slog.New(slog.DiscardHandler), time.Now),
	}
	f.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u1', 'alice', 'x', 1, 1, 1)`)
	f.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at, deleted_at) VALUES ('u2', 'gone', 'x', 1, 1, 1, 5)`)
	f.exec(t, `INSERT INTO teams (id, name, created_at, updated_at) VALUES ('t1', 'Ops', 1, 1)`)
	f.exec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'local', 'This host', '/var/run/docker.sock', 1, 1, 1)`)
	sealed, err := live.Seal(vault.ContextAgentCA+"env-1", []byte("ca private key"))
	if err != nil {
		t.Fatal(err)
	}
	f.caSealed = sealed
	f.exec(t, `INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-1', 'pem', ?, 1)`, sealed)
	for range 3 {
		f.record(t)
	}
	return f
}

func (f *exportFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *exportFixture) record(t *testing.T) {
	t.Helper()
	if err := f.aud.Record(context.Background(), audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: "u1", Name: "alice"},
		Action: audit.ActionAuthLogin, Result: audit.ResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *exportFixture) export(t *testing.T, pass string, o backup.ExportOptions) (backup.ExportResult, []byte, error) {
	t.Helper()
	var buf bytes.Buffer
	res, err := backup.Export(context.Background(), f.st, f.live, f.dir, &buf, []byte(pass), o)
	return res, buf.Bytes(), err
}

var testOptions = backup.ExportOptions{AppVersion: "v9.9.9", InstanceName: "Home lab", PublicURL: "https://bayplan.example"}

// opened is a backup taken apart: its manifest, the raw database bytes and the
// database copied out to a file and opened.
type opened struct {
	manifest backup.Manifest
	dbBytes  []byte
	dbPath   string
	db       *sql.DB
}

func openBackup(t *testing.T, file []byte, pass string) opened {
	t.Helper()
	r, err := backup.Open(bytes.NewReader(file), []byte(pass))
	if err != nil {
		t.Fatalf("backup.Open: %v", err)
	}
	var o opened
	for _, want := range []string{backup.EntryManifest, backup.EntryDB} {
		name, body, err := r.Next()
		if err != nil || name != want {
			t.Fatalf("entry = %q, %v; want %q", name, err, want)
		}
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		if want == backup.EntryManifest {
			if err := json.Unmarshal(data, &o.manifest); err != nil {
				t.Fatalf("manifest.json: %v", err)
			}
		} else {
			o.dbBytes = data
		}
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Fatalf("after the two entries: %v, want io.EOF", err)
	}
	o.dbPath = filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(o.dbPath, o.dbBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", o.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	o.db = db
	return o
}

func (o opened) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := o.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil { //nolint:gosec // a table name from the test
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestExportWritesAFileThatOpensWithThePassphrase(t *testing.T) {
	f := newExportFixture(t)
	res, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Size != int64(len(file)) || res.Size == 0 {
		t.Fatalf("Size = %d, file is %d bytes", res.Size, len(file))
	}
	o := openBackup(t, file, exportPass)

	m := o.manifest
	schema, err := f.st.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != 1 || m.AppVersion != "v9.9.9" || m.Schema != schema ||
		m.InstanceName != "Home lab" || m.PublicURL != "https://bayplan.example" || !m.ChainVerified || m.ExportKey == "" {
		t.Fatalf("manifest = %+v", m)
	}
	want := backup.Counts{Users: 1, Teams: 1, Environments: 1, AuditEntries: 3}
	if m.Counts != want {
		t.Fatalf("counts = %+v, want %+v", m.Counts, want)
	}
	if res.Manifest != m {
		t.Fatalf("result manifest %+v differs from the file's %+v", res.Manifest, m)
	}

	var integrity string
	if err := o.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
	if o.count(t, "users") != 2 || o.count(t, "env_cas") != 1 || o.count(t, "audit_log") != 3 {
		t.Fatal("the database inside the backup lost rows")
	}
}

func TestExportDoesNotOpenWithAnotherPassphrase(t *testing.T) {
	f := newExportFixture(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Open(bytes.NewReader(file), []byte("some other passphrase")); err == nil {
		t.Fatal("a backup opened with the wrong passphrase")
	}
}

func TestExportRefusesAShortPassphraseBeforeAnyWork(t *testing.T) {
	f := newExportFixture(t)
	_, file, err := f.export(t, "fifteen chars!!", testOptions) // 15 characters
	if err != backup.ErrPassphraseTooShort {
		t.Fatalf("Export with 15 characters = %v, want ErrPassphraseTooShort", err)
	}
	if len(file) != 0 {
		t.Fatalf("wrote %d bytes before refusing", len(file))
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != 0 {
		t.Fatalf("a refused export left %d entries in the work directory", len(entries))
	}
}

func TestExportLeavesNothingInTheWorkDirectory(t *testing.T) {
	f := newExportFixture(t)
	if _, _, err := f.export(t, exportPass, testOptions); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != 0 {
		t.Fatalf("the work directory still holds %d entries", len(entries))
	}
}

// Markers that must never be found in a backup's bytes.
var (
	sessionMarker = []byte("SESSION-ID-HASH-MARKER-0123456789")
	linkMarker    = []byte("INVITE-LINK-HASH-MARKER-0123456789")
	joinMarker    = []byte("JOIN-TOKEN-HASH-MARKER-0123456789")
)

// seedExcluded adds a row to each table a backup must leave out.
func (f *exportFixture) seedExcluded(t *testing.T) {
	t.Helper()
	f.exec(t, `INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent, public_id)
		VALUES (?, 'u1', 1, 1, 0, '192.0.2.1', 'agent', 'pub-1')`, sessionMarker)
	f.exec(t, `INSERT INTO user_links (id, user_id, kind, token_hash, created_at, expires_at, created_by)
		VALUES ('l1', 'u1', 'invite', ?, 1, 2, 'u1')`, linkMarker)
	f.exec(t, `INSERT INTO env_join_tokens (id, env_id, token_hash, generation, created_at, created_by)
		VALUES ('j1', 'env-1', ?, 1, 1, 'u1')`, joinMarker)
	f.exec(t, `INSERT INTO backup_runs (id, source, started_at) VALUES ('r1', 'ui', 1)`)
}

func (f *exportFixture) liveCount(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil { //nolint:gosec // a table name from the test
		t.Fatal(err)
	}
	return n
}

func TestExportLeavesOutTheExcludedTablesAndKeepsEveryIncludedRow(t *testing.T) {
	f := newExportFixture(t)
	f.seedExcluded(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)

	for name, class := range backup.Tables {
		switch class {
		case backup.Excluded:
			if n := o.count(t, name); n != 0 {
				t.Errorf("excluded table %s holds %d rows in the backup", name, n)
			}
			if f.liveCount(t, name) == 0 {
				t.Errorf("the test seeded nothing into %s, so it proves nothing", name)
			}
		case backup.Included:
			if got, want := o.count(t, name), f.liveCount(t, name); got != want {
				t.Errorf("included table %s has %d rows in the backup, %d live", name, got, want)
			}
		}
	}
}

func TestExportKeepsOnlyTheAuditPruneAnchorInAppMeta(t *testing.T) {
	f := newExportFixture(t)
	f.exec(t, `INSERT INTO app_meta (key, value, updated_at) VALUES ('audit_last_pruned_hmac', 'ab', 1), ('key_rotated_at', 'x', 1)`)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)
	rows, err := o.db.Query(`SELECT key FROM app_meta ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if len(keys) != 1 || keys[0] != "audit_last_pruned_hmac" {
		t.Fatalf("app_meta keys in the backup = %v, want only audit_last_pruned_hmac", keys)
	}
}

func TestExportSecureDeleteLeavesNoResidueOfExcludedRows(t *testing.T) {
	f := newExportFixture(t)
	f.seedExcluded(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)

	for name, marker := range map[string][]byte{"session id hash": sessionMarker, "invite link hash": linkMarker, "join token hash": joinMarker} {
		if bytes.Contains(o.dbBytes, marker) {
			t.Errorf("the %s is still in the backup's database bytes", name)
		}
	}
	// A copy kept in a free page is not a row, so ask the file itself.
	var free int
	if err := o.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free != 0 {
		t.Errorf("freelist_count = %d, %v; want 0 after VACUUM", free, err)
	}
	// The live-key ciphertext of the CA key is gone too: it was replaced, not appended.
	if bytes.Contains(o.dbBytes, []byte(f.caSealed)) {
		t.Error("the live-key ciphertext of the CA key is in the backup's database bytes")
	}
}

func TestExportKeyOpensEverySealedValueAndTheLiveKeyOpensNone(t *testing.T) {
	f := newExportFixture(t)
	f.exec(t, `INSERT INTO backup_schedule (id, passphrase_sealed) VALUES (1, ?)`, f.sealedFor(t, vault.ContextBackupSchedule, "scheduled passphrase"))
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)

	raw, err := base64.StdEncoding.DecodeString(o.manifest.ExportKey)
	if err != nil || len(raw) != 32 {
		t.Fatalf("exportKey = %q, %v; want 32 base64 bytes", o.manifest.ExportKey, err)
	}
	export, err := vault.New(raw, "test")
	if err != nil {
		t.Fatal(err)
	}
	var ca, sched string
	if err := o.db.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`).Scan(&ca); err != nil {
		t.Fatal(err)
	}
	if err := o.db.QueryRow(`SELECT passphrase_sealed FROM backup_schedule WHERE id = 1`).Scan(&sched); err != nil {
		t.Fatal(err)
	}
	if plain, err := export.Open(vault.ContextAgentCA+"env-1", ca); err != nil || string(plain) != "ca private key" {
		t.Fatalf("CA key under the export key = %q, %v", plain, err)
	}
	if plain, err := export.Open(vault.ContextBackupSchedule, sched); err != nil || string(plain) != "scheduled passphrase" {
		t.Fatalf("schedule passphrase under the export key = %q, %v", plain, err)
	}
	if _, err := f.live.Open(vault.ContextAgentCA+"env-1", ca); !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("the live key opens the backup's CA key: %v", err)
	}
	if _, err := f.live.Open(vault.ContextBackupSchedule, sched); !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("the live key opens the backup's schedule passphrase: %v", err)
	}
}

func (f *exportFixture) sealedFor(t *testing.T, ctx, plain string) string {
	t.Helper()
	sealed, err := f.live.Seal(ctx, []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestExportDoesNotCarryTheMasterKey(t *testing.T) {
	f := newExportFixture(t)
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)
	key := bytes.Repeat([]byte{7}, 32)
	manifest, _ := json.Marshal(o.manifest)
	for name, data := range map[string][]byte{"database": o.dbBytes, "manifest": manifest, "file": file} {
		if bytes.Contains(data, key) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(key))) {
			t.Errorf("the master key is in the backup's %s", name)
		}
	}
}

func TestExportLeavesTheLiveDatabaseAsItWas(t *testing.T) {
	f := newExportFixture(t)
	f.seedExcluded(t)
	before := map[string]int{}
	for name := range backup.Tables {
		if name == "sqlite_sequence" {
			continue
		}
		before[name] = f.liveCount(t, name)
	}
	if _, _, err := f.export(t, exportPass, testOptions); err != nil {
		t.Fatal(err)
	}
	for name, n := range before {
		if got := f.liveCount(t, name); got != n {
			t.Errorf("live table %s went from %d to %d rows", name, n, got)
		}
	}
	var sealed string
	if err := f.st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`).Scan(&sealed); err != nil || sealed != f.caSealed {
		t.Fatalf("the live CA key changed: %v", err)
	}
	if res, err := audit.Verify(context.Background(), f.st, f.live); err != nil || res.Broken != nil {
		t.Fatalf("the live chain after an export = %+v, %v", res, err)
	}
}

func TestExportChainBrokenIsRefusedUnlessToldNotToAndRecordedInTheManifest(t *testing.T) {
	f := newExportFixture(t)
	f.exec(t, `UPDATE audit_log SET action = 'x' || substr(action, 2) WHERE seq = 2`)

	_, file, err := f.export(t, exportPass, testOptions)
	if !errors.Is(err, backup.ErrChainBroken) {
		t.Fatalf("Export over a broken chain = %v, want ErrChainBroken", err)
	}
	if len(file) != 0 {
		t.Fatalf("wrote %d bytes of a refused export", len(file))
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
		t.Fatalf("a refused export left %d entries behind", len(entries))
	}

	opts := testOptions
	opts.AllowBrokenChain = true
	res, file, err := f.export(t, exportPass, opts)
	if err != nil {
		t.Fatalf("Export with AllowBrokenChain: %v", err)
	}
	if res.Manifest.ChainVerified {
		t.Fatal("the result says the chain verified")
	}
	if o := openBackup(t, file, exportPass); o.manifest.ChainVerified {
		t.Fatal("the manifest says chainVerified is true for a broken chain")
	}
}

// blockedWriter holds the first Write until released, so an export stays in
// progress for as long as a test needs.
type blockedWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockedWriter() *blockedWriter {
	return &blockedWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockedWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return len(p), nil
}

func TestExportAllowsOneAtATime(t *testing.T) {
	f := newExportFixture(t)
	slow := newBlockedWriter()
	done := make(chan error, 1)
	go func() {
		_, err := backup.Export(context.Background(), f.st, f.live, f.dir, slow, []byte(exportPass), testOptions)
		done <- err
	}()
	select {
	case <-slow.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first export never started writing")
	}

	_, file, err := f.export(t, exportPass, testOptions)
	if !errors.Is(err, backup.ErrBusy) {
		t.Fatalf("a second Export while one runs = %v, want ErrBusy", err)
	}
	if len(file) != 0 {
		t.Fatalf("the refused export wrote %d bytes", len(file))
	}

	close(slow.release)
	if err := <-done; err != nil {
		t.Fatalf("the first export: %v", err)
	}
	if _, _, err := f.export(t, exportPass, testOptions); err != nil {
		t.Fatalf("an export after the first finished: %v", err)
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
		t.Fatalf("the work directory holds %d entries", len(entries))
	}
}
