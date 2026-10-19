// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

const (
	// stackDeployJobKind is the Kind of a stack deploy in the jobs hub.
	stackDeployJobKind = "stack_deploy"

	metaStackID   = "stackId"
	metaStackName = "stackName"
	metaMode      = "mode"
	metaStartedBy = "startedBy"

	deployModeDeploy = "deploy"
	deployModeUpdate = "update"

	problemDraftChanged             = "draft_changed"
	problemPinsInvalid              = "pins_invalid"
	problemConcurrentUnacknowledged = "concurrent_unacknowledged"
)

// StackDeployRequest is the body of POST .../deploys.
type StackDeployRequest struct {
	DraftVersion int64             `json:"draftVersion" minimum:"1" doc:"The version of the draft you reviewed and mean to deploy. If the draft moved since, the answer is 409 draft_changed and nothing is deployed"`
	Pins         map[string]string `json:"pins,omitempty" doc:"The images the review chose, by swarm service name (the review's pins). The deploy uses exactly these digests and asks no registry. Each is checked against what the server decides itself: a service that keeps its running digest must be sent that digest, and a looked-up one a sha256 digest of the repository its reference names; anything else is 422 pins_invalid. Left out, the deploy looks the images up itself"`
	Mode         string            `json:"mode,omitempty" enum:"deploy,update" doc:"deploy (the default), or update to move the services named in services to a newly looked-up digest"`
	Services     []string          `json:"services,omitempty" maxItems:"200" doc:"For mode update: the services to move, by the name in the compose file or by the swarm name"`
	// AcknowledgeConcurrent says the person saw what changed since the draft was started.
	AcknowledgeConcurrent bool `json:"acknowledgeConcurrent,omitempty" doc:"Set when the review answered concurrent and the person has seen what changed and wants to deploy the draft over it. Without it, a draft that started from an older revision than the newest good one is 409 concurrent_unacknowledged and nothing is deployed"`
}

// StackDeployInput is the path and body of the deploy route.
type StackDeployInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      StackDeployRequest
}

// StackDeployBody is a deploy that has started.
type StackDeployBody struct {
	JobID    string `json:"jobId" doc:"The deploy job: read it at GET .../deploys/{jobId}"`
	Revision int64  `json:"revision" doc:"The number of the revision the deploy took from the draft"`
}

// StackDeployOutput wraps StackDeployBody for huma.
type StackDeployOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackDeployBody
}

// StackJobPathInput is the path of a deploy job.
type StackJobPathInput struct {
	StackPathInput
	JobID string `path:"jobId" doc:"The job ID the deploy answered with"`
}

// StackJobItemBody is one step or service of a deploy.
type StackJobItemBody struct {
	ID      string            `json:"id" doc:"The item: check, lookup, networks, objects, services or cleanup, or one made for an object or service of the deploy, like service:web"`
	Label   string            `json:"label" doc:"The item's name: a step's words, or a service's name"`
	Parent  string            `json:"parent" doc:"The ID of the item this one belongs to, empty for a top-level step. A service is under services; a network is under networks, a secret or config under objects, and an old secret or config under cleanup"`
	Kind    string            `json:"kind" doc:"step for a line of the step list, service for a service row, group for an item that only holds steps and is not shown itself"`
	State   string            `json:"state" enum:"waiting,running,done,failed,skipped,cancelled" doc:"Where the item is. skipped is an item that was not run because an earlier one failed"`
	Code    string            `json:"code" doc:"A stable word for how an item failed or was not run, empty otherwise"`
	Message string            `json:"message" doc:"A sentence for the code. The SPA words the code itself"`
	Detail  string            `json:"detail" doc:"The engine's own words about a failed item, cleaned of control characters, clipped to 2,048 characters and with the stack's secret values hidden. Empty when there are none"`
	Current int64             `json:"current" doc:"How many of total were done"`
	Total   int64             `json:"total" doc:"How many there are to do, 0 until known"`
	Unit    string            `json:"unit" doc:"What current and total count, or empty"`
	Result  map[string]string `json:"result" doc:"What a finished step produced, like the counts of services created and updated"`
}

