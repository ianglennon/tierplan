// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/backup"
)

func TestMarkerRoundTripsAndIsPrivate(t *testing.T) {
	dir := t.TempDir()
	if _, pending, err := backup.ReadMarker(dir); pending || err != nil {
		t.Fatalf("ReadMarker on a clean directory = %v, %v", pending, err)
	}
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	snap := filepath.Join(dir, "backups", "prerestore-20260304T050607Z.db")
	if err := backup.WriteMarker(dir, snap, started); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, backup.PendingMarker))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("marker = %v, %v; want 0600", info, err)
	}
	m, pending, err := backup.ReadMarker(dir)
	if err != nil || !pending {
		t.Fatalf("ReadMarker = %v, %v", pending, err)
	}
	if m.Snapshot != "prerestore-20260304T050607Z.db" || !m.Started.Equal(started) {
		t.Fatalf("marker = %+v", m)
	}
	if got := m.SnapshotPath(dir); got != snap {
		t.Fatalf("SnapshotPath = %q, want %q", got, snap)
	}
	if err := backup.RemoveMarker(dir); err != nil {
		t.Fatal(err)
	}
	if _, pending, _ := backup.ReadMarker(dir); pending {
		t.Fatal("the marker survived RemoveMarker")
	}
	if err := backup.RemoveMarker(dir); err != nil {
		t.Fatalf("RemoveMarker twice: %v", err)
	}
}

func TestMarkerThatCannotBeReadIsStillPending(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, backup.PendingMarker), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := backup.ReadMarker(dir); !pending || err == nil {
		t.Fatalf("ReadMarker = %v, %v; want pending with an error", pending, err)
	}
}

func TestMarkerNeverPointsOutsideTheBackupsDirectory(t *testing.T) {
	dir := t.TempDir()
	m := backup.Marker{Snapshot: "../../etc/passwd"}
	if got, want := m.SnapshotPath(dir), filepath.Join(dir, "backups", "passwd"); got != want {
		t.Fatalf("SnapshotPath = %q, want %q", got, want)
	}
}

func TestRollbackPutsTheSnapshotBackAndDropsTheRestoredJournal(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("bayplan.db", "restored")
	write("bayplan.db-wal", "journal")
	write("bayplan.db-shm", "shm")
	write("backups/prerestore-20260304T050607Z.db", "old")
	if err := backup.WriteMarker(dir, filepath.Join(dir, "backups", "prerestore-20260304T050607Z.db"), time.Now()); err != nil {
		t.Fatal(err)
	}
	m, _, _ := backup.ReadMarker(dir)

	if err := backup.RollbackRestore(dir, m); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "bayplan.db")); string(got) != "old" { //nolint:gosec // a file in the test directory
		t.Fatalf("bayplan.db = %q, want the snapshot", got)
	}
	for _, gone := range []string{"bayplan.db-wal", "bayplan.db-shm", backup.PendingMarker, "backups/prerestore-20260304T050607Z.db"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s still exists", gone)
		}
	}
}

func TestRollbackWithoutASnapshotOnDiskChangesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bayplan.db"), []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteMarker(dir, filepath.Join(dir, "backups", "prerestore-20260304T050607Z.db"), time.Now()); err != nil {
		t.Fatal(err)
	}
	m, _, _ := backup.ReadMarker(dir)
	if err := backup.RollbackRestore(dir, m); err == nil {
		t.Fatal("RollbackRestore succeeded with no snapshot to go back to")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "bayplan.db")); string(got) != "restored" { //nolint:gosec // a file in the test directory
		t.Fatalf("bayplan.db = %q, want it untouched", got)
	}
	if _, pending, _ := backup.ReadMarker(dir); !pending {
		t.Fatal("the marker was removed although nothing was rolled back")
	}
}

func TestRollbackOfARestoreThatReplacedNothingRemovesTheRestoredDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bayplan.db"), []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteMarker(dir, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	m, _, _ := backup.ReadMarker(dir)
	if err := backup.RollbackRestore(dir, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bayplan.db")); err == nil {
		t.Fatal("the restored database was kept")
	}
}
