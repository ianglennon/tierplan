// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package vault

import (
	"bytes"
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// noHardLinks swaps the link seam for one that fails the way a file system
// without hard links does (a FAT or some network volumes: EPERM).
func noHardLinks(t *testing.T, err error) {
	t.Helper()
	old := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: err} }
	t.Cleanup(func() { linkFile = old })
}

func keyLine(k []byte) string { return base64.StdEncoding.EncodeToString(k) + "\n" }

func TestWriteKeyFileWithoutHardLinksCreatesExclusively(t *testing.T) {
	noHardLinks(t, syscall.EPERM)
	dir := t.TempDir()
	path := filepath.Join(dir, "bayplan.key")
	key := bytes.Repeat([]byte{7}, 32)

	created, err := writeKeyFile(path, key)
	if err != nil || !created {
		t.Fatalf("writeKeyFile = %v, %v; want created", created, err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // the test reads the key file it just created
	if err != nil || string(got) != keyLine(key) {
		t.Fatalf("key file = %q, %v; want %q", got, err, keyLine(key))
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 600", st, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only bayplan.key (no temporary file left)", len(entries))
	}
}

func TestWriteKeyFileWithoutHardLinksNeverReplacesAnExistingKey(t *testing.T) {
	noHardLinks(t, syscall.EPERM)
	path := filepath.Join(t.TempDir(), "bayplan.key")
	keyA, keyB := bytes.Repeat([]byte{'A'}, 32), bytes.Repeat([]byte{'B'}, 32)
	if err := os.WriteFile(path, []byte(keyLine(keyA)), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := writeKeyFile(path, keyB)
	if err != nil || created {
		t.Fatalf("writeKeyFile = %v, %v; want not created and no error", created, err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // the test reads the key file it wrote
	if err != nil || string(got) != keyLine(keyA) {
		t.Fatalf("key file = %q, %v; want key A kept byte for byte", got, err)
	}
}

func TestWriteKeyFileLinkSaysExistsMeansTheOtherProcessWon(t *testing.T) {
	noHardLinks(t, fs.ErrExist)
	path := filepath.Join(t.TempDir(), "bayplan.key")
	keyA, keyB := bytes.Repeat([]byte{'A'}, 32), bytes.Repeat([]byte{'B'}, 32)
	if err := os.WriteFile(path, []byte(keyLine(keyA)), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := writeKeyFile(path, keyB)
	if err != nil || created {
		t.Fatalf("writeKeyFile = %v, %v; want not created and no error", created, err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // the test reads the key file it wrote
	if err != nil || string(got) != keyLine(keyA) {
		t.Fatalf("key file = %q, %v; want key A kept", got, err)
	}
}
