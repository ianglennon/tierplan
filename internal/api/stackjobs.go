// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// stackJobKindPrefix is what every kind of stack job in the jobs hub starts with (stack_deploy today; the
// stop, start and removal of the plans after this one are stack jobs too).
const stackJobKindPrefix = "stack_"

// StackJobSummary is one row of the header's jobs list: a stack operation, running or ended.
type StackJobSummary struct {
	ID         string `json:"id" doc:"The job ID: open its console at /env/{envId}/stacks/{stackId}/deploys/{id}"`
	EnvID      string `json:"envId" doc:"The environment the operation runs on"`
	EnvName    string `json:"envName" doc:"The environment's name when the operation started"`
	StackID    string `json:"stackId" doc:"The stack the operation is on"`
	StackName  string `json:"stackName" doc:"The stack's name when the operation started"`
	Kind       string `json:"kind" enum:"deploy,update,rollback,restore,start,stop,remove" doc:"What the operation is"`
	State      string `json:"state" enum:"running,done,failed,cancelled" doc:"Where it is. A job that ended with some of its work done and some not reads failed"`
	StartedAt  string `json:"startedAt" doc:"When the job started, RFC 3339 UTC"`
	FinishedAt string `json:"finishedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	StartedBy  string `json:"startedBy" doc:"Who started it, as a name"`
	Trigger    string `json:"trigger,omitempty" enum:"poll,webhook" doc:"Left out unless set: what started an automatic deploy of a git stack, a poll check or a webhook"`
}

// StackJobListBody is the answer to the list route.
type StackJobListBody struct {
	Jobs []StackJobSummary `json:"jobs" doc:"The stack operations running or ended in the last 10 minutes on the environments the caller may read stacks on, newest first"`
}

// StackJobListOutput wraps StackJobListBody for huma.
type StackJobListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackJobListBody
}

func registerStackJobs(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listStackJobs", Method: http.MethodGet, Path: "/api/stack-jobs",
		Summary: "The stack operations you can follow",
		Description: "The deploys, updates and roll backs of stacks that are running or ended in the last 10 minutes, newest first, for the header's jobs list. " +
			"Every job on an environment where you may read stacks (stacks.read) is listed, whoever started it; a job on any other environment is left out, as if it did not exist. " +
			"A job that has left the server's history is not listed, and its console answers 404 stack_job_not_found.",
		Tags: []string{"stacks"},
	}, rbac.SelfService,
		func(ctx context.Context, _ *struct{}) (*StackJobListOutput, error) {
			return listStackJobs(ctx, d)
		})
}

// listStackJobs lists the stack jobs the caller may follow, newest first. Whether the caller may read
// stacks is decided for each environment once, from the permission itself; a job on an environment they
// cannot read is absent and nothing says it is there (P3 invisibility, T-07-92).
func listStackJobs(ctx context.Context, d StacksDeps) (*StackJobListOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil || d.Jobs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Deploys are not available right now.")
	}
	mayRead := map[string]bool{}
	allowed := func(envID string) bool {
		v, ok := mayRead[envID]
		if !ok {
			dec := d.Checker.Check(ctx, p, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment}, envID)
			v = dec.Err == nil && dec.Allowed
			mayRead[envID] = v
		}
		return v
	}
	list := d.Jobs.List(func(j jobs.Job) bool {
		return strings.HasPrefix(j.Kind, stackJobKindPrefix) && j.EnvID != "" && d.Checker != nil && allowed(j.EnvID)
	})
	body := StackJobListBody{Jobs: make([]StackJobSummary, 0, len(list))}
	for _, j := range list {
		body.Jobs = append(body.Jobs, stackJobSummary(j))
	}
	return &StackJobListOutput{CacheControl: "no-store", Body: body}, nil
}

// stackJobSummary is a stack job as the jobs list shows it.
func stackJobSummary(j jobs.Job) StackJobSummary {
	state := stackJobState(j)
	if state == jobs.JobPartial {
		state = jobs.JobFailed
	}
	kind := j.Meta[metaMode]
	if kind == "" {
		kind = deployModeDeploy
	}
	s := StackJobSummary{
		ID: j.ID, EnvID: j.EnvID, EnvName: j.Meta[metaEnvName], StackID: j.Meta[metaStackID], StackName: j.Meta[metaStackName],
		Kind: kind, State: state, StartedAt: rfc3339(j.CreatedAt), StartedBy: j.Meta[metaStartedBy],
		Trigger: j.Meta[metaTrigger],
	}
	if j.Finished() {
		s.FinishedAt = rfc3339(j.FinishedAt)
	}
	return s
}
