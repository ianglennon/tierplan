// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package vault_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

func newVaultFor(t *testing.T, fill byte) *vault.Vault {
	t.Helper()
	v, err := vault.New(testKey(fill), "test")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRewrapAllMovesEverySealedValueToTheTargetVault(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	a, b := newVaultFor(t, 1), newVaultFor(t, 2)

	caCtx := vault.ContextAgentCA + "env-1"
	caSealed, err := a.Seal(caCtx, []byte("ca private key"))
	if err != nil {
		t.Fatal(err)
	}
	passSealed, err := a.Seal(vault.ContextBackupSchedule, []byte("backup passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
			VALUES ('env-1', 'local', 'This host', '/var/run/docker.sock', 1, 1, 1)`, nil},
		{`INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at) VALUES ('env-1', 'pem', ?, 1)`, []any{caSealed}},
		{`INSERT INTO backup_schedule (id, passphrase_sealed) VALUES (1, ?)`, []any{passSealed}},
	} {
		if _, err := s.Write.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := vault.RewrapAll(ctx, tx, a, b)
	if err != nil {
		t.Fatalf("RewrapAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("RewrapAll = %d, want 2", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var gotCA, gotPass string
	if err := s.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = 'env-1'`).Scan(&gotCA); err != nil {
		t.Fatal(err)
	}
	if err := s.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule WHERE id = 1`).Scan(&gotPass); err != nil {
		t.Fatal(err)
	}
	if plain, err := b.Open(caCtx, gotCA); err != nil || string(plain) != "ca private key" {
		t.Fatalf("CA key under B = %q, %v", plain, err)
	}
	if plain, err := b.Open(vault.ContextBackupSchedule, gotPass); err != nil || string(plain) != "backup passphrase" {
		t.Fatalf("passphrase under B = %q, %v", plain, err)
	}
	if _, err := a.Open(caCtx, gotCA); !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("the old vault still opens the re-wrapped CA key: %v", err)
	}
}

func TestRewrapAllSkipsAnEmptySealedValue(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO backup_schedule (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := vault.RewrapAll(ctx, tx, newVaultFor(t, 1), newVaultFor(t, 2))
	if err != nil || n != 0 {
		t.Fatalf("RewrapAll = %d, %v; want 0, nil", n, err)
	}
}

func TestRewrapAllNamesTheColumnThatDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	a, b := newVaultFor(t, 1), newVaultFor(t, 2)
	// Sealed under the wrong context: the AEAD refuses it.
	bad, err := a.Seal("some other context", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO backup_schedule (id, passphrase_sealed) VALUES (1, ?)`, bad); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = vault.RewrapAll(ctx, tx, a, b)
	if !errors.Is(err, vault.ErrDecrypt) {
		t.Fatalf("RewrapAll error = %v, want ErrDecrypt", err)
	}
	if got := err.Error(); !strings.Contains(got, "backup_schedule") || !strings.Contains(got, "passphrase_sealed") {
		t.Fatalf("error %q does not name the table and column", got)
	}
}

func TestAddKeyLetsOneVaultOpenValuesFromAnOlderKey(t *testing.T) {
	a, b := newVaultFor(t, 1), newVaultFor(t, 2)
	sealed, err := a.Seal("ctx", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open("ctx", sealed); !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("B opened A's value before AddKey: %v", err)
	}
	id, err := b.AddKey(testKey(1))
	if err != nil || id != a.KeyID() {
		t.Fatalf("AddKey = %q, %v; want %q", id, err, a.KeyID())
	}
	if b.KeyID() == a.KeyID() {
		t.Fatal("AddKey changed the active key")
	}
	if plain, err := b.Open("ctx", sealed); err != nil || string(plain) != "v" {
		t.Fatalf("Open after AddKey = %q, %v", plain, err)
	}
	if _, err := b.AddKey([]byte("short")); err == nil {
		t.Fatal("AddKey accepted a short key")
	}
	if _, err := vault.New([]byte("short"), "test"); err == nil {
		t.Fatal("New accepted a short key")
	}
	k, err := vault.GenerateKey()
	if err != nil || len(k) != 32 {
		t.Fatalf("GenerateKey = %d bytes, %v", len(k), err)
	}
}
