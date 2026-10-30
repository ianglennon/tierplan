// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// stateLock is the lock of one stack's sync state, counted so that the entry goes when nobody holds or wants it.
type stateLock struct {
	mu    sync.Mutex
	users int
}

// lockState takes the stack's sync-state lock and returns what releases it. Every writer of the state (a check, a pull,
// a deploy's hooks, an automatic deploy holding a commit back) holds it from the moment it reads the state to the moment
// it writes, so a writer that reads after another has written sees that write and builds on it, and an answer made from
// an older reading can no longer replace a newer one (review WR-B03). Nothing is held across a network call, and a
// holder never takes another lock of this kind: the lock is not re-entrant.
func (s *Service) lockState(stackID string) (unlock func()) {
	s.mu.Lock()
	l := s.stateLocks[stackID]
	if l == nil {
		l = &stateLock{}
		s.stateLocks[stackID] = l
	}
	l.users++
	s.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		if l.users--; l.users == 0 {
			delete(s.stateLocks, stackID)
		}
		s.mu.Unlock()
	}
}

// writeState stores the answer of a check. It is the one place stack_git_state is written, so that what is new in an
// answer is judged in one place (D-16). st is the stack the state belongs to and actor who the answer is for.
//
// An answer of error is new when the stored state was not an error or the reason code differs. A new error starts
// error_since and the run of failures at one, and is audited once as git.sync_failed. The same error seen again only
// adds to the run of failures and keeps its start, and writes nothing to the audit log. Any other answer clears the
// error's start and the run, so the next error is new. An answer that is the one already stored, looked at again,
// changes only the time of the check.
func (s *Service) writeState(ctx context.Context, st stackops.Stack, prev, next stackops.GitState, actor audit.Actor) error {
	fresh := false
	switch {
	case next.Status != stackops.GitStatusError:
		next.ErrorSince, next.ConsecutiveFailures = nil, 0
	case prev.Status == stackops.GitStatusError && prev.ReasonCode == next.ReasonCode:
		next.ConsecutiveFailures = max(prev.ConsecutiveFailures, 1) + 1
		next.ErrorSince = prev.ErrorSince
		if next.ErrorSince == nil {
			next.ErrorSince = next.CheckedAt
		}
	default:
		fresh = true
		next.ConsecutiveFailures = 1
		next.ErrorSince = next.CheckedAt
	}
	if err := s.store(ctx, prev, next); err != nil {
		return err
	}
	if fresh {
		s.auditSyncFailed(ctx, st, next, actor)
	}
	return nil
}

func (s *Service) store(ctx context.Context, prev, next stackops.GitState) error {
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

// shortSHALen is how many characters of a commit the audit log keeps.
const shortSHALen = 7

// auditSyncFailed records that a stack's sync went into an error. The detail names the stack, the reason code and its
// fixed sentence, and the short SHA of the head last seen; nothing from the server or the credential. A failure to write
// the entry is logged and does not undo the state: the stack must read error either way.
func (s *Service) auditSyncFailed(ctx context.Context, st stackops.Stack, next stackops.GitState, actor audit.Actor) {
	if s.audit == nil {
		return
	}
	commit := next.HeadCommit
	if len(commit) > shortSHALen {
		commit = commit[:shortSHALen]
	}
	err := s.audit.Record(ctx, audit.Entry{
		Actor: actor, Action: audit.ActionGitSyncFailed, TargetType: "stack", TargetID: st.ID, EnvID: st.EnvID,
		Result: audit.ResultFailure, Details: map[string]string{
			"stack": st.Name, "reason_code": next.ReasonCode, "reason": next.ReasonDetail, "commit": commit,
		},
	})
	if err != nil {
		s.log.Error("could not audit a failed git sync", "stack", st.ID, "error", err)
	}
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
