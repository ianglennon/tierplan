// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync

import (
	"context"
	"slices"

	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// SourceChange is where a git stack is to follow from now on: the repository, the branch, tag or commit, the folder and
// the compose files in merge order.
type SourceChange struct {
	RepoID       string
	RefKind      gitx.RefKind
	Ref          string
	Folder       string
	ComposeFiles []string
}

// ChangeSource imports the new source at its head into the stack's draft and applies nothing (D-11, D-12): the draft
// records the commit, the folder's tree hash and the new source, so it is the pending source; the repository files of the
// draft are those of the new commit, the overlay files and the variables stay. The stack's settings (what Pull & redeploy,
// the polling and the webhook follow) and its sync state change only when a deploy of this draft ends good, which makes
// the new source the stack's source. Discarding the draft, or a pull, drops the pending source.
//
// The repository must be one of the environment or an instance repository, else it is gitconn.ErrNotFound. The draft must
// still be at version, or the answer is a *stackops.DraftConflictError and nothing is stored. A failure to read the new
// source is returned as gitx and the importer return it, with the draft untouched.
//
// The result names the new commit; DeployedCommit is that of the stack's newest good revision and Touched is 0, since a
// count of commits means nothing across two sources.
func (s *Service) ChangeSource(ctx context.Context, envID, stackID, actor string, version int64, c SourceChange) (PullResult, error) {
	st, err := s.repo.Stack(ctx, envID, stackID)
	if err != nil {
		return PullResult{}, err
	}
	if _, err := s.repo.StackGit(ctx, stackID); err != nil {
		return PullResult{}, err
	}
	draft, err := s.repo.Draft(ctx, stackID)
	if err != nil {
		return PullResult{}, err
	}
	if draft.Version != version {
		return PullResult{}, &stackops.DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version}
	}
	imp, saved, err := s.refreshDraft(ctx, st, draft, actor, version, c)
	if err != nil {
		return PullResult{}, err
	}
	out := PullResult{DraftVersion: saved.Version, Commit: imp.Commit, Ref: imp.Source.Ref}
	dep, deployed, err := s.repo.DeployedGit(ctx, stackID)
	if err != nil {
		return PullResult{}, err
	}
	if deployed {
		out.DeployedCommit = dep.Commit
	}
	return out, nil
}

// PendingSource says whether the draft holds another source than the one the stack follows: the repository, the ref, the
// folder or the compose files differ. A draft that is not from a repository holds none.
func PendingSource(sg stackops.StackGit, draft stackops.Snapshot) bool {
	if draft.Git.Commit == "" {
		return false
	}
	d := draft.Git.Source
	return sg.RepoID != d.RepoID || sg.RefKind != d.RefKind || sg.Ref != d.Ref || sg.Folder != d.Folder ||
		!slices.Equal(composeOf(sg.ComposeFiles), composeOf(d.ComposeFiles))
}

func composeOf(files []string) []string {
	if files == nil {
		return []string{}
	}
	return files
}
