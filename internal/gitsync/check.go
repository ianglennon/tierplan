// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
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
// A check only records. It deploys nothing and makes no draft and no revision (GIT-03), with one exception: a stack whose
// owner turned auto-deploy on is deployed after the check, by autoDeploy, through the same checks as a person's deploy
// (GIT-07). A failure to reach the
// repository is an Outcome of OutcomeError with the reason stored, not an error; an error is returned for a stack that
// is not a git stack of this environment, or for a failure of Bayplan's own storage.
//
// Callers that ask for the same stack while a check runs share it. The check is not tied to the first caller's context
// (a caller that gives up leaves it running for the others), and a caller that gives up returns its context's error.
func (s *Service) Check(ctx context.Context, envID, stackID, trigger string) (Outcome, error) {
	return s.CheckWith(ctx, envID, stackID, CheckOptions{Trigger: trigger})
}

// CheckOptions are the choices of CheckWith.
type CheckOptions struct {
	// Trigger is what started the check (TriggerManual, TriggerPoll, TriggerWebhook).
	Trigger string
	// RemoteHead is the commit the repository advertises for the ref the stack follows, when the caller already asked
	// for it (the poller does, once for all the stacks of one repository). A stack that was last seen at that very
	// commit, and is not in error, is not fetched again: only the time of the check changes. Empty means look.
	RemoteHead string
}

