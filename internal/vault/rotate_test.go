// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package vault_test

import (
	"bytes"
	"context"
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

func neverAsked(t *testing.T) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		t.Helper()
		t.Error("the database was asked, but this case must not ask it")
		return "", nil
	}
}

func answers(id string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return id, nil }
}

func put(t *testing.T, dir, name string, key []byte) {
	t.Helper()
	if err := vault.WriteKeyFile(dir, name, key); err != nil {
		t.Fatal(err)
	}
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func content(t *testing.T, dir, name string) []byte {
	t.Helper()
	k, err := vault.ReadKeyFile(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRecoverRotationDiscardsANewKeyTheDatabaseDoesNotName(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, vault.KeyFile, rotKey(1))
	put(t, dir, vault.NewKeyFile, rotKey(2))

	for name, answer := range map[string]string{"never rotated": "", "another key": vault.KeyIDOf(rotKey(1))} {
		put(t, dir, vault.NewKeyFile, rotKey(2))
		changed, err := vault.RecoverRotation(context.Background(), dir, false, answers(answer))
		if err != nil || !changed {
			t.Fatalf("%s: RecoverRotation = %v, %v; want it to discard", name, changed, err)
		}
		if exists(dir, vault.NewKeyFile) {
			t.Errorf("%s: bayplan.key.new was kept", name)
		}
		if !bytes.Equal(content(t, dir, vault.KeyFile), rotKey(1)) {
			t.Errorf("%s: bayplan.key changed", name)
		}
	}
}

func TestRecoverRotationFinishesBothRenamesWhenTheDatabaseNamesTheNewKey(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, vault.KeyFile, rotKey(1))
	put(t, dir, vault.NewKeyFile, rotKey(2))

	changed, err := vault.RecoverRotation(context.Background(), dir, false, answers(vault.KeyIDOf(rotKey(2))))
	if err != nil || !changed {
		t.Fatalf("RecoverRotation = %v, %v", changed, err)
	}
	if !bytes.Equal(content(t, dir, vault.KeyFile), rotKey(2)) || !bytes.Equal(content(t, dir, vault.OldKeyFile), rotKey(1)) || exists(dir, vault.NewKeyFile) {
		t.Fatal("the key files were not rotated into place")
	}
}

func TestRecoverRotationAfterTheFirstRenameMovesTheNewKeyIntoPlaceWithoutAskingTheDatabase(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, vault.OldKeyFile, rotKey(1))
	put(t, dir, vault.NewKeyFile, rotKey(2))

	changed, err := vault.RecoverRotation(context.Background(), dir, false, neverAsked(t))
	if err != nil || !changed {
		t.Fatalf("RecoverRotation = %v, %v", changed, err)
	}
	if !bytes.Equal(content(t, dir, vault.KeyFile), rotKey(2)) || exists(dir, vault.NewKeyFile) {
		t.Fatal("bayplan.key.new was not moved to bayplan.key")
	}
	// LoadOrCreate now finds that key and mints nothing.
	v, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler))
	if err != nil || v.KeyID() != vault.KeyIDOf(rotKey(2)) {
		t.Fatalf("LoadOrCreate = %v, %v; want the new key", v, err)
	}
}

func TestRecoverRotationLeavesADataDirectoryWithNoNewKeyUntouched(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, vault.KeyFile, rotKey(1))
	put(t, dir, vault.OldKeyFile, rotKey(9))

	changed, err := vault.RecoverRotation(context.Background(), dir, false, neverAsked(t))
	if err != nil || changed {
		t.Fatalf("RecoverRotation = %v, %v; want nothing to do", changed, err)
	}
	if !bytes.Equal(content(t, dir, vault.KeyFile), rotKey(1)) || !bytes.Equal(content(t, dir, vault.OldKeyFile), rotKey(9)) {
		t.Fatal("the key files changed")
	}
	// An empty directory too.
	if changed, err := vault.RecoverRotation(context.Background(), t.TempDir(), false, neverAsked(t)); err != nil || changed {
		t.Fatalf("RecoverRotation on an empty directory = %v, %v", changed, err)
	}
}

func TestRecoverRotationReportsAFailureToReadTheDatabase(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, vault.KeyFile, rotKey(1))
	put(t, dir, vault.NewKeyFile, rotKey(2))
	boom := errors.New("boom")
	_, err := vault.RecoverRotation(context.Background(), dir, false, func(context.Context) (string, error) { return "", boom })
	if !errors.Is(err, boom) || !exists(dir, vault.NewKeyFile) {
		t.Fatalf("RecoverRotation = %v; want the error and bayplan.key.new kept", err)
	}
}

func TestRecoverRotationNeverRenamesIntoTheKeyFileForAnEnvironmentKey(t *testing.T) {
	// The database names the new key: bayplan.key.new stays for the operator,
	// and no bayplan.key is made, in either file layout a crash could leave.
	for name, setup := range map[string]func(dir string){
		"no key file":  func(dir string) {},
		"old key file": func(dir string) { put(t, dir, vault.OldKeyFile, rotKey(1)) },
	} {
		dir := t.TempDir()
		setup(dir)
		put(t, dir, vault.NewKeyFile, rotKey(2))

		changed, err := vault.RecoverRotation(context.Background(), dir, true, answers(vault.KeyIDOf(rotKey(2))))
		if err != nil || changed {
			t.Fatalf("%s: RecoverRotation = %v, %v; want it left alone", name, changed, err)
		}
		if !exists(dir, vault.NewKeyFile) || exists(dir, vault.KeyFile) {
			t.Errorf("%s: bayplan.key.new must stay and bayplan.key must not appear", name)
		}
	}
}

func TestRecoverRotationDiscardsAStaleNewKeyForAnEnvironmentKeyToo(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, vault.NewKeyFile, rotKey(2))
	changed, err := vault.RecoverRotation(context.Background(), dir, true, answers(""))
	if err != nil || !changed || exists(dir, vault.NewKeyFile) {
		t.Fatalf("RecoverRotation = %v, %v; want the stale file discarded", changed, err)
	}
}
