// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/backup"
)

const cliPass = "correct horse battery"

// runCLIWith is runCLI with a standard input and, optionally, a passphrase file
// named by the environment.
func runCLIWith(t *testing.T, dir, stdin, passphraseFile string, args ...string) (stdout []byte, stderr string, err error) {
	t.Helper()
	t.Setenv("COAMING_DATA_DIR", dir)
	t.Setenv("COAMING_ENCRYPTION_KEY", "")
	t.Setenv("COAMING_ENCRYPTION_KEY_FILE", "")
	t.Setenv("COAMING_BACKUP_PASSPHRASE_FILE", passphraseFile)
	var out, errb bytes.Buffer
	root := newRoot()
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())
	return out.Bytes(), errb.String(), err
}

// readBackup opens a backup file and returns its manifest bytes and database
// bytes.
func readBackup(t *testing.T, file []byte, pass string) (manifest, db []byte) {
	t.Helper()
	r, err := backup.Open(bytes.NewReader(file), []byte(pass))
	if err != nil {
		t.Fatalf("backup.Open: %v", err)
	}
	for _, want := range []string{backup.EntryManifest, backup.EntryDB} {
		name, body, err := r.Next()
		if err != nil || name != want {
			t.Fatalf("entry = %q, %v; want %q", name, err, want)
		}
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		if want == backup.EntryManifest {
			manifest = data
		} else {
			db = data
		}
	}
	return manifest, db
}

