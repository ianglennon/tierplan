// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// Updating a workload in one click (plan 12-15, IMG-06, D-05, D-07, D-10, D-13). The request names the workload and the
// digest the confirm showed; the server asks the registry again, refuses if the digest moved, and deploys the digest it
// read itself, never the one in the request.

const (
	problemUpdateMoved       = "update_moved"
	problemNothingToUpdate   = "nothing_to_update"
	problemNotUpdatable      = "not_updatable"
	problemUpdateJobNotFound = "update_job_not_found"
	problemUpdateCheckFailed = "update_check_failed"

	updateJobGone = "This update is no longer being followed. Updates are kept for 10 minutes after they end."
)

// UpdateRunRequest names the workload to update and what the person confirmed.
type UpdateRunRequest struct {
	Kind   string            `json:"kind" enum:"service" doc:"service for a swarm service. Containers and stacks follow in later versions"`
	Name   string            `json:"name" minLength:"1" maxLength:"255" doc:"The service's name"`
	Expect map[string]string `json:"expect,omitempty" maxProperties:"100" doc:"For each service name, the digest the confirm showed. The server compares it with what the registry says now and refuses with 409 update_moved when they differ; it never deploys a digest from the request"`
}

// UpdateRunInput is the path and body of the update route.
type UpdateRunInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      UpdateRunRequest
}

// UpdateRunBody is the answer of an update that started.
type UpdateRunBody struct {
	JobID string `json:"jobId" doc:"The update's job: follow it at GET /api/env/{envId}/update-jobs/{jobId} or its stream"`
}

// UpdateRunOutput wraps UpdateRunBody for huma.
type UpdateRunOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdateRunBody
}

// UpdateJobPathInput is the path of a job of an update.
type UpdateJobPathInput struct {
	EnvironmentPathInput
	JobID string `path:"jobId" doc:"The update's job ID"`
}

