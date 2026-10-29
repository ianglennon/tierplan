// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Deployer starts the deploy of a stack's draft for a principal that is not on a request. The API's StackDeployer is
// the one implementation (through an adapter the server injects), so an automatic deploy goes through the very checks,
// operation lock, revision, job and audit that the Deploy button does. gitsync declares it because it must not import
// the API.
type Deployer interface {
	// DeployDraft deploys the draft at draftVersion for p. trigger is stackops.SyncPoll or stackops.SyncWebhook. A deploy
	// that did not start is a *DeployRefusal.
	DeployDraft(ctx context.Context, p *rbac.Principal, envID, stackID string, draftVersion int64, trigger string) (DeployStarted, error)
	// RevertedDrift says what deploying the draft at draftVersion would put back that was changed outside Bayplan,
	// as one line each ("web replicas 5 → 3"): the P8 "Reverts drift" computation of the review, with the values the
	// caller may not reveal withheld. It reads and changes nothing. An error means it could not be worked out.
	RevertedDrift(ctx context.Context, p *rbac.Principal, envID, stackID string, draftVersion int64) ([]string, error)
}

// DeployStarted is a deploy that has started: the job to follow and the revision it took from the draft.
type DeployStarted struct {
	JobID string
	Seq   int64
	// HostEscalating is true when the stack asks for host access that only an env Admin may deploy (bind mounts,
	// privileges, the host network...). The owner of the automation must then hold env Admin from now on.
	HostEscalating bool
}

// DeployRefusal is the answer to a deploy that did not start. Code is stable and says why; Retry says whether the same
// deploy may succeed later without anyone changing anything (a held operation lock, a full job slot).
type DeployRefusal struct {
	Code, Detail string
	Retry        bool
}

// Error implements error.
func (r *DeployRefusal) Error() string { return "deploy refused: " + r.Code + ": " + r.Detail }

// codeDraftChanged is the refusal of a deploy whose draft moved after the pull: somebody saved it in between.
const codeDraftChanged = "draft_changed"

// localEditsDetail is why an automatic deploy waits for a person when the draft holds changes nobody reviewed (D-29).
const localEditsDetail = "A draft with local changes is waiting for review."

// pendingSourceDetail is why an automatic deploy waits for a person when the draft holds a new source nobody reviewed (D-11).
const pendingSourceDetail = "A new source is waiting for review."

// SetDeployer gives the service the deployer that starts an automatic deploy. The deployer is built from the stack
// routes' dependencies, which hold this service's Deployed hook, so the two cannot be made in one expression: the
// server makes the service, then the deployer, then calls this once before anything runs. Until it is set, a stack
// that is behind is only recorded, as before.
func (s *Service) SetDeployer(d Deployer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deployer = d
}

func (s *Service) deployerNow() Deployer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deployer
}

// autoRuns says whether a stack's automatic deploys run on a check: they are on and nothing paused them. A pause for
// the owner's permission is not "running", but autoDeploy still looks at it, since the owner may have it back.
func autoRuns(sg stackops.StackGit) bool {
	return sg.AutoDeploy && sg.AutoDeployPausedReason == ""
}

// afterCheck is what follows a check that found a stack behind: a stack with auto-deploy on deploys the head. It runs
// on the outcome of every kind of check, including one that was answered from what is already stored, so a deploy that
// could not start (the environment was busy) is tried again at the next round.
func (s *Service) afterCheck(ctx context.Context, envID, stackID string, o CheckOptions, out Outcome) {
	if out != OutcomeBehind || s.access == nil || s.deployerNow() == nil {
		return
	}
	state, err := s.repo.GitState(ctx, stackID)
	if err != nil {
		s.log.Warn("could not read the git state for an automatic deploy", "stack", stackID, "error", err)
		return
	}
	// A note on the state (a changed source) is a person's to review, and a stack in error is not behind.
	if state.Status != stackops.GitStatusBehind || state.ReasonCode != "" || state.HeadCommit == "" {
		return
	}
	s.autoDeploy(ctx, envID, stackID, gitx.Commit{SHA: state.HeadCommit}, o.Trigger)
}

