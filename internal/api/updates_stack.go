// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/updates"
)

// Updating the images of a stack TierPlan manages (plan 12-18, IMG-06, D-06, D-08, D-09, D-15). An update is a revision of the
// stack of its own: the content of the newest good revision with only the chosen services' pins changed, deployed through the
// stack's own engine in the stack's own job. The draft is never read, so an unreviewed edit cannot ride along, and nothing is
// written to the compose files or the variables.

const problemAdoptFirst = "adopt_first"

// adoptFirstSentence is the 409 for an update of a stack TierPlan found but does not manage.
const adoptFirstSentence = "TierPlan doesn't manage this stack yet. Adopt it first so an update has a revision to roll back to."

// updateStackNeedsDeploy is the 403 for someone who may update services but not deploy stacks.
const updateStackNeedsDeploy = "Updating a stack's images deploys the stack, which needs the permission to deploy stacks here."

// stackUpdateRun is one update of a stack asked for: which stack, which of its services and what the confirm showed.
type stackUpdateRun struct {
	// Stack is the stack's name when the request names a stack, empty for a service of a stack (then StackID is known).
	Stack, StackID string
	Services       []string
}

// runStackUpdate starts the update of a stack, or of one service of one, for the route: it checks the permission to deploy
// stacks, finds the stack TierPlan manages, and hands over to updateStackImages. A stack that is only discovered is 409
// adopt_first.
func runStackUpdate(ctx context.Context, d UpdatesDeps, p *rbac.Principal, env *httpx.Env, in *UpdateRunInput, run stackUpdateRun) (UpdateRunBody, error) {
	if d.Applier == nil || d.Stacks.Store == nil || d.Stacks.Locks == nil {
		return UpdateRunBody{}, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
	}
	if d.Checker == nil {
		return UpdateRunBody{}, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
	}
	dec := d.Checker.Check(ctx, p, rbac.Require(rbac.PermStacksDeploy), env.ID)
	if dec.Err != nil {
		return UpdateRunBody{}, snapshotProblem(d.log(), env.ID, dec.Err)
	}
	if !dec.Allowed {
		return UpdateRunBody{}, httpx.Problem(http.StatusForbidden, "", updateStackNeedsDeploy)
	}
	repo := d.Stacks.repo()
	var st stackops.Stack
	var err error
	if run.StackID != "" {
		st, err = repo.Stack(ctx, env.ID, run.StackID)
	} else {
		st, err = managedStackByName(ctx, repo, env.ID, run.Stack)
	}
	switch {
	case errors.Is(err, stackops.ErrStackNotFound):
		return UpdateRunBody{}, unmanagedStackProblem(ctx, d, env, run.Stack)
	case err != nil:
		return UpdateRunBody{}, stackLookupProblem(d.Stacks, env.ID, err)
	}
	out, err := updateStackImages(ctx, d.Stacks, d.Applier, p, env, st, run.Services, in.Body.Expect, p.AuditActor(), "", in.UserAgent)
	if err != nil {
		return UpdateRunBody{}, plainProblem(err)
	}
	return UpdateRunBody{JobID: out.Body.JobID, StackID: st.ID}, nil
}

// unmanagedStackProblem is the answer to an update of a stack TierPlan has no record of: found on the engine, it is adopted first;
// else there is no such stack.
func unmanagedStackProblem(ctx context.Context, d UpdatesDeps, env *httpx.Env, name string) error {
	_, err := d.Applier.PlanStack(ctx, updates.StackRequest{EnvID: env.ID, Stack: name})
	switch {
	case err == nil || errors.Is(err, updates.ErrNothingToUpdate):
		return httpx.Problem(http.StatusConflict, problemAdoptFirst, adoptFirstSentence)
	case errors.Is(err, updates.ErrWorkloadGone):
		return workloadGone("stack")
	}
	return updateRunProblem(d, env, err)
}

