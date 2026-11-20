// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
)

// TestInstanceIDSurvivesExportAndRestore (D-23): the instance ID is a settings row, which a backup
// carries, so a restored TierPlan still presents the ID its nodes pinned. The target directory had
// minted an ID of its own first, and the restore replaces it.
func TestInstanceIDSurvivesExportAndRestore(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t)
	a, err := instance.EnsureInstanceID(ctx, f.st, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}

	// A data directory whose own store has already minted an ID.
	dataDir := t.TempDir()
	fresh, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, fresh, store.MigrateOptions{AppVersion: "test", DataDir: dataDir}); err != nil {
		_ = fresh.Close()
		t.Fatal(err)
	}
	b, err := instance.EnsureInstanceID(ctx, fresh, nil)
	if err != nil {
		_ = fresh.Close()
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("both installs minted %q; the test needs two different IDs", a)
	}

	if _, err := restoreInto(t, file, dataDir, targetVault(t)); err != nil {
		t.Fatalf("restore: %v", err)
	}

	restored, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	got, err := instance.InstanceID(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	if got != a {
		t.Fatalf("restored instance ID = %q, want the exported %q (the target had minted %q)", got, a, b)
	}
}

// TestInstanceIDIsMintedAfterRestoringABackupWithoutOne: a backup taken before any ID existed
// restores without the row, and the boot path (22-18) then mints one.
func TestInstanceIDIsMintedAfterRestoringABackupWithoutOne(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t)
	if _, err := instance.InstanceID(ctx, f.st); !errors.Is(err, instance.ErrNoInstanceID) {
		t.Fatalf("the fixture already has an instance ID: %v", err)
	}
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if _, err := restoreInto(t, file, dataDir, targetVault(t)); err != nil {
		t.Fatalf("restore: %v", err)
	}

	restored, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	if _, err := instance.InstanceID(ctx, restored); !errors.Is(err, instance.ErrNoInstanceID) {
		t.Fatalf("InstanceID after restoring a backup without one = %v, want ErrNoInstanceID", err)
	}
	minted, err := instance.EnsureInstanceID(ctx, restored, func() time.Time { return time.Now() })
	if err != nil {
		t.Fatal(err)
	}
	read, err := instance.InstanceID(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	if read != minted || minted == "" {
		t.Fatalf("InstanceID = %q after EnsureInstanceID returned %q", read, minted)
	}
}
