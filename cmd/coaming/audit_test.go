// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/vault"
)

// seedDataDir creates a migrated database and a generated key file in a new
// data directory and records n audit entries, as a running server would.
func seedDataDir(t *testing.T, n int) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	v, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	w := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), time.Now)
	for range n {
		if err := w.Record(ctx, audit.Entry{
			Actor:  audit.Actor{Type: audit.ActorUser, ID: "u", Name: "alice"},
			Action: audit.ActionAuthLogin, Result: audit.ResultSuccess,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return dir, st
}

func runCLI(t *testing.T, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("COAMING_DATA_DIR", dir)
	t.Setenv("COAMING_ENCRYPTION_KEY", "")
	t.Setenv("COAMING_ENCRYPTION_KEY_FILE", "")
	var out, errb bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func TestAuditVerifyReportsAnEmptyLogAsOK(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	out, _, err := runCLI(t, dir, "audit", "verify")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if strings.TrimSpace(out) != "audit chain OK: 0 entries" {
		t.Fatalf("output = %q", out)
	}
}

func TestAuditVerifyCountsEntries(t *testing.T) {
	dir, _ := seedDataDir(t, 3)
	out, _, err := runCLI(t, dir, "audit", "verify")
	if err != nil || strings.TrimSpace(out) != "audit chain OK: 3 entries" {
		t.Fatalf("verify = %q, %v", out, err)
	}
}

func TestAuditVerifyNamesTheBrokenEntryAndFails(t *testing.T) {
	dir, st := seedDataDir(t, 4)
	var id string
	if err := st.Read.QueryRow("SELECT id FROM audit_log WHERE seq = 2").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec("UPDATE audit_log SET action = 'x' || substr(action, 2) WHERE seq = 2"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, dir, "audit", "verify")
	if err == nil {
		t.Fatal("verify exited 0 on a tampered log")
	}
	want := "audit chain BROKEN at seq 2 (id " + id + "): hmac mismatch"
	if strings.TrimSpace(out) != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestAuditVerifyNeverGeneratesAKey(t *testing.T) {
	other := t.TempDir() // a data directory with no key
	_, errOut, err := runCLI(t, other, "audit", "verify")
	if err == nil || !strings.Contains(errOut, "no encryption key") {
		t.Fatalf("verify without a key = %v, stderr %q", err, errOut)
	}
	if _, _, err := runCLI(t, other, "audit", "verify"); err == nil {
		t.Fatal("still no key on the second try")
	}
	if matches, _ := coamingKeyFiles(other); len(matches) != 0 {
		t.Fatalf("verify created a key file: %v", matches)
	}
}

func TestAuditVerifyWithoutADatabaseSaysSo(t *testing.T) {
	dir := t.TempDir()
	if _, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runCLI(t, dir, "audit", "verify")
	if err == nil || !strings.Contains(errOut, "no database") {
		t.Fatalf("verify without a database = %v, stderr %q", err, errOut)
	}
}

func coamingKeyFiles(dir string) ([]string, error) {
	return filepath.Glob(filepath.Join(dir, vault.KeyFile))
}

func TestAuditVerifyNamesUncheckedRotationAttestationsOnlyWhenThereAreSome(t *testing.T) {
	dir, st := seedDataDir(t, 3)
	ctx := context.Background()
	oldV, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := vault.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	newV, err := vault.New(newKey, "file")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := audit.Rekey(ctx, tx, st.Q, newV)
	if err != nil {
		t.Fatal(err)
	}
	w := audit.NewWriter(st, newV, slog.New(slog.DiscardHandler), time.Now)
	if err := w.RecordRotationTx(ctx, tx, oldV.Subkey(vault.LabelAuditHMAC), oldV.KeyID(), res, audit.SystemCLI); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The server now runs on the new key alone.
	keyPath := filepath.Join(dir, vault.KeyFile)
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(newKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCLI(t, dir, "audit", "verify")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "audit chain OK: 4 entries" ||
		lines[1] != "1 key rotation attestation(s) not checked: the previous key is not loaded" {
		t.Fatalf("output = %q", out)
	}
}
