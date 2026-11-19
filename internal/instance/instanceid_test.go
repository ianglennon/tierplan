// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
)

func countInstanceIDRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Write.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM settings WHERE key = 'instance_id'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInstanceIDIsMintedOnceAndKept(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }

	if _, err := instance.InstanceID(ctx, st); !errors.Is(err, instance.ErrNoInstanceID) {
		t.Fatalf("InstanceID before minting = %v, want ErrNoInstanceID", err)
	}

	first, err := instance.EnsureInstanceID(ctx, st, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(first)
	if err != nil {
		t.Fatalf("minted ID %q does not parse: %v", first, err)
	}
	if id.Version() != 7 {
		t.Fatalf("minted ID is version %d, want 7", id.Version())
	}
	if n := countInstanceIDRows(t, st); n != 1 {
		t.Fatalf("%d instance_id rows, want 1", n)
	}

	second, err := instance.EnsureInstanceID(ctx, st, now)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second call returned %q, want %q", second, first)
	}
	read, err := instance.InstanceID(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if read != first {
		t.Fatalf("InstanceID = %q, want %q", read, first)
	}

	// Never cached: removing the row is seen on the next read.
	if _, err := st.Write.ExecContext(ctx, `DELETE FROM settings WHERE key = 'instance_id'`); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.InstanceID(ctx, st); !errors.Is(err, instance.ErrNoInstanceID) {
		t.Fatalf("InstanceID after delete = %v, want ErrNoInstanceID", err)
	}
}

func TestInstanceIDConcurrentFirstCallsAgree(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	const workers = 8

	type result struct {
		id  string
		err error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	for range workers {
		go func() {
			<-start
			id, err := instance.EnsureInstanceID(ctx, st, time.Now)
			results <- result{id, err}
		}()
	}
	close(start)

	seen := map[string]bool{}
	for range workers {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		seen[r.id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent first calls returned %d distinct IDs, want 1: %v", len(seen), seen)
	}
	if n := countInstanceIDRows(t, st); n != 1 {
		t.Fatalf("%d instance_id rows, want 1", n)
	}
}

func TestInstanceIDNilClockUsesTimeNow(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)

	got, err := instance.EnsureInstanceID(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(got)
	if err != nil {
		t.Fatalf("minted ID %q does not parse: %v", got, err)
	}
	if id.Version() != 7 {
		t.Fatalf("minted ID is version %d, want 7", id.Version())
	}
	var updatedAt int64
	if err := st.Write.QueryRowContext(ctx,
		`SELECT updated_at FROM settings WHERE key = 'instance_id'`).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt <= 0 {
		t.Fatalf("updated_at = %d, want > 0", updatedAt)
	}
}
