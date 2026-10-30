// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
)

const (
	// stackStopJobKind and stackStartJobKind are the Kinds of a stop and a start in the jobs hub.
	stackStopJobKind  = "stack_stop"
	stackStartJobKind = "stack_start"

	problemNothingRunning = "nothing_running"
	problemNotStopped     = "not_stopped"
	// problemStackStopped is the refusal of an operation that needs what runs when the stack is stopped.
	problemStackStopped = "stack_stopped"

	nothingRunning = "Nothing is running."
	notStopped     = "This stack isn't stopped."
	stackStopped   = "This stack is stopped, so there's nothing running to keep. Start it first."
)

// StackLifecycleInput is the path of the stop and start routes. They have no body: a stop parks what runs and a
// start brings back the last deployed version, so neither is given anything to restore.
type StackLifecycleInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// StackLifecycleBody is a stop or a start that has begun.
type StackLifecycleBody struct {
	JobID     string `json:"jobId" doc:"The job: read it at GET .../deploys/{jobId}, the same console route a deploy uses"`
	Recreates bool   `json:"recreates" doc:"True for a start that has to create services again (a global or job service a stop removed), which the page follows in the console; a start that only scales opens nothing. Always false for a stop"`
}

// StackLifecycleOutput wraps StackLifecycleBody for huma.
type StackLifecycleOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackLifecycleBody
}

func registerStackLifecycle(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "stopStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/stop",
		Summary: "Stop a stack",
		Description: "Parks the stack as a job, answering 202 with the job ID at once. On a swarm every replicated service is scaled to 0 and its count is remembered in the service-level label coaming.stack.stopped-replicas (the label is not part of the task template, so no task is replaced), and the global and job services are removed. On a standalone host the Compose SDK stops the stack's containers. Networks, volumes, secrets and configs are never removed. " +
			"The stack is marked stopped once everything has stopped; a stop that fails stops where it failed and what already stopped stays stopped. " +
			"A stack that is stopped already, was never deployed or has nothing running is 409 nothing_running. " +
			"Only one stack operation at a time runs on an environment: while one does, the answer is 409 operation_locked naming it and nothing is stored. " +
			"Writes one stack.stopped audit row when the job ends. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackLifecycleInput) (*StackLifecycleOutput, error) {
			return lifecycleStack(ctx, d, in, stackops.LifecycleStop)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "startStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/start",
		Summary: "Start a stopped stack",
		Description: "Brings a stopped stack back as a job, answering 202 with the job ID and whether it recreates services. It brings back the last version that was deployed, never the saved draft and never a newer image: on a swarm each service is scaled back to the count it remembered and the label removed, and a service a stop removed is created again from the last good revision with the image digest it ran; on a standalone host the Compose SDK starts the stopped containers and a stack file's secrets and configs are written into each again before it starts. " +
			"A start that creates services is judged by the host-access policy for the caller, as a deploy is: below env Admin it is refused 403 privileged_compose when the revision uses host access. " +
			"A stack that is not stopped is 409 not_stopped. While another stack operation runs on the environment the answer is 409 operation_locked and nothing is stored. " +
			"Writes one stack.started audit row when the job ends. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackLifecycleInput) (*StackLifecycleOutput, error) {
			return lifecycleStack(ctx, d, in, stackops.LifecycleStart)
		})
}

