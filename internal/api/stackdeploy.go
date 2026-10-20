// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

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
	// stackDeployJobKind is the Kind of a stack deploy in the jobs hub; a stop and a start are stack_stop and
	// stack_start (stacklifecycle.go).
	stackDeployJobKind = "stack_deploy"

	metaStackID   = "stackId"
	metaStackName = "stackName"
	metaMode      = "mode"
	metaStartedBy = "startedBy"

	deployModeDeploy   = "deploy"
	deployModeUpdate   = "update"
	deployModeRollback = "rollback"

	problemDraftChanged             = "draft_changed"
	problemPinsInvalid              = "pins_invalid"
	problemServicesInvalid          = "services_invalid"
	problemConcurrentUnacknowledged = "concurrent_unacknowledged"
	problemNoPreviousVersion        = "no_previous_version"
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

// StackRollbackInput is the path of the roll back route. It has no body: a roll back is never given
// anything to restore, it restores the last version that ran well.
type StackRollbackInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// noPreviousVersion is the sentence for a stack with no earlier version that ran well.
const noPreviousVersion = "There's no earlier deployed version to roll back to."

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
	ID      string            `json:"id" doc:"The item: check, lookup, networks, objects (volumes on a standalone host), services or cleanup, or one made for an object, service, task or container of the deploy, like service:web, task:<task id> or container:<name>, or on a standalone host pull:<image> under lookup for an image it pulls, and clean-container:<name> or clean-network:<name> under cleanup for what a deploy that stopped removed again"`
	Label   string            `json:"label" doc:"The item's name: a step's words, or a service's name"`
	Parent  string            `json:"parent" doc:"The ID of the item this one belongs to, empty for a top-level step. A service is under services; a network is under networks, a volume under volumes, a secret or config under objects, an old secret or config or a container the file no longer declares under cleanup, a task or a container under its service"`
	Kind    string            `json:"kind" doc:"step for a line of the step list, service for a service row, task for a swarm task listed under its service while the deploy waits for the service to settle, group for an item that only holds steps and is not shown itself"`
	State   string            `json:"state" enum:"waiting,running,done,failed,skipped,cancelled" doc:"Where the item is. skipped is an item that was not run because an earlier one failed"`
	Code    string            `json:"code" doc:"A stable word for how an item failed or was not run, empty otherwise"`
	Message string            `json:"message" doc:"A sentence for the code. The SPA words the code itself"`
	Detail  string            `json:"detail" doc:"The engine's own words about a failed item, cleaned of control characters, clipped to 2,048 characters and with the stack's secret values hidden. Empty when there are none"`
	Current int64             `json:"current" doc:"How many of total were done. On a service row, how many tasks run on its current spec"`
	Total   int64             `json:"total" doc:"How many there are to do, 0 until known. On a service row, how many tasks must run for it to settle"`
	Unit    string            `json:"unit" doc:"What current and total count, or empty: services for the services step, tasks for a service row of a swarm, containers for one of a standalone host"`
	Result  map[string]string `json:"result" doc:"What a step, service or task produced or shows. A service row: action (created or updated), settled (true once its tasks run, false when the deploy did not wait for it), running and desired at the end, phase pulling while a task prepares its image, minutes on a timeout. A task: state (the swarm's) and node (the host name). On a standalone host, a service row: action (created, updated or unchanged), phase (pulling, creating, starting, running or healthy, or failed) and, once done, running and desired; a container row: state, the last word the Compose SDK gave it (creating, starting, running, healthy ...). A pull step: image, ref (the registry written out, without the digest), the credential it used (credential, credentialId, credentialLayer) and, once pulled, digest and upToDate. A cleanup step of a deploy that stopped: cleaned (true when removed), kind (container or network), name and left (in_use or failed) when it stays. The steps: the counts of services created and updated"`
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
	Mode       string               `json:"mode" enum:"deploy,update,rollback,stop,start,remove" doc:"What the operation is: a deploy of the draft, an update of images, a roll back to the last version that ran well, a stop or a start of the stack, or its removal"`
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

// StackCancelBody is the answer to a cancel: the deploy is being stopped.
type StackCancelBody struct {
	JobID string `json:"jobId" doc:"The deploy that is being stopped: follow it at GET .../deploys/{jobId} or its stream until it reads cancelled"`
}

