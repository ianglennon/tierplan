// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// Adopting a stack Bayplan did not make (plan 08-13, STK-13, D-14). Nothing is sent to an engine: the server
// rebuilds the definition from what runs and stores it as the stack's draft and first revision.

const (
	// problemAdoptStale is the type of the 409 for a stack that changed after the page showed it.
	problemAdoptStale = "adopt_stale"
	// problemStackAlreadyManaged is the type of the 409 for a stack Bayplan already manages under that name.
	problemStackAlreadyManaged = "stack_already_managed"
	// problemStackNotAdoptable is the type of the 409 for Bayplan's own stack, its agent's and a name the target cannot hold.
	problemStackNotAdoptable = "stack_not_adoptable"

	// adoptLockKind is the kind of operation the adoption holds the environment's operation lock as.
	adoptLockKind = "adopt"
)

// AdoptStackRequest is the body of the adopt route.
type AdoptStackRequest struct {
	Fingerprint string `json:"fingerprint" required:"true" maxLength:"65536" doc:"The fingerprint the unmanaged stack's page read. When what runs has changed since, the adoption is refused 409 adopt_stale and nothing is stored"`
}

// AdoptStackInput is the path and body of the adopt route.
type AdoptStackInput struct {
	UnmanagedStackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      AdoptStackRequest
}

// maxAdoptChecksPerUser is how many adopt checks one user may have running: each reads the engine and rebuilds a stack.
const maxAdoptChecksPerUser = 2

// newAdoptCheckLimiter is the limiter of the adopt check route.
func newAdoptCheckLimiter() *streamLimiter {
	return &streamLimiter{max: maxAdoptChecksPerUser, open: map[string]int{}}
}

func registerStackAdopt(r *httpx.Router, d StacksDeps, checkLimiter *streamLimiter) {
	registerStackAdoptCheck(r, d, checkLimiter)
	httpx.Register(r, huma.Operation{
		OperationID: "adoptStack", Method: http.MethodPost, Path: "/api/env/{envId}/unmanaged-stacks/{name}/adopt",
		Summary: "Adopt a stack Bayplan did not make",
		Description: "Takes over a swarm namespace or a Compose project that Bayplan does not manage. Bayplan reads what runs, rebuilds the compose definition itself (the body carries none), " +
			"and in one transaction stores the stack (source adopted), its draft and revision 1 (origin adopted, good, pinned to the images that run). Nothing is sent to any engine: the stack's objects get Bayplan's labels on its first deploy. " +
			"The definition holds the environment values as literals. The fingerprint is the one the stack's page read; when what runs has changed since, the answer is 409 adopt_stale and nothing is stored. " +
			"Adopting never fails on the host-access policy: it only records what already runs. " +
			"A stack that is Bayplan's own, runs Bayplan's agent, or has a name its target cannot hold is 409 stack_not_adoptable with the reason; a name Bayplan already manages is 409 stack_already_managed with the stack's ID in errors as stackId; " +
			"a name nothing carries is 404 unmanaged_stack_not_found; a stack that cannot be written as a compose file that passes the check is 422 rebuild_failed. " +
			"Only one stack operation at a time runs on an environment: while one does, the answer is 409 operation_locked and nothing is stored. Answers the stack, as the stack route does. Audited as stack.adopted. Needs stacks.create (Env Admin).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusCreated,
	}, rbac.Requirement{Perm: rbac.PermStacksCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *AdoptStackInput) (*StackDetailOutput, error) {
			return adoptStack(ctx, d, in)
		})
}

// stackAlreadyManaged is the 409 for a name a stack of Bayplan's holds, with that stack's ID.
func stackAlreadyManaged(name, stackID string) error {
	return &huma.ErrorModel{
		Type: problemStackAlreadyManaged, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: name + " is already managed by Bayplan.",
		Errors: []*huma.ErrorDetail{{Location: "stackId", Message: stackID}},
	}
}

