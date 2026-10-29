// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// StackDriftCauseBody is why a service, or a stack, differs from the deployed revision when the audit log or the adoption
// knows (D-24). Nothing of an audit row but the action, the actor's display name, the time and a scale's replica counts
// is in it.
type StackDriftCauseBody struct {
	Action  string `json:"action" enum:"scaled,rolled_back,edited,adopted" doc:"scaled: a scale from the service page. rolled_back: a service roll back. edited: a service edit. adopted: the difference was there when the stack was adopted and its first deploy has not put it right yet"`
	Service string `json:"service" doc:"The service's name in the compose file"`
	By      string `json:"by" doc:"The display name of who did it; empty for adopted"`
	At      string `json:"at" doc:"When, RFC 3339 UTC; empty for adopted"`
	From    string `json:"from" doc:"For scaled, the replica count before; empty when the audit row did not say, and for any other action"`
	To      string `json:"to" doc:"For scaled, the replica count after; empty for any other action"`
}

// causeBody is a cause as the routes answer it, nil for none.
func causeBody(c *stackops.DriftCause) *StackDriftCauseBody {
	if c == nil {
		return nil
	}
	return &StackDriftCauseBody{Action: c.Action, Service: c.Service, By: c.By, At: rfc3339(c.At), From: c.From, To: c.To}
}

// StackDriftServiceBody is one service that differs from the deployed revision.
type StackDriftServiceBody struct {
	Name          string               `json:"name" doc:"The swarm service name"`
	Short         string               `json:"short" doc:"The service's name in the compose file"`
	Change        string               `json:"change" enum:"changed,missing,extra" doc:"changed: the service runs with other settings than the revision deployed. missing: the revision has the service and the swarm does not. extra: the swarm has a service under the stack's ownership that the revision does not have"`
	Current       map[string]any       `json:"current" nullable:"true" doc:"What the revision deployed, masked: a value derived from a secret of the stack is <hidden>, and for a caller without secrets.reveal so is every environment value, command, argument, health check command and log option. Null for an extra service"`
	Next          map[string]any       `json:"next" nullable:"true" doc:"What runs now, masked the same way. Null for a missing service"`
	HiddenChanges []string             `json:"hiddenChanges" doc:"Paths of settings whose value changed to or from one that is hidden. The values are never sent"`
	Settings      int                  `json:"settings" doc:"How many settings differ between current and next, hidden ones included"`
	Cause         *StackDriftCauseBody `json:"cause,omitempty" doc:"Why the service differs, when the audit log (a scale, roll back or edit from Bayplan since the revision deployed) or the adoption knows; left out when it does not, which a reader says is a change made outside Bayplan"`
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
	Cause      *StackDriftCauseBody    `json:"cause,omitempty" doc:"The cause to name for the stack: the newest action of Bayplan's own on a drifted service, or an adoption's difference when there is none; left out when nothing is known"`
	Causes     int                     `json:"causes" doc:"How many drifted services an action of Bayplan's own explains"`
}

// StackDriftSummaryBody is the drift a stack's page can say without the services: the newest check that is still about
// the revision that is deployed.
type StackDriftSummaryBody struct {
	State     string               `json:"state" enum:"none,drifted,stopped,not_deployed,unknown" doc:"As the drift route's state"`
	Revision  int64                `json:"revision" doc:"The revision the check compared with"`
	CheckedAt string               `json:"checkedAt" doc:"When the stack was read, RFC 3339 UTC"`
	Count     int                  `json:"count" doc:"How many settings differ in all"`
	Cause     *StackDriftCauseBody `json:"cause,omitempty" doc:"As the drift route's cause"`
	Causes    int                  `json:"causes" doc:"As the drift route's causes"`
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

const (
	// problemGitStack is the refusal of an accept-live for a stack that deploys from a git repository.
	problemGitStack  = "git_stack"
	gitStackNoAccept = "This stack deploys from git, so what runs can't be copied into its draft. Make the change in the repository."
)

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

	httpx.Register(r, huma.Operation{
		OperationID: "acceptStackDrift", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/drift/accept",
		Summary: "Accept what runs into the stack's draft",
		Description: "Rebuilds the stack's draft from what runs (D-23): the new draft is the deployed revision's files and variables with a compose.yaml rebuilt from the engine by the same rebuilder adoption uses, made on top of that revision (D-54). " +
			"The stack's own secrets and configs stay the files they were, an image keeps the text it was written with while it is still the image that runs, and an environment value, entrypoint word or command word equal to the value of one of the revision's secret variables is written as ${NAME}, so no secret variable's value is copied into the compose file. " +
			"The draft starts from the deployed revision. Nothing is changed on any engine, and nothing is deployed: the person reviews the draft and deploys it, which records a revision. " +
			"draftVersion is the version of the draft the person saw (0 when the stack has none); when the draft has moved since, the answer is 409 draft_conflict and nothing is written. " +
			"While another stack operation runs on the environment the answer is 409 operation_locked. A stack with no deployed revision is 409 no_deployed_revision, and a stack that is stopped is 409 stack_stopped (its services run no replicas, so there is nothing running to keep; start it first). " +
			"A stack that deploys from a git repository, or whose draft is about to point at one, is 409 git_stack: its files are the repository's and read-only, so the change belongs in the repository. " +
			"A stack that cannot be written as a compose file that passes the check is 422 rebuild_failed with the reason in errors[].message; the draft is not changed. " +
			"Audited as stack.drift_accepted. Needs stacks.update (Operator), as saving the draft does; the rebuild reads the values the engine holds, so the secret variables of the revision are used by the server and never sent.",
		Tags: []string{"stacks"}, MaxBodyBytes: 1 << 10,
	}, rbac.Requirement{Perm: rbac.PermStacksUpdate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackDriftAcceptInput) (*StackDriftAcceptOutput, error) {
			return acceptStackDrift(ctx, d, in)
		})
}

