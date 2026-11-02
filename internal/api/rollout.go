// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// RolloutBody is how far a service's rolling update or rollback has got.
type RolloutBody struct {
	State           string `json:"state" enum:"none,updating,paused,completed,rollback_started,rollback_paused,rollback_completed" doc:"The swarm's UpdateStatus state: updating, paused or completed for an update; rollback_started, rollback_paused or rollback_completed for a rollback. none when the service has no UpdateStatus, which is also what a scale leaves"`
	Message         string `json:"message" doc:"Docker's own message about the update, cleaned of control characters; empty when it gave none. Docker's free text, to be shown as plain text"`
	StartedAt       string `json:"startedAt,omitempty" format:"date-time" doc:"When the update or rollback started; absent when Docker says none"`
	CompletedAt     string `json:"completedAt,omitempty" format:"date-time" doc:"When it completed; absent while it has not"`
	Updated         int    `json:"updated" doc:"Tasks that should run and are running on the service's current task template: the new version after an update, the earlier one after a rollback"`
	Total           int    `json:"total" doc:"Tasks the swarm wants running (desired state running)"`
	LatestTaskError string `json:"latestTaskError" doc:"Docker's error text of the newest failed or rejected task, cleaned of control characters; empty when there is none. Docker's free text, to be shown as plain text"`
}

// RolloutOutput wraps RolloutBody for huma.
type RolloutOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RolloutBody
}

// registerServiceRollout mounts the rollout route.
func registerServiceRollout(r *httpx.Router, d ServicesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getServiceRollout", Method: http.MethodGet, Path: "/api/env/{envId}/services/{serviceId}/rollout",
		Summary: "How far a service's rolling update has got",
		Description: "The state of the service's last update or rollback as the swarm reports it, and how many of the tasks it wants running are running on the service's current task template (a task's spec equals the template but for its runtime): updated of total. " +
			"A service with no UpdateStatus, which is also a service that was only scaled, answers state none. latestTaskError is the error of the newest failed or rejected task. " +
			"Docker's message and task error text are cleaned of control characters and invalid UTF-8 on the server and are to be shown as plain text. " +
			"One inspect and one task list read live from a manager; nothing is written. " +
			"404 service_not_found when the swarm has no such service; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. " +
			"Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceDetailInput) (*RolloutOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getServiceRollout(ctx, d, env.ID, in.ServiceID)
			if err != nil {
				return nil, err
			}
			return &RolloutOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// getServiceRollout reads a service and its tasks in one cluster read and counts the tasks
// on the service's current template. Nothing is written.
func getServiceRollout(ctx context.Context, d ServicesDeps, envID, serviceID string) (RolloutBody, error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return RolloutBody{}, err
	}
	var (
		detail dockerx.ServiceDetail
		rows   []dockerx.TaskRow
	)
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		if detail, _, e = c.ServiceInspect(readCtx, serviceID); e != nil {
			return e
		}
		rows, e = c.Tasks(readCtx, dockerx.TaskFilter{ServiceIDs: []string{detail.ID}})
		return e
	})
	if err != nil {
		return RolloutBody{}, serviceReadProblem(d, envID, err)
	}
	return rolloutBody(detail, rows), nil
}

// rolloutBody builds the answer from a service and its tasks (newest first). The counting is
// stackops.Progress, the same a stack deploy waits on.
func rolloutBody(detail dockerx.ServiceDetail, rows []dockerx.TaskRow) RolloutBody {
	c := stackops.Progress(detail, rows)
	body := RolloutBody{
		State: c.State, Message: cleanTaskText(c.Message), Updated: c.Running, Total: c.Desired,
		LatestTaskError: cleanTaskText(c.Err),
	}
	if u := detail.UpdateStatus; u != nil && u.State != "" {
		body.StartedAt = rfc3339(u.StartedAt)
		body.CompletedAt = rfc3339(u.CompletedAt)
	}
	return body
}
