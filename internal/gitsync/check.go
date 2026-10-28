// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"fmt"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// Outcome is what a sync check found.
type Outcome string

// The outcomes of a check.
const (
	// OutcomeInSync: the stack's folder at the head is the one that is deployed.
	OutcomeInSync Outcome = "in_sync"
	// OutcomeBehind: the folder has changed since the deployed commit.
	OutcomeBehind Outcome = "behind"
	// OutcomeError: the check could not be made; the reason is stored with the state.
	OutcomeError Outcome = "error"
	// OutcomeSkipped: there is nothing deployed to compare with. The head is recorded all the same.
	OutcomeSkipped Outcome = "skipped"
)

// What started a check.
const (
	TriggerManual  = "manual"
	TriggerPoll    = "poll"
	TriggerWebhook = "webhook"
)

// BehindCap is how many commits a check walks between the deployed commit and the head. A stack that is more than that
// behind reads as behind by this many, and the screens say so (RESEARCH Code Example 3, T-09-45).
const BehindCap = gitx.DefaultTouchLimit

// checkDeadline bounds one check, fetch and walk together. The fetch has a deadline of its own; this one is the backstop
// for a check that is detached from the caller that started it.
const checkDeadline = 3 * time.Minute

// systemActor is who a check is recorded as: nobody asked for the change it records, it only reports what it saw.
var systemActor = audit.Actor{Type: audit.ActorSystem, ID: "system:gitsync", Name: "system:gitsync"}

// checkCall is a check in flight: res and err are set before done closes.
type checkCall struct {
	done    chan struct{}
	waiters int
	res     Outcome
	err     error
}

// Check looks at a git stack's repository and records what it saw in the stack's state: the head of the tracked ref, the
// hash of the stack folder's tree there, and whether that is the folder that is deployed (D-13). The deployed commit and
// tree come from the newest good revision, not from the stack's current settings (RESEARCH Pitfall 8). Behind counts the
// commits that changed the folder.
//
// A check only records. It deploys nothing, and it makes no draft and no revision (GIT-03). A failure to reach the
// repository is an Outcome of OutcomeError with the reason stored, not an error; an error is returned for a stack that
// is not a git stack of this environment, or for a failure of Bayplan's own storage.
//
// Callers that ask for the same stack while a check runs share it. The check is not tied to the first caller's context
// (a caller that gives up leaves it running for the others), and a caller that gives up returns its context's error.
func (s *Service) Check(ctx context.Context, envID, stackID, trigger string) (Outcome, error) {
	s.mu.Lock()
	c, joined := s.checking[stackID]
	if joined {
		c.waiters++
	} else {
		c = &checkCall{done: make(chan struct{}), waiters: 1}
		s.checking[stackID] = c
	}
	waiting := c.waiters
	s.mu.Unlock()
	if s.joined != nil {
		s.joined(stackID, waiting)
	}
	if !joined {
		go s.runCheck(context.WithoutCancel(ctx), envID, stackID, trigger, c)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-c.done:
		return c.res, c.err
	}
}

// InFlight says whether a check of the stack is running now.
func (s *Service) InFlight(stackID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.checking[stackID]
	return ok
}

func (s *Service) runCheck(ctx context.Context, envID, stackID, trigger string, c *checkCall) {
	ctx, cancel := context.WithTimeout(ctx, checkDeadline)
	defer cancel()
	res, err := s.check(ctx, envID, stackID, trigger)
	s.mu.Lock()
	delete(s.checking, stackID)
	c.res, c.err = res, err
	s.mu.Unlock()
	close(c.done)
}

