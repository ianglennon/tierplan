// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// maxJanitorRunItems is how many items one Clean now may tick.
const maxJanitorRunItems = 5000

// maxJanitorRunBody bounds the body of a run: 5000 items of at most a few hundred bytes each.
const maxJanitorRunBody = 4 << 20

// problemTooManyJanitorRuns is the 429 for an Env Admin who already has the most jobs running.
const problemTooManyJanitorRuns = "too_many_jobs"

// JanitorItemRef names one ticked item of a preview. It only selects: the server plans again
// under the lock and removes the item only if it is removable then.
type JanitorItemRef struct {
	Type   string `json:"type" enum:"container,image,network" doc:"The type of the object, as the preview lists it"`
	NodeID string `json:"nodeId" maxLength:"128" doc:"The ID of the node the object is on, as the preview lists it. Empty for an object that belongs to the whole swarm"`
	Ref    string `json:"ref" pattern:"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$" maxLength:"256" doc:"The engine's ID of the object, as the preview lists it"`
}

// JanitorRunRequest is the body of POST /api/env/{envId}/janitor/runs.
type JanitorRunRequest struct {
	Items []JanitorItemRef `json:"items" minItems:"1" maxItems:"5000" doc:"The items ticked in the preview, 1 to 5000. Only those that are still removable when the run has taken the environment's lock are removed"`
}

// JanitorRunInput is the request of the Clean now route.
type JanitorRunInput struct {
	EnvironmentPathInput
	Body JanitorRunRequest
}

// JanitorRunStartedBody is the answer of POST /api/env/{envId}/janitor/runs.
type JanitorRunStartedBody struct {
	RunID string `json:"runId" doc:"The ID of this run"`
	JobID string `json:"jobId" doc:"The job that does the removing: follow it in the jobs list, one item for each removal"`
}

// JanitorRunStartedOutput wraps JanitorRunStartedBody for huma.
type JanitorRunStartedOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorRunStartedBody
}

// registerJanitorRuns registers Clean now.
func registerJanitorRuns(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "startJanitorRun", Method: http.MethodPost, Path: "/api/env/{envId}/janitor/runs",
		Summary: "Clean the environment now",
		Description: "Removes the items ticked in a preview, and answers 202 with the run and its job at once; the clean-up goes on if the browser closes. " +
			"The run takes the environment's operation lock, the one a deploy takes, and only while it removes. While a stack operation holds it this answers 409 operation_locked and starts nothing. " +
			"With the lock held it plans again from a fresh read of every node and removes the ticked items that are removable now, each once and never more: " +
			"an item that is not on the fresh plan, or whose container is no longer exited or has gained the tierplan.keep label, is left alone, and the job shows it. " +
			"A container is removed through the agent of its node, without force and without its volumes. " +
			"The job has one item for each removal. Needs janitor.run (Env Admin) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusAccepted, MaxBodyBytes: maxJanitorRunBody,
	}, rbac.Requirement{Perm: rbac.PermJanitorRun, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *JanitorRunInput) (*JanitorRunStartedOutput, error) {
			p := httpx.PrincipalFrom(ctx)
			env := httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.Runner == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			ticked := make([]janitor.ItemRef, 0, len(in.Body.Items))
			for _, it := range in.Body.Items {
				ticked = append(ticked, janitor.ItemRef{Type: janitor.Kind(it.Type), NodeID: it.NodeID, Ref: it.Ref})
			}
			started, err := d.Runner.Start(ctx, janitor.RunRequest{
				EnvID: env.ID, EnvName: env.Name, Trigger: janitor.TriggerManual, Actor: p.Name,
				Owner: stackJobOwner(p), Ticked: ticked,
			})
			var locked *janitor.LockedError
			switch {
			case errors.As(err, &locked):
				return nil, operationLocked(env.Name, locked.Holder)
			case errors.Is(err, jobs.ErrTooMany):
				return nil, httpx.Problem(http.StatusTooManyRequests, problemTooManyJanitorRuns, "You already have 5 jobs running. Wait for one to finish.")
			case err != nil:
				d.log().Error("could not start a janitor run", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &JanitorRunStartedOutput{CacheControl: "no-store", Body: JanitorRunStartedBody{RunID: started.RunID, JobID: started.JobID}}, nil
		})
}
