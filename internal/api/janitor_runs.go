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

// maxJanitorRunBody bounds the body of a run: the 5000 items its schema allows, of at most a few
// hundred bytes each.
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

// problemJanitorRunNotFound is the 404 for a run that does not exist in the environment.
const problemJanitorRunNotFound = "janitor_run_not_found"

// JanitorRunPathInput is the path of one run of an environment.
type JanitorRunPathInput struct {
	EnvironmentPathInput
	RunID string `path:"runId" maxLength:"64" doc:"The ID of the run, as Clean now or the schedule gave it"`
}

// JanitorRunCountsBody is what a run counted by how its items ended.
type JanitorRunCountsBody struct {
	RemovedContainers int `json:"removedContainers" minimum:"0" doc:"Containers the run removed"`
	RemovedImages     int `json:"removedImages" minimum:"0" doc:"Images the run removed"`
	RemovedNetworks   int `json:"removedNetworks" minimum:"0" doc:"Networks the run removed"`
	Kept              int `json:"kept" minimum:"0" doc:"Selected items the run left alone: in use, kept by a label or no longer stale"`
	Failed            int `json:"failed" minimum:"0" doc:"Items whose removal errored"`
	SkippedOffline    int `json:"skippedOffline" minimum:"0" doc:"Items not sent because their node was not connected"`
}

// JanitorRunItemBody is one item a run attempted.
type JanitorRunItemBody struct {
	Seq       int               `json:"seq" minimum:"1" doc:"1 for the first item the run attempted, counting up"`
	Type      string            `json:"type" enum:"container,image,network"`
	NodeID    string            `json:"nodeId" doc:"The ID of the node the object was on"`
	Node      string            `json:"node" doc:"The node's name"`
	Ref       string            `json:"ref" doc:"The engine's ID of the object"`
	Name      string            `json:"name"`
	Reason    JanitorReasonBody `json:"reason" doc:"Why the run selected it. For a ticked item the fresh plan did not list, the reason the planner gave for leaving it, and an empty code when the planner did not look at it"`
	Outcome   string            `json:"outcome" enum:"removed,refused_in_use,skipped_protected,skipped_node_offline,error" doc:"How the attempt ended. refused_in_use: the engine said something uses it, so it was kept (a safety success, not an error). skipped_protected: nothing was removed because it is no longer stale (code no_longer_stale) or is already gone (code gone). skipped_node_offline: its node was not connected, so nothing was sent. error: the removal failed, with the code agent_policy_denied, host_read_only, node_unreachable or action_failed"`
	Code      string            `json:"code" doc:"A stable word for why the attempt did not end as a clean removal, empty for one that did: in_use, no_longer_stale, gone, node_offline, agent_policy_denied, host_read_only, node_unreachable or action_failed"`
	SizeBytes *int64            `json:"sizeBytes" nullable:"true" minimum:"0" doc:"The bytes removing the object frees, null when not known"`
}

// JanitorRunBody is one run of the janitor and every item it attempted.
type JanitorRunBody struct {
	ID         string               `json:"id" doc:"The ID of this run"`
	Trigger    string               `json:"trigger" enum:"schedule,manual" doc:"schedule for a run the schedule started, manual for Clean now"`
	Actor      string               `json:"actor" doc:"Who started it: the user's ID, or the name of the system for a scheduled run"`
	ActorName  string               `json:"actorName" doc:"The display name of the person who started it; for a scheduled run, the name of the system"`
	State      string               `json:"state" enum:"waiting,running,completed,partial,skipped,failed" doc:"completed: every item ended as it should. partial: an item errored or the run was cut short. skipped: the run did not run. failed: the run could not do its work"`
	Reason     string               `json:"reason" enum:",deploy_in_progress,server_restarted,no_node_reporting,failed" doc:"Why a run did not complete; empty when it did"`
	StartedAt  string               `json:"startedAt" format:"date-time"`
	FinishedAt *string              `json:"finishedAt" nullable:"true" format:"date-time" doc:"When the run ended; null while it is working"`
	Counts     JanitorRunCountsBody `json:"counts"`
	EstBytes   *int64               `json:"estBytes" nullable:"true" minimum:"0" doc:"The bytes the removals freed that were known. Null when no size was known, which is not zero"`
	JobID      string               `json:"jobId" doc:"The job that did the removing; empty until it exists"`
	Items      []JanitorRunItemBody `json:"items" doc:"Every item the run attempted, in the order it attempted them"`
}

// JanitorRunOutput wraps JanitorRunBody for huma.
type JanitorRunOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorRunBody
}

