// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

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

	deployModeDeploy = "deploy"
	deployModeUpdate = "update"

	problemDraftChanged = "draft_changed"
	problemPinsInvalid  = "pins_invalid"
)

// StackDeployRequest is the body of POST .../deploys.
type StackDeployRequest struct {
	DraftVersion int64             `json:"draftVersion" minimum:"1" doc:"The version of the draft you reviewed and mean to deploy. If the draft moved since, the answer is 409 draft_changed and nothing is deployed"`
	Pins         map[string]string `json:"pins,omitempty" doc:"The images the review chose, by swarm service name (the review's pins). The deploy uses exactly these digests and asks no registry. Each is checked against what the server decides itself: a service that keeps its running digest must be sent that digest, and a looked-up one a sha256 digest of the repository its reference names; anything else is 422 pins_invalid. Left out, the deploy looks the images up itself"`
	Mode         string            `json:"mode,omitempty" enum:"deploy,update" doc:"deploy (the default), or update to move the services named in services to a newly looked-up digest"`
	Services     []string          `json:"services,omitempty" maxItems:"200" doc:"For mode update: the services to move, by the name in the compose file or by the swarm name"`
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

// StackJobItemBody is one step of a deploy.
type StackJobItemBody struct {
	ID      string            `json:"id" doc:"The step: check, networks, objects, services or cleanup"`
	Label   string            `json:"label" doc:"The step's name"`
	State   string            `json:"state" enum:"waiting,running,done,failed,cancelled" doc:"Where the step is"`
	Code    string            `json:"code" doc:"A stable word for how a step failed or was not run, empty otherwise"`
	Message string            `json:"message" doc:"A sentence for the code. The SPA words the code itself"`
	Current int64             `json:"current" doc:"How many of total were done"`
	Total   int64             `json:"total" doc:"How many there are to do, 0 until known"`
	Unit    string            `json:"unit" doc:"What current and total count, or empty"`
	Result  map[string]string `json:"result" doc:"What a finished step produced, like the counts of services created and updated"`
}

// StackJobBody is a deploy job.
type StackJobBody struct {
	ID         string             `json:"id" doc:"The job ID"`
	StackID    string             `json:"stackId" doc:"The stack being deployed"`
	Mode       string             `json:"mode" enum:"deploy,update" doc:"What the operation is"`
	State      string             `json:"state" enum:"running,done,partial,failed,cancelled" doc:"running until every step has ended, then done when every step is"`
	CreatedAt  string             `json:"createdAt" doc:"When the job started, RFC 3339 UTC"`
	FinishedAt string             `json:"finishedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	Items      []StackJobItemBody `json:"items" doc:"The steps, in order"`
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
			"draftVersion must be the version of the draft you reviewed, or the answer is 409 draft_changed. pins, when sent, are the review's: the deploy uses exactly those digests and makes no registry lookup (422 pins_invalid for one the server would not have chosen). " +
			"On a swarm the job creates the stack's networks, then its secrets and configs under names that end in a short hash of their content, then its services, through one manager, each once and never retried; each carries com.docker.stack.namespace and bayplan.stack.id, and each service com.docker.stack.image with the image as written. " +
			"A secret or config that content changes replaces is removed at the end, once every service uses the new one. " +
			"A compose file that does not load is 422 compose_invalid; one that uses a setting not converted yet is 422 compose_unsupported. A standalone stack is 409 target_engine_missing. At most 5 jobs of one user run at once (429 too_many_deploys). " +
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
}

// stackJobOwner is who a stack job is attributed to in the hub, as an audit actor.
func stackJobOwner(p *rbac.Principal) string { return string(p.Type) + ":" + p.ID }

// jobStepLabels name the steps of a swarm deploy.
var jobStepLabels = []jobs.ItemSpec{
	{ID: stackops.ItemCheck, Label: "Check the stack"},
	{ID: stackops.ItemLookup, Label: "Look up images"},
	{ID: stackops.ItemNetworks, Label: "Networks"},
	{ID: stackops.ItemObjects, Label: "Secrets and configs"},
	{ID: stackops.ItemServices, Label: "Services"},
	{ID: stackops.ItemCleanup, Label: "Clean up"},
}

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
	if st.Target != stacks.TargetSwarm {
		return nil, httpx.Problem(http.StatusConflict, problemTargetEngineAbsent, "This version cannot deploy to a standalone Docker host yet.")
	}
	draft, err := repo.Draft(ctx, st.ID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	if draft.Version != in.Body.DraftVersion {
		return nil, draftChanged(&stackops.DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version})
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

	rev, err := repo.NewRevision(ctx, st.ID, p.Name)
	if err != nil {
		return nil, stackInternal(d, "revision", env.ID, err)
	}

	// The job needs the deploy row's ID and the row needs the job's ID, so the run waits on
	// ready until the row is written.
	ready := make(chan struct{})
	var deploy stackops.DeployRow
	reqCtx := context.WithoutCancel(ctx)
	actor, userAgent := p.AuditActor(), in.UserAgent
	in2 := stackops.SwarmDeployInput{
		EnvID: env.ID, Stack: st, Project: loaded.Project, Files: files, Vars: vars,
		Caller: stackops.Caller{Principal: p, Actor: p.Name},
		Pins:   repo, RevisionID: rev.ID, Update: update, Reviewed: in.Body.Pins,
	}
	in2.OnDone = func(res stackops.DeployResult) {
		recordDeployEnd(reqCtx, d, actor, userAgent, st, rev, deploy, mode, res)
	}
	spec := jobs.Spec{
		Kind: stackDeployJobKind, EnvID: env.ID, Owner: stackJobOwner(p),
		Meta:  map[string]string{metaStackID: st.ID, metaStackName: st.Name, metaMode: mode},
		Items: jobStepLabels,
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

// recordDeployEnd stores how a deploy ended: the revision's outcome, the deploy row and the
// audit row. It runs before the job's last step is finished, so a job that reads as finished has
// already been recorded.
func recordDeployEnd(ctx context.Context, d StacksDeps, actor audit.Actor, userAgent string, st stackops.Stack, rev stackops.Snapshot, deploy stackops.DeployRow, mode string, res stackops.DeployResult) {
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
	if !ok || job.Kind != stackDeployJobKind || job.EnvID != in.EnvID || job.Meta[metaStackID] != in.StackID {
		return nil, httpx.Problem(http.StatusNotFound, problemStackJobNotFound, "This deploy is no longer here. Deploys are kept for 10 minutes after they end.")
	}
	return &StackJobOutput{CacheControl: "no-store", Body: stackJobBody(job)}, nil
}

func stackJobBody(j jobs.Job) StackJobBody {
	b := StackJobBody{
		ID: j.ID, StackID: j.Meta[metaStackID], Mode: j.Meta[metaMode], State: j.State,
		CreatedAt: rfc3339(j.CreatedAt), FinishedAt: rfc3339(j.FinishedAt), Items: make([]StackJobItemBody, 0, len(j.Items)),
	}
	for _, it := range j.Items {
		res := it.Result
		if res == nil {
			res = map[string]string{}
		}
		b.Items = append(b.Items, StackJobItemBody{
			ID: it.ID, Label: it.Label, State: it.State, Code: it.Code, Message: it.Message,
			Current: it.Current, Total: it.Total, Unit: it.Unit, Result: res,
		})
	}
	return b
}
