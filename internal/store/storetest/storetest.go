// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package storetest gives tests a real, migrated SQLite store in a temporary
// directory. There is no fake: tests exercise the same SQL as production.
package storetest

import (
	"context"
	"testing"

	"github.com/ianglennon/bayplan/internal/store"
)

// New opens and migrates a store in t.TempDir() and closes it when the test
// ends.
func New(t testing.TB) *store.Store {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	return s
}
