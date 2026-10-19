// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// StackCheckRequest is the body of POST .../check: the working copy to check, in the save shape.
type StackCheckRequest struct {
	Files []DraftFileBody `json:"files" doc:"Every file of the working copy, as a save sends them. A secret file sent as <hidden> is filled from the saved draft"`
	Vars  []DraftVarBody  `json:"vars" maxItems:"500" doc:"Every variable of the working copy. A secret variable sent as <hidden> is filled from the saved draft"`
}

// StackCheckInput is the path and body of the check route.
type StackCheckInput struct {
	StackPathInput
	Body StackCheckRequest
}

// StackProblemBody is one thing the check found.
type StackProblemBody struct {
	Severity string `json:"severity" enum:"error,warning" doc:"An error stops a deploy; a warning only informs"`
	Code     string `json:"code" doc:"A stable name for the kind of problem, such as yaml_invalid or relative_bind"`
	Message  string `json:"message" doc:"What is wrong, then what to do"`
	File     string `json:"file" doc:"The stack file the problem is in"`
	Line     int    `json:"line" doc:"The 1-based line"`
	Col      int    `json:"col" doc:"The 1-based column"`
	EndLine  int    `json:"endLine" doc:"The line the marked range ends on"`
	EndCol   int    `json:"endCol" doc:"The column the marked range ends before"`
}

// StackCheckBody is what a check found.
type StackCheckBody struct {
	Problems    []StackProblemBody `json:"problems" doc:"The problems, ordered by file and line"`
	UnusedFiles []string           `json:"unusedFiles" doc:"Stack files the compose file never refers to"`
}

// StackCheckOutput wraps StackCheckBody for huma.
type StackCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackCheckBody
}

func registerStackCheck(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "checkStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/check",
		Summary: "Check a stack's working copy",
		Description: "Checks the files and variables in the body, as the editor holds them, and answers every problem with its file, line and column. " +
			"The check works only on the bytes it is sent: it reads no file from the Bayplan server, fetches nothing, writes nothing and sends nothing to an engine. " +
			"include, extends and label_file are refused, and every env_file and file: reference must name one of the stack's own files. " +
			"The editor calls it while a person types and on Save, and the review and the deploy run the same check. " +
			"Needs stacks.read (Viewer).",
		Tags: []string{"stacks"}, MaxBodyBytes: maxStackSaveBody,
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackCheckInput) (*StackCheckOutput, error) {
			return checkStack(ctx, d, in)
		})
}

// checkStack runs stacks.Check over the working copy in the request. It is pure: the only reads
// are the stack and its saved draft, the second to fill a secret the browser never held.
func checkStack(ctx context.Context, d StacksDeps, in *StackCheckInput) (*StackCheckOutput, error) {
	env := httpx.EnvFrom(ctx)
	if env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	saved, err := repo.Draft(ctx, st.ID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	wc, err := draftCopy(StackDraftSaveRequest{Files: in.Body.Files, Vars: in.Body.Vars}, saved.Copy)
	if err != nil {
		return nil, err
	}
	if d.OnCheck != nil {
		d.OnCheck(ctx)
	}
	res := stacks.Check(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: wc})

	body := StackCheckBody{Problems: make([]StackProblemBody, 0, len(res.Problems)), UnusedFiles: res.UnusedFiles}
	if body.UnusedFiles == nil {
		body.UnusedFiles = []string{}
	}
	for _, p := range res.Problems {
		body.Problems = append(body.Problems, StackProblemBody{
			Severity: string(p.Severity), Code: p.Code, Message: p.Message, File: p.File,
			Line: p.Line, Col: p.Col, EndLine: p.EndLine, EndCol: p.EndCol,
		})
	}
	return &StackCheckOutput{CacheControl: "no-store", Body: body}, nil
}
