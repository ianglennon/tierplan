// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ianglennon/tierplan/internal/store"
)

// TestExportConnectionEnforcesForeignKeys pins D-03 for the export copy: the connection scrub works on
// has foreign_keys on, so the cascades and restricts of the schema apply while excluded rows are stripped.
func TestExportConnectionEnforcesForeignKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := vacuumInto(ctx, st, copyPath); err != nil {
		t.Fatal(err)
	}

	db, err := openScrubDB(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var fk int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("PRAGMA foreign_keys = %d on the export connection, want 1", fk)
	}
}
