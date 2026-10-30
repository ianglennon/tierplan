// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestPreRestorePrunerKeepsTheNewestThreeAndTouchesNoOtherFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"prerestore-20260101T000000Z.db", "prerestore-20260102T000000Z.db", "prerestore-20260103T000000Z.db",
		"prerestore-20260104T000000Z.db", "prerestore-20260105T000000Z.db",
		"coaming-v0.1.0-20260101T000000Z.db", "coaming-v0.1.1-20260102T000000Z.db",
		"coaming-v0.1.2-20260103T000000Z.db", "coaming-v0.1.3-20260104T000000Z.db",
		"coaming-backup-20260101-0000.coaming", "prerestore-notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prunePreRestore(dir, 3); err != nil {
		t.Fatal(err)
	}
	var pre, others []string
	for _, n := range listNames(t, dir) {
		if strings.HasPrefix(n, "prerestore-2") {
			pre = append(pre, n)
		} else {
			others = append(others, n)
		}
	}
	if strings.Join(pre, ",") != "prerestore-20260103T000000Z.db,prerestore-20260104T000000Z.db,prerestore-20260105T000000Z.db" {
		t.Fatalf("prerestore files left = %v, want the newest three", pre)
	}
	if len(others) != 6 {
		t.Fatalf("other files left = %v, want all six untouched", others)
	}
}

func TestPreRestorePrunerOnAMissingDirectoryIsNotAnError(t *testing.T) {
	if err := prunePreRestore(filepath.Join(t.TempDir(), "nope"), 3); err != nil {
		t.Fatal(err)
	}
}

// WR-A01: when the database cannot be opened the snapshot is a plain copy, and
// the write-ahead log is copied with it, so rows that were only in the log are
// still there when the snapshot is opened, even though Swap deletes the live
// log.
func TestFallbackSnapshotKeepsTheWriteAheadLogBesideTheCopy(t *testing.T) {
	dir := t.TempDir()
	src := t.TempDir()
	live, err := sql.Open("sqlite", "file:"+filepath.Join(src, "coaming.db")+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	live.SetMaxOpenConns(1) // one connection, so the log stays unfolded
	for _, q := range []string{
		`CREATE TABLE t (v TEXT)`, `INSERT INTO t VALUES ('in-the-main-file-or-log')`, `INSERT INTO t VALUES ('only-in-the-log')`,
	} {
		if _, err := live.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Copy the files as a crashed instance would have left them.
	for _, name := range []string{"coaming.db", "coaming.db-wal"} {
		b, err := os.ReadFile(filepath.Join(src, name)) //nolint:gosec // a file in the test directory
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil { //nolint:gosec // a file in the test directory
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "coaming.db-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("the test needs an unfolded log: %v", err)
	}

	// A canceled context makes store.Open fail, which is the fallback path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap, err := PreRestoreSnapshot(ctx, dir, time.Date(2026, 10, 8, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snap + "-wal"); err != nil {
		t.Fatalf("the write-ahead log was not saved beside the fallback snapshot: %v", err)
	}
	// What Swap does next.
	if err := os.Remove(filepath.Join(dir, "coaming.db-wal")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+snap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t WHERE v = 'only-in-the-log'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the snapshot lost a committed row: count %d, err %v", n, err)
	}
}

func TestPrunerRemovesTheWriteAheadLogOfAPrunedSnapshotOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"prerestore-20260101T000000Z.db", "prerestore-20260101T000000Z.db-wal",
		"prerestore-20260102T000000Z.db", "prerestore-20260102T000000Z.db-wal",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prunePreRestore(dir, 1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(listNames(t, dir), ","); got != "prerestore-20260102T000000Z.db,prerestore-20260102T000000Z.db-wal" {
		t.Fatalf("files left = %s", got)
	}
}
