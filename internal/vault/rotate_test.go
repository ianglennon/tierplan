// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package vault_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/vault"
)

func rotKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestWriteKeyFileWritesOneBase64LineAtMode0600AndReadKeyFileReadsItBack(t *testing.T) {
	dir := t.TempDir()
	key := rotKey(3)

	if err := vault.WriteKeyFile(dir, vault.NewKeyFile, key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, vault.NewKeyFile)
	raw, err := os.ReadFile(path) //nolint:gosec // the file the test just wrote
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != base64.StdEncoding.EncodeToString(key)+"\n" {
		t.Fatalf("key file = %q, want one base64 line", raw)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	got, err := vault.ReadKeyFile(dir, vault.NewKeyFile)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("ReadKeyFile = %x, %v; want the key back", got, err)
	}
	// No temporary file is left beside it.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d files after the write, want 1", len(entries))
	}
}

func TestReadKeyFileOfAMissingOrMalformedFileFailsWithoutEchoingIt(t *testing.T) {
	dir := t.TempDir()
	if _, err := vault.ReadKeyFile(dir, vault.OldKeyFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file = %v, want ErrNotExist", err)
	}
	if err := os.WriteFile(filepath.Join(dir, vault.OldKeyFile), []byte("c2VjcmV0LXRleHQ=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := vault.ReadKeyFile(dir, vault.OldKeyFile)
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("malformed file = %v, want an error that does not echo the content", err)
	}
}

func TestLoadAndLoadOrCreateAddTheOldKeyFileAsANonActiveKey(t *testing.T) {
	dir := t.TempDir()
	oldKey, newKey := rotKey(1), rotKey(2)
	if err := vault.WriteKeyFile(dir, vault.KeyFile, newKey); err != nil {
		t.Fatal(err)
	}
	if err := vault.WriteKeyFile(dir, vault.OldKeyFile, oldKey); err != nil {
		t.Fatal(err)
	}
	oldOnly, err := vault.New(oldKey, "file")
	if err != nil {
		t.Fatal(err)
	}
	sealedOld, err := oldOnly.Seal("ctx", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	for name, load := range map[string]func() (*vault.Vault, error){
		"Load":         func() (*vault.Vault, error) { return vault.Load("", dir) },
		"LoadOrCreate": func() (*vault.Vault, error) { return vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler)) },
	} {
		v, err := load()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantActive, _ := vault.New(newKey, "file")
		if v.KeyID() != wantActive.KeyID() {
			t.Errorf("%s: active key %s, want the key file's %s", name, v.KeyID(), wantActive.KeyID())
		}
		plain, err := v.Open("ctx", sealedOld)
		if err != nil || string(plain) != "secret" {
			t.Errorf("%s: a value sealed under the old key = %q, %v; want it to open", name, plain, err)
		}
	}
}

func TestAnUnreadableOldKeyFileIsSkippedAndNeverFatal(t *testing.T) {
	dir := t.TempDir()
	if err := vault.WriteKeyFile(dir, vault.KeyFile, rotKey(2)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, vault.OldKeyFile), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Load("", dir)
	if err != nil {
		t.Fatalf("Load with a damaged %s = %v, want it skipped", vault.OldKeyFile, err)
	}
	if v.KeyID() == "" {
		t.Fatal("no active key")
	}
}
