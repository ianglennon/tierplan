// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
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
	// Force is the person's choice to try again a version of the one service that failed before. A run of a whole stack never sets it.
	Force bool
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
	out, err := updateStackImages(ctx, d.Stacks, d.Applier, p, env, st, run.Services, in.Body.Expect, run.Force, p.AuditActor(), "", in.UserAgent)
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
// the registry says otherwise. force is a person's choice to try again a version of the one service named in services that failed
// before; it means nothing for any other request. The digests deployed are the ones the server read, never one from the request.
//
// It is modelled on rollbackStack: the stack's operation lock is taken before anything is stored, the revision is a copy of the
// newest good one (Repo.NewUpdateRevision, so it reads as an update and never as a roll back), and the deploy is the stack's own
// job through launchStackDeploy, in the stack's console. The images are the revision's own, but for the chosen services, whose
// pins carry the new digest and which the deploy checks like a reviewed deploy does (ReviewedPins).
func updateStackImages(ctx context.Context, d StacksDeps, ap *updates.Applier, p *rbac.Principal, env *httpx.Env, st stackops.Stack, services []string, expect map[string]string, force bool, actor audit.Actor, bulk, userAgent string) (*StackDeployOutput, error) {
	su, err := judgeStackUpdate(ctx, d, ap, p, env, st, services, expect, force, false)
	if err != nil {
		return nil, err
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
	l, err := su.revise(ctx, actor, bulk, userAgent, nil)
	if err != nil {
		return nil, err
	}
	l.release, l.owned = release, &jobOwnsLock
	return launchStackDeploy(ctx, d, l)
}

// updateStackImagesInJob is updateStackImages for a stack's share of a bulk update (plan 12-30, D-06, D-11): it runs inside the bulk
// job, under the environment's lock the job already holds, and reports on r, a reporter scoped to the item. The deploy is the same
// one, with its own revision, its own automatic rollback and its own audit rows (carrying the bulk ID); only the job and the lock
// are the bulk update's. queued is the run of the item, which the deploy begins and ends. A refusal before anything is written is
// an error that refusalOfProblem names; the run is then still queued.
func updateStackImagesInJob(ctx context.Context, d StacksDeps, ap *updates.Applier, p *rbac.Principal, env *httpx.Env, st stackops.Stack, in updates.StackItemInput, r *jobs.Reporter) error {
	su, err := judgeStackUpdate(ctx, d, ap, p, env, st, in.Services, in.Expect, false, true)
	if err != nil {
		return err
	}
	run := in.Run
	l, err := su.revise(ctx, in.Actor, in.BulkID, in.UserAgent, &run)
	if err != nil {
		return err
	}
	// The lock is the bulk job's and is freed by it: the deploy frees nothing.
	owned := true
	l.release, l.owned = func() {}, &owned
	persist := context.WithoutCancel(ctx)
	l.persist = persist
	row, err := d.repo().StartDeploy(persist, st.ID, env.ID, l.kind, l.rev.Seq, r.JobID(), p.Name)
	if err != nil {
		_ = d.repo().SetOutcome(persist, l.rev.ID, stackops.OutcomeFailed, "", "")
		return stackInternal(d, "record the deploy", env.ID, err)
	}
	l.deploy = &row
	for _, spec := range l.items {
		r.Add(spec)
	}
	return runStackDeploy(ctx, d, l, r)
}

// stackUpdate is the update of a stack's images once it has been judged: which services change and to what, the pins the deploy
// is given, and what it needs to be prepared. Nothing has been written.
type stackUpdate struct {
	d          StacksDeps
	ap         *updates.Applier
	p          *rbac.Principal
	env        *httpx.Env
	st         stackops.Stack
	plan       updates.StackPlan
	good       stackops.Snapshot
	goodPins   map[string]string
	pins       map[string]string
	update     map[string]bool
	prep       stackops.RollbackPlan
	masker     stacks.Masker
	standalone bool
	node       string
	caller     stackops.Caller
}

// judgeStackUpdate judges an update of a stack's images: the services it changes, the newest good revision and the pins, the deploy
// prepared and checked, and the policy of what it deploys judged for the caller (T-12-51). lockHeld is true when the caller holds the
// environment's lock itself, as a bulk update does. Nothing is written.
func judgeStackUpdate(ctx context.Context, d StacksDeps, ap *updates.Applier, p *rbac.Principal, env *httpx.Env, st stackops.Stack, services []string, expect map[string]string, force, lockHeld bool) (*stackUpdate, error) {
	if d.Jobs == nil || ap == nil || ap.Runs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
	}
	repo := d.repo()
	if err := d.ops().CheckTarget(ctx, env.ID, st); err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	asUpdates := UpdatesDeps{Log: d.Log}
	// A held lock is answered before the registry is asked, as a single update does.
	if !lockHeld {
		if h, held := d.Locks.Held(env.ID); held {
			return nil, operationLocked(env.Name, h)
		}
	}
	plan, err := ap.PlanStack(ctx, updates.StackRequest{EnvID: env.ID, Stack: st.Name, Services: services, Expect: expect, Recheck: true, Force: force})
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

	su := &stackUpdate{
		d: d, ap: ap, p: p, env: env, st: st, plan: plan, good: good, goodPins: goodPins, pins: pins, update: update,
		standalone: st.Target == stacks.TargetStandalone, masker: stacks.NewMasker(good.Copy.Vars, good.Copy.Files),
		caller: stackops.Caller{Principal: p, Actor: p.Name},
	}
	if su.standalone {
		if su.prep, err = d.ops().PrepareRollbackStandalone(ctx, st, good, goodPins); err != nil {
			return nil, rollbackPrepareProblem(err, su.masker)
		}
		if su.node, err = d.ops().StandaloneNode(ctx, env.ID); err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		if err = d.ops().ValidateStandalonePins(ctx, env.ID, su.node, st, su.prep.Loaded.Project, repo, update, pins); err != nil {
			return nil, pinsProblem(d, env.ID, err)
		}
	} else {
		if su.prep, err = d.ops().PrepareRollback(ctx, st, good, goodPins); err != nil {
			return nil, rollbackPrepareProblem(err, su.masker)
		}
		if err = d.ops().ValidatePins(ctx, env.ID, st, su.prep.Plan.Services, repo, update, pins); err != nil {
			return nil, pinsProblem(d, env.ID, err)
		}
	}
	// What the stack needs to deploy is judged for the caller, as a deploy is (T-12-51): an Operator cannot update a stack
	// only an Admin may deploy.
	pol, err := d.ops().Policy(ctx, su.caller, env.ID, good.Copy, su.prep.Plan)
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
	return su, nil
}