func TestBackupWritesAFileThatOpensAndRecordsSystemCLI(t *testing.T) {
	dir, st := seedDataDir(t, 2)
	out := filepath.Join(t.TempDir(), "b.coaming")

	stdout, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup", "--out", out)
	if err != nil {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	if strings.Contains(string(stdout)+errOut, cliPass) {
		t.Fatal("the passphrase was printed")
	}
	file, err := os.ReadFile(out) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	manifest, db := readBackup(t, file, cliPass)
	if !bytes.Contains(manifest, []byte(`"chainVerified":true`)) || !bytes.HasPrefix(db, []byte("SQLite format 3\x00")) {
		t.Fatalf("manifest %s, database starts %q", manifest, db[:16])
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup file mode = %v, %v; want 0600", info.Mode(), err)
	}

	var actor, source, name string
	if err := st.Read.QueryRow(`SELECT actor_id, json_extract(details, '$.source'), json_extract(details, '$.file')
		FROM audit_log WHERE action = 'backup.exported'`).Scan(&actor, &source, &name); err != nil {
		t.Fatalf("no backup.exported entry: %v", err)
	}
	if actor != "system:cli" || source != "cli" || name != "b.coaming" {
		t.Fatalf("backup.exported actor %q source %q file %q", actor, source, name)
	}
	// The CLI signs with the same key, so the server's chain stays intact.
	if vout, _, err := runCLI(t, dir, "audit", "verify"); err != nil || strings.TrimSpace(vout) != "audit chain OK: 3 entries" {
		t.Fatalf("audit verify after backup = %q, %v", vout, err)
	}
}

func TestBackupReadsThePassphraseFromTheFileTheEnvironmentNames(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	passFile := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(passFile, []byte(cliPass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.coaming")
	// Stdin holds something else: the file wins.
	if _, errOut, err := runCLIWith(t, dir, "some other passphrase\n", passFile, "backup", "--out", out); err != nil {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	file, err := os.ReadFile(out) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	readBackup(t, file, cliPass)
}

func TestBackupWithNoPassphraseSourceSaysWhereToGiveOneAndWritesNothing(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	out := filepath.Join(t.TempDir(), "b.coaming")
	_, errOut, err := runCLIWith(t, dir, "", "", "backup", "--out", out)
	if err == nil {
		t.Fatal("backup with no passphrase exited 0")
	}
	if !strings.Contains(errOut, "set COAMING_BACKUP_PASSPHRASE_FILE or pipe the passphrase on stdin") {
		t.Fatalf("stderr = %q", errOut)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused backup left a file")
	}
}

func TestBackupRefusesAShortPassphraseAndWritesNothing(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	out := filepath.Join(t.TempDir(), "b.coaming")
	_, errOut, err := runCLIWith(t, dir, "too short\n", "", "backup", "--out", out)
	if err == nil || !strings.Contains(errOut, "at least 16 characters") {
		t.Fatalf("backup with a short passphrase = %v, stderr %q", err, errOut)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused backup left a file")
	}
}

func TestBackupHasNoPassphraseFlag(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	_, _, err := runCLIWith(t, dir, "", "", "backup", "--passphrase", cliPass)
	if err == nil {
		t.Fatal("a --passphrase flag is accepted: a passphrase on a command line shows in the process list")
	}
}

func TestBackupToDashWritesTheFileToStandardOutput(t *testing.T) {
	dir, st := seedDataDir(t, 1)
	stdout, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup", "--out", "-")
	if err != nil {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	readBackup(t, stdout, cliPass)
	var name string
	if err := st.Read.QueryRow(`SELECT json_extract(details, '$.file') FROM audit_log WHERE action = 'backup.exported'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "stdout" {
		t.Fatalf("audited file = %q, want stdout", name)
	}
}

func TestBackupDefaultsToTheBackupsFolderOfTheDataDirectoryUnderAManualName(t *testing.T) {
	t.Setenv("COAMING_BACKUP_DIR", "")
	dir, _ := seedDataDir(t, 0)
	if _, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup"); err != nil {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "backups", "coaming-manual-*.coaming"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backups in the data directory = %v, %v; want one", matches, err)
	}
	if backup.IsScheduledName(filepath.Base(matches[0])) {
		t.Fatalf("%s is named like a scheduled backup, so retention would delete it", matches[0])
	}
	file, err := os.ReadFile(matches[0]) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	readBackup(t, file, cliPass)
}

// COAMING_DATA_DIR is commonly relative (./.devdata under make dev); the backup
// must work from it, to --out and to the default folder (UAT gap G-03-4).
func TestBackupWorksWithARelativeDataDirectory(t *testing.T) {
	t.Setenv("COAMING_BACKUP_DIR", "")
	abs, _ := seedDataDir(t, 1)
	t.Chdir(filepath.Dir(abs))
	rel := "./" + filepath.Base(abs)

	out := filepath.Join(rel, "b.coaming")
	if _, errOut, err := runCLIWith(t, rel, cliPass+"\n", "", "backup", "--out", out); err != nil || strings.Contains(errOut, "invalid uri authority") {
		t.Fatalf("backup --out = %v, stderr %q", err, errOut)
	}
	file, err := os.ReadFile(out) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	readBackup(t, file, cliPass)

	if _, errOut, err := runCLIWith(t, rel, cliPass+"\n", "", "backup"); err != nil || strings.Contains(errOut, "invalid uri authority") {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	matches, err := filepath.Glob(filepath.Join(rel, "backups", "coaming-manual-*.coaming"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backups in the data directory = %v, %v; want one", matches, err)
	}
	file, err = os.ReadFile(matches[0]) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	readBackup(t, file, cliPass)
}

func TestBackupRefusesABrokenChainUnlessAllowed(t *testing.T) {
	dir, st := seedDataDir(t, 3)
	if _, err := st.Write.Exec("UPDATE audit_log SET action = 'x' || substr(action, 2) WHERE seq = 2"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.coaming")

	_, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup", "--out", out)
	if err == nil || !strings.Contains(errOut, "--allow-broken-chain") {
		t.Fatalf("backup over a broken chain = %v, stderr %q", err, errOut)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused backup left a file")
	}

	if _, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup", "--out", out, "--allow-broken-chain"); err != nil {
		t.Fatalf("backup --allow-broken-chain = %v, stderr %q", err, errOut)
	}
	file, err := os.ReadFile(out) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readBackup(t, file, cliPass)
	if !bytes.Contains(manifest, []byte(`"chainVerified":false`)) {
		t.Fatalf("manifest = %s, want chainVerified false", manifest)
	}
}

// T-03-113: the default file goes where the server keeps its backups, under a name the
// scheduled-backup retention never matches, and never replaces a file.

func TestBackupDefaultFileGoesToBackupDirWhenItIsSet(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	folder := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv("COAMING_BACKUP_DIR", folder)
	stdout, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup")
	if err != nil {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	matches, err := filepath.Glob(filepath.Join(folder, "coaming-manual-*.coaming"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backups in COAMING_BACKUP_DIR = %v, %v; want one", matches, err)
	}
	if !strings.Contains(string(stdout), matches[0]) {
		t.Fatalf("output %q does not name %s", stdout, matches[0])
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backups", "*.coaming")); len(left) != 0 {
		t.Fatalf("the data directory's backups folder holds %v, want nothing while COAMING_BACKUP_DIR is set", left)
	}
	if tmp, _ := filepath.Glob(filepath.Join(folder, ".coaming-backup-*")); len(tmp) != 0 {
		t.Fatalf("work files left behind: %v", tmp)
	}
	file, err := os.ReadFile(matches[0]) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	readBackup(t, file, cliPass)
}

func TestBackupDefaultNameNeverReplacesAScheduledBackupOfTheSameMinute(t *testing.T) {
	t.Setenv("COAMING_BACKUP_DIR", "")
	dir, _ := seedDataDir(t, 0)
	folder := filepath.Join(dir, "backups")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	// A scheduled backup made in this very minute, and one a minute either side.
	var seeded []string
	for _, d := range []time.Duration{-time.Minute, 0, time.Minute} {
		name := backup.ScheduledName(time.Now().Add(d))
		seeded = append(seeded, name)
		if err := os.WriteFile(filepath.Join(folder, name), []byte("scheduled: keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup"); err != nil {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	for _, name := range seeded {
		got, err := os.ReadFile(filepath.Join(folder, name)) //nolint:gosec // a path the test made
		if err != nil || string(got) != "scheduled: keep" {
			t.Fatalf("scheduled backup %s = %q, %v; want it untouched", name, got, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(folder, "coaming-manual-*.coaming")); len(matches) != 1 {
		t.Fatalf("manual backups = %v, want one", matches)
	}
}

func TestBackupDefaultNameTakesAFreeNameWhenTheSecondIsTaken(t *testing.T) {
	folder := t.TempDir()
	at := time.Date(2026, 10, 8, 14, 5, 9, 0, time.UTC)
	var paths []string
	for i := range 3 {
		d, err := newBackupDestination(newBackupCmd(), t.TempDir(), folder, "", at)
		if err != nil {
			t.Fatalf("destination %d: %v", i, err)
		}
		if _, err := d.w.Write([]byte("backup " + strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
		if err := d.commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		paths = append(paths, d.path)
	}
	want := []string{
		filepath.Join(folder, "coaming-manual-20261008-140509.coaming"),
		filepath.Join(folder, "coaming-manual-20261008-140509-2.coaming"),
		filepath.Join(folder, "coaming-manual-20261008-140509-3.coaming"),
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i, p := range paths {
		got, err := os.ReadFile(p) //nolint:gosec // a path the test made
		if err != nil || string(got) != "backup "+strconv.Itoa(i) {
			t.Fatalf("%s = %q, %v; want the first run's file kept as it was", p, got, err)
		}
	}
}

func TestBackupDefaultNameClaimIsRemovedWhenTheBackupIsAbandoned(t *testing.T) {
	folder := t.TempDir()
	d, err := newBackupDestination(newBackupCmd(), t.TempDir(), folder, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	d.abort()
	if entries, _ := os.ReadDir(folder); len(entries) != 0 {
		t.Fatalf("an abandoned backup left %v behind", entries)
	}
}
