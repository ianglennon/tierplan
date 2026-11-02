// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
)

const (
	// backupsDir is the folder under the data directory that holds snapshots and
	// backups. The migration snapshots of internal/store live there too.
	backupsDir = "backups"
	// preRestorePrefix and preRestoreSuffix name the snapshot a restore takes of
	// the database it replaces. The prefix is distinct from the migration
	// snapshots' "tierplan-", so neither pruner can delete the other's files
	// (RESEARCH Pitfall 9).
	preRestorePrefix = "prerestore-"
	preRestoreSuffix = ".db"
	// preRestoreTimeLayout is the UTC timestamp in a snapshot's name. It sorts
	// lexically in time order.
	preRestoreTimeLayout = "20060102T150405Z"
	// preRestoreKept is how many pre-restore snapshots survive.
	preRestoreKept = 3
)

// PreRestoreSnapshot saves the live database in dataDir as
// backups/prerestore-<UTC time>.db, mode 0600, and returns its path. It is a
// consistent VACUUM INTO copy, flushed to disk with its directory. If the
// database cannot be opened (a restore is often the answer to a damaged one) the
// file is copied as it is together with its write-ahead log
// (prerestore-<UTC time>.db-wal), so the operator still has whatever was there,
// including transactions that had not yet been folded into the main file.
// RollbackRestore moves both back.
func PreRestoreSnapshot(ctx context.Context, dataDir string, now time.Time) (string, error) {
	dir := filepath.Join(dataDir, backupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create backups directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, preRestorePrefix+now.UTC().Format(preRestoreTimeLayout)+preRestoreSuffix)

	st, err := store.Open(ctx, dataDir)
	if err == nil {
		err = vacuumInto(ctx, st, path)
		if cerr := st.Close(); err == nil && cerr != nil {
			err = cerr
		}
		// VACUUM INTO does not promise a durable output file: flush it, and the
		// directory entry, before the live database is replaced.
		if err == nil {
			err = errors.Join(syncFile(path), syncFile(dir))
		}
	}
	if err != nil {
		_ = os.Remove(path)
		_ = os.Remove(path + walSuffix)
		live := filepath.Join(dataDir, store.DatabaseFile)
		cerr := copyExclusive(live, path)
		if cerr == nil {
			// Swap removes the live journal, so what it holds must survive
			// beside the copy for SQLite to recover when the copy is opened.
			if werr := copyExclusive(live+walSuffix, path+walSuffix); werr != nil && !errors.Is(werr, fs.ErrNotExist) {
				cerr = fmt.Errorf("save the write-ahead log: %w", werr)
				_ = os.Remove(path)
			}
		}
		if cerr == nil {
			cerr = syncFile(dir)
		}
		if cerr != nil {
			return "", fmt.Errorf("save the current database before replacing it: %w", errors.Join(err, cerr))
		}
	}
	return path, nil
}

// walSuffix is the extension of SQLite's write-ahead log beside a database file.
const walSuffix = "-wal"

// copyExclusive copies src to a new file dst, mode 0600, refusing to replace an
// existing one.
func copyExclusive(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // the live database
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a new file in the backups directory
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

// prunePreRestore keeps the newest keep pre-restore snapshots in dir and removes
// the rest, with the write-ahead log saved beside a fallback copy. It matches
// only prerestore-<timestamp>.db (and its -wal), so it never removes a
// migration snapshot, a backup or any other file. A missing directory is not an
// error.
func prunePreRestore(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var snaps []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, preRestorePrefix) || !strings.HasSuffix(name, preRestoreSuffix) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, preRestorePrefix), preRestoreSuffix)
		if _, err := time.Parse(preRestoreTimeLayout, stamp); err != nil {
			continue
		}
		snaps = append(snaps, name)
	}
	sort.Strings(snaps) // the timestamp sorts in time order
	var errs []error
	for len(snaps) > keep {
		errs = append(errs, os.Remove(filepath.Join(dir, snaps[0])))
		if err := os.Remove(filepath.Join(dir, snaps[0]+walSuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		snaps = snaps[1:]
	}
	return errors.Join(errs...)
}
