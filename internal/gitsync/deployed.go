// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"errors"
	"slices"
	"strconv"

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
	rolledBack := rev.Origin == stackops.OriginRollback
	// A source the person changed through review (D-11) becomes the stack's when the deploy of it ends good.
	newSource := !rolledBack && (sourceChanged(sg, rev.Git) || !slices.Equal(sg.ComposeFiles, src.ComposeFiles))
	if newSource {
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
	// A good deploy ends whatever a held back commit was waiting for (D-19).
	next.PendingCommit = ""
	switch {
	case rolledBack && sourceChanged(sg, rev.Git):
		// The restored revision came from another folder, ref or repository than the stack follows, so the two trees
		// are not comparable and no commits are counted; the head waits for review.
		next.Status, next.Behind = stackops.GitStatusBehind, 1
		next.ReasonCode, next.ReasonDetail = string(ReasonSourceChanged), detailOf(ReasonSourceChanged, nil)
	case newSource:
		// The head the state holds was read from the source the stack followed before, so it says nothing about the new
		// one. The commit just deployed is the best head known, and the next check looks at the repository again.
		next.HeadCommit, next.HeadTree = rev.Git.Commit, rev.Git.Tree
		next.Status, next.Behind = stackops.GitStatusInSync, 0
	case prev.HeadCommit == "" || prev.Status == stackops.GitStatusError || prev.HeadCommit == rev.Git.Commit || prev.HeadTree == rev.Git.Tree:
		next.HeadCommit, next.HeadTree = rev.Git.Commit, rev.Git.Tree
		next.Status, next.Behind = stackops.GitStatusInSync, 0
	default:
		next.Status, next.Behind = stackops.GitStatusBehind, max(prev.Behind, 1)
	}
	if err := s.writeState(ctx, stackops.Stack{ID: stackID}, prev, next, systemActor); err != nil {
		s.log.Warn("could not store the git state after a deploy", "stack", stackID, "error", err)
		return
	}
	// A deploy ends the notice of a held back commit, and the notice of what an older automatic deploy put back (the
	// event's detail is the number of the revision that did it, which this one may be).
	if prev.EventKind == stackops.GitEventSkipped ||
		(prev.EventKind == stackops.GitEventReverted && prev.EventDetail != strconv.FormatInt(rev.Seq, 10)) {
		s.setEvent(ctx, stackID, "", "")
	}
}

// DeployFailed is told that the deploy of a revision ended failed. For a revision an automatic deploy made it puts the
// stack in error auto_deploy_failed naming the service that failed, and records the commit as pending so a commit that
// failed is not deployed again in a loop; the last good version keeps running and nothing is rolled back (P7 D-14). The
// next commit that changes the folder is tried as usual. A revision a person deployed, or one that is not from a
// repository, changes nothing. A failure is logged and never fails the deploy that is over.
func (s *Service) DeployFailed(ctx context.Context, st stackops.Stack, rev stackops.Snapshot, failedService string) {
	if rev.Git.Commit == "" || rev.SyncTrigger == "" {
		return
	}
	prev, err := s.repo.GitState(ctx, st.ID)
	if err != nil {
		s.log.Warn("could not read the git state after a failed deploy", "stack", st.ID, "error", err)
		return
	}
	detail := "The automatic deploy failed."
	if failedService != "" {
		detail = "The automatic deploy failed at the service " + failedService + "."
	}
	now := s.now().UTC()
	next := prev
	next.CheckedAt = &now
	next.Status, next.ReasonCode, next.ReasonDetail = stackops.GitStatusError, string(ReasonAutoDeployFailed), clipProblem(detail)
	next.PendingCommit = rev.Git.Commit
	if prev.HeadCommit == "" {
		next.HeadCommit, next.HeadTree = rev.Git.Commit, rev.Git.Tree
	}
	if err := s.writeState(ctx, st, prev, next, systemActor); err != nil {
		s.log.Warn("could not store the git state after a failed deploy", "stack", st.ID, "error", err)
	}
}
