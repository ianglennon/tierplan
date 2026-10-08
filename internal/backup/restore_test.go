// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
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
	ctx := context.Background()
	staged, err := backup.Stage(ctx, bytes.NewReader(file), []byte(exportPass), dataDir, schemaOfBinary(t))
	if err != nil {
		return "", err
	}
	defer staged.Discard()
	if err := staged.Prepare(ctx, target, restoreOptions); err != nil {
		return "", err
	}
	return staged.Swap(ctx, dataDir, time.Now().UTC())
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
