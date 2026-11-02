// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup_test

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/backup"
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
	writeFile(t, dir, "tierplan-backup-20261001-0300.tierplan", "one")
	writeFile(t, dir, "tierplan-backup-20261003-0300.tierplan", "three")
	writeFile(t, dir, "tierplan-backup-20261002-0300.tierplan", "two22")
	writeFile(t, dir, "tierplan-20261001T030000Z.db", "migration snapshot")
	writeFile(t, dir, "prerestore-20261001T030000Z.db", "pre-restore snapshot")
	writeFile(t, dir, "notes.txt", "x")
	if err := os.Mkdir(filepath.Join(dir, "tierplan-backup-20261004-0300.tierplan"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := backup.ListBackupFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "tierplan-backup-20261003-0300.tierplan" || got[2].Name != "tierplan-backup-20261001-0300.tierplan" {
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
	writeFile(t, dir, "tierplan-backup-20261001-0300.tierplan", "the bytes")
	f, info, err := backup.OpenBackupFile(dir, "tierplan-backup-20261001-0300.tierplan")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	if string(b) != "the bytes" || info.Size != 9 {
		t.Fatalf("read %q, info %+v", b, info)
	}
}

func TestFilePatternAcceptsOnlyTheNameOfAScheduledBackup(t *testing.T) {
	for name, want := range map[string]bool{
		"tierplan-backup-20261008-0300.tierplan":     true,
		"tierplan-backup-20261008-0300.tierplan.bak": false,
		"tierplan-backup-20261008-300.tierplan":      false,
		"tierplan-backup-2026-10-08-0300.tierplan":   false,
		"TierPlan-backup-20261008-0300.tierplan":     false,
		"x.tierplan":                                false,
		"tierplan.db":                               false,
		"tierplan.key":                              false,
		"tierplan-20261008T030000Z.db":              false,
		"prerestore-20261008T030000Z.db":           false,
		"../tierplan-backup-20261008-0300.tierplan":  false,
		"sub/tierplan-backup-20261008-0300.tierplan": false,
		"tierplan-backup-20261008-0300.tierplan\n":   false,
		"":                                         false,
	} {
		if got := backup.IsScheduledName(name); got != want {
			t.Errorf("IsScheduledName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestFilePatternKeepsEveryOtherNameAwayFromTheFileSystem(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "backups")
	writeFile(t, parent, "tierplan.db", "the database")
	writeFile(t, parent, "tierplan.key", "the master key")
	writeFile(t, dir, "x.tierplan", "not a scheduled name")
	writeFile(t, dir, "tierplan-backup-20261008-0300.tierplan.bak", "x")
	for _, name := range []string{
		"../tierplan.db", "../tierplan.key", "x.tierplan", "tierplan-backup-20261008-0300.tierplan.bak",
		"..%2Ftierplan.db", "/etc/passwd", "tierplan-backup-20261008-0300.tierplan/../../tierplan.key",
	} {
		f, _, err := backup.OpenBackupFile(dir, name)
		if f != nil {
			_ = f.Close()
		}
		if !errors.Is(err, backup.ErrNoSuchBackup) {
			t.Errorf("OpenBackupFile(%q) = %v, want ErrNoSuchBackup", name, err)
		}
	}
}

func TestAFileNameThatIsALinkOutOfTheFolderIsNotServed(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "backups")
	writeFile(t, parent, "tierplan.key", "the master key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const name = "tierplan-backup-20261008-0300.tierplan"
	if err := os.Symlink(filepath.Join(parent, "tierplan.key"), filepath.Join(dir, name)); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	f, _, err := backup.OpenBackupFile(dir, name)
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, backup.ErrNoSuchBackup) {
		t.Fatalf("a link to the master key opened: %v", err)
	}
	listed, err := backup.ListBackupFiles(dir)
	if err != nil || len(listed) != 0 {
		t.Fatalf("a link is listed as a backup: %v, %v", listed, err)
	}
}

func TestADirectoryWithABackupsNameIsNotABackup(t *testing.T) {
	dir := t.TempDir()
	const name = "tierplan-backup-20261008-0300.tierplan"
	writeFile(t, filepath.Join(dir, name), "inner", "x")
	if f, _, err := backup.OpenBackupFile(dir, name); !errors.Is(err, backup.ErrNoSuchBackup) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("a directory opened as a backup: %v", err)
	}
}

func TestStaleWorkFilesOfAKilledExportAreSweptAndNothingElseIs(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "backups")
	now := utc(2026, 10, 8, 12, 0)
	old := now.Add(-3 * time.Hour)
	fresh := now.Add(-time.Minute)
	mk := func(parent, name string, isDir bool, mod time.Time) string {
		t.Helper()
		p := filepath.Join(parent, name)
		if isDir {
			if err := os.MkdirAll(filepath.Join(p, "inner"), 0o700); err != nil {
				t.Fatal(err)
			}
		} else {
			writeFile(t, parent, name, "x")
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return p
	}
	staleDir := mk(data, ".tierplan-backup-123", true, old)
	staleTmp := mk(data, ".tierplan-export-456.tmp", false, old)
	staleSched := mk(dir, ".tierplan-schedule-789.tmp", false, old)
	freshDir := mk(data, ".tierplan-backup-live", true, fresh)
	freshTmp := mk(data, ".tierplan-export-live.tmp", false, fresh)
	db := mk(data, "tierplan.db", false, old)
	key := mk(data, "tierplan.key", false, old)
	saved := mk(dir, "tierplan-backup-20261001-0300.tierplan", false, old)
	snap := mk(dir, "tierplan-20261001T030000Z.db", false, old)
	otherHidden := mk(data, ".tierplan-something-else", false, old)

	backup.SweepWorkFiles(data, dir, now, slog.New(slog.DiscardHandler))

	for _, p := range []string{staleDir, staleTmp, staleSched} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was not swept", p)
		}
	}
	for _, p := range []string{freshDir, freshTmp, db, key, saved, snap, otherHidden} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

func TestManualBackupNamesHaveSecondsAreNeverScheduledNamesAndAreNeitherListedNorServed(t *testing.T) {
	at := time.Date(2026, 10, 8, 14, 5, 9, 0, time.UTC)
	if got := backup.ManualName(at, 1); got != "tierplan-manual-20261008-140509.tierplan" {
		t.Fatalf("ManualName = %q", got)
	}
	if got := backup.ManualName(at, 0); got != backup.ManualName(at, 1) {
		t.Fatalf("attempt 0 = %q, want the plain name", got)
	}
	if got := backup.ManualName(at, 2); got != "tierplan-manual-20261008-140509-2.tierplan" {
		t.Fatalf("attempt 2 = %q", got)
	}
	dir := t.TempDir()
	for n := 1; n <= 3; n++ {
		name := backup.ManualName(at, n)
		if backup.IsScheduledName(name) {
			t.Errorf("%q matches the scheduled pattern", name)
		}
		writeFile(t, dir, name, "manual")
		if _, _, err := backup.OpenBackupFile(dir, name); !errors.Is(err, backup.ErrNoSuchBackup) {
			t.Errorf("OpenBackupFile(%q) = %v, want ErrNoSuchBackup: manual backups are not served", name, err)
		}
	}
	files, err := backup.ListBackupFiles(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("listing = %v, %v; want manual backups left out", files, err)
	}
}
