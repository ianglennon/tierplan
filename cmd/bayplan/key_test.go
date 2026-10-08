// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// seedSecrets puts a sealed CA key and a sealed schedule passphrase, both under
// the data directory's key, into the database.
func seedSecrets(t *testing.T, dir string, st *store.Store) {
	t.Helper()
	v, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := v.Seal(vault.ContextAgentCA+"env-1", []byte("ca private key"))
	if err != nil {
		t.Fatal(err)
	}
	pass, err := v.Seal(vault.ContextBackupSchedule, []byte("schedule passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'local', 'This host', '/var/run/docker.sock', 1, 1, 1)`, nil},
		{`INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-1', 'pem', ?, 1)`, []any{ca}},
		{`INSERT INTO backup_schedule (id, passphrase_sealed) VALUES (1, ?)`, []any{pass}},
	} {
		if _, err := st.Write.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

func TestKeyRotateThenVerifyMovesSecretsAndChainAndServerStartsOnTheNewKey(t *testing.T) {
	dir, st := seedDataDir(t, 20)
	seedSecrets(t, dir, st)
	oldV, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runCLI(t, dir, "key", "rotate")
	if err != nil {
		t.Fatalf("key rotate: %v (stderr %q)", err, errOut)
	}
	if !strings.Contains(out, oldV.KeyID()) || !strings.Contains(out, "The old key is kept as bayplan.key.old") {
		t.Fatalf("output = %q, want the old key ID and the note about bayplan.key.old", out)
	}

	both, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if both.KeyID() == oldV.KeyID() {
		t.Fatal("the active key did not change")
	}
	if !strings.Contains(out, both.KeyID()) {
		t.Fatalf("output %q does not name the new key %s", out, both.KeyID())
	}
	for _, name := range []string{vault.KeyFile, vault.OldKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, vault.NewKeyFile)); err == nil {
		t.Errorf("%s was left behind", vault.NewKeyFile)
	}

	res, err := audit.Verify(context.Background(), st, both)
	if err != nil || res.Broken != nil || res.AttestationsUnchecked != 0 {
		t.Fatalf("verify with both keys = %+v, %v (broken %+v)", res, err, res.Broken)
	}
	var action, actor string
	if err := st.Read.QueryRow(`SELECT action, actor_id FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&action, &actor); err != nil {
		t.Fatal(err)
	}
	if action != audit.ActionKeyRotated || actor != audit.SystemCLI.ID {
		t.Fatalf("last entry = %s by %s, want key.rotated by system:cli", action, actor)
	}
	var sealed string
	if err := st.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := both.Open(vault.ContextAgentCA+"env-1", sealed); err != nil || string(plain) != "ca private key" {
		t.Fatalf("CA key under the new key = %q, %v", plain, err)
	}

	// With both keys on disk the attestation is checked.
	out, _, err = runCLI(t, dir, "audit", "verify")
	if err != nil || strings.TrimSpace(out) != "audit chain OK: 21 entries" {
		t.Fatalf("audit verify = %q, %v", out, err)
	}
	// Deleting the old key loses only the attestation check.
	if err := os.Remove(filepath.Join(dir, vault.OldKeyFile)); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, dir, "audit", "verify")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "audit chain OK: 21 entries" ||
		lines[1] != "1 key rotation attestation(s) not checked: the previous key is not loaded" {
		t.Fatalf("audit verify without the old key = %q", out)
	}

	// The server starts on the rotated data directory.
	_ = st.Close()
	srv := buildServer(t, dir)
	if srv.Vault().KeyID() != both.KeyID() {
		t.Fatalf("the server runs on %s, want the rotated key %s", srv.Vault().KeyID(), both.KeyID())
	}
}

func fileBytes(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file the test made
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestKeyRotateRefusesWhenAnOldKeyFileIsThereAndChangesNothing(t *testing.T) {
	dir, st := seedDataDir(t, 5)
	seedSecrets(t, dir, st)
	if err := vault.WriteKeyFile(dir, vault.OldKeyFile, bytes.Repeat([]byte{4}, 32)); err != nil {
		t.Fatal(err)
	}
	keyBefore, oldBefore := fileBytes(t, dir, vault.KeyFile), fileBytes(t, dir, vault.OldKeyFile)

	_, errOut, err := runCLI(t, dir, "key", "rotate")
	if err == nil {
		t.Fatal("key rotate succeeded with bayplan.key.old present")
	}
	if !strings.Contains(errOut, "bayplan.key.old somewhere safe first") {
		t.Fatalf("stderr = %q, want it to ask the admin to move bayplan.key.old", errOut)
	}
	if !bytes.Equal(fileBytes(t, dir, vault.KeyFile), keyBefore) || !bytes.Equal(fileBytes(t, dir, vault.OldKeyFile), oldBefore) {
		t.Error("a refused rotation changed a key file")
	}
	if _, err := os.Stat(filepath.Join(dir, vault.NewKeyFile)); err == nil {
		t.Errorf("a refused rotation left %s", vault.NewKeyFile)
	}
	var n int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionKeyRotated).Scan(&n); err != nil || n != 0 {
		t.Errorf("key.rotated entries = %d, %v; want none", n, err)
	}
}

func TestKeyRotateRefusesWhileBayplanIsRunningAndChangesNothing(t *testing.T) {
	dir := t.TempDir() + "/data"
	srv := buildServer(t, dir)
	keyBefore := fileBytes(t, dir, vault.KeyFile)
	kid := srv.Vault().KeyID()

	_, errOut, err := runCLI(t, dir, "key", "rotate")
	if err == nil {
		t.Fatal("key rotate succeeded beside a running server")
	}
	if !strings.Contains(errOut, "Bayplan is running") || !strings.Contains(errOut, "docker run --rm") {
		t.Fatalf("stderr = %q, want the running notice and the one-off container command", errOut)
	}
	if !bytes.Equal(fileBytes(t, dir, vault.KeyFile), keyBefore) || srv.Vault().KeyID() != kid {
		t.Error("a refused rotation changed the key")
	}
	for _, name := range []string{vault.NewKeyFile, vault.OldKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("a refused rotation created %s", name)
		}
	}
}

func TestKeyRotateDiscardsAStaleNewKeyFromAnInterruptedRunAndRotates(t *testing.T) {
	dir, st := seedDataDir(t, 5)
	seedSecrets(t, dir, st)
	stale, err := vault.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.WriteKeyFile(dir, vault.NewKeyFile, stale); err != nil {
		t.Fatal(err)
	}

	if _, errOut, err := runCLI(t, dir, "key", "rotate"); err != nil {
		t.Fatalf("key rotate: %v (stderr %q)", err, errOut)
	}
	cur, err := vault.ReadKeyFile(dir, vault.KeyFile)
	if err != nil || vault.KeyIDOf(cur) == vault.KeyIDOf(stale) {
		t.Fatalf("the stale key became the master key: %v", err)
	}
}