// StackJobOutputLine is one line of a deploy's output log.
type StackJobOutputLine struct {
	Seq  uint64 `json:"seq" doc:"The event ID that carried the line; it strictly increases"`
	At   string `json:"at" doc:"When the line was written, RFC 3339 UTC"`
	Text string `json:"text" doc:"What the deploy did, or what the engine said, cleaned of control characters and clipped, with the stack's secret values hidden"`
}

// StackJobBody is a deploy job.
type StackJobBody struct {
	ID         string               `json:"id" doc:"The job ID"`
	StackID    string               `json:"stackId" doc:"The stack being deployed"`
	Mode       string               `json:"mode" enum:"deploy,update" doc:"What the operation is"`
	StartedBy  string               `json:"startedBy" doc:"Who started the deploy, as a name"`
	State      string               `json:"state" enum:"running,done,partial,failed,cancelled" doc:"running until every step has ended, then done when every step is"`
	CreatedAt  string               `json:"createdAt" doc:"When the job started, RFC 3339 UTC"`
	FinishedAt string               `json:"finishedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	Items      []StackJobItemBody   `json:"items" doc:"The steps and services, in the order they were added. Read the steps in run order by following parent: a top-level step, then its children"`
	Output     []StackJobOutputLine `json:"output" doc:"The output log, oldest first: the newest 2,000 lines"`
}

// StackJobOutput wraps StackJobBody for huma.
type StackJobOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackJobBody
}

func registerStackDeploy(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "deployStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/deploys",
		Summary: "Deploy a stack's draft",
		Description: "Takes a revision from the draft and deploys it as a job, answering 202 with the job ID at once; the deploy goes on if the browser closes. " +
			"draftVersion must be the version of the draft you reviewed, or the answer is 409 draft_changed. A draft that started from an older revision than the newest good one (someone deployed since) is 409 concurrent_unacknowledged unless acknowledgeConcurrent is true; a deploy never overwrites a newer one unseen. pins, when sent, are the review's: the deploy uses exactly those digests and makes no registry lookup (422 pins_invalid for one the server would not have chosen). " +
			"On a swarm the job creates the stack's networks, then its secrets and configs under names that end in a short hash of their content, then its services, through one manager, each once and never retried; each carries com.docker.stack.namespace and bayplan.stack.id, and each service com.docker.stack.image with the image as written. " +
			"A secret or config that content changes replaces is removed at the end, once every service uses the new one. " +
			"A compose file that does not load is 422 compose_invalid; one that uses a setting not converted yet is 422 compose_unsupported; one whose check finds an error is 422 stack_invalid. " +
			"A caller below env Admin is refused 403 privileged_compose, with each setting and its line in errors, when the stack asks for host access (a bind of the Docker socket or a host path outside /srv, /mnt and /media, an added capability, privileged, the host network, a device), judged on the values of the stack's variables. " +
			"A name that something Bayplan does not manage holds is 409 stack_name_clash. An environment that is no longer the kind of target the stack was created for is 409 target_changed, and a standalone stack is 409 target_engine_missing. At most 5 jobs of one user run at once (429 too_many_deploys). " +
			"Writes one stack.deployed audit row, with the result, when the job ends. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackDeployInput) (*StackDeployOutput, error) {
			return deployStack(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getStackDeploy", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/deploys/{jobId}",
		Summary: "One deploy job and its steps",
		Description: "The deploy as it stands now. A job is kept for 10 minutes after it ends. A job of another stack, or one that is gone, is 404 stack_job_not_found. " +
			"Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackJobPathInput) (*StackJobOutput, error) {
			return getStackJob(ctx, d, in)
		})

	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/stacks/{stackId}/deploys/{jobId}/stream",
		rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			serveStackJobStream(w, req, d, lim)
		}))
}

const (
	stackJobGone = "This deploy is no longer here. Deploys are kept for 10 minutes after they end."
)

// serveStackJobStream follows one deploy through the shared job stream: the job as it stands, an
// item frame for each change and a done frame at the end, resumed from Last-Event-ID. Only a job of
// this stack on this environment can be followed; any other is the same 404 as one that is gone.
// The deploy does not depend on the stream: closing it stops nothing.
func serveStackJobStream(w http.ResponseWriter, req *http.Request, d StacksDeps, lim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if d.Jobs == nil {
		writeJSONProblem(w, http.StatusServiceUnavailable, problemJobsUnavail, "Deploys are not available right now.")
		return
	}
	stackID, jobID := chi.URLParam(req, "stackId"), chi.URLParam(req, "jobId")
	if _, err := d.repo().Stack(ctx, env.ID, stackID); err != nil {
		writeJSONProblem(w, http.StatusNotFound, problemStackJobNotFound, stackJobGone)
		return
	}
	if job, ok := d.Jobs.Get(jobID); !ok || !stackJobOf(job, env.ID, stackID) {
		writeJSONProblem(w, http.StatusNotFound, problemStackJobNotFound, stackJobGone)
		return
	}
	serveJobStream(w, req, jobStream{
		Hub: d.Jobs, JobID: jobID, Key: string(p.Type) + ":" + p.ID, Limiter: lim, What: "deploy",
		GoneType: problemStackJobNotFound, GoneDetail: stackJobGone, TooManyType: problemTooManyStreams,
		Heartbeat: d.Heartbeat, MaxLifetime: d.MaxLifetime, Log: d.Log,
		Snapshot: func(j jobs.Job) any { return stackJobBody(j, outputOf(d.Jobs, j.ID)) },
		Encode: func(ev jobs.Event) (any, bool) {
			switch {
			case ev.Type == jobs.EventItem && ev.Item != nil:
				return stackJobItemBody(*ev.Item), true
			case ev.Type == jobs.EventOutput && ev.Output != nil:
				return stackJobOutputLine(*ev.Output), true
			case ev.Type == jobs.EventDone && ev.Job != nil:
				return stackJobBody(*ev.Job, outputOf(d.Jobs, ev.Job.ID)), true
			}
			return nil, false
		},
	})
}

// stackJobOf reports whether a job in the hub is a deploy of that stack on that environment.
func stackJobOf(j jobs.Job, envID, stackID string) bool {
	return j.Kind == stackDeployJobKind && j.EnvID == envID && j.Meta[metaStackID] == stackID
}

// stackJobOwner is who a stack job is attributed to in the hub, as an audit actor.
func stackJobOwner(p *rbac.Principal) string { return string(p.Type) + ":" + p.ID }

func deployStack(ctx context.Context, d StacksDeps, in *StackDeployInput) (*StackDeployOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if d.Jobs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Deploys are not available right now.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	if err := d.ops().CheckTarget(ctx, env.ID, st); err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	draft, err := repo.Draft(ctx, st.ID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	if draft.Version != in.Body.DraftVersion {
		return nil, draftChanged(&stackops.DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version})
	}
	if err := concurrentGate(ctx, d, repo, st.ID, env.ID, draft.BaseSeq, in.Body.AcknowledgeConcurrent); err != nil {
		return nil, err
	}
	mode, update := deployModeOf(in.Body)

	// Load and convert before anything is stored or started: a compose file that cannot be
	// deployed is refused here, with the reason, and leaves no revision behind.
	loaded, err := stacks.Load(ctx, stacks.LoadInput{Name: st.Name, Target: st.Target, Copy: draft.Copy})
	if err != nil {
		return nil, composeProblem(err)
	}
	files, vars := stacks.NewFileSet(draft.Copy.Files), make(map[string]string, len(draft.Copy.Vars))
	for _, v := range draft.Copy.Vars {
		vars[v.Name] = v.Value
	}
	objs, err := stacks.ResolveObjects(loaded.Project, files, vars)
	if err != nil {
		return nil, objectProblem(err)
	}
	plan, err := convert.Convert(loaded.Project, convert.Options{StackName: st.Name, StackID: st.ID, Objects: stackops.PlannedObjects(st.Name, objs)})
	if err != nil {
		return nil, convertProblem(err)
	}
	// The images the review chose are checked now, before anything is stored, against what the
	// server decides itself: a pin it would not have chosen is refused and nothing starts.
	if in.Body.Pins != nil {
		if err := d.ops().ValidatePins(ctx, env.ID, st, plan.Services, repo, update, in.Body.Pins); err != nil {
			return nil, pinsProblem(d, env.ID, err)
		}
	}

	// The review's checks are made again on what is about to be deployed (07-18 makes them once
	// more under the lock): the errors of the check, what the caller may not deploy, and a name
	// that something Bayplan does not manage holds. Nothing is stored or sent until they pass.
	checked := stacks.Check(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: draft.Copy})
	if checked.Problems.Errors() > 0 || checked.Loaded == nil {
		return nil, stackInvalid(checked.Problems, "The stack has errors, so it can't be deployed. Review it and fix the problems it lists.")
	}
	caller := stackops.Caller{Principal: p, Actor: p.Name}
	pol, err := d.ops().Policy(ctx, caller, env.ID, draft.Copy, plan)
	if err != nil {
		return nil, stackInternal(d, "policy", env.ID, err)
	}
	if pol.Refused {
		return nil, policyProblem(pol)
	}
	foreign, err := d.ops().ForeignStack(ctx, env.ID, st.Target, st.Name, st.ID)
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	if foreign {
		return nil, nameClashProblem(st.Name)
	}

	rev, err := repo.NewRevision(ctx, st.ID, p.Name)
	if err != nil {
		return nil, stackInternal(d, "revision", env.ID, err)
	}
	// The draft is now the content of this revision, so it starts from it: the next review does not
	// take this deploy for one made by someone else.
	if err := repo.RebaseDraft(ctx, st.ID, rev.Seq); err != nil {
		_ = repo.SetOutcome(context.WithoutCancel(ctx), rev.ID, stackops.OutcomeFailed, "", "")
		return nil, stackInternal(d, "rebase the draft", env.ID, err)
	}
	// The masked rendering is what the next review compares its own with. It is stored before the
	// job starts, with the images this deploy was told to use, and again when the deploy ends good,
	// with the images it ran with.
	rendering := revisionRendering{repo: repo, loaded: checked.Loaded, masker: stacks.NewMasker(draft.Copy.Vars, draft.Copy.Files)}
	var startPins map[string]stacks.LivePin
	if in.Body.Pins != nil {
		startPins = stackops.LivePins(in.Body.Pins)
	} else if startPins, err = repo.GoodPins(ctx, st.ID); err != nil {
		d.Log.Warn("could not read the pins of a stack for its rendering", "stack", st.ID, "error", err)
	}
	if err := rendering.store(ctx, rev.ID, startPins); err != nil {
		d.Log.Warn("could not store the rendering of a revision", "stack", st.ID, "revision", rev.Seq, "error", err)
	}

	// The job needs the deploy row's ID and the row needs the job's ID, so the run waits on
	// ready until the row is written.
	ready := make(chan struct{})
	var deploy stackops.DeployRow
	reqCtx := context.WithoutCancel(ctx)
	actor, userAgent := p.AuditActor(), in.UserAgent
	in2 := stackops.SwarmDeployInput{
		EnvID: env.ID, Stack: st, Project: loaded.Project, Files: files, Vars: vars,
		Caller: caller, Masker: rendering.masker,
		Pins: repo, RevisionID: rev.ID, Update: update, Reviewed: in.Body.Pins,
	}
	in2.OnDone = func(res stackops.DeployResult) {
		recordDeployEnd(reqCtx, d, actor, userAgent, st, rev, deploy, mode, res, rendering)
	}
	spec := jobs.Spec{
		Kind: stackDeployJobKind, EnvID: env.ID, Owner: stackJobOwner(p),
		Meta:  map[string]string{metaStackID: st.ID, metaStackName: st.Name, metaMode: mode, metaStartedBy: p.Name},
		Items: stackops.DeployItemSpecs(),
	}
	job, err := d.Jobs.Start(spec, func(jctx context.Context, r *jobs.Reporter) {
		select {
		case <-ready:
		case <-jctx.Done():
			return
		}
		if deploy.ID == "" {
			return // the row could not be written: the handler cancelled the job
		}
		d.ops().DeploySwarm(jctx, in2, r)
	})
	if errors.Is(err, jobs.ErrTooMany) {
		_ = repo.SetOutcome(reqCtx, rev.ID, stackops.OutcomeFailed, "", "")
		return nil, httpx.Problem(http.StatusTooManyRequests, problemTooManyDeploys, "You already have 5 jobs running. Wait for one to finish.")
	}
	if err != nil {
		_ = repo.SetOutcome(reqCtx, rev.ID, stackops.OutcomeFailed, "", "")
		return nil, stackInternal(d, "start the deploy job", env.ID, err)
	}
	row, err := repo.StartDeploy(reqCtx, st.ID, env.ID, stackops.DeployKindDeploy, rev.Seq, job.ID, p.Name)
	if err != nil {
		_ = d.Jobs.Cancel(job.ID)
		close(ready)
		_ = repo.SetOutcome(reqCtx, rev.ID, stackops.OutcomeFailed, "", "")
		return nil, stackInternal(d, "record the deploy", env.ID, err)
	}
	deploy = row
	close(ready)
	return &StackDeployOutput{CacheControl: "no-store", Body: StackDeployBody{JobID: job.ID, Revision: rev.Seq}}, nil
}

// revisionRendering stores the masked rendering of a revision: the stack as the deploy sent it, with
// each image shown as the pin it ran with.
type revisionRendering struct {
	repo   stackops.Repo
	loaded *stacks.Loaded
	masker stacks.Masker
}

func (r revisionRendering) store(ctx context.Context, snapshotID string, pins map[string]stacks.LivePin) error {
	out, err := stacks.Render(stacks.RenderInput{Loaded: r.loaded, Pins: pins, Masker: r.masker})
	if err != nil {
		return err
	}
	return r.repo.SetRendered(ctx, snapshotID, out)
}

// concurrentGate refuses a deploy of a draft that started before the newest good revision was
// deployed, unless the person has said they saw what changed. The browser asks for the
// acknowledgement too, but a deploy is never an overwrite of a newer one unseen because of a
// checkbox alone (D-09).
func concurrentGate(ctx context.Context, d StacksDeps, repo stackops.Repo, stackID, envID string, baseSeq int64, acknowledged bool) error {
	good, ok, err := repo.GoodRevisionInfo(ctx, stackID)
	if err != nil {
		return stackInternal(d, "read the running revision", envID, err)
	}
	if !ok || good.Seq <= baseSeq || acknowledged {
		return nil
	}
	return &huma.ErrorModel{
		Type: problemConcurrentUnacknowledged, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: good.By + " deployed this stack after you started editing. Review what changed and confirm that you want to deploy your draft over it.",
		Errors: []*huma.ErrorDetail{{Location: "acknowledgeConcurrent", Message: "revision " + itoa(good.Seq)}},
	}
}

// recordDeployEnd stores how a deploy ended: the revision's outcome, the deploy row and the
// audit row. It runs before the job's last step is finished, so a job that reads as finished has
// already been recorded.
func recordDeployEnd(ctx context.Context, d StacksDeps, actor audit.Actor, userAgent string, st stackops.Stack, rev stackops.Snapshot, deploy stackops.DeployRow, mode string, res stackops.DeployResult, rendering revisionRendering) {
	state, outcome, result := stackops.DeployDone, stackops.OutcomeGood, audit.ResultSuccess
	resultWord := "done"
	switch {
	case res.Code == stackops.CodeCancelled:
		state, outcome, result, resultWord = stackops.DeployCancelled, stackops.OutcomeFailed, audit.ResultFailure, "cancelled"
	case !res.OK():
		state, outcome, result, resultWord = stackops.DeployFailed, stackops.OutcomeFailed, audit.ResultFailure, "failed"
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	repo := d.repo()
	if err := repo.SetOutcome(wctx, rev.ID, outcome, "", ""); err != nil {
		d.Log.Error("could not mark a revision", "stack", st.ID, "revision", rev.Seq, "error", err)
	} else if outcome == stackops.OutcomeGood {
		// The revision is the newest good one now, so its pins are the images it ran with.
		pins, perr := repo.GoodPins(wctx, st.ID)
		if perr == nil {
			perr = rendering.store(wctx, rev.ID, pins)
		}
		if perr != nil {
			d.Log.Warn("could not store the rendering of a revision", "stack", st.ID, "revision", rev.Seq, "error", perr)
		}
	}
	if err := repo.FinishDeploy(wctx, deploy.ID, state, res.FailedService, res.Code); err != nil {
		d.Log.Error("could not finish a deploy row", "stack", st.ID, "deploy", deploy.ID, "error", err)
	}
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: actor, Action: audit.ActionStackDeployed, TargetType: stackTargetType, TargetID: st.ID, EnvID: st.EnvID,
		Result: result, Reason: res.Code, IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: map[string]string{
			"stack": st.Name, "mode": mode, "result": resultWord, "revision": itoa(rev.Seq), "job": deploy.JobID,
			"services": itoa(int64(res.Services + res.Updated)), "code": res.Code,
		},
	}); err != nil {
		d.Log.Error("could not audit a stack deploy", "stack", st.ID, "error", err)
	}
}

// targetProblem is the answer to an environment that is no longer what the stack was made for, or
// that this version cannot deploy to; any other error is a failure to read the environment.
func targetProblem(d StacksDeps, envID string, err error) error {
	switch {
	case errors.Is(err, stackops.ErrTargetChanged):
		return httpx.Problem(http.StatusConflict, problemTargetChanged,
			"This environment is no longer the kind of target this stack was created for, so it can't be reviewed or deployed here. Create a new stack for it.")
	case errors.Is(err, stackops.ErrTargetEngineMissing):
		return httpx.Problem(http.StatusConflict, problemTargetEngineAbsent, "This version cannot deploy to a standalone Docker host yet.")
	case errors.Is(err, stackops.ErrNoPolicy):
		return stackInternal(d, "policy", envID, err)
	}
	return snapshotProblem(d.Log, envID, err)
}

// nameClashProblem is the 409 for a name that something Bayplan does not manage holds.
func nameClashProblem(name string) error {
	return &huma.ErrorModel{
		Type: problemNameClash, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: stackops.NameClashMessage(name),
		Errors: []*huma.ErrorDetail{{Location: "name", Message: name}},
	}
}

// policyProblem is the 403 for a stack that uses host access the caller may not deploy, with each
// setting and its place (UI-SPEC, "Host escalation in the rendered stack").
func policyProblem(pol stackops.PolicyResult) error {
	if len(pol.Entries) == 0 {
		return &huma.ErrorModel{
			Type: problemComposeInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: "The compose file can't be checked for host access, so it can't be deployed.",
		}
	}
	lines := make([]string, 0, len(pol.Entries))
	details := make([]*huma.ErrorDetail, 0, len(pol.Entries))
	for _, e := range pol.Entries {
		at := e.File + ":" + itoa(int64(e.Line))
		lines = append(lines, e.Text+" at "+at)
		details = append(details, &huma.ErrorDetail{Location: at, Message: e.Text})
	}
	return &huma.ErrorModel{
		Type: rbac.ReasonPrivilegedCompose, Title: http.StatusText(http.StatusForbidden), Status: http.StatusForbidden,
		Detail: "This stack uses settings that need Admin: " + strings.Join(lines, "; ") + ". Ask an administrator to deploy it.",
		Errors: details,
	}
}

// deployModeOf reads the mode of a deploy request and the services an update names.
func deployModeOf(b StackDeployRequest) (mode string, update map[string]bool) {
	if b.Mode != deployModeUpdate {
		return deployModeDeploy, nil
	}
	update = make(map[string]bool, len(b.Services))
	for _, s := range b.Services {
		update[s] = true
	}
	return deployModeUpdate, update
}

// draftChanged is the 409 for a deploy of a draft that moved since the review.
func draftChanged(c *stackops.DraftConflictError) error {
	e, _ := draftConflict(c).(*huma.ErrorModel)
	e.Type = problemDraftChanged
	e.Detail = "The draft changed after you reviewed it. Review it again before you deploy."
	return e
}

// pinsProblem is the answer to reviewed pins that did not check out: 422 pins_invalid for a pin
// the server would not have chosen, the engine's own class for a read that failed.
func pinsProblem(d StacksDeps, envID string, err error) error {
	var invalid *stackops.PinsInvalidError
	if errors.As(err, &invalid) {
		return &huma.ErrorModel{
			Type: problemPinsInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: "The images in this request don't match what the deploy would use for " + invalid.Service + ". Review the stack again.",
			Errors: []*huma.ErrorDetail{{Location: "pins", Message: invalid.Service}},
		}
	}
	return snapshotProblem(d.Log, envID, err)
}

// objectProblem is the 422 for a secret or config that has no usable content.
func objectProblem(err error) error {
	return &huma.ErrorModel{
		Type: problemComposeInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: stackops.ObjectErrorDetail(err),
	}
}

// composeProblem is the 422 for a compose file that does not load.
func composeProblem(err error) error {
	if errors.Is(err, stacks.ErrTooLarge) {
		return httpx.Problem(http.StatusUnprocessableEntity, problemStackFileInvalid, "compose.yaml is over 1 MiB.")
	}
	return &huma.ErrorModel{
		Type: problemComposeInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: "compose.yaml does not load: " + strings.TrimPrefix(err.Error(), "stacks: load "+stacks.ComposeFile+": "),
	}
}

// convertProblem is the 422 for a compose file the converter cannot turn into swarm objects.
func convertProblem(err error) error {
	var nc *convert.NotConvertedError
	if errors.As(err, &nc) {
		details := make([]*huma.ErrorDetail, 0, len(nc.Fields))
		for _, f := range nc.Fields {
			details = append(details, &huma.ErrorDetail{Location: nc.Service, Message: f})
		}
		return &huma.ErrorModel{
			Type: problemComposeUnsupported, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: "Service " + nc.Service + " uses " + strings.Join(nc.Fields, ", ") + ", which this version of Bayplan cannot deploy yet.", Errors: details,
		}
	}
	if errors.Is(err, convert.ErrNoImage) {
		return &huma.ErrorModel{
			Type: problemComposeInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: "A service has no image. Bayplan deploys images, it does not build them.",
		}
	}
	return &huma.ErrorModel{
		Type: problemComposeInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: strings.TrimPrefix(err.Error(), "convert: "),
	}
}

func getStackJob(ctx context.Context, d StacksDeps, in *StackJobPathInput) (*StackJobOutput, error) {
	if d.Jobs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Deploys are not available right now.")
	}
	if _, err := d.repo().Stack(ctx, in.EnvID, in.StackID); err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	job, ok := d.Jobs.Get(in.JobID)
	if !ok || !stackJobOf(job, in.EnvID, in.StackID) {
		return nil, httpx.Problem(http.StatusNotFound, problemStackJobNotFound, "This deploy is no longer here. Deploys are kept for 10 minutes after they end.")
	}
	return &StackJobOutput{CacheControl: "no-store", Body: stackJobBody(job, outputOf(d.Jobs, job.ID))}, nil
}

func stackJobBody(j jobs.Job, output []jobs.Output) StackJobBody {
	b := StackJobBody{
		ID: j.ID, StackID: j.Meta[metaStackID], Mode: j.Meta[metaMode], StartedBy: j.Meta[metaStartedBy], State: j.State,
		CreatedAt: rfc3339(j.CreatedAt), FinishedAt: rfc3339(j.FinishedAt), Items: make([]StackJobItemBody, 0, len(j.Items)),
		Output: make([]StackJobOutputLine, 0, len(output)),
	}
	for _, it := range j.Items {
		b.Items = append(b.Items, stackJobItemBody(it))
	}
	for _, o := range output {
		b.Output = append(b.Output, stackJobOutputLine(o))
	}
	return b
}

func stackJobOutputLine(o jobs.Output) StackJobOutputLine {
	return StackJobOutputLine{Seq: o.Seq, At: rfc3339(o.At), Text: o.Text}
}

// outputOf is the output log of a job, empty when the hub no longer has it.
func outputOf(hub *jobs.Hub, id string) []jobs.Output {
	out, _ := hub.Output(id)
	return out
}

func stackJobItemBody(it jobs.Item) StackJobItemBody {
	res := it.Result
	if res == nil {
		res = map[string]string{}
	}
	return StackJobItemBody{
		ID: it.ID, Label: it.Label, Parent: it.Parent, Kind: it.Kind, State: it.State, Code: it.Code, Message: it.Message,
		Detail: it.Detail, Current: it.Current, Total: it.Total, Unit: it.Unit, Result: res,
	}
}