// autoDeploy deploys the head of a git stack that has auto-deploy on, on the authority its owner holds now (D-20).
//
// The owner's rights are rechecked first (access.Service.RunAs): an owner who no longer holds stacks.deploy, or env
// Admin for a stack that used host-escalating features, pauses the automation and nothing is deployed. A draft that
// holds changes nobody reviewed blocks it too (D-29). Then the head is pulled into the draft and the draft is deployed
// through the Deployer: the same policy on the rendered spec, operation lock, revision, job, console and audit as a
// person's deploy, judged as the owner. Anything that would stop a manual deploy leaves the stack in error
// auto_deploy_blocked with the commit pending, so it is not tried again until the head changes or a person acts; a held
// lock or a full job slot is not an error and the next round tries again.
//
// trigger is what started the check: a webhook acts as system:webhook, every other check as system:gitsync.
func (s *Service) autoDeploy(ctx context.Context, envID, stackID string, head gitx.Commit, trigger string) {
	deployer := s.deployerNow()
	if s.access == nil || deployer == nil || head.SHA == "" {
		return
	}
	sg, err := s.repo.StackGit(ctx, stackID)
	if err != nil {
		s.log.Warn("could not read the git settings for an automatic deploy", "stack", stackID, "error", err)
		return
	}
	// A pause for a roll back stays until a person turns auto-deploy on again; a pause for the owner is looked at below.
	if !sg.AutoDeploy || (sg.AutoDeployPausedReason != "" && sg.AutoDeployPausedReason != stackops.PausedOwner) {
		return
	}
	state, err := s.repo.GitState(ctx, stackID)
	if err != nil {
		s.log.Warn("could not read the git state for an automatic deploy", "stack", stackID, "error", err)
		return
	}
	if state.PendingCommit == head.SHA {
		return // held back already; a person acts or the head moves
	}
	st, err := s.repo.Stack(ctx, envID, stackID)
	if err != nil {
		s.log.Warn("could not read the stack for an automatic deploy", "stack", stackID, "error", err)
		return
	}

	systemID, syncTrigger := "system:gitsync", stackops.SyncPoll
	if trigger == TriggerWebhook {
		systemID, syncTrigger = "system:webhook", stackops.SyncWebhook
	}
	// The owner's rights are read now, in their own transaction, and a pause is audited there (AUTH-10, D-14).
	p, owned, err := s.access.RunAs(ctx, KindGitAutoDeploy, stackID, systemID)
	if err != nil {
		if errors.Is(err, access.ErrOwnerLacksPermission) || errors.Is(err, access.ErrNotFound) {
			s.pauseForOwner(ctx, st, sg)
			return
		}
		s.log.Warn("could not recheck the owner of an automatic deploy", "stack", stackID, "error", err)
		return
	}
	if sg.AutoDeployPausedReason == stackops.PausedOwner {
		// The owner holds the permission again (or the automation was handed to someone who does).
		if err := s.repo.SetAutoDeploy(ctx, stackID, true, "", 0, nil); err != nil {
			s.log.Warn("could not resume the automatic deploys of a stack", "stack", stackID, "error", err)
			return
		}
	}

	draft, err := s.repo.Draft(ctx, stackID)
	if err != nil {
		s.log.Warn("could not read the draft for an automatic deploy", "stack", stackID, "error", err)
		return
	}
	// A draft that holds another source than the stack follows is waiting for review (D-11): pulling would drop it, and
	// deploying it is a person's decision.
	if PendingSource(sg, draft) {
		s.holdBack(ctx, st, head.SHA, pendingSourceDetail)
		return
	}
	// Bayplan never deploys unreviewed local edits (D-29).
	edited, err := s.hasLocalEdits(ctx, stackID, draft)
	if err != nil {
		s.log.Warn("could not compare the draft with the deployed revision", "stack", stackID, "error", err)
		return
	}
	if edited {
		s.holdBack(ctx, st, head.SHA, localEditsDetail)
		return
	}

	pulled, err := s.Pull(ctx, envID, stackID, p.Name, draft.Version)
	if err != nil {
		s.pullFailed(ctx, st, head.SHA, err)
		return
	}
	// The pull took the newest head; if the ref moved on since the check, that is the commit this deploy is about.
	head = pulled.Commit

	// What the deploy sets back to the repository's word is worked out first, from what runs now, so it can be recorded
	// with the revision the deploy takes. Auto-deploy goes ahead whatever it finds (D-23); a drift that can't be worked
	// out is said so in the audit entry and never stops the deploy.
	reverted, revertsErr := deployer.RevertedDrift(ctx, p, envID, stackID, pulled.DraftVersion)
	if revertsErr != nil {
		s.log.Warn("could not work out what an automatic deploy puts back", "stack", stackID, "error", revertsErr)
	}

	started, err := deployer.DeployDraft(ctx, p, envID, stackID, pulled.DraftVersion, syncTrigger)
	if err != nil {
		var ref *DeployRefusal
		if !errors.As(err, &ref) {
			s.log.Warn("an automatic deploy could not start", "stack", stackID, "error", err)
			return
		}
		if ref.Retry || ref.Code == codeDraftChanged {
			// A held operation lock, a full job slot or a person saving the draft: not an error, the next round tries.
			s.log.Debug("an automatic deploy waits for the next round", "stack", stackID, "code", ref.Code)
			return
		}
		s.holdBack(ctx, st, head.SHA, refusalDetail(ref))
		return
	}

	s.recordAutoDeployed(ctx, st, p, owned, head.SHA, syncTrigger, reverted, revertsErr != nil)
	if len(reverted) > 0 {
		s.recordReverted(ctx, st, started.Seq, reverted)
	}
	if started.HostEscalating {
		// The next recheck of the owner demands env Admin, so an Operator who was handed this does not run it.
		if err := s.access.MarkNeedsEnvAdmin(ctx, KindGitAutoDeploy, stackID); err != nil {
			s.log.Warn("could not record that an automatic deploy used host access", "stack", stackID, "error", err)
		}
	}
}

