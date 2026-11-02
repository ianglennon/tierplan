// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package storetest gives tests a real, migrated SQLite store in a temporary
// directory. There is no fake: tests exercise the same SQL as production.
//
// Running every migration for every test is the largest single cost of a
// store-backed test, so the first New in a process migrates one database with
// the real store.Migrate, keeps its bytes, and every New after that starts from
// a copy of them. A copy is a database as the migrations leave it, so it is not
// a fake either.
package storetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/store"
)

var (
	templateOnce  sync.Once
	templateBytes []byte
	templateErr   error
)

// template returns the bytes of a freshly migrated database, building them the
// first time. The directory it is built in is removed before it returns, so
// nothing accumulates in the temporary directory of a runner.
func template() ([]byte, error) {
	templateOnce.Do(func() { templateBytes, templateErr = buildTemplate() })
	return templateBytes, templateErr
}

func buildTemplate() (data []byte, err error) {
	dir, err := os.MkdirTemp("", "tierplan-storetest-*")
	if err != nil {
		return nil, fmt.Errorf("create template directory: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil && err == nil {
			err = fmt.Errorf("remove template directory: %w", rerr)
		}
	}()

	ctx := context.Background()
	s, err := store.Open(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Fold the WAL into the main file so that the file alone is the database.
	if _, err := s.Write.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if err := s.Close(); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	data, err = os.ReadFile(filepath.Join(dir, store.DatabaseFile)) //nolint:gosec // G304: a file in the temporary directory this function just created
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return data, nil
}

// New opens a migrated store in t.TempDir() and closes it when the test ends.
// Every store is its own database file.
func New(t testing.TB) *store.Store {
	t.Helper()
	tmpl, err := template()
	if err != nil {
		t.Fatalf("storetest: template: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, store.DatabaseFile), tmpl, 0o600); err != nil {
		t.Fatalf("storetest: copy template: %v", err)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Nothing is pending, so this applies no migration; it records the app
	// version and sets the schema version callers read.
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	return s
}
