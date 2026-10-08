// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/backup"
)

const cliPass = "correct horse battery"

// runCLIWith is runCLI with a standard input and, optionally, a passphrase file
// named by the environment.
func runCLIWith(t *testing.T, dir, stdin, passphraseFile string, args ...string) (stdout []byte, stderr string, err error) {
	t.Helper()
	t.Setenv("BAYPLAN_DATA_DIR", dir)
	t.Setenv("BAYPLAN_ENCRYPTION_KEY", "")
	t.Setenv("BAYPLAN_ENCRYPTION_KEY_FILE", "")
	t.Setenv("BAYPLAN_BACKUP_PASSPHRASE_FILE", passphraseFile)
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
	out := filepath.Join(t.TempDir(), "b.bayplan")

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
	if actor != "system:cli" || source != "cli" || name != "b.bayplan" {
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
	out := filepath.Join(t.TempDir(), "b.bayplan")
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
	out := filepath.Join(t.TempDir(), "b.bayplan")
	_, errOut, err := runCLIWith(t, dir, "", "", "backup", "--out", out)
	if err == nil {
		t.Fatal("backup with no passphrase exited 0")
	}
	if !strings.Contains(errOut, "set BAYPLAN_BACKUP_PASSPHRASE_FILE or pipe the passphrase on stdin") {
		t.Fatalf("stderr = %q", errOut)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused backup left a file")
	}
}

func TestBackupRefusesAShortPassphraseAndWritesNothing(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	out := filepath.Join(t.TempDir(), "b.bayplan")
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

func TestBackupDefaultsToTheBackupsFolderOfTheDataDirectory(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	if _, errOut, err := runCLIWith(t, dir, cliPass+"\n", "", "backup"); err != nil {
		t.Fatalf("backup = %v, stderr %q", err, errOut)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "backups", "bayplan-backup-*.bayplan"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backups in the data directory = %v, %v; want one", matches, err)
	}
	file, err := os.ReadFile(matches[0]) //nolint:gosec // a path the test made
	if err != nil {
		t.Fatal(err)
	}
	readBackup(t, file, cliPass)
}

// BAYPLAN_DATA_DIR is commonly relative (./.devdata under make dev); the backup
// must work from it, to --out and to the default folder (UAT gap G-03-4).
func TestBackupWorksWithARelativeDataDirectory(t *testing.T) {
	abs, _ := seedDataDir(t, 1)
	t.Chdir(filepath.Dir(abs))
	rel := "./" + filepath.Base(abs)

	out := filepath.Join(rel, "b.bayplan")
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
	matches, err := filepath.Glob(filepath.Join(rel, "backups", "bayplan-backup-*.bayplan"))
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
	out := filepath.Join(t.TempDir(), "b.bayplan")

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