// StackCancelOutput wraps StackCancelBody for huma.
type StackCancelOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackCancelBody
}

// deployCancels remembers who cancelled a deploy, so the audit row written when the deploy ends can
// say it: the person who started a deploy is not always the person who stopped it.
type deployCancels struct {
	mu sync.Mutex
	by map[string]string
}

func (c *deployCancels) note(jobID, who string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.by[jobID] = who
}

func (c *deployCancels) forget(jobID string) { _ = c.take(jobID) }

// take returns who cancelled the job and forgets it, empty when nobody did.
func (c *deployCancels) take(jobID string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	who := c.by[jobID]
	delete(c.by, jobID)
	return who
}

func registerStackDeploy(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "deployStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/deploys",
		Summary: "Deploy a stack's draft",
		Description: "Takes a revision from the draft and deploys it as a job, answering 202 with the job ID at once; the deploy goes on if the browser closes. " +
			"draftVersion must be the version of the draft you reviewed, or the answer is 409 draft_changed. A draft that started from an older revision than the newest good one (someone deployed since) is 409 concurrent_unacknowledged unless acknowledgeConcurrent is true; a deploy never overwrites a newer one unseen. pins, when sent, are the review's: the deploy uses exactly those digests and makes no registry lookup (422 pins_invalid for one the server would not have chosen). " +
			"On a standalone host the job checks the stack, pins each image the same way (a service that runs the text it is deployed with keeps its digest; a new or changed one is looked up on the host with the stored credential), pulls each image of a container it will create or recreate that the host does not have, with the stored credential and never the SDK's anonymous pull (a Pull step under the lookup with the bytes done of the total; a refused pull ends it with its pull_denied, pull_unauthorized, image_not_found, rate_limited or registry_tls code and nothing is created), and runs the Compose SDK's up over a guarded client: containers created, those whose configuration changed recreated, the others left alone, those of services the file no longer declares removed, and a wait until they run; they carry com.docker.compose.project and bayplan.stack.id, and bayplan.stack.image holds the image as written. A stack secret or config is written into the container, not mounted from a file. " +
			"On a swarm the job checks the stack, looks up each image and pins it to a digest (the one the service runs, or a fresh lookup through a manager), creates the stack's networks, then its secrets and configs under names that end in a short hash of their content, then its services, through one manager, each once and never retried; each carries com.docker.stack.namespace and bayplan.stack.id, and each service com.docker.stack.image with the image as written. " +
			"A service is done when it converges, not when the swarm took it: the job polls each service every 2 seconds, up to BAYPLAN_STACK_CONVERGE_TIMEOUT (10 minutes by default), until its tasks run on its current spec and its update completed. " +
			"A secret or config that content changes replaces is removed at the end, once every service uses the new one. " +
			"A compose file that does not load is 422 compose_invalid; one that uses a setting a swarm can't deploy at all is 422 compose_unsupported; one whose check finds an error is 422 stack_invalid. " +
			"A caller below env Admin is refused 403 privileged_compose, with each setting and its line in errors, when the stack asks for host access (a bind of the Docker socket or a host path outside /srv, /mnt and /media, an added capability, privileged, the host network, a device), judged on the values of the stack's variables. " +
			"A name that something Bayplan does not manage holds (a swarm namespace or a Compose project) is 409 stack_name_clash. An environment that is no longer the kind of target the stack was created for is 409 target_changed. At most 5 jobs of one user run at once (429 too_many_deploys). " +
			"Only one stack operation at a time runs on an environment: while one does, on any stack, a deploy is refused 409 operation_locked with errors naming the stack, the job and when it started, and nothing is stored. " +
			"Just before it writes anything the job judges the host access of the stack again for the caller, so a grant removed since the request still stops it (the check step fails needs_admin). " +
			"With mode update, only the services named in services take a newly looked-up digest (the ones the review chose, sent as pins) and every other service keeps the digest it runs; a request that names no service, or one the stack does not have, is 422 services_invalid and nothing is stored. " +
			"Writes one stack.deployed audit row, with the mode and the result, when the job ends. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackDeployInput) (*StackDeployOutput, error) {
			return deployStack(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "rollbackStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/rollback",
		Summary: "Roll a stack back to the version that ran before",
		Description: "Redeploys the last version of the stack that ran well, as a job, answering 202 with the job ID at once. " +
			"That is the newest revision whose deploy succeeded, or, when the newest revision is itself the one that runs, the good one before it. " +
			"It takes a revision of its own from it (the files, the variables and the images it ran) and runs the same swarm deploy on it, so it is audited as stack.deployed with the mode rollback. " +
			"It uses exactly the images that revision ran and looks nothing up: a tag that has moved since is not followed. The draft is not touched. " +
			"A stack with no earlier version that ran well is 409 no_previous_version. " +
			"Everything a deploy is judged on is judged again: a revision that uses host access is refused 403 privileged_compose below env Admin, a stack that no longer loads is 422, and while another stack operation runs on the environment the answer is 409 operation_locked and nothing is stored. " +
			"Rolling back a stack of a standalone host is 409 target_engine_missing: its engine is not in this version. A roll back is only ever sent by a person; nothing rolls a stack back by itself. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackRollbackInput) (*StackDeployOutput, error) {
			return rollbackStack(ctx, d, in)
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

	httpx.Register(r, huma.Operation{
		OperationID: "cancelStackDeploy", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/deploys/{jobId}/cancel",
		Summary: "Cancel a deploy that is running",
		Description: "Asks the deploy to stop at the step it has reached and answers 202 at once; follow the job until it reads cancelled. " +
			"A service being written when the cancel arrives is finished and no other is started; services already updated stay updated and nothing is rolled back. " +
			"The networks, secrets and configs this deploy created that no service uses are removed again. " +
			"The deploy is recorded in the audit log as stack.deployed with the result cancelled, and who cancelled it. " +
			"A job that has ended answers 409 job_finished and nothing changes, so cancelling twice is safe; a job of another stack, or one that is gone, is 404 stack_job_not_found. A cancel is a change and is never retried. " +
			"Needs stacks.deploy (Operator), not necessarily the person who started the deploy.",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackJobPathInput) (*StackCancelOutput, error) {
			return cancelStackJob(ctx, d, in)
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
	// The stack's record may be gone (a removal deletes it): the job names the stack and the environment it ran on,
	// so a job of this stack can be followed to its end all the same.
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

// cancelStackJob asks a running deploy of this stack to stop. The job's context ends; the deploy
// finishes the write it is on, cleans up and ends the job (stackops.DeploySwarm).
func cancelStackJob(ctx context.Context, d StacksDeps, in *StackJobPathInput) (*StackCancelOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if d.Jobs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Deploys are not available right now.")
	}
	gone := httpx.Problem(http.StatusNotFound, problemStackJobNotFound, stackJobGone)
	job, ok := d.Jobs.Get(in.JobID)
	if !ok || !stackJobOf(job, in.EnvID, in.StackID) {
		// Which 404 it is depends on the stack; a job of this stack needs no stack record (a removal deletes it).
		if _, err := d.repo().Stack(ctx, in.EnvID, in.StackID); err != nil {
			return nil, stackLookupProblem(d, in.EnvID, err)
		}
		return nil, gone
	}
	// Who cancelled is noted first: the deploy can end, and be audited, the moment it is cancelled.
	d.cancels.note(in.JobID, p.Name)
	switch err := d.Jobs.Cancel(in.JobID); {
	case errors.Is(err, jobs.ErrFinished):
		d.cancels.forget(in.JobID)
		return nil, httpx.Problem(http.StatusConflict, problemJobFinished, "This deploy has already ended.")
	case errors.Is(err, jobs.ErrNotFound):
		d.cancels.forget(in.JobID)
		return nil, gone
	case err != nil:
		d.cancels.forget(in.JobID)
		d.Log.Error("could not cancel a stack deploy", "job", in.JobID, "error", err)
		return nil, huma.Error500InternalServerError("internal error")
	}
	return &StackCancelOutput{CacheControl: "no-store", Body: StackCancelBody{JobID: in.JobID}}, nil
}

// stackJobOf reports whether a job in the hub is a stack operation (a deploy, a roll back, a stop or a start) on
// that stack of that environment: they share one console and one stream.
func stackJobOf(j jobs.Job, envID, stackID string) bool {
	return strings.HasPrefix(j.Kind, stackJobKindPrefix) && j.EnvID == envID && j.Meta[metaStackID] == stackID
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
	if st.Target == stacks.TargetStandalone {
		return deployStandaloneStack(ctx, d, p, env, st, draft, in, mode, update)
	}

	// The draft is judged once, by the check pipeline the review ran, before anything is stored or
	// started: a compose file that cannot be deployed is refused here, with the problems the review
	// lists, and leaves no revision behind. The project it returns is the one that is converted,
	// rendered and deployed.
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: draft.Copy, Deadline: d.CheckDeadline})
	if err != nil {
		return nil, deployableProblem(d, env.ID, err)
	}
	if mode == deployModeUpdate {
		if err := stackops.ValidateUpdateServices(loaded.Project, st.Target, st.Name, in.Body.Services); err != nil {
			return nil, targetProblem(d, env.ID, err)
		}
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

	// What the caller may not deploy and a name that something Bayplan does not manage holds are
	// judged next (07-18 judges them once more under the lock). Nothing is stored or sent until they pass.
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

	// One stack operation at a time runs on an environment. The lock is taken once the deploy has been
	// judged and before anything is stored, so a refused operation leaves no revision behind; it is
	// held for the life of the job, which frees it however it ends (STK-09, T-07-87).
	release, held, ok := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackID: st.ID, StackName: st.Name, Kind: mode, By: p.Name, Started: time.Now().UTC(),
	})
	if !ok {
		return nil, operationLocked(env.Name, held)
	}
	jobOwnsLock := false
	defer func() {
		if !jobOwnsLock {
			release()
		}
	}()

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
	rendering := revisionRendering{repo: repo, loaded: loaded, masker: stacks.NewMasker(draft.Copy.Vars, draft.Copy.Files)}
	var startPins map[string]stacks.LivePin
	if in.Body.Pins != nil {
		startPins = stackops.LivePins(in.Body.Pins)
	} else if startPins, err = repo.GoodPins(ctx, st.ID); err != nil {
		d.Log.Warn("could not read the pins of a stack for its rendering", "stack", st.ID, "error", err)
	}
	if err := rendering.store(ctx, rev.ID, startPins); err != nil {
		d.Log.Warn("could not store the rendering of a revision", "stack", st.ID, "revision", rev.Seq, "error", err)
	}

	swarmIn := stackops.SwarmDeployInput{
		EnvID: env.ID, Stack: st, Project: loaded.Project, Files: files, Vars: vars, Copy: draft.Copy,
		Caller: caller, Masker: rendering.masker,
		Pins: repo, RevisionID: rev.ID, Update: update, Reviewed: in.Body.Pins,
	}
	out, err := launchStackDeploy(ctx, d, launch{
		p: p, env: env, st: st, rev: rev, mode: mode, kind: stackops.DeployKindDeploy, userAgent: in.UserAgent,
		items: stackops.DeployItemSpecs(),
		run: func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult)) {
			swarmIn.OnDone = onDone
			d.ops().DeploySwarm(jctx, swarmIn, r)
		},
		rendering: rendering, release: release, owned: &jobOwnsLock,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// deployStandaloneStack is deployStack for a stack of a standalone host (STK-02): the draft is judged the
// same way, and the job runs the Compose SDK instead of the swarm engine. Every step before the lock is a
// read or a check; nothing is stored or started until the draft, the objects, the pins, the host-access
// policy and the name have all passed.
func deployStandaloneStack(ctx context.Context, d StacksDeps, p *rbac.Principal, env *httpx.Env, st stackops.Stack, draft stackops.Snapshot, in *StackDeployInput, mode string, update map[string]bool) (*StackDeployOutput, error) {
	repo := d.repo()
	checked := stacks.Check(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: draft.Copy, Deadline: d.CheckDeadline})
	if checked.Loaded == nil {
		return nil, stackInvalid(checked.Problems, "The stack has errors, so it can't be deployed. Review it and fix the problems it lists.")
	}
	if mode == deployModeUpdate {
		if err := stackops.ValidateUpdateServices(checked.Loaded.Project, st.Target, st.Name, in.Body.Services); err != nil {
			return nil, targetProblem(d, env.ID, err)
		}
	}
	if _, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, checked.Loaded.Project, stacks.NewFileSet(draft.Copy.Files), varMap(draft.Copy.Vars)); err != nil {
		return nil, objectProblem(err)
	}
	if checked.Problems.Errors() > 0 {
		return nil, stackInvalid(checked.Problems, "The stack has errors, so it can't be deployed. Review it and fix the problems it lists.")
	}
	node, err := d.ops().StandaloneNode(ctx, env.ID)
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	// The images the review chose are checked now, before anything is stored, against what the server decides
	// itself: a pin it would not have chosen is refused and nothing starts.
	if in.Body.Pins != nil {
		if err := d.ops().ValidateStandalonePins(ctx, env.ID, node, st, checked.Loaded.Project, repo, update, in.Body.Pins); err != nil {
			return nil, pinsProblem(d, env.ID, err)
		}
	}
	caller := stackops.Caller{Principal: p, Actor: p.Name}
	pol, err := d.ops().Policy(ctx, caller, env.ID, draft.Copy, convert.Plan{})
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

	release, held, ok := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackID: st.ID, StackName: st.Name, Kind: mode, By: p.Name, Started: time.Now().UTC(),
	})
	if !ok {
		return nil, operationLocked(env.Name, held)
	}
	jobOwnsLock := false
	defer func() {
		if !jobOwnsLock {
			release()
		}
	}()

	rev, err := repo.NewRevision(ctx, st.ID, p.Name)
	if err != nil {
		return nil, stackInternal(d, "revision", env.ID, err)
	}
	if err := repo.RebaseDraft(ctx, st.ID, rev.Seq); err != nil {
		_ = repo.SetOutcome(context.WithoutCancel(ctx), rev.ID, stackops.OutcomeFailed, "", "")
		return nil, stackInternal(d, "rebase the draft", env.ID, err)
	}
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

	standaloneIn := stackops.StandaloneDeployInput{
		EnvID: env.ID, NodeID: node, Stack: st, Copy: draft.Copy, Reviewed: in.Body.Pins, Update: update,
		Pins: repo, RevisionID: rev.ID, Caller: caller, Masker: rendering.masker,
	}
	return launchStackDeploy(ctx, d, launch{
		p: p, env: env, st: st, rev: rev, mode: mode, kind: stackops.DeployKindDeploy, userAgent: in.UserAgent,
		items: stackops.StandaloneItemSpecs(),
		run: func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult)) {
			standaloneIn.OnDone = onDone
			d.ops().DeployStandalone(jctx, standaloneIn, r)
		},
		rendering: rendering, release: release, owned: &jobOwnsLock,
	})
}

