// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"slices"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// PullResult is what a pull did to a stack's draft.
type PullResult struct {
	// DraftVersion is the version of the draft after the pull: the review and the deploy that follow are made at it.
	DraftVersion int64
	// Commit is the commit the draft was refreshed from.
	Commit gitx.Commit
	// Ref is the branch, tag or commit the stack follows.
	Ref string
	// DeployedCommit is the commit the stack's newest good revision was made from; empty when nothing was deployed
	// from the repository.
	DeployedCommit string
	// Touched is how many commits changed the stack's folder between the deployed commit and Commit; 0 when there is
	// no deployed commit to compare with.
	Touched int
}

// Pull fetches the head of the ref the stack follows and refreshes the stack's draft from it: the draft's repository
// files become those of the commit (only the files the compose files name), Bayplan's own files and the variables stay,
// and the draft records the commit, the folder's tree hash and the source. It deploys nothing and changes nothing that
// runs; the only way from a pull to a deploy is the review and the deploy that follow (D-17). A stack that follows a
// fixed commit imports that commit again, without any fetch of a moving ref.
//
// The draft must still be at version, or the answer is a *stackops.DraftConflictError and nothing is stored. A failure to
// read the repository is returned as gitx returned it, with the draft untouched. The stack's sync state learns the new
// head, unless it is in error, which a check looks at again for real.
func (s *Service) Pull(ctx context.Context, envID, stackID, actor string, version int64) (PullResult, error) {
	st, err := s.repo.Stack(ctx, envID, stackID)
	if err != nil {
		return PullResult{}, err
	}
	sg, err := s.repo.StackGit(ctx, stackID)
	if err != nil {
		return PullResult{}, err
	}
	draft, err := s.repo.Draft(ctx, stackID)
	if err != nil {
		return PullResult{}, err
	}
	// A draft that moved is refused before any fetch is made; the refresh checks it again when it stores.
	if draft.Version != version {
		return PullResult{}, &stackops.DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version}
	}
	var overlays []Overlay
	for _, f := range draft.Copy.Files {
		if f.Origin == stacks.OriginOverlay {
			overlays = append(overlays, Overlay{Path: f.Path, Content: f.Content})
		}
	}
	kind := gitx.RefKind(sg.RefKind)
	held := ""
	if kind == gitx.RefCommit {
		held = sg.Ref
	}
	imp, err := s.read(ctx, ImportRequest{
		EnvID: envID, RepoID: sg.RepoID, RefKind: kind, Ref: sg.Ref, Folder: sg.Folder, ComposeFiles: sg.ComposeFiles,
		Name: st.Name, Target: st.Target, Vars: draft.Copy.Vars, Overlays: overlays,
	}, held)
	if err != nil {
		return PullResult{}, err
	}

	var repoFiles []stacks.File
	for _, f := range imp.Files {
		if f.Origin == stacks.OriginRepo {
			repoFiles = append(repoFiles, f)
		}
	}
	for _, o := range overlays {
		if slices.ContainsFunc(repoFiles, func(f stacks.File) bool { return f.Path == o.Path }) {
			s.log.Warn("a commit holds a file at the path of an overlay; the repository's file stands", "stack", stackID, "path", o.Path)
		}
	}
	saved, err := s.repo.RefreshDraftFromGit(ctx, stackID, version, actor, repoFiles, imp.Revision())
	if err != nil {
		return PullResult{}, err
	}

	out := PullResult{DraftVersion: saved.Version, Commit: imp.Commit, Ref: sg.Ref}
	dep, deployed, err := s.repo.DeployedGit(ctx, stackID)
	if err != nil {
		return PullResult{}, err
	}
	if deployed {
		out.DeployedCommit = dep.Commit
		if !sourceChanged(sg, dep) {
			out.Touched = s.CommitsSince(ctx, sg.RepoID, sg.Folder, dep.Commit, imp.Commit.SHA)
		}
	}
	s.recordPulledHead(ctx, st, sg, dep, deployed, imp, out.Touched)
	return out, nil
}

// CommitsSince is how many commits changed the folder between from and to, read from the mirror of the repository only:
// nothing is fetched. A from that is not an ancestor of to counts as one, as a check counts it; a walk that hit the cap
// reads as BehindCap; an empty from, the same commit, or a mirror that can't be read is 0.
func (s *Service) CommitsSince(ctx context.Context, repoID, folder, from, to string) int {
	if from == "" || to == "" || from == to {
		return 0
	}
	touch, err := s.git.CommitsTouching(ctx, gitx.MirrorID(repoID), to, from, folder, BehindCap)
	if err != nil {
		s.log.Warn("could not count the commits that changed a folder", "repository", repoID, "error", err)
		return 0
	}
	switch {
	case touch.NotAncestor:
		return 1
	case touch.Capped:
		return BehindCap
	}
	return touch.Count
}

// CommitsBetween lists the commits that changed the folder between from and to, newest first with ties by ID, read from
// the mirror of the repository only: nothing is fetched. It says false when the list can't be made: a mirror that
// can't be read or does not hold the commits, or a from that is not an ancestor of to.
func (s *Service) CommitsBetween(ctx context.Context, repoID, folder, from, to string) (gitx.Touch, bool) {
	if from == "" || to == "" {
		return gitx.Touch{}, false
	}
	touch, err := s.git.CommitsTouching(ctx, gitx.MirrorID(repoID), to, from, folder, BehindCap)
	if err != nil {
		s.log.Warn("could not list the commits that changed a folder", "repository", repoID, "error", err)
		return gitx.Touch{}, false
	}
	if touch.NotAncestor {
		return gitx.Touch{}, false
	}
	return touch, true
}

// recordPulledHead tells the sync state which head the pull found, so that the strip and the deploy that follows judge
// the stack against it. A state in error is left alone. A failure to write it is logged: the draft is refreshed and the
// pull is done.
func (s *Service) recordPulledHead(ctx context.Context, st stackops.Stack, sg stackops.StackGit, dep stackops.GitRevision, deployed bool, imp Import, touched int) {
	prev, err := s.repo.GitState(ctx, st.ID)
	if err != nil || prev.Status == stackops.GitStatusError {
		if err != nil {
			s.log.Warn("could not read the git state after a pull", "stack", st.ID, "error", err)
		}
		return
	}
	now := s.now().UTC()
	next := prev
	next.CheckedAt = &now
	next.HeadCommit, next.HeadTree = imp.Commit.SHA, imp.Tree
	next.DeployedCommit, next.DeployedTree = "", ""
	next.ReasonCode, next.ReasonDetail = "", ""
	next.Status, next.Behind = stackops.GitStatusUnknown, 0
	switch {
	case !deployed:
	case sourceChanged(sg, dep):
		next.DeployedCommit, next.DeployedTree = dep.Commit, dep.Tree
		next.Status, next.Behind = stackops.GitStatusBehind, 1
		next.ReasonCode, next.ReasonDetail = string(ReasonSourceChanged), detailOf(ReasonSourceChanged, nil)
	case imp.Tree == dep.Tree:
		next.DeployedCommit, next.DeployedTree = dep.Commit, dep.Tree
		next.Status = stackops.GitStatusInSync
	default:
		next.DeployedCommit, next.DeployedTree = dep.Commit, dep.Tree
		next.Status, next.Behind = stackops.GitStatusBehind, max(touched, 1)
	}
	if err := s.writeState(ctx, st, prev, next, systemActor); err != nil {
		s.log.Warn("could not store the head a pull found", "stack", st.ID, "error", err)
	}
}