// recordAutoDeployed audits a deploy Bayplan started by itself: the actor is the system principal that ran it, the
// details name the stack, the commit, what started it and the owner it acted for (T-09-77), and the lines it put back to
// what the repository says (T-09-99): the audit entry lists them, or says they could not be worked out.
func (s *Service) recordAutoDeployed(ctx context.Context, st stackops.Stack, p *rbac.Principal, owned access.OwnedResource, commit, trigger string, reverted []string, revertsUnknown bool) {
	if s.audit == nil {
		return
	}
	details := map[string]string{"stack": st.Name, "commit": commit, "trigger": trigger, "owner": owned.OwnerID}
	switch {
	case revertsUnknown:
		details["reverted"] = "could not be worked out"
	case len(reverted) > 0:
		details["revertedCount"] = strconv.Itoa(len(reverted))
		details["reverted"] = strings.Join(reverted, "; ")
	}
	err := s.audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionGitAutoDeployed, TargetType: "stack", TargetID: st.ID, EnvID: st.EnvID,
		Result: audit.ResultSuccess, Details: details,
	})
	if err != nil {
		s.log.Error("could not audit an automatic deploy", "stack", st.ID, "error", err)
	}
}

// recordReverted keeps what an automatic deploy put back with the revision it made, and notes the event for the bell
// (D-23). The event's detail is the revision's number, which the bell reads the lines from and which tells a later
// deploy (Deployed) that the notice is about an older one. A failure is logged and never undoes the deploy.
func (s *Service) recordReverted(ctx context.Context, st stackops.Stack, seq int64, lines []string) {
	rev, err := s.repo.Revision(ctx, st.ID, seq)
	if err != nil {
		s.log.Warn("could not find the revision an automatic deploy made", "stack", st.ID, "revision", seq, "error", err)
		return
	}
	if err := s.repo.SetRevertedDrift(ctx, rev.ID, lines); err != nil {
		s.log.Warn("could not record what an automatic deploy put back", "stack", st.ID, "revision", seq, "error", err)
		return
	}
	s.setEvent(ctx, st.ID, stackops.GitEventReverted, strconv.FormatInt(seq, 10))
}

// pauseForOwner records that a stack's automatic deploys are paused because their owner no longer holds what they need.
// RunAs has already paused the owned row and audited it. Only the first time writes the stack's pause and the event: a
// stack that is paused already is not told again.
func (s *Service) pauseForOwner(ctx context.Context, st stackops.Stack, sg stackops.StackGit) {
	if sg.AutoDeployPausedReason == stackops.PausedOwner {
		return
	}
	if err := s.repo.SetAutoDeploy(ctx, st.ID, true, stackops.PausedOwner, 0, nil); err != nil {
		s.log.Warn("could not pause the automatic deploys of a stack", "stack", st.ID, "error", err)
		return
	}
	s.setEvent(ctx, st.ID, stackops.GitEventPaused, "owner lacks permission")
}

