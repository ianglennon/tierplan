// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Excluding a whole stack from the janitor (plan 11-28, JAN-07, D-10, D-21). A stack that is excluded keeps
// everything it owns: no container of its services or its one-off runs, and no image it labels, is ever
// removed by a schedule or by Clean now, until it is included again. The other way, x-tierplan: {keep: true}
// in the compose file, is read from the file and never stored here.

// StackJanitorBody is where a stack stands with the janitor.
type StackJanitorBody struct {
	Excluded bool `json:"excluded" doc:"True when the stack is excluded from the janitor: everything it owns is left alone by every cleanup until it is included again"`
}

// StackJanitorRequest is the body of PUT /api/env/{envId}/stacks/{stackId}/janitor.
type StackJanitorRequest struct {
	Excluded bool `json:"excluded" doc:"True to exclude the stack from the janitor, false to include it again"`
}

// StackJanitorInput is the path and body of the stack exclusion route.
type StackJanitorInput struct {
	StackPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      StackJanitorRequest
}

// StackJanitorOutput wraps StackJanitorBody for huma.
type StackJanitorOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackJanitorBody
}

// registerJanitorStack registers the route that excludes a stack from the janitor.
func registerJanitorStack(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "setStackJanitorExclusion", Method: http.MethodPut, Path: "/api/env/{envId}/stacks/{stackId}/janitor",
		Summary: "Exclude a stack from the janitor, or include it again",
		Description: "Sets whether the janitor leaves everything the stack owns alone (excluded true): its containers, matched by the stack's tierplan.stack.id, its swarm namespace or its compose project, " +
			"are listed as protected (stack_excluded) in what a cleanup would not remove and are never removed, by a schedule or by Clean now, until the stack is included again. " +
			"The change applies at once and is audited as stack.janitor_excluded or stack.janitor_included; the same value twice writes nothing and records nothing. " +
			"A stack ID of another environment is 404 stack_not_found, as one that does not exist. Needs janitor.policy (Operator with the extra permission).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermJanitorPolicy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackJanitorInput) (*StackJanitorOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.Store == nil || d.Audit == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setStackJanitorExclusion(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &StackJanitorOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// setStackJanitorExclusion stores the exclusion of a stack and audits a change in one transaction. A value the
// stack already has writes nothing.
func setStackJanitorExclusion(ctx context.Context, d JanitorDeps, p *rbac.Principal, envID string, in *StackJanitorInput) (StackJanitorBody, error) {
	internal := func(step string, err error) error {
		d.log().Error("could not change a stack's exclusion from the janitor", "step", step, "environment", envID, "stack", in.StackID, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return StackJanitorBody{}, internal("begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	st, err := q.GetStack(ctx, sqlc.GetStackParams{EnvID: envID, ID: in.StackID})
	if errors.Is(err, sql.ErrNoRows) {
		return StackJanitorBody{}, stackNotFound()
	}
	if err != nil {
		return StackJanitorBody{}, internal("read", err)
	}
	if (st.JanitorExcluded != 0) == in.Body.Excluded {
		return StackJanitorBody{Excluded: in.Body.Excluded}, nil
	}
	value, action := int64(0), audit.ActionStackJanitorIncluded
	if in.Body.Excluded {
		value, action = 1, audit.ActionStackJanitorExcluded
	}
	if _, err := q.SetStackJanitorExcluded(ctx, sqlc.SetStackJanitorExcludedParams{JanitorExcluded: value, ID: st.ID}); err != nil {
		return StackJanitorBody{}, internal("set", err)
	}
	if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: stackTargetType, TargetID: st.ID, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent, Details: map[string]string{"name": st.Name},
	}); err != nil {
		return StackJanitorBody{}, internal("audit", err)
	}
	if err := tx.Commit(); err != nil {
		return StackJanitorBody{}, internal("commit", err)
	}
	return StackJanitorBody{Excluded: in.Body.Excluded}, nil
}