// revise makes the revision of the update, with the lock held, and the operation that deploys it. queued is the run of a bulk
// update's item, which the operation begins; nil for an update of its own, whose run it records.
func (su *stackUpdate) revise(ctx context.Context, actor audit.Actor, bulk, userAgent string, queued *updates.Run) (launch, error) {
	d, ap, p, env, st, plan, good, repo := su.d, su.ap, su.p, su.env, su.st, su.plan, su.good, su.d.repo()
	// The content comes from the newest good revision. A deploy that ended while this request was judged took the lock off
	// before the job read as finished, so the newest good revision is read again now, holding the lock: the answer no other
	// operation can change.
	if now, found, gerr := repo.GoodRevisionInfo(ctx, st.ID); gerr != nil {
		return launch{}, stackInternal(d, "read the running revision", env.ID, gerr)
	} else if !found || now.Seq != good.Seq {
		return launch{}, deployedChanged(st.Name, now.Seq)
	}

	rev, err := repo.NewUpdateRevision(ctx, good, revisionCreator(ctx, repo, p))
	if err != nil {
		return launch{}, stackInternal(d, "revision", env.ID, err)
	}
	rendering := revisionRendering{repo: repo, loaded: su.prep.Loaded, masker: su.masker}
	if err := rendering.store(ctx, rev.ID, stackops.LivePins(su.pins)); err != nil {
		d.Log.Warn("could not store the rendering of a revision", "stack", st.ID, "revision", rev.Seq, "error", err)
	}
	// The platforms the check read are what the deploy sends, so a revision that predates the platform record needs no
	// registry call of its own.
	for _, t := range plan.Targets {
		d.Platforms.Remember(env.ID, t.Repo, t.To, t.Platforms)
	}

	run := updates.Run{
		EnvID: env.ID, BulkID: bulk, Kind: "stack", Workload: st.Name, WorkloadID: st.ID, StackID: st.ID,
		State: updates.RunRunning, Actor: updateActorKey(actor), RevisionSeq: rev.Seq, StartedAt: time.Now().UTC(),
	}
	if queued != nil {
		run.ID, run.EnvID, run.BulkID, run.JobID, run.StartedAt = queued.ID, queued.EnvID, queued.BulkID, queued.JobID, queued.StartedAt
	} else {
		runID, err := uuid.NewV7()
		if err != nil {
			_ = repo.SetOutcome(context.WithoutCancel(ctx), rev.ID, stackops.OutcomeFailed, "", "")
			return launch{}, stackInternal(d, "new run id", env.ID, err)
		}
		run.ID = runID.String()
	}
	for _, t := range plan.Targets {
		run.Items = append(run.Items, updates.RunItem{Service: t.Key, Image: t.Image, FromDigest: t.From, ToDigest: t.To})
	}

	extra := deployOpts{}.auditExtras(p)
	l := launch{
		p: p, env: env, st: st, rev: rev, mode: deployModeUpdate, kind: stackops.DeployKindDeploy, userAgent: userAgent,
		rendering: rendering,
	}
	// The stack's own record of the end of a deploy runs first, so the revision's outcome is stored; then the run, the audit
	// rows and the draft. All of it is before the lock is freed and the job reads as finished.
	l.record = func(rctx context.Context, deploy stackops.DeployRow, res stackops.DeployResult) {
		recordDeployEnd(rctx, d, actor, userAgent, st, rev, deploy, deployModeUpdate, res, rendering, extra)
		if res.NeedsRestore() {
			// The rollback ends the run, with what became of the stack, before the lock is freed.
			return
		}
		endStackUpdate(rctx, d, ap, actor, userAgent, st, good, rev, run, plan, res)
	}
	// An update that fails after it changed a service is undone by TierPlan in the same job, holding the same lock (IMG-07, D-12).
	rb := stackRollback{
		d: d, ap: ap, p: p, env: env, st: st, good: good, goodPins: su.goodPins, failed: rev, run: run, plan: plan, actor: actor,
		userAgent: userAgent, extra: extra, standalone: su.standalone, node: su.node,
	}
	l.onFailed = func(fctx context.Context, r *jobs.Reporter, _ stackops.Snapshot, code string) error {
		return rollbackFailedUpdate(fctx, rb, r, code)
	}
	// The run is on record before the engine is touched: the job writes it first, with its own ID. A run that waited in a bulk
	// update begins instead, with what it knows now.
	begin := func(jctx context.Context, r *jobs.Reporter) bool {
		run.JobID = r.JobID()
		var err error
		if queued != nil {
			var begun bool
			if begun, err = ap.Runs.Begin(context.WithoutCancel(jctx), run); err == nil && !begun {
				err = errors.New("the run did not wait any more")
			}
		} else {
			err = ap.Runs.Insert(context.WithoutCancel(jctx), run)
		}
		if err != nil {
			d.Log.Error("an image update of a stack could not be recorded", "stack", st.ID, "error", err)
			r.Output("The update could not be recorded, so nothing was changed.")
			return false
		}
		return true
	}
	notRecorded := stackops.DeployResult{Code: stackops.CodeEngineError}
	if su.standalone {
		standaloneIn := stackops.StandaloneDeployInput{
			EnvID: env.ID, NodeID: su.node, Stack: st, Copy: good.Copy, Reviewed: su.pins, Update: su.update,
			Pins: repo, RevisionID: rev.ID, Caller: su.caller, Masker: rendering.masker, Subnets: repo,
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
		return l, nil
	}
	swarmIn := stackops.SwarmDeployInput{
		EnvID: env.ID, Stack: st, Project: su.prep.Loaded.Project, Files: su.prep.Files, Vars: su.prep.Vars, Copy: good.Copy,
		Caller: su.caller, Masker: rendering.masker,
		Pins: repo, Subnets: repo, RevisionID: rev.ID, Update: su.update, Reviewed: su.pins,
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
	return l, nil
}

// updateActorKey is what a run keeps of who started it: the user's ID, else the name.
func updateActorKey(a audit.Actor) string {
	if a.ID != "" {
		return a.ID
	}
	return a.Name
}

// endStackUpdate records the end of an update of a stack that is not rolled back: the run, one audit row for each service, the
// memory of the digest and, for an update that ended good, the draft. An unedited draft is rebased onto the update revision so the
// next deploy of it asks for no acknowledgement; an edited one is left alone, so its review shows the update as a deploy made since
// (RESEARCH Pitfall 9). A failed update never touches the draft.
func endStackUpdate(ctx context.Context, d StacksDeps, ap *updates.Applier, actor audit.Actor, userAgent string, st stackops.Stack, from, rev stackops.Snapshot, run updates.Run, plan updates.StackPlan, res stackops.DeployResult) {
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	e := stackEnd{state: updates.RunDone, revision: rev.Seq}
	if !res.OK() {
		e.state, e.reason = updates.RunFailed, res.Code
		if res.Code == stackops.CodeCancelled {
			e.reason = updates.ReasonCancelled
		}
		e.cause = e.reason
	}
	endStackRun(wctx, d, ap, actor, userAgent, st, run, plan, e)
	if e.state != updates.RunDone {
		return
	}
	if _, err := d.repo().RebaseUneditedDraft(wctx, st.ID, from, rev.Seq); err != nil {
		d.Log.Warn("could not rebase the draft of a stack after an image update", "stack", st.ID, "error", err)
	}
}

// stackEnd is how the run of an update of a stack ended. reason is what the run keeps; cause is why the update failed, which differs
// from reason only when the rollback failed too (the run then keeps restore_failed). revision is the update's revision and rollback
// the revision of the automatic rollback, 0 for none.
type stackEnd struct {
	state          updates.RunState
	reason, cause  string
	revision, back int64
}

// endStackRun ends the run, remembers or forgets the digest of each service, and writes the audit rows of the end of an update of
// a stack: image.updated for each service when it ended good, else image.update_failed and, when the stack was put back or that
// was tried, image.update_rolled_back with the result of it (IMG-07, D-14, AUTH-08). A write that fails is logged and changes nothing
// else.
func endStackRun(ctx context.Context, d StacksDeps, ap *updates.Applier, actor audit.Actor, userAgent string, st stackops.Stack, run updates.Run, plan updates.StackPlan, e stackEnd) {
	if _, err := ap.Runs.Finish(ctx, run.ID, e.state, e.reason, e.revision, e.back); err != nil {
		d.Log.Error("an image update of a stack could not be finished on record", "run", run.ID, "error", err)
	}
	rememberStackFailures(ctx, d, ap, st, run, plan, e)
	if d.Audit == nil {
		return
	}
	for _, t := range plan.Targets {
		row := func(action string, result audit.Result, why string) audit.Entry {
			en := audit.Entry{
				Actor: actor, TargetType: "service", TargetID: t.Key, EnvID: st.EnvID, IP: httpx.ClientIP(ctx), UserAgent: userAgent,
				Action: action, Result: result, Reason: why,
				// The documented detail keys of an image update. The stack and the revision are in the stack's own audit row.
				Details: map[string]string{
					"kind": "service", "workload": t.Key, "image": t.Image, "from_digest": t.From, "to_digest": t.To,
				},
			}
			if run.BulkID != "" {
				en.Details["bulk"] = run.BulkID
			}
			return en
		}
		var entries []audit.Entry
		switch e.state {
		case updates.RunDone:
			entries = append(entries, row(audit.ActionImageUpdated, audit.ResultSuccess, ""))
		case updates.RunRolledBack, updates.RunRollbackFailed:
			failed := row(audit.ActionImageUpdateFailed, audit.ResultFailure, e.cause)
			failed.Details["reason"] = e.cause
			back := row(audit.ActionImageUpdateRolledBack, audit.ResultSuccess, "")
			back.Details["reason"] = e.cause
			if e.state == updates.RunRollbackFailed {
				back.Result, back.Reason = audit.ResultFailure, e.reason
			}
			entries = append(entries, failed, back)
		default:
			failed := row(audit.ActionImageUpdateFailed, audit.ResultFailure, e.reason)
			failed.Details["reason"] = e.reason
			entries = append(entries, failed)
		}
		for _, en := range entries {
			if err := d.Audit.Record(ctx, en); err != nil {
				d.Log.Error("an image update of a stack could not be written to the audit log", "stack", st.ID, "service", t.Key, "action", en.Action, "error", err)
			}
		}
	}
}

// rememberStackFailures keeps what the next judgement needs of an update of a stack, for each service it changed, as an update of
// the service on its own does: a good update forgets the failure, a rolled-back or rollback-failed one and one whose pull failed
// remember the digest that failed, and a cancelled one or one refused before anything changed says nothing about the version.
func rememberStackFailures(ctx context.Context, d StacksDeps, ap *updates.Applier, st stackops.Stack, run updates.Run, plan updates.StackPlan, e stackEnd) {
	if ap.Failures == nil {
		return
	}
	for _, t := range plan.Targets {
		kind, name := "container", t.Name
		if t.Swarm {
			kind, name = "service", t.Key
		}
		if name == "" {
			name = t.Key
		}
		var err error
		switch {
		case e.state == updates.RunDone:
			_, err = ap.Failures.Clear(ctx, st.EnvID, kind, name)
		case e.cause == updates.ReasonCancelled:
			continue
		case e.state == updates.RunRolledBack, e.state == updates.RunRollbackFailed:
			err = ap.Failures.Record(ctx, updates.Failure{
				EnvID: st.EnvID, Kind: kind, Workload: name, Digest: t.To, Reason: e.cause, RunID: run.ID,
				RollbackFailed: e.state == updates.RunRollbackFailed,
			})
		case e.state == updates.RunFailed && isPullFailureCode(e.cause):
			err = ap.Failures.Record(ctx, updates.Failure{
				EnvID: st.EnvID, Kind: kind, Workload: name, Digest: t.To, Reason: updates.ReasonPullFailed, RunID: run.ID,
			})
		default:
			continue
		}
		if err != nil {
			d.Log.Error("the outcome of an image update of a stack could not be remembered", "stack", st.ID, "service", t.Key, "error", err)
		}
	}
}

// isPullFailureCode reports whether a deploy's code says the registry or the node's pull of an image failed, as against the node
// being out of reach, read-only or refusing: only the first says something about the version.
func isPullFailureCode(code string) bool {
	switch code {
	case "", stackops.CodeHostReadOnly, stackops.CodeNodeUnreachable, stackops.CodeAgentPolicyDenied, dockerx.PullCodeCancelled:
		return false
	}
	_, ok := stackops.PullMessages[code]
	return ok
}

// updateFailureReason words why an update's deploy failed after it had changed a service, in the reasons a run keeps.
func updateFailureReason(code string) string {
	switch code {
	case stackops.CodeConvergeTimeout:
		return updates.ReasonTimeout
	case stackops.CodeTasksFailed, stackops.CodeUpdateStopped, stackops.CodeNetworkFull:
		return updates.ReasonDidntStart
	case stackops.CodeServiceRejected:
		return updates.ReasonRejected
	case stackops.CodeServiceChanged:
		return updates.ReasonServiceChanged
	case updates.ReasonServerRestarted:
		// Not a deploy's code: startup recovery (RecoverStackUpdate) rolls back an update a restart interrupted.
		return updates.ReasonServerRestarted
	}
	return updates.ReasonEngine
}

// stackRollback is what the automatic rollback of a failed update of a stack needs, taken from the update that failed: who asked,
// the stack, the newest good revision before the update and its pins, the update's own revision and run, and how the stack deploys.
type stackRollback struct {
	d          StacksDeps
	ap         *updates.Applier
	p          *rbac.Principal
	env        *httpx.Env
	st         stackops.Stack
	good       stackops.Snapshot
	goodPins   map[string]string
	failed     stackops.Snapshot
	run        updates.Run
	plan       updates.StackPlan
	actor      audit.Actor
	userAgent  string
	extra      map[string]string
	standalone bool
	node       string
}

// restoreWindow bounds the rollback: each service gets the converge timeout to run again, a few at a time, and the writes around
// them a few minutes. The rollback does not end when the job is cancelled, so it ends here.
func (rb stackRollback) restoreWindow() time.Duration {
	t := rb.d.ConvergeTimeout
	if t <= 0 {
		t = stackops.DefaultConvergeTimeout
	}
	return 2*t + 5*time.Minute
}

// rollbackFailedUpdate puts a stack back after the deploy of an image update failed after it had changed a service (IMG-07, D-12). It
// runs in the update's own job, before the environment's lock is freed, so no other operation can come between the failure and the
// restore. The restore is a revision of its own, "rollback of #N", a copy of the newest good revision before the update, and it is
// deployed with the revision's own images, platforms and subnets by the stack's engine, for every service of the stack together, so
// the stack is never left half on the new version. A service the engine already rolled back by its own failure action is left as it
// is by that deploy; the revision and the audit rows are written once all the same. It never calls the git roll-back hook, so a git
// stack's automatic deploys stay on, and it never writes the services' update_config or any file of the stack.
//
// The run ends rolled_back, or rollback_failed when the previous version could not be put back; either way every updated service
// remembers the digest that failed. The two steps "Roll back to revision #N" and "Wait for the previous version to run" are added to
// the job. An error is returned only for the log: the run has been ended on record by then.
func rollbackFailedUpdate(ctx context.Context, rb stackRollback, r *jobs.Reporter, code string) error {
	d, repo := rb.d, rb.d.repo()
	cause := updateFailureReason(code)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rb.restoreWindow())
	defer cancel()

	r.Add(jobs.ItemSpec{ID: updates.ItemRollback, Label: "Roll back to revision #" + itoa(rb.good.Seq), Kind: stackops.KindStep})
	r.Add(jobs.ItemSpec{ID: updates.ItemRollbackWait, Label: "Wait for the previous version to run", Kind: stackops.KindStep})
	r.Update(updates.ItemRollback, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	r.Output("The update failed. Putting the stack back to revision #" + itoa(rb.good.Seq))

	// end is the one place the rollback finishes: the steps, the run, the memory and the audit rows.
	end := func(rolled stackops.DeployResult, rollbackSeq int64, rollbackErr error) error {
		e := stackEnd{state: updates.RunRolledBack, reason: cause, cause: cause, revision: rb.failed.Seq, back: rollbackSeq}
		if rollbackErr != nil || !rolled.OK() {
			e.state, e.reason = updates.RunRollbackFailed, updates.ReasonRestoreFailed
			if cause == updates.ReasonServerRestarted {
				// A rollback after a restart keeps the restart as its reason whether or not it worked.
				e.reason = cause
			}
		}
		wctx, stop := doneActionAuditContext(ctx)
		defer stop()
		endStackRun(wctx, d, rb.ap, rb.actor, rb.userAgent, rb.st, rb.run, rb.plan, e)
		return rollbackErr
	}
	// refused fails the steps for a rollback that never reached the engine.
	refused := func(code string, err error) error {
		r.Finish(updates.ItemRollback, jobs.ItemFailed, code, stackops.CodeMessage(code), nil)
		r.Finish(updates.ItemRollbackWait, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		return err
	}

	masker := stacks.NewMasker(rb.good.Copy.Vars, rb.good.Copy.Files)
	var prep stackops.RollbackPlan
	var err error
	if rb.standalone {
		prep, err = d.ops().PrepareRollbackStandalone(ctx, rb.st, rb.good, rb.goodPins)
	} else {
		prep, err = d.ops().PrepareRollback(ctx, rb.st, rb.good, rb.goodPins)
	}
	if err != nil {
		d.Log.Error("the revision before a failed image update cannot be redeployed", "stack", rb.st.ID, "revision", rb.good.Seq, "error", masker.Mask(err.Error()))
		return end(stackops.DeployResult{}, 0, refused(stackops.CodeRevisionNotDeployable, errors.New("the previous revision cannot be redeployed")))
	}
	rev, err := repo.CopyRevision(ctx, rb.good, rb.p.Name, stackops.OriginRollback)
	if err != nil {
		return end(stackops.DeployResult{}, 0, refused(stackops.CodeEngineError, fmt.Errorf("write the rollback revision: %w", err)))
	}
	rendering := revisionRendering{repo: repo, loaded: prep.Loaded, masker: masker}
	if err := rendering.store(ctx, rev.ID, stackops.LivePins(rb.goodPins)); err != nil {
		d.Log.Warn("could not store the rendering of a revision", "stack", rb.st.ID, "revision", rev.Seq, "error", err)
	}
	row, err := repo.StartDeploy(ctx, rb.st.ID, rb.env.ID, stackops.DeployKindRollback, rev.Seq, r.JobID(), rb.p.Name)
	if err != nil {
		_ = repo.SetOutcome(ctx, rev.ID, stackops.OutcomeFailed, "", "")
		return end(stackops.DeployResult{}, rev.Seq, refused(stackops.CodeEngineError, fmt.Errorf("record the rollback: %w", err)))
	}

	caller := stackops.Caller{Principal: rb.p, Actor: rb.p.Name}
	var items []jobs.ItemSpec
	var run func(jctx context.Context, rr *jobs.Reporter, onDone func(stackops.DeployResult))
	if rb.standalone {
		in := stackops.StandaloneDeployInput{
			EnvID: rb.env.ID, NodeID: rb.node, Stack: rb.st, Copy: rb.good.Copy, Reviewed: rb.goodPins, Rollback: true,
			Pins: repo, RevisionID: rev.ID, Caller: caller, Masker: masker, Subnets: repo, RestoredSubnets: rb.good.Subnets,
		}
		items = stackops.StandaloneItemSpecs()
		run = func(jctx context.Context, rr *jobs.Reporter, onDone func(stackops.DeployResult)) {
			in.OnDone = onDone
			d.ops().DeployStandalone(jctx, in, rr)
		}
	} else {
		in := stackops.SwarmDeployInput{
			EnvID: rb.env.ID, Stack: rb.st, Project: prep.Loaded.Project, Files: prep.Files, Vars: prep.Vars, Copy: rb.good.Copy,
			Caller: caller, Masker: masker,
			Pins: repo, RevisionID: rev.ID, Reviewed: rb.goodPins, Rollback: true,
			// The revision's own record of its platforms goes with its digests, so the rollback asks no registry for them.
			RestoredPlatforms: rb.good.Platforms,
			Subnets:           repo, RestoredSubnets: rb.good.Subnets,
		}
		items = stackops.DeployItemSpecs()
		run = func(jctx context.Context, rr *jobs.Reporter, onDone func(stackops.DeployResult)) {
			in.OnDone = onDone
			d.ops().DeploySwarm(jctx, in, rr)
		}
	}
	res, wrote := runRestoreDeploy(ctx, r, items, run)

	// The revision, the deploy row and the stack's own audit row, as a roll back made by a person is recorded.
	rctx, stop := doneActionAuditContext(ctx)
	recordDeployEnd(rctx, d, rb.actor, rb.userAgent, rb.st, rev, row, deployModeRollback, res, rendering, rb.extra)
	stop()

	switch {
	case res.OK():
		r.Output("The stack runs revision #" + itoa(rb.good.Seq) + " again")
		r.Finish(updates.ItemRollback, jobs.ItemDone, "", "", nil)
		r.Update(updates.ItemRollbackWait, func(it *jobs.Item) { it.State = jobs.ItemRunning })
		r.Finish(updates.ItemRollbackWait, jobs.ItemDone, "", "", nil)
	case wrote || isWaitCode(res.Code):
		r.Finish(updates.ItemRollback, jobs.ItemDone, "", "", nil)
		r.Finish(updates.ItemRollbackWait, jobs.ItemFailed, res.Code, stackops.CodeMessage(res.Code), nil)
	default:
		_ = refused(res.Code, nil)
	}
	return end(res, rev.Seq, nil)
}

// isWaitCode reports whether a deploy's code is that of a service that was written and did not settle.
func isWaitCode(code string) bool {
	switch code {
	case stackops.CodeConvergeTimeout, stackops.CodeUpdateStopped, stackops.CodeTasksFailed, stackops.CodeNetworkFull:
		return true
	}
	return false
}

// runRestoreDeploy runs a deploy in a job of its own, which nobody lists, and shows it in the job r: its output lines as they are
// written, and the moment every service has been written, which ends the step that writes and starts the step that waits. The
// deploy has to run apart because the steps of the update's own deploy have all ended, and a step that ended cannot be reported
// on again. ctx bounds it and a cancel of the stack's job does not reach it: a restore that began is finished. It answers what the
// deploy ended with, failed when it never said, and whether every service had been written by then.
func runRestoreDeploy(ctx context.Context, r *jobs.Reporter, items []jobs.ItemSpec, run func(jctx context.Context, rr *jobs.Reporter, onDone func(stackops.DeployResult))) (stackops.DeployResult, bool) {
	hub := jobs.New(jobs.Options{})
	defer hub.Close()
	var result stackops.DeployResult
	var said atomic.Bool
	job, err := hub.Start(jobs.Spec{Kind: "stack.restore", Owner: "restore", Items: items}, func(_ context.Context, rr *jobs.Reporter) {
		run(ctx, rr, func(x stackops.DeployResult) { result = x; said.Store(true) })
	})
	if err != nil {
		return stackops.DeployResult{Code: stackops.CodeEngineError}, false
	}
	var wrote atomic.Bool
	var last uint64
	finished := make(chan struct{})
	_, events, cancelSub, serr := hub.Subscribe(ctx, job.ID, 0)
	go func() {
		defer close(finished)
		if serr != nil {
			return
		}
		for ev := range events {
			switch {
			case ev.Type == jobs.EventOutput && ev.Output != nil:
				r.Output(ev.Output.Text)
				last = ev.Output.Seq
			case ev.Item != nil && ev.Item.ID == stackops.ItemServices && ev.Item.Total > 0 && ev.Item.Current >= ev.Item.Total:
				if wrote.CompareAndSwap(false, true) {
					r.Finish(updates.ItemRollback, jobs.ItemDone, "", "", nil)
					r.Update(updates.ItemRollbackWait, func(it *jobs.Item) { it.State = jobs.ItemRunning })
				}
			}
		}
	}()
	if _, err := hub.Wait(ctx, job.ID); err != nil {
		cancelSub()
		<-finished
		return stackops.DeployResult{Code: stackops.CodeEngineError}, wrote.Load()
	}
	<-finished
	cancelSub()
	// Whatever the live reader did not carry over.
	if lines, ok := hub.Output(job.ID); ok {
		for _, l := range lines {
			if l.Seq > last {
				r.Output(l.Text)
			}
		}
	}
	if !said.Load() {
		return stackops.DeployResult{Code: stackops.CodeEngineError}, wrote.Load()
	}
	return result, wrote.Load()
}
