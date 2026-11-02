// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// DetachResult is what a Detach made the draft from.
type DetachResult struct {
	// Revision is the number of the revision whose files became the draft; 0 for a stack that was never deployed, whose
	// draft is its own.
	Revision int64
	// Commit is the commit that revision was made from; empty when it records none.
	Commit string
}

// Detach turns a git stack back into an inline stack you edit in TierPlan (D-25), without touching what runs. The files of
// the deployed revision become the draft (a stack never deployed keeps the draft it has), as inline files: the overlay
// files are inline files that stay secret. The variables and the ordered compose file list are kept. The stack's git
// settings and sync state are deleted, so the poller no longer lists it and its webhook address answers 404, and the
// owned rows of its webhook and its auto-deploy are forgotten, all in one transaction. No deploy starts and no revision is
// made, so the history keeps every revision.
//
// A stack that is not a git stack is stackops.ErrNoGit and nothing changes. A check that was running when the stack was
// detached finds no git settings when it writes (stackops.Repo.WriteGitState), so it leaves no state behind, and a deploy
// an automatic deploy has not started yet finds its owner forgotten (access.Service.RunAs), so none starts.
func (s *Service) Detach(ctx context.Context, envID, stackID, actor string) (DetachResult, error) {
	if _, err := s.repo.Stack(ctx, envID, stackID); err != nil {
		return DetachResult{}, err
	}
	if _, err := s.repo.StackGit(ctx, stackID); err != nil {
		return DetachResult{}, err
	}
	rev, ok, err := s.repo.LatestGoodRevision(ctx, stackID)
	if err != nil {
		return DetachResult{}, err
	}
	if !ok {
		// Never deployed: nothing runs, and the draft the stack was made with is what there is to edit.
		if rev, err = s.repo.Draft(ctx, stackID); err != nil {
			return DetachResult{}, err
		}
		rev.Seq = 0
	}
	err = s.repo.Detach(ctx, stackID, rev, actor, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		if s.access == nil {
			return nil
		}
		for _, kind := range []string{KindGitAutoDeploy, KindGitWebhook} {
			if err := s.access.DeregisterOwnedTx(ctx, tx, q, kind, stackID); err != nil {
				return fmt.Errorf("forget the %s automation: %w", kind, err)
			}
		}
		return nil
	})
	if err != nil {
		return DetachResult{}, err
	}
	return DetachResult{Revision: rev.Seq, Commit: rev.Git.Commit}, nil
}
