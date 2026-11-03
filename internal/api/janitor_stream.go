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
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// problemJanitorJobNotFound is the 404 for a run whose job is not in the jobs hub: it is not this
// environment's, it never had one, or it ended longer ago than the hub keeps a job.
const problemJanitorJobNotFound = "janitor_job_not_found"

const janitorJobGone = "This clean-up is no longer being followed. Open the run to read how it ended."

// JanitorJobSummary is one row of the Jobs indicator's list of clean-ups.
type JanitorJobSummary struct {
	JobID     string `json:"jobId" doc:"The job ID: follow it at /api/env/{envId}/janitor/runs/{runId}/stream"`
	RunID     string `json:"runId" doc:"The run this job is: open it at /env/{envId}/janitor/runs/{runId}"`
	EnvID     string `json:"envId" doc:"The environment the clean-up runs on"`
	State     string `json:"state" enum:"running,done,partial,failed,cancelled" doc:"Where the job is. It counts the job's items, so it can read failed or partial for a run whose record says completed, because an item the engine kept in use ends failed in the job. Read the run for how it ended"`
	Done      int    `json:"done" minimum:"0" doc:"How many removals have ended"`
	Total     int    `json:"total" minimum:"0" doc:"How many removals the run planned, 0 until the plan is known"`
	Trigger   string `json:"trigger" enum:"schedule,manual" doc:"schedule for a run the schedule started, manual for Clean now"`
	By        string `json:"by" doc:"Who started it, as a name: the person for Clean now, TierPlan for a scheduled run"`
	StartedAt string `json:"startedAt" format:"date-time" doc:"When the job started"`
}

// JanitorJobListBody is the answer to the list route.
type JanitorJobListBody struct {
	Jobs []JanitorJobSummary `json:"jobs" doc:"The clean-ups running or ended in the last 10 minutes on the environments the caller may read, newest first"`
}

// JanitorJobListOutput wraps JanitorJobListBody for huma.
type JanitorJobListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorJobListBody
}

// JanitorJobItemFrame is one removal of a clean-up as the stream sends it.
type JanitorJobItemFrame struct {
	ID      string            `json:"id" doc:"The item: the type of the object, its node and its engine ID, joined by slashes"`
	Label   string            `json:"label" doc:"The object's name"`
	Kind    string            `json:"kind" doc:"container, image or network, or step for the one item of a run that could not plan"`
	State   string            `json:"state" enum:"waiting,running,done,failed,skipped,cancelled" doc:"Where the removal is"`
	Code    string            `json:"code" doc:"A stable word for why it did not end as a clean removal, empty otherwise"`
	Message string            `json:"message" doc:"A sentence for the code"`
	Result  map[string]string `json:"result" doc:"The node and the engine ID of the object, once the removal ended"`
}