// holdBack leaves a commit for a person: nothing is deployed, the stack goes to error auto_deploy_blocked with the first
// problem, the commit is recorded as pending so it is not tried again, and the state notes the event for the bell
// (D-19).
func (s *Service) holdBack(ctx context.Context, st stackops.Stack, commit, detail string) {
	prev, err := s.repo.GitState(ctx, st.ID)
	if err != nil {
		s.log.Warn("could not read the git state to hold back a commit", "stack", st.ID, "error", err)
		return
	}
	detail = clipProblem(detail)
	now := s.now().UTC()
	next := prev
	next.CheckedAt = &now
	next.Status, next.ReasonCode, next.ReasonDetail = stackops.GitStatusError, string(ReasonAutoDeployBlocked), detail
	next.PendingCommit = commit
	if err := s.writeState(ctx, st, prev, next, systemActor); err != nil {
		s.log.Warn("could not store a held back commit", "stack", st.ID, "error", err)
		return
	}
	if prev.PendingCommit != commit {
		s.setEvent(ctx, st.ID, stackops.GitEventSkipped, detail)
	}
}

// pullFailed is the state after the pull of an automatic deploy failed. A draft somebody saved meanwhile is tried again
// next round; a commit whose files break the limits is held back; a repository that could not be read is a fetch
// failure like a check's.
func (s *Service) pullFailed(ctx context.Context, st stackops.Stack, commit string, err error) {
	var conflict *stackops.DraftConflictError
	if errors.As(err, &conflict) {
		return
	}
	var pe *PathError
	var fe *stacks.FileError
	var le *gitx.LimitError
	if errors.As(err, &pe) || errors.As(err, &fe) || errors.As(err, &le) {
		s.holdBack(ctx, st, commit, err.Error())
		return
	}
	code := reasonOf(err)
	prev, serr := s.repo.GitState(ctx, st.ID)
	if serr != nil {
		s.log.Warn("could not read the git state after a failed pull", "stack", st.ID, "error", serr)
		return
	}
	now := s.now().UTC()
	next := prev
	next.CheckedAt = &now
	next.Status, next.ReasonCode, next.ReasonDetail = stackops.GitStatusError, string(code), detailOf(code, err)
	if werr := s.writeState(ctx, st, prev, next, systemActor); werr != nil {
		s.log.Warn("could not store a failed pull", "stack", st.ID, "error", werr)
	}
}

// refusalDetail is the sentence of a refusal that holds a commit back: what the deploy said, else a fixed one.
func refusalDetail(r *DeployRefusal) string {
	if r.Detail != "" {
		return r.Detail
	}
	return "The deploy was refused (" + r.Code + ")."
}

// setEvent notes an event on the stack's state; a failure is logged and never undoes what happened.
func (s *Service) setEvent(ctx context.Context, stackID, kind, detail string) {
	if err := s.repo.SetGitEvent(ctx, stackID, kind, clipProblem(detail), s.now().UTC()); err != nil {
		s.log.Warn("could not note an automatic deploy event", "stack", stackID, "event", kind, "error", err)
	}
}

// hasLocalEdits says whether the draft's variables or overlay files differ from those of the newest good revision: edits
// somebody saved and nobody reviewed and deployed (D-29). The files of the repository are not compared, since a pull
// replaces them. A stack with no good revision has nothing deployed to differ from.
func (s *Service) hasLocalEdits(ctx context.Context, stackID string, draft stackops.Snapshot) (bool, error) {
	good, ok, err := s.repo.LatestGoodRevision(ctx, stackID)
	if err != nil || !ok {
		return false, err
	}
	return !sameVars(draft.Copy.Vars, good.Copy.Vars) || !sameOverlays(draft.Copy.Files, good.Copy.Files), nil
}

func sameVars(a, b []stacks.Var) bool {
	sort := func(vs []stacks.Var) []stacks.Var {
		out := slices.Clone(vs)
		slices.SortFunc(out, func(x, y stacks.Var) int { return strings.Compare(x.Name, y.Name) })
		return out
	}
	return slices.Equal(sort(a), sort(b))
}

// sameOverlays compares the files Bayplan holds for paths the repository does not have, by path and content.
func sameOverlays(a, b []stacks.File) bool {
	pick := func(files []stacks.File) map[string]stacks.File {
		out := map[string]stacks.File{}
		for _, f := range files {
			if f.Origin == stacks.OriginOverlay {
				out[f.Path] = f
			}
		}
		return out
	}
	x, y := pick(a), pick(b)
	if len(x) != len(y) {
		return false
	}
	for path, f := range x {
		g, ok := y[path]
		if !ok || !bytes.Equal(f.Content, g.Content) || f.Secret != g.Secret {
			return false
		}
	}
	return true
}
