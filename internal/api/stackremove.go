// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

const (
	// stackRemoveJobKind is the Kind of a removal in the jobs hub.
	stackRemoveJobKind = "stack_remove"

	// problemConfirmMismatch (restore.go) is the typed name that differs.
	problemVolumeNotOwned = "volume_not_owned"
)

// RemovalPreviewInput is the path of the removal preview.
type RemovalPreviewInput struct {
	StackPathInput
}

// RemovalVolumeBody is one volume of the stack on one node.
type RemovalVolumeBody struct {
	Name        string `json:"name" doc:"The volume's name"`
	NodeID      string `json:"nodeId" doc:"The node that holds it: a volume is local to a node, so a swarm stack's volume has a row for each node that has it"`
	NodeName    string `json:"nodeName" doc:"The node's display name"`
	UsedOutside int    `json:"usedOutside" doc:"How many containers outside this stack mount the volume on that node; removing the volume fails while they exist"`
}

// RemovalPreviewBody is what a removal of the stack would delete.
type RemovalPreviewBody struct {
	Services     []string            `json:"services" doc:"The stack's swarm services, by name"`
	Containers   []string            `json:"containers" doc:"The stack's containers on a standalone host, by name"`
	Networks     []string            `json:"networks" doc:"The networks that carry this stack's own labels"`
	Secrets      []string            `json:"secrets" doc:"The secrets that carry this stack's own labels (the hashed ones a deploy made), by name on the swarm"`
	Configs      []string            `json:"configs" doc:"The configs that carry this stack's own labels, by name on the swarm"`
	Volumes      []RemovalVolumeBody `json:"volumes" doc:"The stack's own volumes, one for each node that holds one. A removal deletes only the ones the request names"`
	Externals    []string            `json:"externals" doc:"The networks, secrets, configs and volumes the compose file names as external. A removal never touches them"`
	NotReporting []string            `json:"notReporting" doc:"The nodes that did not answer, whose volumes are left out of the list"`
	HasDraft     bool                `json:"hasDraft" doc:"True when the stack has a saved draft with changes; a removal deletes it with the definition unless the definition is kept"`
}

// RemovalPreviewOutput wraps RemovalPreviewBody for huma.
type RemovalPreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RemovalPreviewBody
}

// RemoveVolumeRequest names one volume to delete.
type RemoveVolumeRequest struct {
	Name   string `json:"name" minLength:"1" maxLength:"255" doc:"The volume's name"`
	NodeID string `json:"nodeId" minLength:"1" maxLength:"200" doc:"The node it is on"`
}

// RemoveStackRequest is the body of the remove route.
type RemoveStackRequest struct {
	ConfirmName    string                `json:"confirmName" maxLength:"200" doc:"The stack's name, typed exactly. Anything else is 422 confirm_mismatch and nothing is removed"`
	Volumes        []RemoveVolumeRequest `json:"volumes" maxItems:"2000" doc:"The volumes to delete, each on its node. A volume that is not listed stays. One that is not this stack's own on that node is 422 volume_not_owned and nothing is removed"`
	KeepDefinition bool                  `json:"keepDefinition" doc:"True keeps the stack's files, variables and revisions, and the stack reads Not deployed"`
}

// RemoveStackInput is the path and body of the remove route.
type RemoveStackInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      RemoveStackRequest
}

// RemoveStackBody is a removal that has begun.
type RemoveStackBody struct {
	JobID string `json:"jobId" doc:"The job: read it at GET .../deploys/{jobId}, the same console route a deploy uses. It stays readable after the stack is gone"`
}

// RemoveStackOutput wraps RemoveStackBody for huma.
type RemoveStackOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RemoveStackBody
}

func registerStackRemove(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "removalPreview", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/removal-preview",
		Summary: "What removing a stack would delete",
		Description: "Lists the services (or containers), networks, secrets and configs that carry the stack's own labels, the stack's volumes one for each node that holds one, and the external objects the compose file names, which a removal never touches. " +
			"An object is the stack's only when it carries both its namespace and its TierPlan stack ID. Each volume says how many containers outside the stack mount it. It is a read. Needs stacks.delete (Env Admin).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksDelete, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *RemovalPreviewInput) (*RemovalPreviewOutput, error) {
			return removalPreview(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "removeStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/remove",
		Summary: "Remove a stack",
		Description: "Removes everything the stack owns as a job, answering 202 with the job ID at once: its services or containers, then a bounded wait until its tasks or containers are gone, then its networks, then its own secrets and configs, then the volumes the request names, then the definition. " +
			"External networks, secrets, configs and volumes are never touched, and a volume is deleted only when the request names it and it carries this stack's labels on that node. " +
			"The name must be typed exactly (422 confirm_mismatch). With keepDefinition the files, variables and revisions stay and the stack reads Not deployed. A failure part-way leaves what was removed removed and keeps the definition. " +
			"Only one stack operation at a time runs on an environment: while one does, the answer is 409 operation_locked and nothing is stored. A stack that is gone is 404. " +
			"Writes one stack.removed audit row when the job ends. Needs stacks.delete (Env Admin).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksDelete, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *RemoveStackInput) (*RemoveStackOutput, error) {
			return removeStack(ctx, d, in)
		})
}

