// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

var (
	// ErrOldKeyPresent means bayplan.key.old already exists. A rotation would
	// replace it, and it may be the only copy of a previous key, so the admin
	// moves it first.
	ErrOldKeyPresent = errors.New("bayplan.key.old already exists")
	// ErrNewKeyPresent means bayplan.key.new already exists: a rotation for a key
	// supplied through BAYPLAN_ENCRYPTION_KEY has not been finished, or its file
	// has not been deleted.
	ErrNewKeyPresent = errors.New("bayplan.key.new already exists")
	// ErrKeyMismatch means the key in use is not the one the database says it was
	// last rotated to, so its sealed values cannot be opened with it.
	ErrKeyMismatch = errors.New("the key in use is not the database's active key")
)

// RotateResult is what RotateKey did. KeyFileSwapped is true when the new key
// is now bayplan.key and the old one bayplan.key.old; it is false for a key
// supplied through BAYPLAN_ENCRYPTION_KEY, where the new key waits in NewKeyPath
// for the operator to put it into the variable.
type RotateResult struct {
	OldKID, NewKID string
	Rewrapped      int
	Entries        int
	KeyFileSwapped bool
	NewKeyPath     string
}

// rotateHook is called at each point of RotateKey where a crash could land. A
// test makes it return an error to stand in for the crash: RotateKey then
// returns that error at once and cleans nothing up, as a killed process would.
var rotateHook = func(point string) error { return nil }

// Crash points, in the order RotateKey reaches them.
const (
	pointNewKeyWritten = "new-key-written"
	pointBeforeCommit  = "before-commit"
	pointAfterCommit   = "after-commit"
	pointFirstRename   = "first-rename"
)

// ActiveKeyID is the key ID the database says it was last rotated to
// (app_meta active_key_id), or "" when it has never been rotated.
func ActiveKeyID(ctx context.Context, st *store.Store) (string, error) {
	id, err := st.RQ.GetMeta(ctx, vault.MetaActiveKeyID)
	// A database that is not migrated yet has no app_meta table, and so no rotation.
	if errors.Is(err, sql.ErrNoRows) || (err != nil && strings.Contains(err.Error(), "no such table")) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", vault.MetaActiveKeyID, err)
	}
	return id, nil
}

// StoredActiveKeyID is ActiveKeyID for the database in dataDir, for the start-up
// recovery that runs before the server opens its own store. It creates nothing:
// no database, no app_meta table or no rotation yet all answer "".
func StoredActiveKeyID(ctx context.Context, dataDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dataDir, store.DatabaseFile)); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	return ActiveKeyID(ctx, st)
}