func (s *Service) check(ctx context.Context, envID, stackID, _ string) (Outcome, error) {
	if _, err := s.repo.Stack(ctx, envID, stackID); err != nil {
		return "", err
	}
	sg, err := s.repo.StackGit(ctx, stackID)
	if err != nil {
		return "", err
	}
	prev, err := s.repo.GitState(ctx, stackID)
	if err != nil {
		return "", err
	}
	dep, deployed, err := s.repo.DeployedGit(ctx, stackID)
	if err != nil {
		return "", err
	}
	repo, err := s.conns.GetRepo(ctx, sg.RepoID)
	if err != nil {
		return "", fmt.Errorf("read the repository: %w", err)
	}
	now := s.now().UTC()
	next := prev
	next.CheckedAt = &now
	next.DeployedCommit, next.DeployedTree = "", ""
	if deployed {
		next.DeployedCommit, next.DeployedTree = dep.Commit, dep.Tree
	}

	rem, err := s.conns.Remote(ctx, repo)
	if err != nil {
		s.log.Error("could not prepare a git remote for a sync check", "stack", stackID, "error", err)
		return s.fail(ctx, prev, next, ReasonFetchFailed, err)
	}
	mirror := gitx.MirrorID(repo.ID)
	head, err := s.git.FetchRef(ctx, mirror, rem, gitx.RefKind(sg.RefKind), sg.Ref)
	s.recordUse(ctx, repo, err)
	if err != nil {
		return s.fail(ctx, prev, next, reasonOf(err), err)
	}
	tree, err := s.git.FolderTree(ctx, mirror, head.SHA, sg.Folder)
	if err != nil {
		return s.fail(ctx, prev, next, reasonOf(err), err)
	}
	next.HeadCommit, next.HeadTree = head.SHA, tree
	next.Status, next.Behind = stackops.GitStatusUnknown, 0
	next.ReasonCode, next.ReasonDetail, next.ErrorSince, next.ConsecutiveFailures = "", "", nil, 0

	out := OutcomeSkipped
	switch {
	case !deployed:
		// Nothing was deployed from the repository, so there is nothing to be behind.
	case sourceChanged(sg, dep):
		// The folder or ref the stack follows is not the one that was deployed, so the two trees are not comparable
		// and no commits are counted (RESEARCH Pitfall 8).
		next.Status, next.Behind = stackops.GitStatusBehind, 1
		next.ReasonCode, next.ReasonDetail = string(ReasonSourceChanged), detailOf(ReasonSourceChanged, nil)
		out = OutcomeBehind
	case tree == dep.Tree:
		// The head may have moved without touching the folder: the stack stays in sync (D-13).
		next.Status = stackops.GitStatusInSync
		out = OutcomeInSync
	default:
		touch, err := s.git.CommitsTouching(ctx, mirror, head.SHA, dep.Commit, sg.Folder, BehindCap)
		if err != nil {
			return s.fail(ctx, prev, next, reasonOf(err), err)
		}
		next.Status, next.Behind = stackops.GitStatusBehind, max(touch.Count, 1)
		out = OutcomeBehind
	}
	if err := s.writeState(ctx, prev, next, systemActor); err != nil {
		return "", err
	}
	return out, nil
}

// fail records a check that could not be made: the state goes to error with the reason, keeping the last answer about
// the head and how far behind the stack was, since the last good deployed state keeps running (D-16). The time the
// error began and the run of failures are kept from one check to the next.
func (s *Service) fail(ctx context.Context, prev, next stackops.GitState, code ReasonCode, cause error) (Outcome, error) {
	next.Status, next.ReasonCode, next.ReasonDetail = stackops.GitStatusError, string(code), detailOf(code, cause)
	next.HeadCommit, next.HeadTree, next.Behind = prev.HeadCommit, prev.HeadTree, prev.Behind
	next.ConsecutiveFailures = 1
	next.ErrorSince = next.CheckedAt
	if prev.Status == stackops.GitStatusError {
		next.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		if prev.ErrorSince != nil {
			next.ErrorSince = prev.ErrorSince
		}
	}
	if err := s.writeState(ctx, prev, next, systemActor); err != nil {
		return "", err
	}
	return OutcomeError, nil
}

// sourceChanged says whether the settings a stack follows now differ from where its deployed revision came from.
func sourceChanged(sg stackops.StackGit, dep stackops.GitRevision) bool {
	d := dep.Source
	return sg.RepoID != d.RepoID || sg.RefKind != d.RefKind || sg.Ref != d.Ref || sg.Folder != d.Folder
}