func removalVolumes(vols []stackops.VolumeChoice) []RemovalVolumeBody {
	out := make([]RemovalVolumeBody, 0, len(vols))
	for _, v := range vols {
		out = append(out, RemovalVolumeBody{Name: v.Name, NodeID: v.NodeID, NodeName: v.NodeName, UsedOutside: v.UsedOutside})
	}
	return out
}

// removalPreviewOf reads what a removal would delete, with the saved draft the repo knows.
func removalPreviewOf(ctx context.Context, d StacksDeps, envID string, st stackops.Stack) (stackops.RemovalPreview, error) {
	repo := d.repo()
	decl, err := repo.Declared(ctx, st)
	if err != nil {
		return stackops.RemovalPreview{}, err
	}
	preview, err := d.ops().Preview(ctx, envID, st, decl)
	if err != nil {
		return stackops.RemovalPreview{}, err
	}
	pending, err := repo.DraftStates(ctx, envID)
	if err != nil {
		return stackops.RemovalPreview{}, err
	}
	preview.HasDraft = pending[st.ID]
	return preview, nil
}

func removalPreview(ctx context.Context, d StacksDeps, in *RemovalPreviewInput) (*RemovalPreviewOutput, error) {
	env := httpx.EnvFrom(ctx)
	if env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	st, err := d.repo().Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	if err := d.ops().CheckTarget(ctx, env.ID, st); err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	preview, err := removalPreviewOf(ctx, d, env.ID, st)
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	return &RemovalPreviewOutput{CacheControl: "no-store", Body: RemovalPreviewBody{
		Services: preview.Services, Containers: preview.Containers, Networks: preview.Networks,
		Secrets: preview.Secrets, Configs: preview.Configs, Volumes: removalVolumes(preview.Volumes),
		Externals: preview.Externals, NotReporting: preview.NotReporting, HasDraft: preview.HasDraft,
	}}, nil
}

// chosenVolumes matches the volumes a request names against the stack's own volumes now, and returns the
// stack's rows for them: the request is never trusted for a name of a node or a count. A volume that is not
// one of the stack's own on that node is refused and nothing is removed (T-07-131).
func chosenVolumes(asked []RemoveVolumeRequest, owned []stackops.VolumeChoice) ([]stackops.VolumeChoice, error) {
	var out []stackops.VolumeChoice
	for _, a := range asked {
		i := slices.IndexFunc(owned, func(v stackops.VolumeChoice) bool { return v.Name == a.Name && v.NodeID == a.NodeID })
		if i < 0 {
			return nil, &huma.ErrorModel{
				Type: problemVolumeNotOwned, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
				Detail: a.Name + " isn't one of this stack's volumes on that node, so it can't be removed with the stack.",
				Errors: []*huma.ErrorDetail{{Location: "volumes", Message: a.Name}},
			}
		}
		if !slices.ContainsFunc(out, func(v stackops.VolumeChoice) bool { return v.Name == a.Name && v.NodeID == a.NodeID }) {
			out = append(out, owned[i])
		}
	}
	return out, nil
}

