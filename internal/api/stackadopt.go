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

func registerStackAdopt(r *httpx.Router, d StacksDeps) {
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
	var failed *stackops.RebuildError
	var refused *stackops.NotAdoptableError
	switch {
	case err == nil:
	case errors.Is(err, stackops.ErrNameTaken):
		// The loser of two adoptions that raced: the winner's stack holds the name now.
		now, lerr := repo.Stacks(ctx, env.ID)
		if lerr != nil {
			return nil, stackInternal(d, "list", env.ID, lerr)
		}
		if i := slices.IndexFunc(now, func(s stackops.Stack) bool { return s.Name == in.Name }); i >= 0 {
			return nil, stackAlreadyManaged(in.Name, now[i].ID)
		}
		return nil, stackInternal(d, "adopt", env.ID, err)
	case errors.Is(err, stackops.ErrAdoptStale):
		return nil, httpx.Problem(http.StatusConflict, problemAdoptStale,
			"What is running on "+env.Name+" changed since the check. Run the check again before you adopt.")
	case errors.As(err, &refused):
		return nil, httpx.Problem(http.StatusConflict, problemStackNotAdoptable, notAdoptableReason(refused.Found))
	case errors.Is(err, stackops.ErrUnmanagedGone):
		return nil, unmanagedNotFound("")
	case errors.As(err, &failed):
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, failed.Reason)
	case errors.Is(err, stackops.ErrRebuildTarget):
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, "Bayplan can't rebuild this kind of stack.")
	default:
		return nil, snapshotProblem(d.Log, env.ID, err)
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