// RotateKey replaces the master key (D-33). Bayplan must be stopped: the caller
// holds the data-directory lock for the whole call. old is the vault the
// instance runs on.
//
// In order: the new key is generated and written to bayplan.key.new, fsynced;
// then one write transaction re-wraps every sealed value from old to the new
// key, re-signs the whole audit chain under it, appends a key.rotated entry
// carrying an attestation made with the old key, and records the new key's ID
// in app_meta. The commit is the point of no return. A key from a file is then
// swapped into place (bayplan.key becomes bayplan.key.old, bayplan.key.new
// becomes bayplan.key); a key from the environment is left in bayplan.key.new.
// A crash at any point is repaired at the next start by vault.RecoverRotation.
//
// The new key is never returned or logged, only written to a 0600 file.
func RotateKey(ctx context.Context, st *store.Store, dataDir string, old *vault.Vault, actor audit.Actor) (res RotateResult, err error) {
	oldPath := filepath.Join(dataDir, vault.OldKeyFile)
	newPath := filepath.Join(dataDir, vault.NewKeyFile)
	if _, err := os.Stat(oldPath); err == nil {
		return res, fmt.Errorf("%w: move %s somewhere safe first", ErrOldKeyPresent, oldPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("look for %s: %w", oldPath, err)
	}
	if _, err := os.Stat(newPath); err == nil {
		return res, fmt.Errorf("%w: if BAYPLAN_ENCRYPTION_KEY is already set from it, delete %s, then run this again", ErrNewKeyPresent, newPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("look for %s: %w", newPath, err)
	}
	active, err := ActiveKeyID(ctx, st)
	if err != nil {
		return res, err
	}
	if active != "" && active != old.KeyID() {
		return res, fmt.Errorf("%w: the database was rotated to %s but the key in use is %s", ErrKeyMismatch, active, old.KeyID())
	}

	newKey, err := vault.GenerateKey()
	if err != nil {
		return res, err
	}
	if err := vault.WriteKeyFile(dataDir, vault.NewKeyFile, newKey); err != nil {
		return res, err
	}
	// Until the commit nothing depends on the new key file, so a failure that is
	// not a simulated crash removes it again.
	var (
		committed bool
		crashed   bool
	)
	defer func() {
		if err != nil && !committed && !crashed {
			_ = os.Remove(newPath)
		}
	}()
	crash := func(point string) error {
		if herr := rotateHook(point); herr != nil {
			crashed = true
			return herr
		}
		return nil
	}
	if err := crash(pointNewKeyWritten); err != nil {
		return res, err
	}

	next, err := old.Succeed(newKey)
	if err != nil {
		return res, err
	}
	res.OldKID, res.NewKID = old.KeyID(), next.KeyID()

	if err := rotateTx(ctx, st, old, next, actor, &res, func() error { return crash(pointBeforeCommit) }); err != nil {
		return res, err
	}
	committed = true
	// The pools run with synchronous=NORMAL, where a commit is not yet on disk:
	// a power loss could undo the re-wrap while the key file swap below, which is
	// fsynced, survives. Fold the log into the database file and flush it before
	// any key file moves. If that fails the key files are left as they are and
	// the next start finishes the swap, as after a crash at this point.
	if err := flushRotation(ctx, st); err != nil {
		return res, err
	}
	if err := crash(pointAfterCommit); err != nil {
		return res, err
	}

	if old.KeySource() == "file" {
		if err := vault.RenameKeyFile(dataDir, vault.KeyFile, vault.OldKeyFile); err != nil {
			return res, fmt.Errorf("the database now uses the new key; the next start finishes the key file swap: %w", err)
		}
		if err := crash(pointFirstRename); err != nil {
			return res, err
		}
		if err := vault.RenameKeyFile(dataDir, vault.NewKeyFile, vault.KeyFile); err != nil {
			return res, fmt.Errorf("the database now uses the new key; the next start finishes the key file swap: %w", err)
		}
		res.KeyFileSwapped = true
	} else {
		res.NewKeyPath = newPath
	}

	verified, err := audit.Verify(ctx, st, next)
	if err != nil {
		return res, fmt.Errorf("verify the audit chain after rotation: %w", err)
	}
	if verified.Broken != nil {
		return res, fmt.Errorf("the audit chain does not verify after rotation (entry %d: %s)", verified.Broken.Seq, verified.Broken.Reason)
	}
	return res, nil
}

// flushRotation makes the committed rotation durable: a TRUNCATE checkpoint
// syncs the write-ahead log, copies it into the database file, syncs that file
// and empties the log. It fails when the checkpoint could not complete.
func flushRotation(ctx context.Context, st *store.Store) error {
	var busy, logFrames, checkpointed int
	if err := st.Write.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("the database now uses the new key but it could not be flushed to disk, so the key files were not swapped; the next start finishes the rotation: %w", err)
	}
	if busy != 0 {
		return errors.New("the database now uses the new key but it could not be flushed to disk (the database is busy), so the key files were not swapped; the next start finishes the rotation")
	}
	return nil
}

// rotateTx is the one write transaction of a rotation.
func rotateTx(ctx context.Context, st *store.Store, old, next *vault.Vault, actor audit.Actor, res *RotateResult, beforeCommit func() error) error {
	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	rewrapped, err := vault.RewrapAll(ctx, tx, old, next)
	if err != nil {
		return fmt.Errorf("re-wrap the stored secrets: %w", err)
	}
	rekeyed, err := audit.Rekey(ctx, tx, st.Q, next)
	if err != nil {
		return fmt.Errorf("re-sign the audit log: %w", err)
	}
	w := audit.NewWriter(st, next, slog.New(slog.DiscardHandler), nil)
	if err := w.RecordRotationTx(ctx, tx, old.Subkey(vault.LabelAuditHMAC), old.KeyID(), rekeyed, actor); err != nil {
		return fmt.Errorf("record the rotation: %w", err)
	}
	if err := st.Q.WithTx(tx).UpsertMeta(ctx, sqlc.UpsertMetaParams{
		Key: vault.MetaActiveKeyID, Value: next.KeyID(), UpdatedAt: time.Now().UTC().UnixNano(),
	}); err != nil {
		return fmt.Errorf("record the active key: %w", err)
	}
	if err := beforeCommit(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the rotation: %w", err)
	}
	res.Rewrapped, res.Entries = rewrapped, rekeyed.Entries
	return nil
}
