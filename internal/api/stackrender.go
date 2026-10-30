// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/stacks"
)

// problemStackInvalid is the 422 for a working copy whose check found an error, so there is no
// stack to render yet.
const problemStackInvalid = "stack_invalid"

// StackRenderInput is the path and body of the render route. The body is the one the check takes.
type StackRenderInput struct {
	StackPathInput
	Body StackCheckRequest
}

// StackRenderBody is a stack as it will be deployed.
type StackRenderBody struct {
	Name string `json:"name" doc:"The stack's name, which names the downloaded file"`
	YAML string `json:"yaml" doc:"The compose file after variables and env_file are filled in, each service's image shown as the pin it will run (tag@sha256:..., or the plain reference with a trailing comment when it will be looked up at the next deploy). Every secret value is <hidden>"`
}

// StackRenderOutput wraps StackRenderBody for huma.
type StackRenderOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackRenderBody
}

func registerStackRender(r *httpx.Router, d StacksDeps, checkLimiter *streamLimiter) {
	httpx.Register(r, huma.Operation{
		OperationID: "renderStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/render",
		Summary: "Render a stack's working copy",
		Description: "Answers the stack as it will be deployed: the files and variables in the body, as the editor holds them, with every variable and env_file filled in and each service's image shown as the digest it will run. " +
			"A service whose image text is unchanged since the last successful deploy shows its pinned tag@sha256 reference; any other image is shown as written with a trailing comment, since the deploy looks it up. " +
			"Every secret variable value and secret file content is <hidden>. The render is a view: it never changes the stack's files, and writes nothing and sends nothing to an engine. " +
			"It runs the same check as POST .../check, shares its limit of two running per user (429 check_busy), and answers 422 stack_invalid, with each error in errors[], when the check found an error. " +
			"The answer is never cached. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"}, MaxBodyBytes: maxStackSaveBody,
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackRenderInput) (*StackRenderOutput, error) {
			return renderStack(ctx, d, checkLimiter, in)
		})
}

// renderStack checks the working copy in the request, then renders what the check loaded through
// the working copy's masker. The check and the render are pure: the only reads are the stack, its
// saved draft (to fill a secret the browser never held) and the pins of its last good revision.
func renderStack(ctx context.Context, d StacksDeps, lim *streamLimiter, in *StackRenderInput) (*StackRenderOutput, error) {
	c, release, err := runStackCheck(ctx, d, lim, in.StackID, in.Body)
	if err != nil {
		return nil, err
	}
	defer release()
	if c.res.Problems.Errors() > 0 || c.res.Loaded == nil {
		return nil, invalidStackProblem(c.res.Problems)
	}
	pins, err := d.repo().GoodPins(ctx, c.stack.ID)
	if err != nil {
		return nil, stackInternal(d, "read pins", c.stack.EnvID, err)
	}
	out, err := stacks.Render(stacks.RenderInput{
		Loaded: c.res.Loaded, Pins: pins, Masker: stacks.NewMasker(c.copy.Vars, c.copy.Files),
	})
	if err != nil {
		return nil, stackInternal(d, "render", c.stack.EnvID, err)
	}
	return &StackRenderOutput{CacheControl: "no-store", Body: StackRenderBody{Name: c.stack.Name, YAML: string(out)}}, nil
}

// invalidStackProblem is the 422 for a working copy with errors: every error with its place.
func invalidStackProblem(ps stacks.Problems) error {
	return stackInvalid(ps, "The stack has errors, so there is nothing to render yet. Fix the problems the check lists.")
}

// stackInvalid is the 422 stack_invalid with every error of ps at its file and line.
func stackInvalid(ps stacks.Problems, detail string) error {
	details := make([]*huma.ErrorDetail, 0, ps.Errors())
	for _, p := range ps {
		if p.Severity != stacks.SeverityError {
			continue
		}
		details = append(details, &huma.ErrorDetail{Location: p.File + ":" + strconv.Itoa(p.Line), Message: p.Message})
	}
	return &huma.ErrorModel{
		Type: problemStackInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: details,
	}
}
