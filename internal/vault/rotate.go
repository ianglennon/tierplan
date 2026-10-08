// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package vault

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// NewKeyFile is where `bayplan key rotate` puts the new master key before
	// it commits. After the commit it is renamed to KeyFile (a file-sourced
	// key) or left for the operator to move into BAYPLAN_ENCRYPTION_KEY.
	NewKeyFile = "bayplan.key.new"
	// OldKeyFile keeps the previous master key after a rotation until the admin
	// deletes it. The vault loads it as a non-active key, which is what lets the
	// rotation's attestation be checked (D-33).
	OldKeyFile = "bayplan.key.old"

	// MetaActiveKeyID is the app_meta key that names the master key the database
	// was last rotated to. It is absent until the first rotation.
	MetaActiveKeyID = "active_key_id"
)

// KeyIDOf is the key ID a master key has, as recorded on its ciphertexts.
func KeyIDOf(key []byte) string { return keyID(key) }

// WriteKeyFile writes key to <dir>/<name> as one line of base64 at mode 0600:
// to a temporary file in the same directory, fsynced, renamed over name, and the
// directory fsynced. An existing file of that name is replaced, so a caller that
// must not do that checks first. The key is never part of an error.
func WriteKeyFile(dir, name string, key []byte) error {
	if len(key) != keyLen {
		return fmt.Errorf("vault: the key must be exactly %d bytes, got %d", keyLen, len(key))
	}
	tmp, err := os.CreateTemp(dir, ".bayplan.key-*")
	if err != nil {
		return fmt.Errorf("write key file %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // a no-op once renamed

	if err := tmp.Chmod(keyFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write key file %s: %w", name, err)
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write key file %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write key file %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write key file %s: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("write key file %s: %w", name, err)
	}
	return syncDir(dir)
}

// ReadKeyFile reads the master key in <dir>/<name>. A missing file satisfies
// errors.Is(err, fs.ErrNotExist). The content is never part of an error.
func ReadKeyFile(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path) //nolint:gosec // a key file name inside the data directory
	if err != nil {
		return nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	key, err := decodeKey(string(raw))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return key, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the operator data directory
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// RenameKeyFile renames <dir>/<from> to <dir>/<to>, replacing <to>, and syncs
// the directory so the rename survives a crash.
func RenameKeyFile(dir, from, to string) error {
	if err := os.Rename(filepath.Join(dir, from), filepath.Join(dir, to)); err != nil {
		return fmt.Errorf("rename %s to %s: %w", from, to, err)
	}
	return syncDir(dir)
}

// RecoverRotation finishes or discards a master-key rotation that a crash cut
// short, before the vault is loaded. It must run with the data-directory lock
// held. activeKID reports the key ID the database names as active (app_meta
// active_key_id; empty when there is no database or no rotation yet), and
// keyFromEnv says the running key comes from BAYPLAN_ENCRYPTION_KEY. It reports
// whether it changed anything.
//
// The commit of the database is the point of no return, and it decides:
//
//   - bayplan.key.new absent: nothing to do.
//   - bayplan.key absent and bayplan.key.old present: the crash came between the
//     two renames, so bayplan.key.new becomes bayplan.key. The database is not
//     asked. Starting without this would let LoadOrCreate mint a fresh key.
//   - the database names the key in bayplan.key.new: the commit happened, so the
//     renames are finished (bayplan.key to bayplan.key.old, then bayplan.key.new
//     to bayplan.key).
//   - otherwise the commit never happened and bayplan.key.new is deleted.
//
// A key from the environment is never replaced by a file: bayplan.key.new stays
// for the operator while the database names it, and is deleted only when it is a
// leftover of a rotation that never committed.
func RecoverRotation(ctx context.Context, dataDir string, keyFromEnv bool, activeKID func(context.Context) (string, error)) (changed bool, err error) {
	newKey, err := ReadKeyFile(dataDir, NewKeyFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		// Unreadable: it cannot be the key of a committed rotation, because the
		// commit follows a successful write of it.
		newKey = nil
	}

	_, keyErr := os.Stat(filepath.Join(dataDir, KeyFile))
	keyMissing := errors.Is(keyErr, fs.ErrNotExist)
	if keyErr != nil && !keyMissing {
		return false, fmt.Errorf("look for %s: %w", KeyFile, keyErr)
	}
	_, oldErr := os.Stat(filepath.Join(dataDir, OldKeyFile))
	oldPresent := oldErr == nil

	if !keyFromEnv && keyMissing && oldPresent && newKey != nil {
		if err := RenameKeyFile(dataDir, NewKeyFile, KeyFile); err != nil {
			return false, err
		}
		return true, nil
	}

	kid, err := activeKID(ctx)
	if err != nil {
		return false, fmt.Errorf("read the active key ID: %w", err)
	}
	committed := newKey != nil && kid != "" && kid == keyID(newKey)

	switch {
	case !committed:
		if err := os.Remove(filepath.Join(dataDir, NewKeyFile)); err != nil {
			return false, fmt.Errorf("remove the unused %s: %w", NewKeyFile, err)
		}
		return true, syncDir(dataDir)
	case keyFromEnv:
		return false, nil
	case keyMissing:
		// No old file to prove a rotation, but the database names this key.
		if err := RenameKeyFile(dataDir, NewKeyFile, KeyFile); err != nil {
			return false, err
		}
		return true, nil
	case oldPresent:
		return false, fmt.Errorf("a rotation was committed but %s already exists: move it somewhere safe, then start again", OldKeyFile)
	}
	if err := RenameKeyFile(dataDir, KeyFile, OldKeyFile); err != nil {
		return false, err
	}
	if err := RenameKeyFile(dataDir, NewKeyFile, KeyFile); err != nil {
		return true, err
	}
	return true, nil
}
