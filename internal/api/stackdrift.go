// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// StackDriftServiceBody is one service that differs from the deployed revision.
type StackDriftServiceBody struct {
	Name          string         `json:"name" doc:"The swarm service name"`
	Short         string         `json:"short" doc:"The service's name in the compose file"`
	Change        string         `json:"change" enum:"changed,missing,extra" doc:"changed: the service runs with other settings than the revision deployed. missing: the revision has the service and the swarm does not. extra: the swarm has a service under the stack's ownership that the revision does not have"`
	Current       map[string]any `json:"current" nullable:"true" doc:"What the revision deployed, masked: a value derived from a secret of the stack is <hidden>, and for a caller without secrets.reveal so is every environment value, command, argument, health check command and log option. Null for an extra service"`
	Next          map[string]any `json:"next" nullable:"true" doc:"What runs now, masked the same way. Null for a missing service"`
	HiddenChanges []string       `json:"hiddenChanges" doc:"Paths of settings whose value changed to or from one that is hidden. The values are never sent"`
	Settings      int            `json:"settings" doc:"How many settings differ between current and next, hidden ones included"`
}

// StackDriftBody is how what runs compares with the deployed revision of a stack.
type StackDriftBody struct {
	State      string                  `json:"state" enum:"none,drifted,stopped,not_deployed,unknown" doc:"none: what runs matches the revision. drifted: it differs. stopped: a person stopped the stack, which is not drift. not_deployed: the stack has no revision that deployed. unknown: the revision cannot be compared (it can no longer be redeployed, or this kind of stack is not checked yet)"`
	Revision   int64                   `json:"revision" doc:"The number of the revision compared with, the newest one that deployed; 0 for none"`
	DeployedAt string                  `json:"deployedAt" doc:"When that revision's deploy was started, RFC 3339 UTC; empty for none"`
	DeployedBy string                  `json:"deployedBy" doc:"Who started it, as a name; empty for none"`
	CheckedAt  string                  `json:"checkedAt" doc:"When the stack was read, RFC 3339 UTC"`
	Count      int                     `json:"count" doc:"How many settings differ in all: the settings of each changed service plus one for each missing or extra service"`
	Services   []StackDriftServiceBody `json:"services" doc:"The services that differ, in name order (changed and missing ones, then extra ones)"`
}

// StackDriftSummaryBody is the drift a stack's page can say without the services: the newest check that is still about
// the revision that is deployed.
type StackDriftSummaryBody struct {
	State     string `json:"state" enum:"none,drifted,stopped,not_deployed,unknown" doc:"As the drift route's state"`
	Revision  int64  `json:"revision" doc:"The revision the check compared with"`
	CheckedAt string `json:"checkedAt" doc:"When the stack was read, RFC 3339 UTC"`
	Count     int    `json:"count" doc:"How many settings differ in all"`
}

// StackDriftInput is the path of the drift route and whether to read the stack again.
type StackDriftInput struct {
	EnvID   string `path:"envId" doc:"The environment ID"`
	StackID string `path:"stackId" doc:"The stack ID"`
	Fresh   bool   `query:"fresh" doc:"True reads the stack again instead of answering the newest check. Callers that ask at the same moment share one read"`
}

// StackDriftOutput wraps StackDriftBody for huma.
type StackDriftOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackDriftBody
}

// maxDriftChecksPerUser is how many reads of a stack for drift one user may have running: each reads the swarm.
const maxDriftChecksPerUser = 2

// newDriftLimiter is the limiter of the drift route.
func newDriftLimiter() *streamLimiter {
	return &streamLimiter{max: maxDriftChecksPerUser, open: map[string]int{}}
}

func registerStackDrift(r *httpx.Router, d StacksDeps, lim *streamLimiter) {
	httpx.Register(r, huma.Operation{
		OperationID: "getStackDrift", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/drift",
		Summary: "How what runs compares with the deployed revision",
		Description: "Drift is any difference between the stack's deployed revision (the newest one that deployed, converted with the images it stored) and what the swarm runs: replicas, image, environment, ports, labels, mounts, placement, and services added or missing under the stack's ownership. " +
			"What the swarm adds by itself is not drift (the spec version, the update status, the counter that forces new tasks, Bayplan's own bayplan.* labels). A stopped stack is state stopped, and a stack with no revision that deployed is not_deployed. " +
			"The newest check is answered unless fresh is true or there is none; callers that ask for a fresh check of one stack at the same moment share one read. Nothing is changed on any engine. " +
			"The answer holds the live settings of running services, so for a caller without secrets.reveal on the environment every environment value, command, argument, health check command and log option is <hidden>, " +
			"and a setting that differs only in such a value is listed by its path in hiddenChanges, with no value. " +
			"One user may have two reads running; a third is 429 check_busy. A stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackDriftInput) (*StackDriftOutput, error) {
			return getStackDrift(ctx, d, lim, in)
		})
}

