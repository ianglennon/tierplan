// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/updates"
)

// ImageUpdateJobSummary is one row of the header's list of image updates: one running or ended in the last 10 minutes.
type ImageUpdateJobSummary struct {
	ID        string `json:"id" doc:"The job ID: follow it at GET /api/env/{envId}/update-jobs/{id}"`
	EnvID     string `json:"envId" doc:"The environment the update runs on"`
	EnvName   string `json:"envName" doc:"The environment's name when the update started"`
	Workload  string `json:"workload" doc:"The service's, container's or stack's name"`
	Kind      string `json:"kind" enum:"service,container,stack" doc:"What is updated"`
	State     string `json:"state" enum:"running,done,failed,cancelled" doc:"Where it is. A job that ended with some of its work done and some not reads failed"`
	StartedAt string `json:"startedAt" doc:"When the job started, RFC 3339 UTC"`
	EndedAt   string `json:"endedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	Bulk      string `json:"bulk,omitempty" doc:"Left out unless set: the bulk update this one belongs to"`
}

// ImageUpdateJobListBody is the answer to the list route.
type ImageUpdateJobListBody struct {
	Jobs []ImageUpdateJobSummary `json:"jobs" doc:"The image updates running or ended in the last 10 minutes on the environments the caller may read, newest first"`
}

// ImageUpdateJobListOutput wraps ImageUpdateJobListBody for huma.
type ImageUpdateJobListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImageUpdateJobListBody
}

func registerUpdateJobs(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listImageUpdateJobs", Method: http.MethodGet, Path: "/api/update-jobs",
		Summary: "The image updates you can follow",
		Description: "The image updates that are running or ended in the last 10 minutes, newest first, for the header's Jobs indicator. " +
			"Every update on an environment you may read (environments.read) is listed, whoever started it; an update on any other environment is left out, as if it did not exist. " +
			"An update that has left the server's history is not listed, and its job answers 404 update_job_not_found.",
		Tags: []string{"environments"},
	}, rbac.SelfService,
		func(ctx context.Context, _ *struct{}) (*ImageUpdateJobListOutput, error) {
			return listImageUpdateJobs(ctx, d)
		})
}

// listImageUpdateJobs lists the image update jobs the caller may follow, newest first. Whether the caller may read an
// environment is decided once for each, from the permission itself; a job on an environment they cannot read is absent and
// nothing says it is there (P3 invisibility, T-12-43).
func listImageUpdateJobs(ctx context.Context, d UpdatesDeps) (*ImageUpdateJobListOutput, error) {
	p, hub := httpx.PrincipalFrom(ctx), d.hub()
	if p == nil || hub == nil || d.Checker == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
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
	list := hub.List(func(j jobs.Job) bool { return j.Kind == updates.JobKind && j.EnvID != "" && allowed(j.EnvID) })
	body := ImageUpdateJobListBody{Jobs: make([]ImageUpdateJobSummary, 0, len(list))}
	for _, j := range list {
		state := j.State
		if state == jobs.JobPartial {
			state = jobs.JobFailed
		}
		s := ImageUpdateJobSummary{
			ID: j.ID, EnvID: j.EnvID, EnvName: j.Meta[updates.MetaEnvName], Workload: j.Meta[updates.MetaWorkload],
			Kind: j.Meta[updates.MetaKind], State: state, StartedAt: rfc3339(j.CreatedAt), Bulk: j.Meta[updates.MetaBulk],
		}
		if j.Finished() {
			s.EndedAt = rfc3339(j.FinishedAt)
		}
		body.Jobs = append(body.Jobs, s)
	}
	return &ImageUpdateJobListOutput{CacheControl: "no-store", Body: body}, nil
}
