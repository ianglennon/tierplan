// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// ErrKeepTooLow is returned by the prune calls for a keep below 1: a limit that would keep no revision
// at all is a bug in the caller, never a request to delete a stack's history.
var ErrKeepTooLow = errors.New("a stack keeps at least one revision")

// Prune applies the retention limit (D-01) to one stack and answers how many revisions it deleted. The
// stack keeps its newest keep revisions by seq, its newest good revision (the one deployed) however old,
// and any revision whose deploy is still running. Everything else is deleted, and the foreign keys take
// each deleted revision's files and variables with it, so a sealed variable or secret file does not outlive
// its revision. The draft is never touched. Pruning twice with the same keep deletes nothing the second
// time.
func (r Repo) Prune(ctx context.Context, stackID string, keep int) (int, error) {
	return pruneStack(ctx, r.Store.Q, stackID, keep)
}

// PruneAll prunes every stack, each in a statement of its own so no stack waits on another's, and
// answers how many revisions it deleted in all. It stops at the first stack that fails.
func (r Repo) PruneAll(ctx context.Context, keep int) (int, error) {
	if keep < 1 {
		return 0, ErrKeepTooLow
	}
	ids, err := r.Store.RQ.ListStackIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list stacks: %w", err)
	}
	total := 0
	for _, id := range ids {
		n, err := pruneStack(ctx, r.Store.Q, id, keep)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// EnforceKeepTx is PruneAll inside the caller's write transaction, so a change of the limit and the
// revisions it deletes stand or fall together. It answers how many revisions it deleted.
func (r Repo) EnforceKeepTx(ctx context.Context, tx *sql.Tx, keep int) (int, error) {
	if keep < 1 {
		return 0, ErrKeepTooLow
	}
	q := r.Store.Q.WithTx(tx)
	ids, err := q.ListStackIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list stacks: %w", err)
	}
	total := 0
	for _, id := range ids {
		n, err := pruneStack(ctx, q, id, keep)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// PrunePreview says how many revisions across how many stacks a limit of keep would delete now. It only
// reads, and it applies the rule Prune applies.
func (r Repo) PrunePreview(ctx context.Context, keep int) (revisions, stacks int, err error) {
	if keep < 1 {
		return 0, 0, ErrKeepTooLow
	}
	row, err := r.Store.RQ.CountPrunableRevisions(ctx, int64(keep))
	if err != nil {
		return 0, 0, fmt.Errorf("count the revisions a limit of %d would delete: %w", keep, err)
	}
	return int(row.Revisions), int(row.Stacks), nil
}

func pruneStack(ctx context.Context, q *sqlc.Queries, stackID string, keep int) (int, error) {
	if keep < 1 {
		return 0, ErrKeepTooLow
	}
	n, err := q.PruneRevisions(ctx, sqlc.PruneRevisionsParams{StackID: stackID, Keep: int64(keep)})
	if err != nil {
		return 0, fmt.Errorf("prune the revisions of stack %s: %w", stackID, err)
	}
	return int(n), nil
}