// varMap is the stack's variable table by name.
func varMap(vars []stacks.Var) map[string]string {
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		out[v.Name] = v.Value
	}
	return out
}

// rollbackStack redeploys the last version of a stack that ran well. It is judged like a deploy and runs
// the same engine, on a revision copied from the one it restores, with that revision's own images: it
// asks no registry (STK-07, D-14).
func rollbackStack(ctx context.Context, d StacksDeps, in *StackRollbackInput) (*StackDeployOutput, error) {
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
	if st.Target == stacks.TargetStandalone {
		// Rolling a standalone stack back is a later plan's: the engine for it is not here yet.
		return nil, targetProblem(d, env.ID, stackops.ErrTargetEngineMissing)
	}
	target, ok, err := repo.RollbackTarget(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "roll back target", env.ID, err)
	}
	if !ok {
		return nil, httpx.Problem(http.StatusConflict, problemNoPreviousVersion, noPreviousVersion)
	}
	pins, err := repo.RevisionPins(ctx, target.ID)
	if err != nil {
		return nil, stackInternal(d, "roll back pins", env.ID, err)
	}

	// The revision is judged once, by the check pipeline, and converted before anything is stored: one that
	// cannot be redeployed as it stands is refused here, with the reason, and leaves no revision behind. The
	// project it returns is the one that is rendered and deployed.
	prep, err := d.ops().PrepareRollback(ctx, st, target, pins)
	if err != nil {
		return nil, rollbackPrepareProblem(err)
	}
	// What a deploy of this revision would need is judged for the caller, as a deploy is: an Operator
	// cannot roll back to a version only an Admin may deploy (T-07-91). The job judges it again.
	caller := stackops.Caller{Principal: p, Actor: p.Name}
	pol, err := d.ops().Policy(ctx, caller, env.ID, target.Copy, prep.Plan)
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

	release, held, ok := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackID: st.ID, StackName: st.Name, Kind: deployModeRollback, By: p.Name, Started: time.Now().UTC(),
	})
	if !ok {
		return nil, operationLocked(env.Name, held)
	}
	jobOwnsLock := false
	defer func() {
		if !jobOwnsLock {
			release()
		}
	}()

	// The roll back is a revision of its own: a copy of the one it restores, with its images and its
	// rendering. The draft is not rebased: it still starts from the revision it began at, so the next
	// review shows the roll back as a deploy made since.
	rev, err := repo.CopyRevision(ctx, target, p.Name)
	if err != nil {
		return nil, stackInternal(d, "revision", env.ID, err)
	}
	rendering := revisionRendering{repo: repo, loaded: prep.Loaded, masker: stacks.NewMasker(target.Copy.Vars, target.Copy.Files)}
	if err := rendering.store(ctx, rev.ID, stackops.LivePins(pins)); err != nil {
		d.Log.Warn("could not store the rendering of a revision", "stack", st.ID, "revision", rev.Seq, "error", err)
	}
	swarmIn := stackops.SwarmDeployInput{
		EnvID: env.ID, Stack: st, Project: prep.Loaded.Project, Files: prep.Files, Vars: prep.Vars, Copy: target.Copy,
		Caller: caller, Masker: rendering.masker,
		Pins: repo, RevisionID: rev.ID, Reviewed: pins, Rollback: true,
	}
	return launchStackDeploy(ctx, d, launch{
		p: p, env: env, st: st, rev: rev, mode: deployModeRollback, kind: stackops.DeployKindRollback, userAgent: in.UserAgent,
		items: stackops.DeployItemSpecs(),
		run: func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult)) {
			swarmIn.OnDone = onDone
			d.ops().DeploySwarm(jctx, swarmIn, r)
		},
		rendering: rendering, release: release, owned: &jobOwnsLock,
	})
}

