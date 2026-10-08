// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/vault"
)

// rotatedDir is a data directory a server has run on, then had its master key
// rotated by the command. It returns the directory and the old and new key IDs.
func rotatedDir(t *testing.T) (dir, oldKID, newKID string) {
	t.Helper()
	dir = t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store().Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'local', 'This host', '/var/run/docker.sock', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	ca, err := srv.Vault().Seal(vault.ContextAgentCA+"env-1", []byte("ca private key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store().Write.Exec(`INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-1', 'pem', ?, 1)`, ca); err != nil {
		t.Fatal(err)
	}
	if err := srv.Audit().Record(context.Background(), audit.Entry{
		Actor: audit.SystemCLI, Action: audit.ActionAuthLogin, Result: audit.ResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	old := srv.Vault()
	res, err := backup.RotateKey(context.Background(), srv.Store(), dir, old, audit.SystemCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, res.OldKID, res.NewKID
}

func mv(t *testing.T, dir, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(dir, from), filepath.Join(dir, to)); err != nil {
		t.Fatal(err)
	}
}

func buildAt(t *testing.T, dir string) *Server {
	t.Helper()
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func assertServesOnNewKey(t *testing.T, srv *Server, newKID string) {
	t.Helper()
	if srv.Vault().KeyID() != newKID {
		t.Fatalf("the server runs on %s, want the rotated key %s", srv.Vault().KeyID(), newKID)
	}
	var sealed string
	if err := srv.Store().Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := srv.Vault().Open(vault.ContextAgentCA+"env-1", sealed); err != nil || string(plain) != "ca private key" {
		t.Fatalf("CA key = %q, %v", plain, err)
	}
	res, err := audit.Verify(context.Background(), srv.Store(), srv.Vault())
	if err != nil || res.Broken != nil {
		t.Fatalf("verify = %+v, %v (broken %+v)", res, err, res.Broken)
	}
}

func TestRotatedDataDirectoryStartsOnTheNewKey(t *testing.T) {
	dir, _, newKID := rotatedDir(t)
	assertServesOnNewKey(t, buildAt(t, dir), newKID)
}

func TestRotationCrashedAfterTheCommitIsFinishedAtTheNextStart(t *testing.T) {
	dir, oldKID, newKID := rotatedDir(t)
	// Put the key files back as they stand when the process dies after the
	// commit and before any rename: the old key in bayplan.key, the new one in
	// bayplan.key.new, no bayplan.key.old.
	mv(t, dir, vault.KeyFile, vault.NewKeyFile)
	mv(t, dir, vault.OldKeyFile, vault.KeyFile)

	srv := buildAt(t, dir)
	assertServesOnNewKey(t, srv, newKID)
	if _, err := os.Stat(filepath.Join(dir, vault.NewKeyFile)); err == nil {
		t.Errorf("%s was left behind", vault.NewKeyFile)
	}
	kept, err := vault.ReadKeyFile(dir, vault.OldKeyFile)
	if err != nil || vault.KeyIDOf(kept) != oldKID {
		t.Errorf("%s = %v, %v; want the previous key", vault.OldKeyFile, kept, err)
	}
}

func TestRotationCrashedBetweenTheRenamesNeverCreatesAFreshKey(t *testing.T) {
	dir, _, newKID := rotatedDir(t)
	// bayplan.key renamed to bayplan.key.old, bayplan.key.new not yet in place.
	mv(t, dir, vault.KeyFile, vault.NewKeyFile)

	srv := buildAt(t, dir)
	assertServesOnNewKey(t, srv, newKID)
}

func TestRotationCrashedBeforeTheCommitDiscardsTheStaleNewKeyAndStartsOnTheOldKey(t *testing.T) {
	dir := t.TempDir() + "/data"
	first := buildAt(t, dir)
	oldKID := first.Vault().KeyID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	stale, err := vault.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.WriteKeyFile(dir, vault.NewKeyFile, stale); err != nil {
		t.Fatal(err)
	}

	srv := buildAt(t, dir)
	if srv.Vault().KeyID() != oldKID {
		t.Fatalf("the server runs on %s, want the old key %s", srv.Vault().KeyID(), oldKID)
	}
	if _, err := os.Stat(filepath.Join(dir, vault.NewKeyFile)); err == nil {
		t.Errorf("the stale %s was kept", vault.NewKeyFile)
	}
}

func envConfig(t *testing.T, dir string, key []byte) map[string]string {
	t.Helper()
	return map[string]string{"BAYPLAN_DATA_DIR": dir, "BAYPLAN_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(key)}
}

func TestEnvironmentKeyRotationNeedsTheNewVariableAndKeepsTheNewKeyFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/data"
	oldKey, err := vault.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Build(ctx, testConfig(t, envConfig(t, dir, oldKey)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store().Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'local', 'This host', '/var/run/docker.sock', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	ca, err := srv.Vault().Seal(vault.ContextAgentCA+"env-1", []byte("ca private key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store().Write.Exec(`INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-1', 'pem', ?, 1)`, ca); err != nil {
		t.Fatal(err)
	}
	res, err := backup.RotateKey(ctx, srv.Store(), dir, srv.Vault(), audit.SystemCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	// The old variable no longer matches the database: refuse, say what to do.
	_, err = Build(ctx, testConfig(t, envConfig(t, dir, oldKey)), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("the server started on the old variable after a rotation")
	}
	want := "Set BAYPLAN_ENCRYPTION_KEY to the contents of " + filepath.Join(dir, vault.NewKeyFile)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Build = %v, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(oldKey)) {
		t.Fatal("the error echoes the key")
	}
	// The refused start released the lock.
	newKey, err := vault.ReadKeyFile(dir, vault.NewKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if vault.KeyIDOf(newKey) != res.NewKID {
		t.Fatalf("%s holds %s, want %s", vault.NewKeyFile, vault.KeyIDOf(newKey), res.NewKID)
	}

	// The new variable works, verifies, and the file is left for the operator.
	ok, err := Build(ctx, testConfig(t, envConfig(t, dir, newKey)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Build on the new variable: %v", err)
	}
	t.Cleanup(func() { _ = ok.Close() })
	assertServesOnNewKey(t, ok, res.NewKID)
	if _, err := os.Stat(filepath.Join(dir, vault.NewKeyFile)); err != nil {
		t.Errorf("%s was removed: %v", vault.NewKeyFile, err)
	}
	if _, err := os.Stat(filepath.Join(dir, vault.KeyFile)); err == nil {
		t.Errorf("%s was created for an environment-sourced key", vault.KeyFile)
	}
}
