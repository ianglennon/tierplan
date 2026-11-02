// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package storetest_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
)

// BenchmarkNew measures what every test that needs a store pays to get one.
func BenchmarkNew(b *testing.B) {
	for b.Loop() {
		storetest.New(b)
	}
}

func TestEveryStoreFromNewIsIndependent(t *testing.T) {
	ctx := context.Background()
	a, b := storetest.New(t), storetest.New(t)

	if _, err := a.Write.ExecContext(ctx,
		`INSERT INTO app_meta (key, value, updated_at) VALUES ('storetest_marker', 'a', 1)`); err != nil {
		t.Fatalf("write to the first store: %v", err)
	}

	var inA, inB int
	if err := a.Read.QueryRowContext(ctx, `SELECT count(*) FROM app_meta WHERE key = 'storetest_marker'`).Scan(&inA); err != nil {
		t.Fatalf("read the first store: %v", err)
	}
	if err := b.Read.QueryRowContext(ctx, `SELECT count(*) FROM app_meta WHERE key = 'storetest_marker'`).Scan(&inB); err != nil {
		t.Fatalf("read the second store: %v", err)
	}
	if inA != 1 || inB != 0 {
		t.Fatalf("marker rows: first store %d (want 1), second store %d (want 0)", inA, inB)
	}
	if a.Path == b.Path {
		t.Fatalf("both stores use %s", a.Path)
	}
}

func TestAStoreFromNewIsFullyMigrated(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)

	target, err := store.TargetSchema()
	if err != nil {
		t.Fatalf("target schema: %v", err)
	}
	got, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if got != target {
		t.Fatalf("schema version = %d, want the highest migration %d", got, target)
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "test", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if res.From != target || res.To != target || res.SnapshotPath != "" {
		t.Fatalf("second migrate = %+v, want nothing applied at version %d", res, target)
	}
}

func TestTheDatabaseFileOfANewStoreIsReadableOnlyByItsOwner(t *testing.T) {
	s := storetest.New(t)

	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatalf("stat the database: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("database mode = %o, want 600", mode)
	}
}

func TestNewIsSafeFromParallelTests(t *testing.T) {
	for i := range 8 {
		t.Run(fmt.Sprintf("writer %d", i), func(t *testing.T) {
			t.Parallel()
			s := storetest.New(t)
			if _, err := s.Write.ExecContext(context.Background(),
				`INSERT INTO app_meta (key, value, updated_at) VALUES ('storetest_marker', ?, 1)`, i); err != nil {
				t.Fatalf("write: %v", err)
			}
		})
	}
}