// CheckWith is Check with options. Callers that join a check already in flight share it, with the options of the
// caller that started it.
func (s *Service) CheckWith(ctx context.Context, envID, stackID string, o CheckOptions) (Outcome, error) {
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
		go s.runCheck(context.WithoutCancel(ctx), envID, stackID, o, c)
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

func (s *Service) runCheck(ctx context.Context, envID, stackID string, o CheckOptions, c *checkCall) {
	ctx, cancel := context.WithTimeout(ctx, checkDeadline)
	defer cancel()
	res, err := s.check(ctx, envID, stackID, o)
	if err == nil {
		// A stack with auto-deploy on that was found behind is deployed before the check reads as over, so a caller that
		// waits on the check sees the deploy started.
		s.afterCheck(ctx, envID, stackID, o, res)
	}
	s.mu.Lock()
	delete(s.checking, stackID)
	c.res, c.err = res, err
	s.mu.Unlock()
	close(c.done)
}

func (s *Service) check(ctx context.Context, envID, stackID string, o CheckOptions) (Outcome, error) {
	st, err := s.repo.Stack(ctx, envID, stackID)
	if err != nil {
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
	now := s.now().UTC()
	next := prev
	next.CheckedAt = &now
	next.DeployedCommit, next.DeployedTree = "", ""
	if deployed {
		next.DeployedCommit, next.DeployedTree = dep.Commit, dep.Tree
	}
	if gitx.RefKind(sg.RefKind) == gitx.RefCommit {
		return s.checkFixed(ctx, st, sg, prev, next, dep, deployed)
	}
	if out, ok, err := s.checkCheaply(ctx, o, sg, prev, next, dep, deployed); ok || err != nil {
		return out, err
	}
	repo, err := s.conns.GetRepo(ctx, sg.RepoID)
	if err != nil {
		return "", fmt.Errorf("read the repository: %w", err)
	}

	rem, err := s.conns.Remote(ctx, repo)
	if err != nil {
		s.log.Error("could not prepare a git remote for a sync check", "stack", stackID, "error", err)
		return s.fail(ctx, st, prev, next, ReasonFetchFailed, err)
	}
	mirror := gitx.MirrorID(repo.ID)
	head, err := s.git.FetchRef(ctx, mirror, rem, gitx.RefKind(sg.RefKind), sg.Ref)
	s.recordUse(ctx, repo, err)
	if err != nil {
		return s.fail(ctx, st, prev, next, reasonOf(err), err)
	}
	tree, err := s.git.FolderTree(ctx, mirror, head.SHA, sg.Folder)
	if err != nil {
		return s.fail(ctx, st, prev, next, reasonOf(err), err)
	}
	next.HeadCommit, next.HeadTree = head.SHA, tree
	next.Status, next.Behind = stackops.GitStatusUnknown, 0
	next.ReasonCode, next.ReasonDetail, next.ErrorSince, next.ConsecutiveFailures = "", "", nil, 0
	// A commit an automatic deploy held back stays pending while it is the head. Another head, or a person pressing
	// Check now, lets the next automatic deploy look at it afresh (D-19).
	held := prev.PendingCommit
	if o.Trigger == TriggerManual || held != head.SHA {
		held = ""
	}
	next.PendingCommit = held

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
		next.PendingCommit = ""
		out = OutcomeInSync
	default:
		touch, err := s.git.CommitsTouching(ctx, mirror, head.SHA, dep.Commit, sg.Folder, BehindCap)
		if err != nil {
			return s.fail(ctx, st, prev, next, reasonOf(err), err)
		}
		// A base that is not an ancestor (a force-push, a tag moved elsewhere) is behind by at least the commit that
		// replaced it. A walk that stopped at the cap reads as the cap: the count is then "this many or more", and
		// behind >= BehindCap is how the screens know to say so.
		next.Status, next.Behind = stackops.GitStatusBehind, max(touch.Count, 1)
		if touch.Capped {
			next.Behind = BehindCap
		}
		out = OutcomeBehind
		// The head changes the folder, so its files must pass the checks before anyone is asked to deploy them.
		problem, err := s.headProblem(ctx, st, sg, head)
		if err != nil {
			return s.failWith(ctx, st, prev, withHead(next, head.SHA, tree), reasonOf(err), detailOf(reasonOf(err), err))
		}
		if problem != "" && autoRuns(sg) {
			// The stack deploys by itself, so a commit that fails the checks is a blocker: it is held back and left for a
			// person, who hears of it (D-19).
			blocked := withHead(next, head.SHA, tree)
			blocked.PendingCommit = head.SHA
			out, err := s.failWith(ctx, st, prev, blocked, ReasonAutoDeployBlocked, problem)
			if err == nil && prev.PendingCommit != head.SHA {
				s.setEvent(ctx, st.ID, stackops.GitEventSkipped, problem)
			}
			return out, err
		}
		if problem != "" {
			return s.failWith(ctx, st, prev, withHead(next, head.SHA, tree), ReasonChecksFailed, problem)
		}
		if autoRuns(sg) && held == head.SHA {
			// Held back before and nobody has acted: it keeps waiting, and keeps saying why.
			code, detail := heldReason(prev)
			return s.failWith(ctx, st, prev, withHead(next, head.SHA, tree), code, detail)
		}
	}
	if err := s.writeState(ctx, st, prev, next, systemActor); err != nil {
		return "", err
	}
	return out, nil
}

// checkCheaply is the check of a stack whose repository was already asked and still advertises the commit the stack
// was last seen at: nothing can have changed in the repository, so nothing is fetched and only the time of the check
// is written (RESEARCH Pitfall 4). It applies only when the stored answer is still the answer to the question: the
// stack is not in error (an error is looked at again for real), the deployed revision and the stack's source are what
// the answer was made with, and the answer carries no note. ok is false when the check has to be made in full.
func (s *Service) checkCheaply(ctx context.Context, o CheckOptions, sg stackops.StackGit, prev, next stackops.GitState, dep stackops.GitRevision, deployed bool) (Outcome, bool, error) {
	if o.RemoteHead == "" || prev.CheckedAt == nil || prev.HeadCommit != o.RemoteHead || prev.Status == stackops.GitStatusError || prev.ReasonCode != "" {
		return "", false, nil
	}
	if next.DeployedCommit != prev.DeployedCommit || next.DeployedTree != prev.DeployedTree || (deployed && sourceChanged(sg, dep)) {
		return "", false, nil
	}
	err := s.repo.TouchGitChecked(ctx, prev.StackID, *next.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	switch prev.Status {
	case stackops.GitStatusInSync:
		return OutcomeInSync, true, nil
	case stackops.GitStatusBehind:
		return OutcomeBehind, true, nil
	}
	return OutcomeSkipped, true, nil
}

// fail records a check that could not be made: the state goes to error with the reason, keeping the last answer about
// the head and how far behind the stack was, since the last good deployed state keeps running (D-16). The time the
// error began and the run of failures are worked out by writeState.
func (s *Service) fail(ctx context.Context, st stackops.Stack, prev, next stackops.GitState, code ReasonCode, cause error) (Outcome, error) {
	next.HeadCommit, next.HeadTree, next.Behind = prev.HeadCommit, prev.HeadTree, prev.Behind
	return s.failWith(ctx, st, prev, next, code, detailOf(code, cause))
}

// failWith is fail for a check that did learn the head: next keeps what it holds and takes the error and its sentence.
func (s *Service) failWith(ctx context.Context, st stackops.Stack, prev, next stackops.GitState, code ReasonCode, detail string) (Outcome, error) {
	next.Status, next.ReasonCode, next.ReasonDetail = stackops.GitStatusError, string(code), detail
	if err := s.writeState(ctx, st, prev, next, systemActor); err != nil {
		return "", err
	}
	return OutcomeError, nil
}

// heldReason is the reason a commit that was held back keeps showing: the one the state already holds when it is an
// automatic deploy's, else the generic one for a held back commit.
func heldReason(prev stackops.GitState) (ReasonCode, string) {
	switch code := ReasonCode(prev.ReasonCode); code {
	case ReasonAutoDeployBlocked, ReasonAutoDeployFailed:
		return code, prev.ReasonDetail
	}
	detail := prev.EventDetail
	if detail == "" {
		detail = detailOf(ReasonAutoDeployBlocked, nil)
	}
	return ReasonAutoDeployBlocked, detail
}

func withHead(g stackops.GitState, commit, tree string) stackops.GitState {
	g.HeadCommit, g.HeadTree = commit, tree
	return g
}

// checkFixed is the check of a stack that follows one commit. That commit can't move, so there is nothing to fetch and
// nothing to be behind (D-12): the stack is in sync with what it was deployed from. A commit other than the deployed
// one is a change of source, which a person reviews (RESEARCH Pitfall 8).
func (s *Service) checkFixed(ctx context.Context, st stackops.Stack, sg stackops.StackGit, prev, next stackops.GitState, dep stackops.GitRevision, deployed bool) (Outcome, error) {
	next.HeadCommit, next.HeadTree, next.Behind = sg.Ref, "", 0
	next.Status = stackops.GitStatusUnknown
	next.ReasonCode, next.ReasonDetail = "", ""
	out := OutcomeSkipped
	switch {
	case !deployed:
	case sourceChanged(sg, dep):
		next.Status, next.Behind = stackops.GitStatusBehind, 1
		next.ReasonCode, next.ReasonDetail = string(ReasonSourceChanged), detailOf(ReasonSourceChanged, nil)
		out = OutcomeBehind
	default:
		next.HeadCommit, next.HeadTree = dep.Commit, dep.Tree
		next.Status = stackops.GitStatusInSync
		out = OutcomeInSync
	}
	if err := s.writeState(ctx, st, prev, next, systemActor); err != nil {
		return "", err
	}
	return out, nil
}

// maxProblemChars is the longest problem sentence kept as the detail of a failed check.
const maxProblemChars = 300

// headProblem reads the stack's files at the head with the stack's variables and overlay files and runs the checks a
// deploy would: it answers the first blocking problem, or "" when the head passes. A file the compose files need that
// neither the repository nor an overlay holds counts as one. An error is a failure to read the repository, not a
// failed check.
func (s *Service) headProblem(ctx context.Context, st stackops.Stack, sg stackops.StackGit, head gitx.Commit) (string, error) {
	draft, err := s.repo.Draft(ctx, st.ID)
	if err != nil {
		return "", err
	}
	var overlays []Overlay
	for _, f := range draft.Copy.Files {
		if f.Origin == stacks.OriginOverlay {
			overlays = append(overlays, Overlay{Path: f.Path, Content: f.Content})
		}
	}
	imp, err := s.read(ctx, ImportRequest{
		EnvID: st.EnvID, RepoID: sg.RepoID, RefKind: gitx.RefCommit, Ref: head.SHA, Folder: sg.Folder, ComposeFiles: sg.ComposeFiles,
		Name: st.Name, Target: st.Target, Vars: draft.Copy.Vars, Overlays: overlays,
	}, head.SHA)
	if err != nil {
		var pe *PathError
		var fe *stacks.FileError
		var le *gitx.LimitError
		if errors.As(err, &pe) || errors.As(err, &fe) || errors.As(err, &le) {
			return clipProblem(err.Error()), nil
		}
		return "", err
	}
	for _, p := range stacks.Problems(imp.Problems).Sorted() {
		if p.Severity != stacks.SeverityError {
			continue
		}
		if p.File != "" {
			return clipProblem(p.File + ": " + p.Message), nil
		}
		return clipProblem(p.Message), nil
	}
	if len(imp.Missing) > 0 {
		return clipProblem(imp.Missing[0] + " isn't in the repository."), nil
	}
	return "", nil
}

func clipProblem(s string) string {
	if r := []rune(s); len(r) > maxProblemChars {
		return string(r[:maxProblemChars])
	}
	return s
}

// sourceChanged says whether the settings a stack follows now differ from where its deployed revision came from.
func sourceChanged(sg stackops.StackGit, dep stackops.GitRevision) bool {
	d := dep.Source
	return sg.RepoID != d.RepoID || sg.RefKind != d.RefKind || sg.Ref != d.Ref || sg.Folder != d.Folder
}
