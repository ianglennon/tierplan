// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// writeState stores the answer of a check. It is the one place stack_git_state is written, so that what is new in an
// answer (a stack that has just gone into error) is judged in one place: the audit row and the bell item of a new error
// hang on this seam (D-16). actor is who the answer is for; nothing is audited yet.
//
// An answer that is the one already stored, looked at again, changes only the time of the check.
func (s *Service) writeState(ctx context.Context, prev, next stackops.GitState, _ audit.Actor) error {
	if prev.CheckedAt != nil && sameAnswer(prev, next) {
		if next.CheckedAt == nil {
			return nil
		}
		err := s.repo.TouchGitChecked(ctx, next.StackID, *next.CheckedAt)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return s.repo.WriteGitState(ctx, next)
}

// sameAnswer says whether two states differ only in when they were checked.
func sameAnswer(a, b stackops.GitState) bool {
	return a.StackID == b.StackID && a.DeployedCommit == b.DeployedCommit && a.DeployedTree == b.DeployedTree &&
		a.HeadCommit == b.HeadCommit && a.HeadTree == b.HeadTree && a.Behind == b.Behind && a.Status == b.Status &&
		a.ReasonCode == b.ReasonCode && a.ReasonDetail == b.ReasonDetail && sameTime(a.ErrorSince, b.ErrorSince) &&
		a.ConsecutiveFailures == b.ConsecutiveFailures && a.PendingCommit == b.PendingCommit && sameTime(a.NextCheckAt, b.NextCheckAt)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