func adoptStack(ctx context.Context, d StacksDeps, in *AdoptStackInput) (*StackDetailOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	own, err := repo.Stacks(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "list", env.ID, err)
	}
	if i := slices.IndexFunc(own, func(s stackops.Stack) bool { return s.Name == in.Name }); i >= 0 {
		return nil, stackAlreadyManaged(in.Name, own[i].ID)
	}
	target, err := d.stackTargetOf(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	row, err := d.Store.RQ.GetEnvironment(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "adopt: read the environment", env.ID, err)
	}

	// The lock is held for the commit only: nothing is deployed, so there is no job to hand it to.
	release, held, ok := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackName: in.Name, Kind: adoptLockKind, By: p.Name, Started: time.Now().UTC(),
	})
	if !ok {
		return nil, operationLocked(env.Name, held)
	}
	defer release()

	adopted, err := d.ops().AdoptWithOutcome(ctx, repo, stackops.AdoptInput{
		EnvID: env.ID, Name: in.Name, Target: target, Fingerprint: in.Body.Fingerprint,
		Caller: stackops.Caller{Principal: p, Actor: p.Name}, Managed: own, Self: d.selfMarks(row.Kind),
	})
	switch {
	case err == nil:
	case errors.Is(err, stackops.ErrAdoptStale):
		return nil, httpx.Problem(http.StatusConflict, problemAdoptStale,
			"What is running on "+env.Name+" changed since the check. Run the check again before you adopt.")
	default:
		return nil, adoptProblem(ctx, d, repo, env.ID, in.Name, err)
	}

	st := adopted.Stack
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackAdopted, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "target": string(st.Target), "source": st.Source.Ref,
		"services": itoa(int64(adopted.Services)), "revision": itoa(adopted.Revision),
	})
	body, err := stackDetailOf(ctx, d, st)
	if err != nil {
		return nil, stackInternal(d, "detail", env.ID, err)
	}
	return &StackDetailOutput{CacheControl: "no-store", Body: body}, nil
}

// adoptProblem is the answer for the refusals an adoption and its check share: a name Bayplan holds, a stack that
// cannot be adopted, one that is gone, and one that cannot be written as a compose file. Anything else is the
// engine's failure.
func adoptProblem(ctx context.Context, d StacksDeps, repo stackops.Repo, envID, name string, err error) error {
	var failed *stackops.RebuildError
	var refused *stackops.NotAdoptableError
	switch {
	case errors.Is(err, stackops.ErrNameTaken):
		// Also the loser of two adoptions that raced: the winner's stack holds the name now.
		now, lerr := repo.Stacks(ctx, envID)
		if lerr != nil {
			return stackInternal(d, "list", envID, lerr)
		}
		if i := slices.IndexFunc(now, func(s stackops.Stack) bool { return s.Name == name }); i >= 0 {
			return stackAlreadyManaged(name, now[i].ID)
		}
		return stackInternal(d, "adopt", envID, err)
	case errors.As(err, &refused):
		return httpx.Problem(http.StatusConflict, problemStackNotAdoptable, notAdoptableReason(refused.Found))
	case errors.Is(err, stackops.ErrUnmanagedGone):
		return unmanagedNotFound("")
	case errors.As(err, &failed):
		return httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, failed.Reason)
	case errors.Is(err, stackops.ErrRebuildTarget):
		return httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, "Bayplan can't rebuild this kind of stack.")
	}
	return snapshotProblem(d.Log, envID, err)
}

