// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
