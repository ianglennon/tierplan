// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// StackVariableRevealInput is the path of the reveal route.
type StackVariableRevealInput struct {
	StackPathInput
	Name      string `path:"name" doc:"The variable name. A name that is not a valid variable name is 404 variable_not_found"`
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// StackVariableRevealBody is a secret variable's saved value.
type StackVariableRevealBody struct {
	Value string `json:"value" doc:"The variable's saved value, in clear"`
}

// StackVariableRevealOutput wraps StackVariableRevealBody for huma.
type StackVariableRevealOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackVariableRevealBody
}

func registerStackVariables(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "revealStackVariable", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/variables/{name}/reveal",
		Summary: "Reveal a secret variable's saved value",
		Description: "Answers the saved value of a secret variable of the stack, in clear: the one way back to a value every other answer shows as <hidden>. " +
			"The value is read from the saved draft, or from the last deployed definition when there is no draft. A variable that is not in it, is not secret or has no value is 404 variable_not_found. " +
			"The answer is never cached. Every reveal is audited as stack.value_revealed with the stack, the kind (variable) and the name, never the value; if the audit row cannot be written the value is not sent. " +
			"A refusal is audited as permission.denied. Needs secrets.reveal (Admin, or an Operator granted the extra).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermSecretsReveal, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackVariableRevealInput) (*StackVariableRevealOutput, error) {
			return revealStackVariable(ctx, d, in)
		})
}

// variableNotFound is the 404 for a variable there is nothing to reveal for.
func variableNotFound() error {
	return httpx.Problem(http.StatusNotFound, problemVariableNotFound, "This stack has no saved secret variable with that name.")
}

// revealStackVariable gives back a secret variable's saved value and writes the audit row first:
// a value that cannot be accounted for is not given.
func revealStackVariable(ctx context.Context, d StacksDeps, in *StackVariableRevealInput) (*StackVariableRevealOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if err := stacks.CheckVarNames([]stacks.Var{{Name: in.Name}}); err != nil {
		return nil, variableNotFound()
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	snap, err := repo.Draft(ctx, st.ID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	var value string
	found := false
	for _, v := range snap.Copy.Vars {
		if v.Name == in.Name && v.Secret && v.Value != "" {
			value, found = v.Value, true
			break
		}
	}
	if !found {
		return nil, variableNotFound()
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionStackValueRevealed, TargetType: stackTargetType, TargetID: st.ID, EnvID: env.ID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{"stack": st.Name, "kind": "variable", "name": in.Name},
	}); err != nil {
		return nil, stackInternal(d, "audit a reveal", env.ID, err)
	}
	return &StackVariableRevealOutput{CacheControl: "no-store", Body: StackVariableRevealBody{Value: value}}, nil
}