// AdoptCheckBody is what adopting a stack would be like: the first deploy compared with what runs, before anything is
// stored.
type AdoptCheckBody struct {
	Fingerprint   string                   `json:"fingerprint" doc:"The state of the engine the check was made of. Adopt sends it back; when what runs has changed since, the adoption is refused 409 adopt_stale"`
	Clean         bool                     `json:"clean" doc:"True when nothing will change on the first deploy except Bayplan's ownership labels"`
	Gaps          []StackReviewServiceBody `json:"gaps" doc:"The services the first deploy would change in more than an ownership label, as the review lists services. For a caller without secrets.reveal every environment value, command, argument, health check command and log option reads <hidden>, and a setting that differs only in such a value is listed by its path in hiddenChanges"`
	GapCount      int                      `json:"gapCount" doc:"How many services are in gaps"`
	OwnershipOnly []string                 `json:"ownershipOnly" doc:"The names, in the compose file, of the services that differ from what runs in nothing but Bayplan's ownership labels: the first deploy recreates each of them (a brief restart) and changes nothing else"`
}

// AdoptCheckOutput wraps AdoptCheckBody for huma.
type AdoptCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AdoptCheckBody
}

func registerStackAdoptCheck(r *httpx.Router, d StacksDeps, lim *streamLimiter) {
	httpx.Register(r, huma.Operation{
		OperationID: "checkAdoption", Method: http.MethodPost, Path: "/api/env/{envId}/unmanaged-stacks/{name}/adopt-check",
		Summary: "Check what adopting a stack would change on its first deploy",
		Description: "Rebuilds the compose definition of a stack Bayplan does not manage from what runs, as adopting does, and reviews it against what runs: every difference that is not one of Bayplan's ownership labels is a gap, " +
			"a change the first deploy would make, and the services that differ only in an ownership label are listed apart. It reads the engine and changes nothing, on the engine or in the store, and a gap never blocks adopting. " +
			"The fingerprint it answers is the one to send with the adoption. " +
			"The answer holds the live settings of running services, so for a caller without secrets.reveal on the environment every environment value, command, argument, health check command and log option is <hidden>. " +
			"A stack that is Bayplan's own, runs Bayplan's agent, or has a name its target cannot hold is 409 stack_not_adoptable; a name Bayplan already manages is 409 stack_already_managed with the stack's ID in errors as stackId; " +
			"a name nothing carries is 404 unmanaged_stack_not_found; a stack that cannot be written as a compose file that passes the check is 422 rebuild_failed. " +
			"One user may have two checks running; a third is 429 check_busy. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UnmanagedStackPathInput) (*AdoptCheckOutput, error) {
			return checkAdoption(ctx, d, lim, in)
		})
}

func checkAdoption(ctx context.Context, d StacksDeps, lim *streamLimiter, in *UnmanagedStackPathInput) (*AdoptCheckOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemCheckBusy, "Too many checks are running. Try again in a moment.")
	}
	defer release()
	if d.OnAdoptCheck != nil {
		d.OnAdoptCheck(ctx)
	}

	repo := d.repo()
	own, err := repo.Stacks(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "list", env.ID, err)
	}
	target, err := d.stackTargetOf(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	row, err := d.Store.RQ.GetEnvironment(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "adopt check: read the environment", env.ID, err)
	}
	res, err := d.ops().AdoptCheck(ctx, stackops.AdoptInput{
		EnvID: env.ID, Name: in.Name, Target: target, Caller: stackops.Caller{Principal: p, Actor: p.Name}, Managed: own, Self: d.selfMarks(row.Kind),
	}, redact.CanReveal(ctx, d.Checker, p, env.ID))
	if err != nil {
		return nil, adoptProblem(ctx, d, repo, env.ID, in.Name, err)
	}
	return &AdoptCheckOutput{CacheControl: "no-store", Body: adoptCheckBody(res)}, nil
}

func adoptCheckBody(res stackops.AdoptCheckResult) AdoptCheckBody {
	only := res.OwnershipOnly
	if only == nil {
		only = []string{}
	}
	return AdoptCheckBody{
		Fingerprint: res.Fingerprint, Clean: res.Clean, Gaps: serviceBodies(res.Gaps), GapCount: len(res.Gaps), OwnershipOnly: only,
	}
}