// JanitorJobFrame is a clean-up's job as the stream sends it: the snapshot frame and the done frame.
type JanitorJobFrame struct {
	ID         string                `json:"id" doc:"The job ID"`
	RunID      string                `json:"runId" doc:"The run this job is"`
	State      string                `json:"state" enum:"running,done,partial,failed,cancelled" doc:"running until every item has ended"`
	Trigger    string                `json:"trigger" enum:"schedule,manual"`
	By         string                `json:"by" doc:"Who started it, as a name"`
	CreatedAt  string                `json:"createdAt" doc:"When the job started, RFC 3339 UTC"`
	FinishedAt string                `json:"finishedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	Done       int                   `json:"done" doc:"How many removals have ended"`
	Total      int                   `json:"total" doc:"How many removals the run planned"`
	Items      []JanitorJobItemFrame `json:"items" doc:"One item for each planned removal, in the order they were added"`
}

// registerJanitorStream registers the stream that follows a run and the list of running clean-ups.
func registerJanitorStream(r *httpx.Router, d JanitorDeps) {
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
		OperationID: "listJanitorJobs", Method: http.MethodGet, Path: "/api/janitor-jobs",
		Summary: "The clean-ups you can follow",
		Description: "The janitor runs that are working or ended in the last 10 minutes, newest first, for the header's Jobs list. " +
			"A run on an environment where you may read (environments.read) is listed, whoever started it, a scheduled one too; a run on any other environment is left out, as if it did not exist. " +
			"A job that has left the server's history is not listed, and its stream answers 404 janitor_job_not_found; the run is still read from its environment's history.",
		Tags: []string{"environments"},
	}, rbac.SelfService,
		func(ctx context.Context, _ *struct{}) (*JanitorJobListOutput, error) {
			return listJanitorJobs(ctx, d)
		})

	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/janitor/runs/{runId}/stream",
		rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			serveJanitorRunStream(w, req, d, lim)
		}))
}

// listJanitorJobs lists the clean-ups the caller may follow, newest first. Whether the caller may read
// an environment is decided once for each, from the permission itself; a job on an environment they
// cannot read is absent and nothing says it is there (T-11-39).
func listJanitorJobs(ctx context.Context, d JanitorDeps) (*JanitorJobListOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil || d.Jobs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Clean-ups are not available right now.")
	}
	mayRead := map[string]bool{}
	allowed := func(envID string) bool {
		v, ok := mayRead[envID]
		if !ok {
			dec := d.Checker.Check(ctx, p, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment}, envID)
			v = dec.Err == nil && dec.Allowed
			mayRead[envID] = v
		}
		return v
	}
	list := d.Jobs.List(func(j jobs.Job) bool {
		return j.Kind == janitor.JobKind && j.EnvID != "" && d.Checker != nil && allowed(j.EnvID)
	})
	body := JanitorJobListBody{Jobs: make([]JanitorJobSummary, 0, len(list))}
	for _, j := range list {
		done, total := janitorJobProgress(j)
		body.Jobs = append(body.Jobs, JanitorJobSummary{
			JobID: j.ID, RunID: j.Meta[janitor.MetaRunID], EnvID: j.EnvID, State: j.State, Done: done, Total: total,
			Trigger: j.Meta[janitor.MetaTrigger], By: janitorJobBy(j), StartedAt: rfc3339(j.CreatedAt),
		})
	}
	return &JanitorJobListOutput{CacheControl: "no-store", Body: body}, nil
}

// janitorJobProgress is how many of a job's removals have ended and how many there are.
func janitorJobProgress(j jobs.Job) (done, total int) {
	for _, it := range j.Items {
		if it.State != jobs.ItemWaiting && it.State != jobs.ItemRunning {
			done++
		}
	}
	return done, len(j.Items)
}

// janitorJobBy is who started a job, as a name: a scheduled run is the system's.
func janitorJobBy(j jobs.Job) string {
	if j.Meta[janitor.MetaTrigger] == janitor.TriggerSchedule {
		return janitorSystemName
	}
	return j.Meta[janitor.MetaBy]
}

func janitorJobFrame(j jobs.Job) JanitorJobFrame {
	done, total := janitorJobProgress(j)
	f := JanitorJobFrame{
		ID: j.ID, RunID: j.Meta[janitor.MetaRunID], State: j.State, Trigger: j.Meta[janitor.MetaTrigger], By: janitorJobBy(j),
		CreatedAt: rfc3339(j.CreatedAt), Done: done, Total: total, Items: make([]JanitorJobItemFrame, 0, len(j.Items)),
	}
	if j.Finished() {
		f.FinishedAt = rfc3339(j.FinishedAt)
	}
	for _, it := range j.Items {
		f.Items = append(f.Items, janitorJobItemFrame(it))
	}
	return f
}

func janitorJobItemFrame(it jobs.Item) JanitorJobItemFrame {
	res := it.Result
	if res == nil {
		res = map[string]string{}
	}
	return JanitorJobItemFrame{ID: it.ID, Label: it.Label, Kind: it.Kind, State: it.State, Code: it.Code, Message: it.Message, Result: res}
}

// serveJanitorRunStream follows one clean-up through the shared job stream: the job as it stands, an
// item frame for each removal as it starts and ends, and a done frame at the end, resumed from
// Last-Event-ID. Only a job that is a janitor run of this environment can be followed; any other is
// the same 404 as a job that is gone. The run does not depend on the stream: closing it stops nothing.
func serveJanitorRunStream(w http.ResponseWriter, req *http.Request, d JanitorDeps, lim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if d.Jobs == nil || d.Runs == nil {
		writeJSONProblem(w, http.StatusServiceUnavailable, problemJobsUnavail, "Clean-ups are not available right now.")
		return
	}
	run, _, err := d.Runs.GetRun(ctx, env.ID, chi.URLParam(req, "runId"))
	switch {
	case errors.Is(err, janitor.ErrRunNotFound):
		writeJSONProblem(w, http.StatusNotFound, problemJanitorRunNotFound, "This run does not exist in this environment.")
		return
	case err != nil:
		d.log().Error("could not read a janitor run to follow it", "environment", env.ID, "error", err)
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	job, ok := d.Jobs.Get(run.JobID)
	if run.JobID == "" || !ok || job.Kind != janitor.JobKind || job.EnvID != env.ID {
		writeJSONProblem(w, http.StatusNotFound, problemJanitorJobNotFound, janitorJobGone)
		return
	}
	serveJobStream(w, req, jobStream{
		Hub: d.Jobs, JobID: job.ID, Key: string(p.Type) + ":" + p.ID, Limiter: lim, What: "clean-up",
		GoneType: problemJanitorJobNotFound, GoneDetail: janitorJobGone, TooManyType: problemTooManyStreams,
		Heartbeat: d.Heartbeat, MaxLifetime: d.MaxLifetime, Log: d.log(),
		Snapshot: func(j jobs.Job) any { return janitorJobFrame(j) },
		Encode: func(ev jobs.Event) (any, bool) {
			switch {
			case ev.Type == jobs.EventItem && ev.Item != nil:
				return janitorJobItemFrame(*ev.Item), true
			case ev.Type == jobs.EventDone && ev.Job != nil:
				return janitorJobFrame(*ev.Job), true
			}
			return nil, false
		},
	})
}
