// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"errors"
	"slices"

	"github.com/ianglennon/bayplan/internal/stackops"
)

// Deployed is told that a revision of a stack ended good. For a revision made from a repository it makes the stack's
// settings follow what was deployed (the repository, ref, folder and compose files of the revision's source) and sets
// the sync state's deployed commit and tree from the revision, then works out the status against the head the state
// holds: the stack is in sync when that head is the revision's commit or has its tree, and behind otherwise. A revision
// that is not from a repository changes nothing. A failure is logged and never fails the deploy that is over.
//
// The head the state holds is the last one a check or a pull saw. A state in error, or one that holds no head, takes the
// revision as its head: the next check looks at the repository again and corrects it.
func (s *Service) Deployed(ctx context.Context, stackID string, rev stackops.Snapshot) {
	if rev.Git.Commit == "" {
		return
	}
	sg, err := s.repo.StackGit(ctx, stackID)
	if errors.Is(err, stackops.ErrNoGit) {
		return
	}
	if err != nil {
		s.log.Warn("could not read the git settings after a deploy", "stack", stackID, "error", err)
		return
	}
	src := rev.Git.Source
	if sourceChanged(sg, rev.Git) || !slices.Equal(sg.ComposeFiles, src.ComposeFiles) {
		if err := s.repo.SetAppliedSource(ctx, stackID, src); err != nil {
			s.log.Warn("could not make the git settings follow the deployed revision", "stack", stackID, "error", err)
		}
	}

	prev, err := s.repo.GitState(ctx, stackID)
	if err != nil {
		s.log.Warn("could not read the git state after a deploy", "stack", stackID, "error", err)
		return
	}
	now := s.now().UTC()
	next := prev
	next.CheckedAt = &now
	next.DeployedCommit, next.DeployedTree = rev.Git.Commit, rev.Git.Tree
	next.ReasonCode, next.ReasonDetail = "", ""
	switch {
	case prev.HeadCommit == "" || prev.Status == stackops.GitStatusError || prev.HeadCommit == rev.Git.Commit || prev.HeadTree == rev.Git.Tree:
		next.HeadCommit, next.HeadTree = rev.Git.Commit, rev.Git.Tree
		next.Status, next.Behind = stackops.GitStatusInSync, 0
	default:
		next.Status, next.Behind = stackops.GitStatusBehind, max(prev.Behind, 1)
	}
	if err := s.writeState(ctx, stackops.Stack{ID: stackID}, prev, next, systemActor); err != nil {
		s.log.Warn("could not store the git state after a deploy", "stack", stackID, "error", err)
	}
}
