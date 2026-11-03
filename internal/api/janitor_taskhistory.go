// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The swarm's task history limit (plan 11-11, JAN-03, D-17, D-21): how many finished tasks per slot
// Docker keeps. A person with janitor.run reads and changes it here; the janitor never does, and the
// limit is not tied to how many exited containers it keeps.

const (
	// problemNotSwarm is a task history route called for an environment that is not a swarm.
	problemNotSwarm = "not_swarm"
	// problemTaskHistoryNoManager is a swarm no manager answered for.
	problemTaskHistoryNoManager = "no_manager"
	// problemSwarmChanged is a change refused because the swarm was changed between the read and the update.
	problemSwarmChanged = "swarm_changed"
	// problemTaskHistoryFailed is a change the manager did not accept.
	problemTaskHistoryFailed = "task_history_failed"

	// swarmChangedDetail is the sentence of problemSwarmChanged.
	swarmChangedDetail = "The swarm changed while TierPlan was saving. Try again."
	// agentTooOldDetail is the sentence of an agent that refuses the change by its own policy, which is
	// what an agent older than this server does for a call it does not know.
	agentTooOldDetail = "Update the agent on a manager to change this limit."
)

// TaskHistoryBody is the task history limit of a swarm.
type TaskHistoryBody struct {
	Limit         int64 `json:"limit" minimum:"-1" doc:"How many finished tasks per slot (per node for a global service) the swarm keeps, as the manager reports it. -1 means it keeps every task and removes none; Docker's default when none was set is 5"`
	DockerDefault int   `json:"dockerDefault" minimum:"0" doc:"The limit Docker uses when none was set"`
	KeepsAll      bool  `json:"keepsAll" doc:"True when the limit is negative, so the swarm never removes a finished task"`
}

// TaskHistoryOutput wraps TaskHistoryBody for huma.
type TaskHistoryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TaskHistoryBody
}

// TaskHistoryChangeBody is the answer of a change: the limit the swarm has now and whether this request changed it.
type TaskHistoryChangeBody struct {
	TaskHistoryBody
	Changed bool `json:"changed" doc:"True when this request changed the limit. False when the swarm already had the limit asked for: nothing was sent to it and nothing was audited"`
}

// TaskHistoryChangeOutput wraps TaskHistoryChangeBody for huma.
type TaskHistoryChangeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TaskHistoryChangeBody
}

// TaskHistoryRequest is the body of PUT /api/env/{envId}/janitor/task-history.
type TaskHistoryRequest struct {
	Limit int64 `json:"limit" minimum:"0" maximum:"1000" doc:"How many finished tasks per slot the swarm keeps, a whole number from 0 to 1000. Anything else is 422"`
}

// TaskHistoryInput is the path and body of the change route.
type TaskHistoryInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      TaskHistoryRequest
}

// taskHistoryBody is the wire form of a limit.
func taskHistoryBody(limit int64) TaskHistoryBody {
	return TaskHistoryBody{Limit: limit, DockerDefault: dockerx.DefaultTaskHistoryLimit, KeepsAll: limit < 0}
}

// RegisterJanitorTaskHistory registers the swarm task history limit routes.
func RegisterJanitorTaskHistory(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getTaskHistoryLimit", Method: http.MethodGet, Path: "/api/env/{envId}/janitor/task-history",
		Summary: "The swarm's task history limit",
		Description: "How many finished tasks per slot the swarm keeps (its TaskHistoryRetentionLimit), read live from a manager, with -1 as the manager reports it: " +
			"the swarm keeps every task. dockerDefault is 5. 409 not_swarm for a standalone host; 503 no_manager when no manager of the swarm answers. " +
			"Needs environments.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, _ *EnvironmentPathInput) (*TaskHistoryOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			set, err := readTaskHistory(ctx, d, env.ID)
			if err != nil {
				return nil, err
			}
			return &TaskHistoryOutput{CacheControl: "no-store", Body: taskHistoryBody(set.TaskHistoryLimit)}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "setTaskHistoryLimit", Method: http.MethodPut, Path: "/api/env/{envId}/janitor/task-history",
		Summary: "Set the swarm's task history limit",
		Description: "Sets how many finished tasks per slot the swarm keeps, a whole number from 0 to 1000; anything else is 422 and nothing is sent. " +
			"TierPlan reads the swarm's settings and version from a manager, changes only that one setting and sends one update with that version through a manager. " +
			"It never rotates a join token or the unlock key. Setting the limit the swarm already has answers 200 with changed false and sends and records nothing. " +
			"A real change is audited as swarm.task_history_limit_changed with the limit it replaced (from) and the one it set (to). " +
			"The update is never retried: if the swarm changed between the read and the update the answer is 409 swarm_changed and the person tries again. " +
			"409 not_swarm for a standalone host; 409 host_read_only when the host's agent is read-only; 503 no_manager when no manager answers; " +
			"502 agent_policy_denied when the manager's agent refuses the change by its own policy, which is what an agent older than this server does. " +
			"Needs janitor.run (Env Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermJanitorRun, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *TaskHistoryInput) (*TaskHistoryChangeOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.Aggregator == nil || d.Audit == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setTaskHistory(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &TaskHistoryChangeOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// readTaskHistory reads the swarm's settings from a manager. A host that is not a swarm is 409 not_swarm.
func readTaskHistory(ctx context.Context, d JanitorDeps, envID string) (dockerx.SwarmSettings, error) {
	if d.Clusters == nil {
		return dockerx.SwarmSettings{}, httpx.Problem(http.StatusConflict, problemNotSwarm, "This environment is not a swarm, so it has no task history limit.")
	}
	swarm, err := d.Clusters.IsSwarm(ctx, envID)
	if err != nil {
		_, problem := taskHistoryProblem(d, envID, err, false)
		return dockerx.SwarmSettings{}, problem
	}
	if !swarm {
		return dockerx.SwarmSettings{}, httpx.Problem(http.StatusConflict, problemNotSwarm, "This environment is not a swarm, so it has no task history limit.")
	}
	var set dockerx.SwarmSettings
	err = d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var rerr error
		set, rerr = c.SwarmSettings(ctx)
		return rerr
	})
	if err != nil {
		_, problem := taskHistoryProblem(d, envID, err, false)
		return dockerx.SwarmSettings{}, problem
	}
	return set, nil
}