// managedStackByName finds the stack of that name among those TierPlan manages in the environment.
func managedStackByName(ctx context.Context, repo stackops.Repo, envID, name string) (stackops.Stack, error) {
	all, err := repo.Stacks(ctx, envID)
	if err != nil {
		return stackops.Stack{}, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return stackops.Stack{}, stackops.ErrStackNotFound
}

// updateStackImages updates services of a stack TierPlan manages to the digests the registry has now, as a revision of the
// stack. services limits it to those services (by the name the stack's pins use); none means every service with an update
// available, leaving out the skipped and ignored ones. expect is what the confirm showed, refused with 409 update_moved when
// the registry says otherwise. The digests deployed are the ones the server read, never one from the request.
//
// It is modelled on rollbackStack: the stack's operation lock is taken before anything is stored, the revision is a copy of the
// newest good one (Repo.NewUpdateRevision, so it reads as an update and never as a roll back), and the deploy is the stack's own
// job through launchStackDeploy, in the stack's console. The images are the revision's own, but for the chosen services, whose
// pins carry the new digest and which the deploy checks like a reviewed deploy does (ReviewedPins).
func updateStackImages(ctx context.Context, d StacksDeps, ap *updates.Applier, p *rbac.Principal, env *httpx.Env, st stackops.Stack, services []string, expect map[string]string, actor audit.Actor, bulk, userAgent string) (*StackDeployOutput, error) {
	if d.Jobs == nil || ap == nil || ap.Runs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
	}
	repo := d.repo()
	if err := d.ops().CheckTarget(ctx, env.ID, st); err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	asUpdates := UpdatesDeps{Log: d.Log}
	// A held lock is answered before the registry is asked, as a single update does.
	if h, held := d.Locks.Held(env.ID); held {
		return nil, operationLocked(env.Name, h)
	}
	plan, err := ap.PlanStack(ctx, updates.StackRequest{EnvID: env.ID, Stack: st.Name, Services: services, Expect: expect, Recheck: true})
	if err != nil {
		return nil, updateRunProblem(asUpdates, env, err)
	}

	good, ok, err := repo.LatestGoodRevision(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "read the running revision", env.ID, err)
	}
	if !ok {
		return nil, httpx.Problem(http.StatusConflict, problemNoDeployedRevision, noDeployedRevision)
	}
	goodPins, err := repo.RevisionPins(ctx, good.ID)
	if err != nil {
		return nil, stackInternal(d, "read the pins", env.ID, err)
	}
	// The pins are the newest good revision's, but for the chosen services: the repository and tag it was deployed with and
	// the digest the registry gave. A pin that is not of the service's own repository is refused by the deploy (T-12-50).
	pins := maps.Clone(goodPins)
	update := make(map[string]bool, len(plan.Targets))
	for _, t := range plan.Targets {
		old := pins[t.Key]
		if old == "" {
			return nil, pinsProblem(d, env.ID, &stackops.PinsInvalidError{Service: t.Key, Reason: "missing"})
		}
		base, _, _ := strings.Cut(old, "@")
		pins[t.Key] = base + "@" + t.To
		update[t.Key] = true
	}

	standalone := st.Target == stacks.TargetStandalone
	masker := stacks.NewMasker(good.Copy.Vars, good.Copy.Files)
	var prep stackops.RollbackPlan
	node := ""
	if standalone {
		if prep, err = d.ops().PrepareRollbackStandalone(ctx, st, good, goodPins); err != nil {
			return nil, rollbackPrepareProblem(err, masker)
		}
		if node, err = d.ops().StandaloneNode(ctx, env.ID); err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		if err = d.ops().ValidateStandalonePins(ctx, env.ID, node, st, prep.Loaded.Project, repo, update, pins); err != nil {
			return nil, pinsProblem(d, env.ID, err)
		}
	} else {
		if prep, err = d.ops().PrepareRollback(ctx, st, good, goodPins); err != nil {
			return nil, rollbackPrepareProblem(err, masker)
		}
		if err = d.ops().ValidatePins(ctx, env.ID, st, prep.Plan.Services, repo, update, pins); err != nil {
			return nil, pinsProblem(d, env.ID, err)
		}
	}
	// What the stack needs to deploy is judged for the caller, as a deploy is (T-12-51): an Operator cannot update a stack
	// only an Admin may deploy.
	caller := stackops.Caller{Principal: p, Actor: p.Name}
	pol, err := d.ops().Policy(ctx, caller, env.ID, good.Copy, prep.Plan)
	if err != nil {
		return nil, stackInternal(d, "policy", env.ID, err)
	}
	if pol.Refused {
		return nil, policyProblem(pol)
	}
	foreign, err := d.ops().ForeignStackOf(ctx, env.ID, st)
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	if foreign {
		return nil, nameClashProblem(st.Name)
	}

	release, held, taken := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackID: st.ID, StackName: st.Name, Kind: deployModeUpdate, By: p.Name, Started: time.Now().UTC(),
	})
	if !taken {
		return nil, operationLocked(env.Name, held)
	}
	jobOwnsLock := false
	defer func() {
		if !jobOwnsLock {
			release()
		}
	}()
	// The content comes from the newest good revision. A deploy that ended while this request was judged took the lock off
	// before the job read as finished, so the newest good revision is read again now, holding the lock: the answer no other
	// operation can change.
	if now, found, gerr := repo.GoodRevisionInfo(ctx, st.ID); gerr != nil {
		return nil, stackInternal(d, "read the running revision", env.ID, gerr)
	} else if !found || now.Seq != good.Seq {
		return nil, deployedChanged(st.Name, now.Seq)
	}

	rev, err := repo.NewUpdateRevision(ctx, good, revisionCreator(ctx, repo, p))
	if err != nil {
		return nil, stackInternal(d, "revision", env.ID, err)
	}
	rendering := revisionRendering{repo: repo, loaded: prep.Loaded, masker: masker}
	if err := rendering.store(ctx, rev.ID, stackops.LivePins(pins)); err != nil {
		d.Log.Warn("could not store the rendering of a revision", "stack", st.ID, "revision", rev.Seq, "error", err)
	}
	// The platforms the check read are what the deploy sends, so a revision that predates the platform record needs no
	// registry call of its own.
	for _, t := range plan.Targets {
		d.Platforms.Remember(env.ID, t.Repo, t.To, t.Platforms)
	}

	runID, err := uuid.NewV7()
	if err != nil {
		_ = repo.SetOutcome(context.WithoutCancel(ctx), rev.ID, stackops.OutcomeFailed, "", "")
		return nil, stackInternal(d, "new run id", env.ID, err)
	}
	run := updates.Run{
		ID: runID.String(), EnvID: env.ID, BulkID: bulk, Kind: "stack", Workload: st.Name, WorkloadID: st.ID, StackID: st.ID,
		State: updates.RunRunning, Actor: updateActorKey(actor), RevisionSeq: rev.Seq, StartedAt: time.Now().UTC(),
	}
	for _, t := range plan.Targets {
		run.Items = append(run.Items, updates.RunItem{Service: t.Key, Image: t.Image, FromDigest: t.From, ToDigest: t.To})
	}

	extra := deployOpts{}.auditExtras(p)
	l := launch{
		p: p, env: env, st: st, rev: rev, mode: deployModeUpdate, kind: stackops.DeployKindDeploy, userAgent: userAgent,
		rendering: rendering, release: release, owned: &jobOwnsLock,
	}
	// The stack's own record of the end of a deploy runs first, so the revision's outcome is stored; then the run, the audit
	// rows and the draft. All of it is before the lock is freed and the job reads as finished.
	l.record = func(rctx context.Context, deploy stackops.DeployRow, res stackops.DeployResult) {
		recordDeployEnd(rctx, d, actor, userAgent, st, rev, deploy, deployModeUpdate, res, rendering, extra)
		endStackUpdate(rctx, d, ap, actor, userAgent, st, good, rev, run, plan, res)
	}
	// The run is on record before the engine is touched: the job writes it first, with its own ID.
	begin := func(jctx context.Context, r *jobs.Reporter) bool {
		run.JobID = r.JobID()
		if err := ap.Runs.Insert(context.WithoutCancel(jctx), run); err != nil {
			d.Log.Error("an image update of a stack could not be recorded", "stack", st.ID, "error", err)
			r.Output("The update could not be recorded, so nothing was changed.")
			return false
		}
		return true
	}
	notRecorded := stackops.DeployResult{Code: stackops.CodeEngineError}
	if standalone {
		standaloneIn := stackops.StandaloneDeployInput{
			EnvID: env.ID, NodeID: node, Stack: st, Copy: good.Copy, Reviewed: pins, Update: update,
			Pins: repo, RevisionID: rev.ID, Caller: caller, Masker: rendering.masker, Subnets: repo,
		}
		l.items = stackops.StandaloneItemSpecs()
		l.run = func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult)) {
			if !begin(jctx, r) {
				onDone(notRecorded)
				return
			}
			standaloneIn.OnDone = onDone
			d.ops().DeployStandalone(jctx, standaloneIn, r)
		}
		return launchStackDeploy(ctx, d, l)
	}
	swarmIn := stackops.SwarmDeployInput{
		EnvID: env.ID, Stack: st, Project: prep.Loaded.Project, Files: prep.Files, Vars: prep.Vars, Copy: good.Copy,
		Caller: caller, Masker: rendering.masker,
		Pins: repo, Subnets: repo, RevisionID: rev.ID, Update: update, Reviewed: pins,
	}
	l.items = stackops.DeployItemSpecs()
	l.run = func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult)) {
		if !begin(jctx, r) {
			onDone(notRecorded)
			return
		}
		swarmIn.OnDone = onDone
		d.ops().DeploySwarm(jctx, swarmIn, r)
	}
	return launchStackDeploy(ctx, d, l)
}