// StackDriftAcceptInput is the path and body of the accept route.
type StackDriftAcceptInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		DraftVersion int64 `json:"draftVersion" minimum:"0" doc:"The version of the draft the person saw, 0 for a stack that has none. A draft that has moved since is 409 draft_conflict"`
	}
}

// StackDriftAcceptBody says what an accept did.
type StackDriftAcceptBody struct {
	Version  int64  `json:"version" doc:"The draft's version now"`
	SavedBy  string `json:"savedBy" doc:"Who saved the draft last, which is the caller"`
	SavedAt  string `json:"savedAt" doc:"When, RFC 3339 UTC"`
	Revision int64  `json:"revision" doc:"The number of the deployed revision the draft starts from"`
	Services int    `json:"services" doc:"How many services the draft was rebuilt from"`
}

// StackDriftAcceptOutput wraps StackDriftAcceptBody for huma.
type StackDriftAcceptOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackDriftAcceptBody
}

func acceptStackDrift(ctx context.Context, d StacksDeps, in *StackDriftAcceptInput) (*StackDriftAcceptOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	if h, ok := d.Locks.Held(env.ID); ok {
		return nil, operationLocked(env.Name, h)
	}
	res, err := d.ops().AcceptLive(ctx, repo, stackops.AcceptLiveInput{
		EnvID: env.ID, Stack: st, DraftVersion: in.Body.DraftVersion, Caller: stackops.Caller{Principal: p, Actor: p.Name},
	})
	var conflict *stackops.DraftConflictError
	var failed *stackops.RebuildError
	switch {
	case err == nil:
	case errors.As(err, &conflict):
		return nil, draftConflict(conflict)
	case errors.Is(err, stackops.ErrNoDeployedRevision):
		return nil, httpx.Problem(http.StatusConflict, problemNoDeployedRevision, noDeployedRevision)
	case errors.Is(err, stackops.ErrStackStopped):
		return nil, httpx.Problem(http.StatusConflict, problemStackStopped, stackStopped)
	case errors.Is(err, stackops.ErrGitStack):
		return nil, httpx.Problem(http.StatusConflict, problemGitStack, gitStackNoAccept)
	case errors.As(err, &failed):
		return nil, rebuildNotAccepted(failed.Reason)
	case errors.Is(err, stackops.ErrRebuildTarget):
		return nil, rebuildNotAccepted("Bayplan can't rebuild this kind of stack")
	default:
		return nil, targetProblem(d, env.ID, err)
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackDriftAccepted, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "revision": itoa(res.Revision), "version": itoa(res.Draft.Version), "services": itoa(int64(res.Services)),
	})
	return &StackDriftAcceptOutput{CacheControl: "no-store", Body: StackDriftAcceptBody{
		Version: res.Draft.Version, SavedBy: res.Draft.CreatedBy, SavedAt: rfc3339(res.Draft.CreatedAt),
		Revision: res.Revision, Services: res.Services,
	}}, nil
}

// rebuildNotAccepted is the 422 for a rebuild that failed: the draft was not changed. The reason is a sentence
// that holds no value the caller may not read (the rebuild is masked by the same code as the Generated compose tab).
func rebuildNotAccepted(reason string) error {
	return &huma.ErrorModel{
		Type: problemRebuildFailed, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: "Couldn't rebuild the draft from what's running: " + strings.TrimRight(reason, ". ") + ". Your draft wasn't changed.",
		Errors: []*huma.ErrorDetail{{Location: "reason", Message: reason}},
	}
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
		return ops.Drift(cctx, stackops.DriftInput{EnvID: env.ID, Stack: st, Deployed: &good, Pins: pins, Audit: d.Store})
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

// driftCurrent says whether a stored result still describes the stack (stackops.DriftCurrent).
func driftCurrent(res stackops.DriftResult, st stackops.Stack, deployedSeq int64) bool {
	return stackops.DriftCurrent(res, st, deployedSeq)
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
			HiddenChanges: b.HiddenChanges, Settings: b.Settings, Cause: causeBody(res.Services[i].Cause),
		}
	}
	body := StackDriftBody{
		State: res.State, Revision: res.Revision, CheckedAt: rfc3339(res.CheckedAt), Count: res.Count, Services: services,
		Cause: causeBody(res.Latest), Causes: res.Causes,
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
	return &StackDriftSummaryBody{
		State: res.State, Revision: res.Revision, CheckedAt: rfc3339(res.CheckedAt), Count: res.Count,
		Cause: causeBody(res.Latest), Causes: res.Causes,
	}
}
