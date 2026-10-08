// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ianglennon/bayplan/internal/backup"
)

// The scheduled backups on disk: what is listed and what can be opened (plan
// 03-27, T-03-112).

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTheBackupsFolderListsOnlyScheduledBackupsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bayplan-backup-20261001-0300.bayplan", "one")
	writeFile(t, dir, "bayplan-backup-20261003-0300.bayplan", "three")
	writeFile(t, dir, "bayplan-backup-20261002-0300.bayplan", "two22")
	writeFile(t, dir, "bayplan-20261001T030000Z.db", "migration snapshot")
	writeFile(t, dir, "prerestore-20261001T030000Z.db", "pre-restore snapshot")
	writeFile(t, dir, "notes.txt", "x")
	if err := os.Mkdir(filepath.Join(dir, "bayplan-backup-20261004-0300.bayplan"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := backup.ListBackupFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "bayplan-backup-20261003-0300.bayplan" || got[2].Name != "bayplan-backup-20261001-0300.bayplan" {
		t.Fatalf("listed %+v", got)
	}
	if got[1].Size != 5 || got[1].Created.Format("2006-01-02T15:04Z") != "2026-10-02T03:00Z" {
		t.Fatalf("the middle backup = %+v, want 5 bytes made 2026-10-02 03:00 UTC", got[1])
	}
}

func TestAFolderThatDoesNotExistYetHoldsNoBackups(t *testing.T) {
	got, err := backup.ListBackupFiles(filepath.Join(t.TempDir(), "never-made"))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want an empty, non-nil list", got, err)
	}
}

func TestAScheduledBackupOpensByItsName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bayplan-backup-20261001-0300.bayplan", "the bytes")
	f, info, err := backup.OpenBackupFile(dir, "bayplan-backup-20261001-0300.bayplan")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	if string(b) != "the bytes" || info.Size != 9 {
		t.Fatalf("read %q, info %+v", b, info)
	}
}