// setTaskHistory reads the swarm's settings, and when the limit differs sends one update through a
// manager and records it. The update is sent once and seen through, so a browser that goes away while
// it runs does not turn a change that happened into a failure.
func setTaskHistory(ctx context.Context, d JanitorDeps, p *rbac.Principal, envID string, in *TaskHistoryInput) (TaskHistoryChangeBody, error) {
	set, err := readTaskHistory(ctx, d, envID)
	if err != nil {
		return TaskHistoryChangeBody{}, err
	}
	want := in.Body.Limit
	if set.TaskHistoryLimit == want {
		return TaskHistoryChangeBody{TaskHistoryBody: taskHistoryBody(want)}, nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		return c.SetTaskHistoryLimit(writeCtx, set, want)
	})
	if err != nil {
		code, problem := taskHistoryProblem(d, envID, err, true)
		auditTaskHistory(ctx, d, p, envID, in.UserAgent, set.TaskHistoryLimit, want, code)
		return TaskHistoryChangeBody{}, problem
	}
	auditTaskHistory(ctx, d, p, envID, in.UserAgent, set.TaskHistoryLimit, want, actionDone)
	return TaskHistoryChangeBody{TaskHistoryBody: taskHistoryBody(want), Changed: true}, nil
}

// auditTaskHistory writes the row of one change: a success when code is done, else a failure whose
// reason is the code. It is written on a context that outlives the request: the swarm has already
// been changed or has refused. Its details are exactly from and to.
func auditTaskHistory(ctx context.Context, d JanitorDeps, p *rbac.Principal, envID, userAgent string, from, to int64, code string) {
	result, reason := audit.ResultSuccess, ""
	if code != actionDone {
		result, reason = audit.ResultFailure, code
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionSwarmTaskHistoryChanged, TargetType: auditTargetEnvironment, TargetID: envID, EnvID: envID,
		Result: result, Reason: reason, IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: map[string]string{"from": strconv.FormatInt(from, 10), "to": strconv.FormatInt(to, 10)},
	}); err != nil {
		// The swarm has been changed or has refused and the caller must still be told which; the lost
		// row is the server's fault and is logged.
		d.log().Error("could not audit a task history limit change", "environment", envID, "error", err)
	}
}

// taskHistoryProblem turns the error of a read or an update of the swarm's settings into the code
// its audit row records and the problem the SPA words. write says the error came from the update,
// where an agent's refusal means the agent is older than this server.
func taskHistoryProblem(d JanitorDeps, envID string, err error, write bool) (string, error) {
	switch {
	case errors.Is(err, dockerx.ErrSwarmChanged):
		return problemSwarmChanged, httpx.Problem(http.StatusConflict, problemSwarmChanged, swarmChangedDetail)
	case errors.Is(err, envs.ErrHostReadOnly):
		return codeHostReadOnly, httpx.Problem(http.StatusConflict, codeHostReadOnly, swarmWriteDetail[codeHostReadOnly])
	case errors.Is(err, dockerx.ErrNotSwarmManager):
		return problemTaskHistoryNoManager, httpx.Problem(http.StatusServiceUnavailable, problemTaskHistoryNoManager, swarmWriteDetail[codeNoManagerConnected])
	}
	switch code := swarmWriteCode(err); code {
	case codeNoManagerConnected:
		return problemTaskHistoryNoManager, httpx.Problem(http.StatusServiceUnavailable, problemTaskHistoryNoManager, swarmWriteDetail[code])
	case codeAgentPolicyDeny, codeAgentHostPolicy:
		if write {
			d.log().Warn("an agent refused a task history change", "environment", envID)
			return codeAgentPolicyDeny, httpx.Problem(http.StatusBadGateway, problemAgentPolicyDenied, agentTooOldDetail)
		}
		return codeAgentPolicyDeny, snapshotProblem(d.log(), envID, err)
	}
	if envs.ReasonFor(err) == envs.ReasonNoQuorum {
		return codeNoQuorum, httpx.Problem(http.StatusServiceUnavailable, problemNoQuorum, "The swarm has lost its quorum.")
	}
	var down envs.ErrNotOnline
	if errors.As(envs.ClassifyDockerError(err), &down) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !write {
		return codeActionFailed, snapshotProblem(d.log(), envID, err)
	}
	d.log().Warn("the swarm did not accept a task history change", "environment", envID, "error", err)
	return problemTaskHistoryFailed, httpx.Problem(http.StatusBadGateway, problemTaskHistoryFailed, "The swarm did not accept the change. Nothing was changed.")
}
