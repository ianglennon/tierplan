// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/stacks"
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

// StackFileRevealRequest names the secret file to reveal.
type StackFileRevealRequest struct {
	Path string `json:"path" maxLength:"255" doc:"The file's path inside the stack. A path that is not a saved secret file is 404 file_not_found"`
}

// StackFileRevealInput is the path and body of the file reveal route.
type StackFileRevealInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      StackFileRevealRequest
}

// StackFileRevealBody is a secret file's saved content.
type StackFileRevealBody struct {
	Content string `json:"content" doc:"The file's saved content, in clear"`
}

// StackFileRevealOutput wraps StackFileRevealBody for huma.
type StackFileRevealOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackFileRevealBody
}

func registerStackVariables(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "revealStackFile", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/files/reveal",
		Summary: "Reveal a secret stack file's saved content",
		Description: "Answers the saved content of a secret file of the stack, in clear: the one way back to a content every other answer shows as <hidden>. " +
			"A file is secret when the compose file uses it under secrets: or it was saved as secret. The content is read from the saved draft, or from the last deployed definition when there is no draft. " +
			"A path that is not in the stack, is not secret or is empty is 404 file_not_found. " +
			"The answer is never cached. Every reveal is audited as stack.value_revealed with the stack, the kind (file) and the path, never the content; if the audit row cannot be written the content is not sent. " +
			"A refusal is audited as permission.denied. Needs secrets.reveal (Admin, or an Operator granted the extra).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermSecretsReveal, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackFileRevealInput) (*StackFileRevealOutput, error) {
			return revealStackFile(ctx, d, in)
		})

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

// fileNotFound is the 404 for a file there is nothing to reveal for.
func fileNotFound() error {
	return httpx.Problem(http.StatusNotFound, problemFileNotFound, "This stack has no saved secret file with that path.")
}

// revealStackFile gives back a secret file's saved content and writes the audit row first: a
// content that cannot be accounted for is not given.
func revealStackFile(ctx context.Context, d StacksDeps, in *StackFileRevealInput) (*StackFileRevealOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if stacks.ValidPath(in.Body.Path) != nil {
		return nil, fileNotFound()
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
	var content string
	found := false
	for _, f := range snap.Copy.Files {
		if f.Path == in.Body.Path && f.Secret && len(f.Content) > 0 {
			content, found = string(f.Content), true
			break
		}
	}
	if !found {
		return nil, fileNotFound()
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionStackValueRevealed, TargetType: stackTargetType, TargetID: st.ID, EnvID: env.ID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{"stack": st.Name, "kind": "file", "name": in.Body.Path},
	}); err != nil {
		return nil, stackInternal(d, "audit a reveal", env.ID, err)
	}
	return &StackFileRevealOutput{CacheControl: "no-store", Body: StackFileRevealBody{Content: content}}, nil
}