func getStackDrift(ctx context.Context, d StacksDeps, lim *streamLimiter, in *StackDriftInput) (*StackDriftOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	info, hasInfo, err := repo.GoodRevisionInfo(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "read the deployed revision", env.ID, err)
	}
	deployedSeq := int64(0)
	if hasInfo {
		deployedSeq = info.Seq
	}
	redacted := !redact.CanReveal(ctx, d.Checker, p, env.ID)
	answer := func(res stackops.DriftResult) *StackDriftOutput {
		return &StackDriftOutput{CacheControl: "no-store", Body: driftBody(res, info, hasInfo, st.Target, redacted)}
	}

	if cached, ok := d.Drift.Get(st.ID); ok && !in.Fresh && driftCurrent(cached, st, deployedSeq) {
		return answer(cached), nil
	}
	ops := d.ops()
	if !hasInfo || st.Stopped {
		// Neither state reads an engine, so neither holds a slot or waits for another reader.
		res, err := ops.Drift(ctx, stackops.DriftInput{EnvID: env.ID, Stack: st, Deployed: deployedStub(info, hasInfo)})
		if err != nil {
			return nil, targetProblem(d, env.ID, err)
		}
		d.Drift.Put(st.ID, res)
		return answer(res), nil
	}

	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemCheckBusy, "Too many drift checks are running. Try again in a moment.")
	}
	defer release()
	if d.OnDrift != nil {
		d.OnDrift(ctx)
	}
	res, err := d.Drift.Check(ctx, st.ID, func(cctx context.Context) (stackops.DriftResult, error) {
		good, ok, err := repo.LatestGoodRevision(cctx, st.ID)
		if err != nil {
			return stackops.DriftResult{}, err
		}
		if !ok {
			return ops.Drift(cctx, stackops.DriftInput{EnvID: env.ID, Stack: st})
		}
		pins, err := repo.RevisionPins(cctx, good.ID)
		if err != nil {
			return stackops.DriftResult{}, err
		}
		return ops.Drift(cctx, stackops.DriftInput{EnvID: env.ID, Stack: st, Deployed: &good, Pins: pins})
	})
	if err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	return answer(res), nil
}

// deployedStub is the deployed revision for a check that reads no content: its number, nil for none.
func deployedStub(info stackops.DeployedRevision, has bool) *stackops.Snapshot {
	if !has {
		return nil
	}
	return &stackops.Snapshot{Seq: info.Seq}
}

// driftCurrent says whether a stored result still describes the stack: it was made against the revision that is
// deployed, and the stack has not been stopped or started since.
func driftCurrent(res stackops.DriftResult, st stackops.Stack, deployedSeq int64) bool {
	return res.Revision == deployedSeq &&
		(res.State == stackops.DriftStopped) == st.Stopped &&
		(res.State == stackops.DriftNotDeployed) == (deployedSeq == 0)
}

// driftBody is a result as the route answers it. A caller who may not reveal secrets gets the services with the live
// values withheld (D-47).
func driftBody(res stackops.DriftResult, info stackops.DeployedRevision, hasInfo bool, target stacks.Target, redacted bool) StackDriftBody {
	changes := make([]stacks.ServiceChange, len(res.Services))
	for i, s := range res.Services {
		changes[i] = s.Diff
	}
	if redacted {
		changes = stackops.RedactChanges(changes, target)
	}
	bodies := serviceBodies(changes)
	services := make([]StackDriftServiceBody, len(bodies))
	for i, b := range bodies {
		services[i] = StackDriftServiceBody{
			// The review reads the running service as current and the deploy as next; drift reads from the deployed
			// revision to what runs now.
			Name: b.Name, Short: b.Short, Change: res.Services[i].Change, Current: b.Next, Next: b.Current,
			HiddenChanges: b.HiddenChanges, Settings: b.Settings,
		}
	}
	body := StackDriftBody{
		State: res.State, Revision: res.Revision, CheckedAt: rfc3339(res.CheckedAt), Count: res.Count, Services: services,
	}
	if hasInfo {
		body.DeployedAt, body.DeployedBy = rfc3339(info.At), info.By
	}
	return body
}

// driftSummaryOf is the newest check of the stack when it is still about what is deployed, nil otherwise.
func driftSummaryOf(d StacksDeps, st stackops.Stack, deployedSeq int64) *StackDriftSummaryBody {
	if d.Drift == nil {
		return nil
	}
	res, ok := d.Drift.Get(st.ID)
	if !ok || !driftCurrent(res, st, deployedSeq) {
		return nil
	}
	return &StackDriftSummaryBody{State: res.State, Revision: res.Revision, CheckedAt: rfc3339(res.CheckedAt), Count: res.Count}
}
