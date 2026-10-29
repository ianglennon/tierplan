// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
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

// StackPlaceBody is a line of a stack file that uses a variable.
type StackPlaceBody struct {
	File string `json:"file" doc:"The stack file"`
	Line int    `json:"line" doc:"The 1-based line"`
}

// StackVariableBody is one row of the Variables pane: a variable the compose file refers to or the
// table holds, with the value a deploy will use.
type StackVariableBody struct {
	Name       string           `json:"name" doc:"The variable's name"`
	Value      string           `json:"value" doc:"The value a deploy will use: the table's value, else the default written in the compose file, else empty. A secret variable's value is <hidden>"`
	Secret     bool             `json:"secret" doc:"True when the variable is marked secret in the table"`
	Source     string           `json:"source" enum:"table,repo-env,default,unset" doc:"Where the value comes from: table (the stack's variables), repo-env (the .env file of the repository, for a stack from git), default (the ${NAME:-default} written in the compose file) or unset (none of them)"`
	SecretLike bool             `json:"secretLike,omitempty" doc:"Left out unless true. True when the value comes from the repository's .env and the name looks like a secret (PASSWORD, TOKEN, KEY and the like): the value is then <hidden> and a warning offers to set a secret variable"`
	Default    string           `json:"default" doc:"The default written in the compose file, when hasDefault is true"`
	HasDefault bool             `json:"hasDefault" doc:"True when a reference in the compose file writes a default, even an empty one"`
	Required   bool             `json:"required" doc:"True when the variable has no value in the table and no default, so a deploy is refused"`
	Unused     bool             `json:"unused" doc:"True when the variable is in the table and no file refers to it"`
	Places     []StackPlaceBody `json:"places" doc:"Where the compose file uses the variable, ordered by line. Empty for an unused variable"`
}

// StackCheckBody is what a check found.
type StackCheckBody struct {
	Problems    []StackProblemBody  `json:"problems" doc:"The problems, ordered by file and line"`
	UnusedFiles []string            `json:"unusedFiles" doc:"Stack files the compose file never refers to"`
	Variables   []StackVariableBody `json:"variables" doc:"The rows of the Variables pane, ordered by name. Empty when the compose file is not valid YAML"`
	SecretFiles []string            `json:"secretFiles" doc:"The stack files that are secret: the ones the compose file uses under secrets: (even before a save has sealed them) and the ones already saved as secret. Ordered by path"`
}

// StackCheckOutput wraps StackCheckBody for huma.
type StackCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackCheckBody
}

// maxChecksPerUser is how many checks one user may have running: the editor sends one as a person
// types, and a second can still be finishing, so two are served and a third waits for the next edit.
const maxChecksPerUser = 2

// problemCheckBusy is the 429 for a third check of one user.
const problemCheckBusy = "check_busy"

func registerStackCheck(r *httpx.Router, d StacksDeps, checkLimiter *streamLimiter) {
	httpx.Register(r, huma.Operation{
		OperationID: "checkStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/check",
		Summary: "Check a stack's working copy",
		Description: "Checks the files and variables in the body, as the editor holds them, and answers every problem with its file, line and column. " +
			"The check works only on the bytes it is sent: it reads no file from the Bayplan server, fetches nothing, writes nothing and sends nothing to an engine. " +
			"include, extends and label_file are refused, and every env_file and file: reference must name one of the stack's own files. " +
			"The editor calls it while a person types and on Save, and the review and the deploy run the same check. " +
			"One user may have two checks running; a third is 429 check_busy. A check stops after 5 seconds and answers 200 with one error problem, check_timeout. " +
			"Needs stacks.read (Viewer).",
		Tags: []string{"stacks"}, MaxBodyBytes: maxStackSaveBody,
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackCheckInput) (*StackCheckOutput, error) {
			return checkStack(ctx, d, checkLimiter, in)
		})
}

// checkedCopy is a stack's working copy after the check: the stack, the working copy built from
// the request and the saved draft, and what stacks.Check found in it.
type checkedCopy struct {
	stack stackops.Stack
	copy  stacks.WorkingCopy
	res   stacks.CheckResult
}