// updateActorKey is what a run keeps of who started it: the user's ID, else the name.
func updateActorKey(a audit.Actor) string {
	if a.ID != "" {
		return a.ID
	}
	return a.Name
}

// endStackUpdate records the end of an update of a stack: the run, one audit row for each service, and, for an update that
// ended good, the draft. An unedited draft is rebased onto the update revision so the next deploy of it asks for no
// acknowledgement; an edited one is left alone, so its review shows the update as a deploy made since (RESEARCH Pitfall 9).
// A failed update never touches the draft.
func endStackUpdate(ctx context.Context, d StacksDeps, ap *updates.Applier, actor audit.Actor, userAgent string, st stackops.Stack, from, rev stackops.Snapshot, run updates.Run, plan updates.StackPlan, res stackops.DeployResult) {
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	state, reason := updates.RunDone, ""
	if !res.OK() {
		state, reason = updates.RunFailed, res.Code
		if res.Code == stackops.CodeCancelled {
			reason = updates.ReasonCancelled
		}
	}
	if _, err := ap.Runs.Finish(wctx, run.ID, state, reason, rev.Seq, 0); err != nil {
		d.Log.Error("an image update of a stack could not be finished on record", "run", run.ID, "error", err)
	}
	if d.Audit != nil {
		for _, t := range plan.Targets {
			e := audit.Entry{
				Actor: actor, TargetType: "service", TargetID: t.Key, EnvID: st.EnvID, IP: httpx.ClientIP(wctx), UserAgent: userAgent,
				// The documented detail keys of an image update. The stack and the revision are in the stack's own audit row.
				Details: map[string]string{
					"kind": "service", "workload": t.Key, "image": t.Image, "from_digest": t.From, "to_digest": t.To,
				},
			}
			if run.BulkID != "" {
				e.Details["bulk"] = run.BulkID
			}
			if state == updates.RunDone {
				e.Action, e.Result = audit.ActionImageUpdated, audit.ResultSuccess
			} else {
				e.Action, e.Result, e.Reason = audit.ActionImageUpdateFailed, audit.ResultFailure, reason
				e.Details["reason"] = reason
			}
			if err := d.Audit.Record(wctx, e); err != nil {
				d.Log.Error("an image update of a stack could not be written to the audit log", "stack", st.ID, "service", t.Key, "error", err)
			}
		}
	}
	if state != updates.RunDone {
		return
	}
	if _, err := d.repo().RebaseUneditedDraft(wctx, st.ID, from, rev.Seq); err != nil {
		d.Log.Warn("could not rebase the draft of a stack after an image update", "stack", st.ID, "error", err)
	}
}