// UpdateJobBody is the job of an image update, as the console shows it.
type UpdateJobBody struct {
	ID         string               `json:"id" doc:"The job ID"`
	RunID      string               `json:"runId" doc:"The update's run, kept after the job leaves the server's history"`
	Kind       string               `json:"kind" enum:"service,container,stack" doc:"What is updated"`
	Workload   string               `json:"workload" doc:"The service's, container's or stack's name"`
	FromDigest string               `json:"fromDigest" doc:"The digest it ran"`
	ToDigest   string               `json:"toDigest" doc:"The digest it is updated to, as the server read it from the registry"`
	StartedBy  string               `json:"startedBy" doc:"Who started the update, as a name"`
	Bulk       string               `json:"bulk,omitempty" doc:"Left out unless set: the bulk update this one belongs to"`
	State      string               `json:"state" enum:"running,done,partial,failed,cancelled" doc:"running until every step has ended, then done when every step is"`
	CreatedAt  string               `json:"createdAt" doc:"When the job started, RFC 3339 UTC"`
	FinishedAt string               `json:"finishedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	Items      []StackJobItemBody   `json:"items" doc:"The steps, in the order they run: check, pull, update, wait and, when the image has a health check, health"`
	Output     []StackJobOutputLine `json:"output" doc:"The output log, oldest first"`
}

// UpdateJobOutput wraps UpdateJobBody for huma.
type UpdateJobOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdateJobBody
}

// NewManagedStackLookup answers the applier's question of whether the environment manages a stack: it has a record of it.
func NewManagedStackLookup(repo stackops.Repo) func(ctx context.Context, envID, stackID string) (bool, error) {
	return func(ctx context.Context, envID, stackID string) (bool, error) {
		_, err := repo.Stack(ctx, envID, stackID)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, stackops.ErrStackNotFound):
			return false, nil
		}
		return false, err
	}
}

func registerUpdateRun(r *httpx.Router, d UpdatesDeps) {
	if d.Heartbeat <= 0 {
		d.Heartbeat = defaultHeartbeat
	}
	if d.MaxLifetime <= 0 {
		d.MaxLifetime = defaultMaxStreamLifetime
	}
	if d.MaxStreamsPerUser <= 0 {
		d.MaxStreamsPerUser = defaultMaxStreams
	}
	httpx.Register(r, huma.Operation{
		OperationID: "runImageUpdate", Method: http.MethodPost, Path: "/api/env/{envId}/updates/run",
		Summary: "Update a service to its newer image",
		Description: "Asks the registry for the digest of the service's tag now, refuses with 409 update_moved (and the refreshed digest) when it is not the one in expect, " +
			"then updates the service to it as a job: the image is pulled on the nodes that run its tasks, the service is changed in nothing but its image, and the update waits for the new tasks to run. " +
			"The digest deployed is always the one the server read. 409 operation_locked when another operation holds the environment, 409 nothing_to_update when the service runs the newest version already, " +
			"409 not_updatable (code agent, job_mode, managed_by_stack, pinned_in_file or not_from_registry), 409 update_check_failed when the registry could not be asked, 404 not_found when the service is not there. " +
			"Audited as image.updated or image.update_failed when it ends. Needs services.control (Operator) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateRunInput) (*UpdateRunOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := runImageUpdate(ctx, d, p, env, in)
			if err != nil {
				return nil, err
			}
			return &UpdateRunOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "getImageUpdateJob", Method: http.MethodGet, Path: "/api/env/{envId}/update-jobs/{jobId}",
		Summary: "An image update's job",
		Description: "The job of an image update as it stands: its steps and output. Anyone who may read the environment may read it. " +
			"A job that is not an update of this environment, or that has left the server's history, is 404 update_job_not_found.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateJobPathInput) (*UpdateJobOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			hub := d.hub()
			if hub == nil {
				return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
			}
			job, ok := hub.Get(in.JobID)
			if !ok || job.Kind != updates.JobKind || job.EnvID != env.ID {
				return nil, httpx.Problem(http.StatusNotFound, problemUpdateJobNotFound, updateJobGone)
			}
			return &UpdateJobOutput{CacheControl: "no-store", Body: updateJobBody(job, outputOf(hub, job.ID))}, nil
		})

	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/update-jobs/{jobId}/stream",
		rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			serveUpdateJobStream(w, req, d, lim)
		}))
}

// hub is the jobs hub the applier runs its jobs in, nil when updates are not wired.
func (d UpdatesDeps) hub() *jobs.Hub {
	if d.Applier == nil {
		return nil
	}
	return d.Applier.Jobs
}

// runImageUpdate starts the update of a service and answers its job.
func runImageUpdate(ctx context.Context, d UpdatesDeps, p *rbac.Principal, env *httpx.Env, in *UpdateRunInput) (UpdateRunBody, error) {
	if d.Applier == nil {
		return UpdateRunBody{}, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
	}
	jobID, err := d.Applier.Start(ctx, updates.Request{
		EnvID: env.ID, EnvName: env.Name, Kind: in.Body.Kind, Name: in.Body.Name, Expect: in.Body.Expect,
		Actor: p.AuditActor(), IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent, Owner: stackJobOwner(p),
	})
	if err != nil {
		return UpdateRunBody{}, updateRunProblem(d, env, err)
	}
	return UpdateRunBody{JobID: jobID}, nil
}

// updateRunProblem words why an update was not started.
func updateRunProblem(d UpdatesDeps, env *httpx.Env, err error) error {
	var (
		moved     *updates.MovedError
		locked    *updates.LockedError
		notUpdate *updates.NotUpdatableError
		failed    *updates.CheckFailedError
	)
	switch {
	case errors.As(err, &moved):
		details := make([]*huma.ErrorDetail, 0, len(moved.Items))
		for _, it := range moved.Items {
			details = append(details, &huma.ErrorDetail{
				Location: "items", Message: it.Service,
				Value: map[string]string{"service": it.Service, "fromDigest": it.FromDigest, "toDigest": it.ToDigest},
			})
		}
		return &huma.ErrorModel{
			Type: problemUpdateMoved, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
			Detail: "A newer version appeared since you confirmed. Review it and confirm again.", Errors: details,
		}
	case errors.As(err, &locked):
		return operationLocked(env.Name, locked.Holder)
	case errors.Is(err, updates.ErrNothingToUpdate):
		return httpx.Problem(http.StatusConflict, problemNothingToUpdate, "It already runs the newest version.")
	case errors.As(err, &notUpdate):
		return &huma.ErrorModel{
			Type: problemNotUpdatable, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
			Detail: "This can't be updated here.", Errors: []*huma.ErrorDetail{{Location: "code", Message: notUpdate.Code}},
		}
	case errors.As(err, &failed):
		return &huma.ErrorModel{
			Type: problemUpdateCheckFailed, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
			Detail: "The registry could not be asked for the newest version, so nothing was changed.",
			Errors: []*huma.ErrorDetail{{Location: "reason", Message: string(failed.Reason)}},
		}
	case errors.Is(err, updates.ErrWorkloadGone):
		return workloadGone("service")
	case errors.Is(err, updates.ErrUnsupportedKind):
		return httpx.Problem(http.StatusUnprocessableEntity, "", "That kind of workload can't be updated yet.")
	case errors.Is(err, jobs.ErrTooMany):
		return httpx.Problem(http.StatusTooManyRequests, problemTooManyDeploys, "You already have 5 jobs running. Wait for one to finish.")
	}
	return snapshotProblem(d.log(), env.ID, err)
}

// updateJobBody turns a job of the hub into its body.
func updateJobBody(j jobs.Job, output []jobs.Output) UpdateJobBody {
	b := UpdateJobBody{
		ID: j.ID, RunID: j.Meta[updates.MetaRunID], Kind: j.Meta[updates.MetaKind], Workload: j.Meta[updates.MetaWorkload],
		FromDigest: j.Meta[updates.MetaFrom], ToDigest: j.Meta[updates.MetaTo], StartedBy: j.Meta[updates.MetaBy],
		Bulk: j.Meta[updates.MetaBulk], State: j.State, CreatedAt: rfc3339(j.CreatedAt), FinishedAt: rfc3339(j.FinishedAt),
		Items: make([]StackJobItemBody, 0, len(j.Items)), Output: make([]StackJobOutputLine, 0, len(output)),
	}
	for _, it := range j.Items {
		b.Items = append(b.Items, stackJobItemBody(it))
	}
	for _, o := range output {
		b.Output = append(b.Output, stackJobOutputLine(o))
	}
	return b
}

// serveUpdateJobStream follows one update through the shared job stream: the job as it stands, a frame for each change and a
// done frame at the end, resumed from Last-Event-ID. Only an update job of this environment can be followed; any other is the
// same 404 as one that is gone. The update does not depend on the stream: closing it stops nothing.
func serveUpdateJobStream(w http.ResponseWriter, req *http.Request, d UpdatesDeps, lim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	hub := d.hub()
	if hub == nil {
		writeJSONProblem(w, http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
		return
	}
	jobID := chi.URLParam(req, "jobId")
	if job, ok := hub.Get(jobID); !ok || job.Kind != updates.JobKind || job.EnvID != env.ID {
		writeJSONProblem(w, http.StatusNotFound, problemUpdateJobNotFound, updateJobGone)
		return
	}
	serveJobStream(w, req, jobStream{
		Hub: hub, JobID: jobID, Key: string(p.Type) + ":" + p.ID, Limiter: lim, What: "update",
		GoneType: problemUpdateJobNotFound, GoneDetail: updateJobGone, TooManyType: problemTooManyStreams,
		Heartbeat: d.Heartbeat, MaxLifetime: d.MaxLifetime, Log: d.log(),
		Snapshot: func(j jobs.Job) any { return updateJobBody(j, outputOf(hub, j.ID)) },
		Encode: func(ev jobs.Event) (any, bool) {
			switch {
			case ev.Type == jobs.EventItem && ev.Item != nil:
				return stackJobItemBody(*ev.Item), true
			case ev.Type == jobs.EventOutput && ev.Output != nil:
				return stackJobOutputLine(*ev.Output), true
			case ev.Type == jobs.EventDone && ev.Job != nil:
				return updateJobBody(*ev.Job, outputOf(hub, ev.Job.ID)), true
			}
			return nil, false
		},
	})
}