// launch is a stack operation that has been judged, locked and given its revision, and now becomes a job.
type launch struct {
	p         *rbac.Principal
	env       *httpx.Env
	st        stackops.Stack
	rev       stackops.Snapshot
	mode      string
	kind      string // the kind of the row in stack_deploys
	userAgent string
	// jobKind is the Kind of the job in the hub; empty is a deploy's.
	jobKind string
	// items are the items the job starts with and run is the engine: it does the operation on the job's
	// context, reporting on r, and calls onDone once with the result before the job's last item ends.
	items []jobs.ItemSpec
	run   func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult))
	// record, when set, stores how the operation ended in place of recordDeployEnd, which is a deploy's: a stop
	// and a start take no revision.
	record    func(ctx context.Context, deploy stackops.DeployRow, res stackops.DeployResult)
	rendering revisionRendering
	// release frees the environment's operation lock; owned is set once the job holds it.
	release func()
	owned   *bool
}

// launchStackDeploy starts the job of a deploy or a roll back and records it, answering 202 with the
// job. The job needs the deploy row's ID and the row needs the job's ID, so the run waits on ready until
// the row is written. The job frees the lock when the operation ends, before the job reads as finished.
func launchStackDeploy(ctx context.Context, d StacksDeps, l launch) (*StackDeployOutput, error) {
	repo := d.repo()
	ready := make(chan struct{})
	var deploy stackops.DeployRow
	reqCtx := context.WithoutCancel(ctx)
	actor, userAgent := l.p.AuditActor(), l.userAgent
	onDone := func(res stackops.DeployResult) {
		if l.record != nil {
			l.record(reqCtx, deploy, res)
		} else {
			recordDeployEnd(reqCtx, d, actor, userAgent, l.st, l.rev, deploy, l.mode, res, l.rendering)
		}
		// The lock is free before the job reads as finished, so whoever sees it end can start the next
		// operation at once. Freeing it again when the run returns changes nothing.
		l.release()
	}
	kind := l.jobKind
	if kind == "" {
		kind = stackDeployJobKind
	}
	// A stop and a start take no revision; there is none to mark failed.
	failRevision := func() {
		if l.rev.ID != "" {
			_ = repo.SetOutcome(reqCtx, l.rev.ID, stackops.OutcomeFailed, "", "")
		}
	}
	spec := jobs.Spec{
		Kind: kind, EnvID: l.env.ID, Owner: stackJobOwner(l.p),
		Meta: map[string]string{
			metaStackID: l.st.ID, metaStackName: l.st.Name, metaMode: l.mode, metaStartedBy: l.p.Name, metaEnvName: l.env.Name,
		},
		Items: l.items,
	}
	if d.BeforeJob != nil {
		d.BeforeJob(ctx)
	}
	job, err := d.Jobs.Start(spec, func(jctx context.Context, r *jobs.Reporter) {
		defer l.release()
		select {
		case <-ready:
		case <-jctx.Done():
			return
		}
		if deploy.ID == "" {
			return // the row could not be written: the handler cancelled the job
		}
		l.run(jctx, r, onDone)
	})
	if errors.Is(err, jobs.ErrTooMany) {
		failRevision()
		return nil, httpx.Problem(http.StatusTooManyRequests, problemTooManyDeploys, "You already have 5 jobs running. Wait for one to finish.")
	}
	if err != nil {
		failRevision()
		return nil, stackInternal(d, "start the deploy job", l.env.ID, err)
	}
	// The job owns the lock now, and the holder can name it.
	*l.owned = true
	d.Locks.Attach(l.env.ID, job.ID)
	row, err := repo.StartDeploy(reqCtx, l.st.ID, l.env.ID, l.kind, l.rev.Seq, job.ID, l.p.Name)
	if err != nil {
		_ = d.Jobs.Cancel(job.ID)
		close(ready)
		failRevision()
		return nil, stackInternal(d, "record the deploy", l.env.ID, err)
	}
	deploy = row
	close(ready)
	return &StackDeployOutput{CacheControl: "no-store", Body: StackDeployBody{JobID: job.ID, Revision: l.rev.Seq}}, nil
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
	cancelledBy := d.cancels.take(deploy.JobID)
	repo := d.repo()
	if err := repo.SetOutcome(wctx, rev.ID, outcome, "", ""); err != nil {
		d.Log.Error("could not mark a revision", "stack", st.ID, "revision", rev.Seq, "error", err)
	} else if outcome == stackops.OutcomeGood {
		// A deploy brings every service up as the file says, so a stack a person stopped is running again.
		if err := repo.SetStopped(wctx, st.ID, false); err != nil {
			d.Log.Warn("could not record that a deployed stack is running", "stack", st.ID, "error", err)
		}
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
	// The stack's state is read from the cluster cache, so read the swarm again: a stack that reads
	// finished must not show what it ran before. A failed read leaves the last one. A standalone host's
	// snapshot was dropped by the write itself.
	if st.Target == stacks.TargetSwarm && d.Clusters != nil {
		rctx, cancelRead := context.WithTimeout(wctx, writeTimeout)
		_ = d.Clusters.Refresh(rctx, st.EnvID)
		cancelRead()
	}
	details := map[string]string{
		"stack": st.Name, "mode": mode, "result": resultWord, "revision": itoa(rev.Seq), "job": deploy.JobID,
		"services": itoa(int64(res.Services + res.Updated)), "code": res.Code,
	}
	if resultWord == "cancelled" && cancelledBy != "" {
		details["cancelledBy"] = cancelledBy
	}
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: actor, Action: audit.ActionStackDeployed, TargetType: stackTargetType, TargetID: st.ID, EnvID: st.EnvID,
		Result: result, Reason: res.Code, IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
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
	var invalid *stackops.ServicesInvalidError
	if errors.As(err, &invalid) {
		return servicesProblem(invalid)
	}
	return snapshotProblem(d.Log, envID, err)
}

// servicesProblem is the 422 for an update of images that names no service or one the stack does not have.
func servicesProblem(e *stackops.ServicesInvalidError) error {
	detail := "Choose at least one service to update."
	var details []*huma.ErrorDetail
	if !e.Empty {
		detail = "These services are not in the stack: " + strings.Join(e.Unknown, ", ") + "."
		for _, name := range e.Unknown {
			details = append(details, &huma.ErrorDetail{Location: "services", Message: name})
		}
	}
	return &huma.ErrorModel{
		Type: problemServicesInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: details,
	}
}

// operationLocked is the 409 for an operation asked for while another holds the environment: it names
// the stack, the job and when it started, so the page can say who to wait for and open it.
func operationLocked(envName string, h stackops.Holder) error {
	started := rfc3339(h.Started)
	return &huma.ErrorModel{
		Type: problemOperationLocked, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: "Another stack operation is running on " + envName + ": " + h.StackName + ", started " + started + ". Try again when it finishes.",
		Errors: []*huma.ErrorDetail{
			{Location: "stack", Message: h.StackName},
			{Location: "job", Message: h.JobID},
			{Location: "started", Message: started},
		},
	}
}

// deployableProblem is the answer to a draft the check pipeline refused: a 422 stack_invalid that lists
// the check's masked problems, the very ones the review shows. Any other failure is the server's.
func deployableProblem(d StacksDeps, envID string, err error) error {
	var nd *stacks.NotDeployableError
	if errors.As(err, &nd) {
		return stackInvalid(nd.Problems, "The stack has errors, so it can't be deployed. Review it and fix the problems it lists.")
	}
	return stackInternal(d, "check", envID, err)
}

// rollbackPrepareProblem is the answer to a revision that cannot be redeployed as it stands.
func rollbackPrepareProblem(err error) error {
	var pe *stackops.PrepareError
	if !errors.As(err, &pe) {
		return huma.Error500InternalServerError("internal error")
	}
	switch pe.Stage {
	case stackops.PrepareLoad:
		var nd *stacks.NotDeployableError
		if errors.As(pe.Err, &nd) {
			return stackInvalid(nd.Problems, "That version has errors now, so it can't be deployed again.")
		}
		return httpx.Problem(http.StatusUnprocessableEntity, problemComposeInvalid, "That version can't be read, so it can't be deployed again.")
	case stackops.PrepareObjects:
		return objectProblem(pe.Err)
	case stackops.PrepareConvert:
		return convertProblem(pe.Err)
	}
	var invalid *stackops.PinsInvalidError
	service := ""
	if errors.As(pe.Err, &invalid) {
		service = invalid.Service
	}
	return &huma.ErrorModel{
		Type: problemPinsInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: "That version has no recorded image for " + service + ", so it can't be rolled back to. Deploy the stack again instead.",
		Errors: []*huma.ErrorDetail{{Location: "pins", Message: service}},
	}
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
			Detail: "Service " + nc.Service + " uses " + strings.Join(nc.Fields, ", ") + ", which a swarm stack can't deploy.", Errors: details,
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
	job, ok := d.Jobs.Get(in.JobID)
	if !ok || !stackJobOf(job, in.EnvID, in.StackID) {
		// Which 404 it is depends on the stack; a job of this stack needs no stack record (a removal deletes it).
		if _, err := d.repo().Stack(ctx, in.EnvID, in.StackID); err != nil {
			return nil, stackLookupProblem(d, in.EnvID, err)
		}
		return nil, httpx.Problem(http.StatusNotFound, problemStackJobNotFound, "This deploy is no longer here. Deploys are kept for 10 minutes after they end.")
	}
	return &StackJobOutput{CacheControl: "no-store", Body: stackJobBody(job, outputOf(d.Jobs, job.ID))}, nil
}

func stackJobBody(j jobs.Job, output []jobs.Output) StackJobBody {
	b := StackJobBody{
		ID: j.ID, StackID: j.Meta[metaStackID], Mode: j.Meta[metaMode], StartedBy: j.Meta[metaStartedBy], State: stackJobState(j),
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

// stackJobState is the state of a deploy job as the console reads it. The hub calls a job partial
// when some of its items are done, so a deploy that was cancelled after its first steps would read
// partial; it reads cancelled when it was stopped by a cancel and nothing failed. An object the
// cleanup could not remove is not a failure of the deploy: it is reported on its own.
func stackJobState(j jobs.Job) string {
	if j.State != jobs.JobPartial && j.State != jobs.JobFailed {
		return j.State
	}
	cancelled := false
	for _, it := range j.Items {
		switch {
		case it.State == jobs.ItemFailed && it.Code != stackops.CodeRemoveFailed:
			return j.State
		case it.State == jobs.ItemCancelled && it.Code == stackops.CodeCancelled:
			cancelled = true
		}
	}
	if cancelled {
		return jobs.JobCancelled
	}
	return j.State
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
