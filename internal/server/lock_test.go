// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/store"
)

func TestBuildLockRefusesASecondServerOnTheSameDataDirectory(t *testing.T) {
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"})
	first, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a second Build on the same data directory succeeded")
	}
	if !errors.Is(err, datadir.ErrLocked) || !strings.Contains(err.Error(), datadir.LockFile) {
		t.Fatalf("second Build = %v, want ErrLocked naming %s", err, datadir.LockFile)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Build after Close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	// Close is safe to repeat, and still holds nothing.
	if err := third.Close(); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
}

func TestBuildLockIsHeldBeforeTheDatabaseIsOpened(t *testing.T) {
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir})
	release, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	if _, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler)); !errors.Is(err, datadir.ErrLocked) {
		t.Fatalf("Build with the lock held elsewhere = %v, want ErrLocked", err)
	}
	// Nothing was created: no database, no key.
	for _, name := range []string{store.DatabaseFile, "bayplan.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("a refused Build created %s", name)
		}
	}
}

func TestBuildThatFailsAfterTakingTheLockReleasesIt(t *testing.T) {
	dir := t.TempDir() + "/data"
	cfg := testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir})
	first, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Store().Write.Exec("INSERT INTO goose_db_version (version_id, is_applied) VALUES (999, 1)"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler)); !errors.Is(err, store.ErrDowngrade) {
		t.Fatalf("Build = %v, want ErrDowngrade", err)
	}
	release, err := datadir.Lock(dir)
	if err != nil {
		t.Fatalf("a failed Build kept the lock: %v", err)
	}
	_ = release()
}
