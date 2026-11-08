// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/backup"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
)

// mirrorMatchesServed fails the test unless <dir>/tls/server.crt holds the certificate the server is serving.
func mirrorMatchesServed(t *testing.T, srv *Server, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, selfSignedDir, mirrorCertFile)) //nolint:gosec // the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s holds no certificate", mirrorCertFile)
	}
	if served := leafOf(t, srv.https.Config()); !bytes.Equal(served.Raw, block.Bytes) {
		t.Errorf("the mirror is not the certificate the server serves")
	}
}

// A backup taken on one install and restored onto a new data directory with a master key of its own
// serves the same SPKI pin, so every agent that holds the pin still connects (TLS-09, D-11).
func TestARestoreOntoANewDataDirKeepsThePin(t *testing.T) {
	ctx := context.Background()
	dirA := t.TempDir() + "/data"
	a, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dirA}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	pinA := a.TLSPin()
	if pinA == "" {
		t.Fatal("install A serves no pin")
	}
	var file bytes.Buffer
	if _, err := backup.Export(ctx, a.Store(), a.Vault(), dirA, &file, []byte(restoreBackupPass),
		backup.ExportOptions{AppVersion: "v0.1.1", InstanceName: "A"}); err != nil {
		t.Fatal(err)
	}
	keyA := a.Vault().KeyID()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// The new host has a master key of its own; the restore re-wraps the sealed key to it.
	dirB := t.TempDir() + "/data"
	if err := os.MkdirAll(dirB, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := vault.LoadOrCreate("", dirB, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if target.KeyID() == keyA {
		t.Fatal("the new data directory generated the old master key")
	}
	schema, err := store.TargetSchema()
	if err != nil {
		t.Fatal(err)
	}
	staged, err := backup.Stage(ctx, bytes.NewReader(file.Bytes()), []byte(restoreBackupPass), dirB, schema)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer staged.Discard()
	if err := staged.Prepare(ctx, target, backup.PrepareOptions{Source: "cli", AppVersion: "v0.1.1"}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := staged.Swap(ctx, dirB, time.Now().UTC()); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	b := buildAt(t, dirB)
	if got := b.TLSPin(); got != pinA {
		t.Errorf("restored install serves pin %q, want install A's %q", got, pinA)
	}
	if b.Vault().KeyID() == keyA {
		t.Error("install B runs on A's master key; the test did not restore across keys")
	}
	mirrorMatchesServed(t, b, dirB)
}

// Rotating the master key re-wraps the sealed TLS key, so the restart that follows serves the pin it
// served before.
func TestKeyRotationKeepsThePin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/data"
	a, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	pin := a.TLSPin()
	if pin == "" {
		t.Fatal("the install serves no pin")
	}
	before := a.Vault().KeyID()
	if _, err := backup.RotateKey(ctx, a.Store(), dir, a.Vault(), audit.SystemCLI); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	again := buildAt(t, dir)
	if again.Vault().KeyID() == before {
		t.Fatal("the restart still runs on the old master key")
	}
	if got := again.TLSPin(); got != pin {
		t.Errorf("pin after the rotation = %q, want %q", got, pin)
	}
	mirrorMatchesServed(t, again, dir)
}