// removeStack judges a removal, takes the environment's operation lock and starts the job. Every step before
// the lock is a read; nothing is stored until the stack, the typed name and the volumes have all passed.
func removeStack(ctx context.Context, d StacksDeps, in *RemoveStackInput) (*RemoveStackOutput, error) {
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
	// The name is checked on the server: a browser that skipped the typed field removes nothing.
	if in.Body.ConfirmName != st.Name {
		return nil, &huma.ErrorModel{
			Type: problemConfirmMismatch, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: "Type the stack's name exactly to remove it.",
			Errors: []*huma.ErrorDetail{{Location: "confirmName", Message: "does not match"}},
		}
	}
	if err := d.ops().CheckTarget(ctx, env.ID, st); err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	node := ""
	if st.Target == stacks.TargetStandalone {
		if node, err = d.ops().StandaloneNode(ctx, env.ID); err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
	}
	var volumes []stackops.VolumeChoice
	if len(in.Body.Volumes) > 0 {
		preview, err := d.ops().Preview(ctx, env.ID, st, stackops.Declared{})
		if err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		if volumes, err = chosenVolumes(in.Body.Volumes, preview.Volumes); err != nil {
			return nil, err
		}
	}

	release, held, ok := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackID: st.ID, StackName: st.Name, Kind: stackops.ModeRemove, By: p.Name, Started: time.Now().UTC(),
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

	var rres stackops.RemoveResult
	input := stackops.RemoveInput{
		EnvID: env.ID, NodeID: node, Stack: st, Volumes: volumes, KeepDefinition: in.Body.KeepDefinition,
		Caller: stackops.Caller{Principal: p, Actor: p.Name}, Store: repo, Masker: stacks.NewMasker(nil, nil),
	}
	out, err := launchStackDeploy(ctx, d, launch{
		p: p, env: env, st: st, mode: stackops.ModeRemove, kind: stackops.ModeRemove, userAgent: in.UserAgent, jobKind: stackRemoveJobKind,
		items: stackops.RemoveItemSpecs(st.Target, volumes, in.Body.KeepDefinition),
		run: func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult)) {
			input.OnDone = func(res stackops.RemoveResult) {
				rres = res
				onDone(stackops.DeployResult{Code: res.Code, FailedService: res.FailedName})
			}
			d.ops().Remove(jctx, input, r)
		},
		record: func(rctx context.Context, deploy stackops.DeployRow, res stackops.DeployResult) {
			recordRemoveEnd(rctx, d, p.AuditActor(), in.UserAgent, st, deploy, rres, res)
		},
		release: release, owned: &jobOwnsLock,
	})
	if err != nil {
		return nil, err
	}
	return &RemoveStackOutput{CacheControl: "no-store", Body: RemoveStackBody{JobID: out.Body.JobID}}, nil
}

// recordRemoveEnd stores how a removal ended: the operation row (which is gone with the stack when the
// definition was deleted) and the audit row. It runs before the job's last step is finished, so a job that
// reads as finished has already been recorded.
func recordRemoveEnd(ctx context.Context, d StacksDeps, actor audit.Actor, userAgent string, st stackops.Stack, deploy stackops.DeployRow, rres stackops.RemoveResult, res stackops.DeployResult) {
	state, result := stackops.DeployDone, audit.ResultSuccess
	resultWord := "done"
	switch {
	case res.Code == stackops.CodeCancelled:
		state, result, resultWord = stackops.DeployCancelled, audit.ResultFailure, "cancelled"
	case !res.OK():
		state, result, resultWord = stackops.DeployFailed, audit.ResultFailure, "failed"
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	cancelledBy := d.cancels.take(deploy.JobID)
	// A deleted stack took its operations with it: there is no row left to finish.
	if !rres.DefinitionDeleted {
		if err := d.repo().FinishDeploy(wctx, deploy.ID, state, res.FailedService, res.Code); err != nil {
			d.Log.Error("could not finish a stack removal row", "stack", st.ID, "deploy", deploy.ID, "error", err)
		}
	}
	// The stack's state is read from the cluster cache, so read the swarm again: a stack that reads finished
	// must not show what it ran before.
	if st.Target == stacks.TargetSwarm && d.Clusters != nil {
		rctx, cancelRead := context.WithTimeout(wctx, writeTimeout)
		_ = d.Clusters.Refresh(rctx, st.EnvID)
		cancelRead()
	}
	definition := "kept"
	if rres.DefinitionDeleted {
		definition = "deleted"
	}
	details := map[string]string{
		"stack": st.Name, "volumes": itoa(int64(rres.Volumes)), "kept": itoa(int64(rres.KeptVolumes)),
		"definition": definition, "result": resultWord,
	}
	if resultWord == "cancelled" && cancelledBy != "" {
		details["cancelledBy"] = cancelledBy
	}
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: actor, Action: audit.ActionStackRemoved, TargetType: stackTargetType, TargetID: st.ID, EnvID: st.EnvID,
		Result: result, Reason: res.Code, IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		d.Log.Error("could not audit a stack removal", "stack", st.ID, "error", err)
	}
}
