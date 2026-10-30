// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ianglennon/coaming/internal/store"
)

// PendingMarker is the file, in the data directory, that a restore writes right
// before it swaps the database (Staged.SwapWithMarker). The next start finishes
// the restore: it removes the marker once the restored database has started,
// and when the restored database cannot start it puts the pre-restore snapshot
// back and removes the marker (T-03-105).
const PendingMarker = "restore-pending"

// Marker is the content of the restore-pending file.
type Marker struct {
	// Snapshot is the file name, inside backups/, of the pre-restore snapshot.
	// It is empty when the restore replaced no database.
	Snapshot string `json:"snapshot"`
	// Started is when the swap happened.
	Started time.Time `json:"started"`
}

// SnapshotPath is where the snapshot is, or "" when there is none. Only the base
// name is used, so a marker can never point outside backups/.
func (m Marker) SnapshotPath(dataDir string) string {
	if m.Snapshot == "" {
		return ""
	}
	return filepath.Join(dataDir, backupsDir, filepath.Base(m.Snapshot))
}

// WriteMarker records a swap. snapshotPath is what PreRestoreSnapshot returned,
// empty when no database was replaced. The file is 0600, written whole and
// flushed, with the directory, before WriteMarker returns.
func WriteMarker(dataDir, snapshotPath string, started time.Time) error {
	m := Marker{Started: started.UTC()}
	if snapshotPath != "" {
		m.Snapshot = filepath.Base(snapshotPath)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode the restore marker: %w", err)
	}
	path := filepath.Join(dataDir, PendingMarker)
	tmp, err := os.CreateTemp(dataDir, ".restore-pending-*")
	if err != nil {
		return fmt.Errorf("write the restore marker: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write the restore marker: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write the restore marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write the restore marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write the restore marker: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write the restore marker: %w", err)
	}
	return syncFile(dataDir)
}

// ReadMarker reads the marker. The bool is false when there is none. A marker
// that cannot be read is reported with true and the error, so the caller knows a
// restore was pending even though it cannot tell which snapshot to go back to.
func ReadMarker(dataDir string) (Marker, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, PendingMarker)) //nolint:gosec // a fixed name in the data directory
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, false, nil
	}
	if err != nil {
		return Marker{}, true, fmt.Errorf("read the restore marker: %w", err)
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return Marker{}, true, fmt.Errorf("the restore marker is not valid: %w", err)
	}
	return m, true, nil
}

// RemoveMarker deletes the marker. A missing one is not an error.
func RemoveMarker(dataDir string) error {
	if err := os.Remove(filepath.Join(dataDir, PendingMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the restore marker: %w", err)
	}
	return syncFile(dataDir)
}

// RollbackRestore puts the snapshot a pending restore took back as the live
// database and removes the marker. The restored database's journal files go with
// it, so nothing of it is replayed into the old one. It must be called with the
// data-directory lock held and no store open. A marker with no snapshot means the
// restore replaced nothing, so the restored database is removed and the next
// start makes a fresh one.
func RollbackRestore(dataDir string, m Marker) error {
	live := filepath.Join(dataDir, store.DatabaseFile)
	snap := m.SnapshotPath(dataDir)
	if snap != "" {
		if _, err := os.Stat(snap); err != nil {
			return fmt.Errorf("the pre-restore snapshot %s cannot be found: %w", snap, err)
		}
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if err := os.Remove(live + ext); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", live+ext, err)
		}
	}
	if snap == "" {
		if err := os.Remove(live); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the restored database: %w", err)
		}
	} else {
		// A snapshot saved as a plain copy of a damaged database carries its
		// write-ahead log; the log goes back first so the database is never
		// live without it.
		if err := os.Rename(snap+walSuffix, live+walSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("move the pre-restore write-ahead log back: %w", err)
		}
		if err := os.Rename(snap, live); err != nil {
			return fmt.Errorf("move the pre-restore snapshot back: %w", err)
		}
	}
	if err := syncFile(dataDir); err != nil {
		return fmt.Errorf("flush the data directory: %w", err)
	}
	return RemoveMarker(dataDir)
}