// lifecycleStack judges a stop or a start, takes the environment's operation lock and starts the job. Every step
// before the lock is a read; nothing is stored until the stack, its target and the host-access policy have all
// passed.
func lifecycleStack(ctx context.Context, d StacksDeps, in *StackLifecycleInput, mode string) (*StackLifecycleOutput, error) {
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
	node := ""
	if st.Target == stacks.TargetStandalone {
		if node, err = d.ops().StandaloneNode(ctx, env.ID); err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
	}
	caller := stackops.Caller{Principal: p, Actor: p.Name}
	recreates := false
	switch mode {
	case stackops.LifecycleStop:
		if st.Stopped {
			return nil, httpx.Problem(http.StatusConflict, problemNothingRunning, nothingRunning)
		}
		held, err := d.ops().Workload(ctx, env.ID, node, st)
		if err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		deployed := true
		if st.Target == stacks.TargetStandalone {
			// The SDK stops the containers of a project it is given: a standalone stack that never ran well has none.
			_, deployed, err = repo.GoodRevisionInfo(ctx, st.ID)
			if err != nil {
				return nil, stackInternal(d, "read the running revision", env.ID, err)
			}
		}
		if held == 0 || !deployed {
			return nil, httpx.Problem(http.StatusConflict, problemNothingRunning, nothingRunning)
		}
	default:
		if !st.Stopped {
			return nil, httpx.Problem(http.StatusConflict, problemNotStopped, notStopped)
		}
		check, err := d.ops().CheckStart(ctx, env.ID, st, repo, caller)
		var pe *stackops.PrepareError
		switch {
		case errors.As(err, &pe):
			return nil, rollbackPrepareProblem(err, pe.Masker)
		case errors.Is(err, stackops.ErrNoPolicy):
			return nil, stackInternal(d, "policy", env.ID, err)
		case err != nil:
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		if check.Policy.Refused {
			return nil, policyProblem(check.Policy)
		}
		recreates = check.Recreates
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

	kind := stackStopJobKind
	if mode == stackops.LifecycleStart {
		kind = stackStartJobKind
	}
	var lres stackops.LifecycleResult
	input := stackops.LifecycleInput{
		EnvID: env.ID, NodeID: node, Stack: st, Caller: caller, Store: repo,
		Masker: stacks.NewMasker(nil, nil),
	}
	out, err := launchStackDeploy(ctx, d, launch{
		p: p, env: env, st: st, mode: mode, kind: mode, userAgent: in.UserAgent, jobKind: kind,
		items: stackops.LifecycleItemSpecs(mode, st.Target),
		run: func(jctx context.Context, r *jobs.Reporter, onDone func(stackops.DeployResult)) {
			input.OnDone = func(res stackops.LifecycleResult) {
				lres = res
				onDone(stackops.DeployResult{Code: res.Code, FailedService: res.FailedService})
			}
			if mode == stackops.LifecycleStop {
				d.ops().Stop(jctx, input, r)
			} else {
				d.ops().Start(jctx, input, r)
			}
		},
		record: func(rctx context.Context, deploy stackops.DeployRow, res stackops.DeployResult) {
			recordLifecycleEnd(rctx, d, p.AuditActor(), in.UserAgent, st, deploy, mode, lres, res)
		},
		release: release, owned: &jobOwnsLock,
	})
	if err != nil {
		return nil, err
	}
	return &StackLifecycleOutput{CacheControl: "no-store", Body: StackLifecycleBody{JobID: out.Body.JobID, Recreates: recreates}}, nil
}

// recordLifecycleEnd stores how a stop or a start ended: the operation row and the audit row. The stack's own
// record of being stopped was written by the engine when everything had stopped or started. It runs before the
// job's last step is finished, so a job that reads as finished has already been recorded.
func recordLifecycleEnd(ctx context.Context, d StacksDeps, actor audit.Actor, userAgent string, st stackops.Stack, deploy stackops.DeployRow, mode string, lres stackops.LifecycleResult, res stackops.DeployResult) {
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
	if err := d.repo().FinishDeploy(wctx, deploy.ID, state, res.FailedService, res.Code); err != nil {
		d.Log.Error("could not finish a stack operation row", "stack", st.ID, "deploy", deploy.ID, "error", err)
	}
	// The stack's state is read from the cluster cache, so read the swarm again: a stack that reads finished
	// must not show what it ran before.
	if st.Target == stacks.TargetSwarm && d.Clusters != nil {
		rctx, cancelRead := context.WithTimeout(wctx, writeTimeout)
		_ = d.Clusters.Refresh(rctx, st.EnvID)
		cancelRead()
	}
	action := audit.ActionStackStopped
	if mode == stackops.LifecycleStart {
		action = audit.ActionStackStarted
	}
	details := map[string]string{
		"stack": st.Name, "services": itoa(int64(lres.Services())), "recreated": itoa(int64(len(lres.Recreated))), "result": resultWord,
	}
	if resultWord == "cancelled" && cancelledBy != "" {
		details["cancelledBy"] = cancelledBy
	}
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: actor, Action: action, TargetType: stackTargetType, TargetID: st.ID, EnvID: st.EnvID,
		Result: result, Reason: res.Code, IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		d.Log.Error("could not audit a stack stop or start", "stack", st.ID, "action", action, "error", err)
	}
}
