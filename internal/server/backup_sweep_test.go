// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A process killed in the middle of a backup leaves its private work directory or
// its temporary file behind, holding a copy of the database before it was
// encrypted. The next start removes what is old enough not to be a live backup of
// another process, and nothing else (plan 03-27, hand-off from 03-09).
func TestStartupSweepsWhatAKilledBackupLeftBehindAndNothingElse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(filepath.Join(dir, ".tierplan-backup-dead", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(dir, ".tierplan-export-dead.tmp"),
		filepath.Join(backups, ".tierplan-schedule-dead.tmp"),
		filepath.Join(backups, "tierplan-backup-20261001-0300.tierplan"),
		filepath.Join(dir, "notes.txt"),
	} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-3 * time.Hour)
	for _, p := range []string{
		filepath.Join(dir, ".tierplan-backup-dead"), filepath.Join(dir, ".tierplan-export-dead.tmp"),
		filepath.Join(backups, ".tierplan-schedule-dead.tmp"), filepath.Join(backups, "tierplan-backup-20261001-0300.tierplan"),
		filepath.Join(dir, "notes.txt"),
	} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	live := filepath.Join(dir, ".tierplan-export-live.tmp")
	if err := os.WriteFile(live, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	for _, gone := range []string{
		filepath.Join(dir, ".tierplan-backup-dead"), filepath.Join(dir, ".tierplan-export-dead.tmp"),
		filepath.Join(backups, ".tierplan-schedule-dead.tmp"),
	} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s survived the start", gone)
		}
	}
	for _, kept := range []string{
		live, filepath.Join(backups, "tierplan-backup-20261001-0300.tierplan"), filepath.Join(dir, "notes.txt"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}