// registerJanitorRuns registers Clean now and reading a run.
func registerJanitorRuns(r *httpx.Router, d JanitorDeps) {
	registerJanitorRunRead(r, d)
	httpx.Register(r, huma.Operation{
		OperationID: "startJanitorRun", Method: http.MethodPost, Path: "/api/env/{envId}/janitor/runs",
		Summary: "Clean the environment now",
		Description: "Removes the items ticked in a preview, and answers 202 with the run and its job at once; the clean-up goes on if the browser closes. " +
			"The run takes the environment's operation lock, the one a deploy takes, and only while it removes. While a stack operation holds it this answers 409 operation_locked and starts nothing. " +
			"With the lock held it plans again from a fresh read of every node and removes the ticked items that are removable now, each once and never more: " +
			"an item that is not on the fresh plan, or whose container is no longer exited or has gained the tierplan.keep label, is left alone and the run records it. " +
			"An item the engine refuses because something uses it is kept, recorded as such and not tried again; one that fails is recorded with a code and not tried again either. " +
			"A container is removed through the agent of its node, without force and without its volumes. " +
			"Networks are removed after the containers, each judged again from a fresh read made then and removed by the very code the Address space page's delete uses: " +
			"the network's whole inspect is saved in the audit log first, one remove is sent, and the audit rows carry the source janitor and the person who started the run. " +
			"A network the engine says is in use is kept; one that is no longer orphaned or unused is left alone; while a node is not reporting no network is removed. " +
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
				EnvID: env.ID, EnvName: env.Name, Trigger: janitor.TriggerManual, Actor: p.Name, ActorID: p.ID,
				Owner: stackJobOwner(p), Principal: p, Ticked: ticked,
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

// registerJanitorRunRead registers the read of one run.
func registerJanitorRunRead(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getJanitorRun", Method: http.MethodGet, Path: "/api/env/{envId}/janitor/runs/{runId}",
		Summary: "Read one janitor run",
		Description: "Answers one run of the janitor in this environment: what started it and who, its state, when it began and ended, what it counted, " +
			"and every item it attempted with why it was selected and how the attempt ended. A run is on record before it removes anything, " +
			"so a run that is still working answers with state running and the items so far. " +
			"A run of another environment, or one that does not exist, is 404 janitor_run_not_found. Needs environments.read on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *JanitorRunPathInput) (*JanitorRunOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Runs == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			run, items, err := d.Runs.GetRun(ctx, env.ID, in.RunID)
			switch {
			case errors.Is(err, janitor.ErrRunNotFound):
				return nil, httpx.Problem(http.StatusNotFound, problemJanitorRunNotFound, "This run does not exist in this environment.")
			case err != nil:
				d.log().Error("could not read a janitor run", "environment", env.ID, "run", in.RunID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &JanitorRunOutput{CacheControl: "no-store", Body: janitorRunBody(ctx, d, run, items)}, nil
		})
}

// janitorRunBody is a run as the API shows it.
func janitorRunBody(ctx context.Context, d JanitorDeps, run janitor.Run, items []janitor.RunItem) JanitorRunBody {
	b := JanitorRunBody{
		ID: run.ID, Trigger: run.Trigger, Actor: run.Actor, ActorName: janitorActorName(ctx, d, run.Actor),
		State: run.State, Reason: run.Reason, StartedAt: rfc3339(run.StartedAt), JobID: run.JobID,
		Counts: JanitorRunCountsBody{
			RemovedContainers: run.Counts.RemovedContainers, RemovedImages: run.Counts.RemovedImages,
			RemovedNetworks: run.Counts.RemovedNetworks, Kept: run.Counts.Kept, Failed: run.Counts.Failed,
			SkippedOffline: run.Counts.SkippedOffline,
		},
		Items: make([]JanitorRunItemBody, 0, len(items)),
	}
	if !run.FinishedAt.IsZero() {
		finished := rfc3339(run.FinishedAt)
		b.FinishedAt = &finished
	}
	if run.EstBytes >= 0 {
		est := run.EstBytes
		b.EstBytes = &est
	}
	for _, it := range items {
		ib := JanitorRunItemBody{
			Seq: it.Seq, Type: string(it.Type), NodeID: it.NodeID, Node: it.Node, Ref: it.Ref, Name: it.Name,
			Reason: JanitorReasonBody{
				Code: string(it.Reason.Code), Service: it.Reason.Service, Slot: it.Reason.Slot, Project: it.Reason.Project,
				Stack: it.Reason.Stack, Node: it.Reason.Node, Rank: it.Reason.Rank,
				AgeSeconds: int64(it.Reason.Age.Seconds()), RemainingSeconds: int64(it.Reason.Remaining.Seconds()),
			},
			Outcome: string(it.Outcome), Code: it.Code,
		}
		if it.Size >= 0 {
			size := it.Size
			ib.SizeBytes = &size
		}
		b.Items = append(b.Items, ib)
	}
	return b
}

// janitorActorName is the display name of whoever started a run: the user's display name, else
// their username. An actor that is not a user (a scheduled run names the system) is its own name.
func janitorActorName(ctx context.Context, d JanitorDeps, actor string) string {
	if d.Store != nil {
		if u, err := d.Store.RQ.GetUserByID(ctx, actor); err == nil {
			if u.DisplayName != "" {
				return u.DisplayName
			}
			return u.Username
		}
	}
	return actor
}
