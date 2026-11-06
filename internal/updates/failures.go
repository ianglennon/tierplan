// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// The remote digest an image update failed with, remembered per workload (IMG-07, D-14), so the badge can
// say "Update failed, rolled back" for that exact digest and stop offering it until a newer one appears. A
// workload has at most one remembered failure: a newer one replaces it and a later good update clears it.
// Every failure belongs to one environment and is read there alone.

// Failure is the update that last failed on one workload.
type Failure struct {
	EnvID string
	// Kind is "service" or "container"; Workload is its name.
	Kind, Workload string
	// Digest is the remote digest the update failed with.
	Digest string
	// Reason is why it failed; RunID the update run that failed.
	Reason, RunID string
	// RollbackFailed is true when restoring the previous image failed too.
	RollbackFailed bool
	FailedAt       time.Time
}

// FailureStore keeps failures in SQLite. Q runs on the write connection W, like SQLStore. Now stamps a
// failure that has no FailedAt; the wall clock when nil.
type FailureStore struct {
	Q   *sqlc.Queries
	W   *sql.DB
	Now func() time.Time
}

// FailureKey is the key ForEnv files a failure under: kind + "/" + workload.
func FailureKey(kind, workload string) string { return kind + "/" + workload }

// Record remembers a failure for its workload, replacing the one it had.
func (s FailureStore) Record(ctx context.Context, f Failure) error {
	failed := f.FailedAt
	if failed.IsZero() {
		failed = time.Now()
		if s.Now != nil {
			failed = s.Now()
		}
	}
	var rb int64
	if f.RollbackFailed {
		rb = 1
	}
	if err := s.Q.UpsertImageUpdateFailure(ctx, sqlc.UpsertImageUpdateFailureParams{
		EnvID: f.EnvID, Kind: f.Kind, Workload: f.Workload, Digest: f.Digest, Reason: f.Reason, RunID: f.RunID,
		RollbackFailed: rb, FailedAt: failed.UnixNano(),
	}); err != nil {
		return fmt.Errorf("remember a failed image update: %w", err)
	}
	return nil
}

// Clear forgets the failure of a workload and reports whether there was one.
func (s FailureStore) Clear(ctx context.Context, envID, kind, workload string) (bool, error) {
	n, err := s.Q.DeleteImageUpdateFailure(ctx, sqlc.DeleteImageUpdateFailureParams{EnvID: envID, Kind: kind, Workload: workload})
	if err != nil {
		return false, fmt.Errorf("forget a failed image update: %w", err)
	}
	return n > 0, nil
}

// ForEnv returns the failures of one environment, keyed by FailureKey(kind, workload).
func (s FailureStore) ForEnv(ctx context.Context, envID string) (map[string]Failure, error) {
	rows, err := s.Q.ListImageUpdateFailures(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the failed image updates of an environment: %w", err)
	}
	out := make(map[string]Failure, len(rows))
	for _, r := range rows {
		out[FailureKey(r.Kind, r.Workload)] = Failure{
			EnvID: r.EnvID, Kind: r.Kind, Workload: r.Workload, Digest: r.Digest, Reason: r.Reason, RunID: r.RunID,
			RollbackFailed: r.RollbackFailed != 0, FailedAt: fromNanos(r.FailedAt),
		}
	}
	return out, nil
}
