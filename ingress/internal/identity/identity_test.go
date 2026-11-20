// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
)

const (
	keyRel    = "state/node.key"
	recordRel = "state/identity.json"
)

// preparedRoot is a temporary root with the state/ and tmp/ directories the supervisor's layout makes.
func preparedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"state", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatalf("make %s: %v", d, err)
		}
	}
	return root
}

func readFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func TestKeyCreatedOnceAndReloaded(t *testing.T) {
	root := preparedRoot(t)
	key, err := New(fsx.OS(), root).LoadOrCreateKey()
	if err != nil {
		t.Fatalf("create the key: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("key is %d bytes, want %d", len(key), ed25519.PrivateKeySize)
	}
	path := filepath.Join(root, keyRel)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the key file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode is %o, want 600", info.Mode().Perm())
	}
	block, rest := pem.Decode(readFile(t, root, keyRel))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("the key file is not one PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse PKCS#8: %v", err)
	}
	if got, ok := parsed.(ed25519.PrivateKey); !ok || !bytes.Equal(got, key) {
		t.Fatalf("the file does not hold the returned Ed25519 key")
	}

	before := readFile(t, root, keyRel)
	again, err := New(fsx.OS(), root).LoadOrCreateKey()
	if err != nil {
		t.Fatalf("reload the key: %v", err)
	}
	if !bytes.Equal(again, key) {
		t.Fatalf("a new Store over the same root returned a different key")
	}
	s := New(fsx.OS(), root)
	for i := 0; i < 2; i++ {
		if k, err := s.LoadOrCreateKey(); err != nil || !bytes.Equal(k, key) {
			t.Fatalf("call %d on one Store: %v", i, err)
		}
	}
	if !bytes.Equal(readFile(t, root, keyRel), before) {
		t.Fatalf("the key file changed on reload")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	root := preparedRoot(t)
	s := New(fsx.OS(), root)
	if got, err := s.Enrolled(); err != nil || got {
		t.Fatalf("Enrolled on an empty volume = %v, %v; want false, nil", got, err)
	}
	if _, ok, err := s.Load(); err != nil || ok {
		t.Fatalf("Load on an empty volume = ok %v, err %v; want false, nil", ok, err)
	}
	want := Record{NodeID: "n-1", EnvID: "e-1", InstanceID: "i-1"}
	if err := s.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, recordRel))
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode is %o, want 600", info.Mode().Perm())
	}
	fresh := New(fsx.OS(), root)
	got, ok, err := fresh.Load()
	if err != nil || !ok || got != want {
		t.Fatalf("Load on a new Store = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
	if enrolled, err := fresh.Enrolled(); err != nil || !enrolled {
		t.Fatalf("Enrolled = %v, %v; want true, nil", enrolled, err)
	}
}

func TestSaveRefusesIncompleteRecord(t *testing.T) {
	root := preparedRoot(t)
	s := New(fsx.OS(), root)
	for name, r := range map[string]Record{
		"empty node ID":     {EnvID: "e-1", InstanceID: "i-1"},
		"empty instance ID": {NodeID: "n-1", EnvID: "e-1"},
		"empty record":      {},
	} {
		if err := s.Save(r); err == nil {
			t.Errorf("%s: Save accepted it", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, recordRel)); !os.IsNotExist(err) {
		t.Fatalf("a refused record left a file behind: %v", err)
	}
	if enrolled, err := s.Enrolled(); err != nil || enrolled {
		t.Fatalf("Enrolled after refusals = %v, %v; want false, nil", enrolled, err)
	}
}
