// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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
		"bayplan-v0.1.0-20260101T000000Z.db", "bayplan-v0.1.1-20260102T000000Z.db",
		"bayplan-v0.1.2-20260103T000000Z.db", "bayplan-v0.1.3-20260104T000000Z.db",
		"bayplan-backup-20260101-0000.bayplan", "prerestore-notes.txt",
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