// runStackCheck takes one of the user's check slots, reads the stack and its saved draft (to fill
// a secret the browser never held), builds the working copy from the request and runs
// stacks.Check over it. The slot is held until release is called, which the caller must do once
// it has finished with the result. It is pure: nothing is written and no engine is called. The
// check route and the render route share it, and so share the two-checks-per-user limit.
func runStackCheck(ctx context.Context, d StacksDeps, lim *streamLimiter, stackID string, body StackCheckRequest) (c checkedCopy, release func(), err error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return c, nil, huma.Error500InternalServerError("internal error")
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		return c, nil, httpx.Problem(http.StatusTooManyRequests, problemCheckBusy,
			"You already have two checks running. The editor checks again when you stop typing.")
	}
	fail := func(err error) (checkedCopy, func(), error) {
		release()
		return checkedCopy{}, nil, err
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, stackID)
	if err != nil {
		return fail(stackLookupProblem(d, env.ID, err))
	}
	saved, err := repo.Draft(ctx, st.ID)
	if err != nil {
		return fail(stackLookupProblem(d, env.ID, err))
	}
	wc, err := draftCopy(StackDraftSaveRequest{Files: body.Files, Vars: body.Vars}, saved.Copy)
	if err != nil {
		return fail(err)
	}
	if st.Source.Kind == stacks.SourceGit {
		// A stack from a repository is checked by its own rules, which need each file's origin and the ordered
		// compose files; the browser holds neither, so they come from the saved draft (D-09, D-11).
		wc.ComposeFiles = slices.Clone(saved.Copy.ComposeFiles)
		wc = stackops.GitDraftOrigins(saved.Copy, wc)
	}
	if d.OnCheck != nil {
		d.OnCheck(ctx)
	}
	res := stacks.Check(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: wc, Deadline: d.CheckDeadline})
	return checkedCopy{stack: st, copy: wc, res: res}, release, nil
}

// checkStack runs stacks.Check over the working copy in the request. It is pure: the only reads
// are the stack and its saved draft, the second to fill a secret the browser never held.
func checkStack(ctx context.Context, d StacksDeps, lim *streamLimiter, in *StackCheckInput) (*StackCheckOutput, error) {
	c, release, err := runStackCheck(ctx, d, lim, in.StackID, in.Body)
	if err != nil {
		return nil, err
	}
	defer release()
	res := c.res

	body := StackCheckBody{Problems: problemBodies(res.Problems), UnusedFiles: res.UnusedFiles}
	if body.UnusedFiles == nil {
		body.UnusedFiles = []string{}
	}
	body.Variables = variableBodies(res.Variables)
	body.SecretFiles = res.SecretFiles
	if body.SecretFiles == nil {
		body.SecretFiles = []string{}
	}
	return &StackCheckOutput{CacheControl: "no-store", Body: body}, nil
}

// problemBodies turns the problems of a check into their answer. A list is never null.
func problemBodies(ps stacks.Problems) []StackProblemBody {
	out := make([]StackProblemBody, 0, len(ps))
	for _, p := range ps {
		out = append(out, StackProblemBody{
			Severity: string(p.Severity), Code: p.Code, Message: p.Message, File: p.File,
			Line: p.Line, Col: p.Col, EndLine: p.EndLine, EndCol: p.EndCol,
		})
	}
	return out
}

// variableBodies turns the resolved variables of a check into their answer. A list is never null.
func variableBodies(vars []stacks.ResolvedVar) []StackVariableBody {
	out := make([]StackVariableBody, 0, len(vars))
	for _, v := range vars {
		places := make([]StackPlaceBody, 0, len(v.Places))
		for _, pl := range v.Places {
			places = append(places, StackPlaceBody{File: pl.File, Line: pl.Line})
		}
		out = append(out, StackVariableBody{
			Name: v.Name, Value: v.Value, Secret: v.Secret, SecretLike: v.SecretLike, Source: v.Source, Default: v.Default,
			HasDefault: v.HasDefault, Required: v.Required, Unused: v.Unused, Places: places,
		})
	}
	return out
}
