// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

const (
	problemFolderInvalid       = "folder_invalid"
	problemComposeFilesMissing = "compose_files_required"
	problemComposePathInvalid  = "compose_path_invalid"
	problemComposeFileMissing  = "compose_file_missing"
)

// GitStackCreateRequest is the body of the create-from-repository route.
type GitStackCreateRequest struct {
	Name         string         `json:"name" maxLength:"255" doc:"The stack's name, held to the rule of the environment's target, as for a stack made in Bayplan (422 stack_name_invalid; 409 stack_name_taken, 409 stack_name_clash)"`
	RepoID       string         `json:"repoId" maxLength:"64" doc:"The repository: one of this environment's, or an instance repository. Another environment's is 404 repository_not_found"`
	RefKind      string         `json:"refKind" enum:"branch,tag,commit" doc:"What ref names: a branch, a tag, or a commit by its ID or a prefix of 7 to 40 hex characters"`
	Ref          string         `json:"ref" maxLength:"2048" doc:"The branch, tag or commit. 422 ref_invalid for a name git would refuse"`
	Folder       string         `json:"folder,omitempty" maxLength:"4096" doc:"The folder of the repository the stack lives in, without a leading or trailing slash; the root when empty (422 folder_invalid for a path outside the repository)"`
	ComposeFiles []string       `json:"composeFiles" minItems:"1" maxItems:"50" doc:"The compose files, relative to the folder, in merge order: a key of a later file wins over the same key of an earlier one. 422 compose_path_invalid for a path outside the folder or one named twice; 422 compose_file_missing for one the folder does not hold at the commit"`
	Vars         []DraftVarBody `json:"vars,omitempty" maxItems:"500" doc:"The stack's variables. A value here beats the same name in the folder's .env"`
}

// GitStackCreateBody is the answer of the create-from-repository route.
type GitStackCreateBody struct {
	StackID          string             `json:"stackId" doc:"The new stack's ID"`
	DraftVersion     int64              `json:"draftVersion" doc:"The version of the draft the stack was created with"`
	Commit           string             `json:"commit" doc:"The full 40-hex commit ID the ref resolved to; the draft holds the folder's files at this commit"`
	NotImported      []string           `json:"notImported" doc:"The first 50 names, relative to the folder, of the files the stack does not need: they are not stored"`
	NotImportedTotal int                `json:"notImportedTotal" doc:"How many files of the folder are not imported"`
	Problems         []StackProblemBody `json:"problems" doc:"What the check of the imported files found. Nothing is deployed, so problems do not stop the creation; they stop the deploy"`
}

// GitStackCreateOutput wraps GitStackCreateBody for huma.
type GitStackCreateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitStackCreateBody
}

// GitStackCreateInput is the path and body of the create-from-repository route.
type GitStackCreateInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      GitStackCreateRequest
}

// RegisterGitStacks registers the routes that make a stack from a repository. s is the deps of the stack routes
// (its names, targets and storage) and g those of the git routes (its repositories and the importer).
func RegisterGitStacks(r *httpx.Router, s StacksDeps, g GitDeps) {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if g.Log == nil {
		g.Log = s.Log
	}
	httpx.Register(r, huma.Operation{
		OperationID: "createGitStack", Method: http.MethodPost, Path: "/api/env/{envId}/git-stacks",
		Summary: "Create a stack from a folder of a git repository",
		Description: "Fetches the ref into the server's mirror of the repository and creates a stack whose draft holds the files at the commit it resolved to: the compose files, the folder's .env when it has one, " +
			"and every env_file, secrets and configs file the compose files name. The other files of the folder are not stored; the first 50 names and the total come back as notImported. " +
			"The stack's source is git, it polls the repository every 5 minutes, auto-deploy is off and it has no webhook. Nothing is deployed and nothing is sent to any engine. " +
			"The name follows the rules of creating a stack in Bayplan (422 stack_name_invalid, 409 stack_name_taken, 409 stack_name_clash). The repository must be one of this environment or an instance repository (404 repository_not_found). " +
			"422 ref_invalid for a ref name git would refuse; 422 ref_missing, commit_missing, commit_ambiguous or folder_missing when the ref, commit or folder is not there; 422 folder_invalid, compose_path_invalid or compose_file_missing for the paths; " +
			"422 file_too_large (a file over 1 MiB) or stack_too_large (more than 50 files or 8 MiB); otherwise 422 with the code of the git failure as its type and the host in its errors, as for browsing. Needs stacks.create (Admin).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxStackBody,
	}, rbac.Requirement{Perm: rbac.PermStacksCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *GitStackCreateInput) (*GitStackCreateOutput, error) {
			return createGitStack(ctx, s, g, in)
		})
}

func createGitStack(ctx context.Context, d StacksDeps, g GitDeps, in *GitStackCreateInput) (*GitStackCreateOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	body := in.Body
	target, err := d.stackTargetOf(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	if !stacks.NameValid(target, body.Name) {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStackNameInvalid, stackNameInvalidMessage(target))
	}
	if g.Sync == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	// The layer rule: a repository of this environment or of the instance, else it is not found.
	repo, err := g.browseRepo(ctx, env.ID, body.RepoID)
	if err != nil {
		return nil, err
	}
	vars, err := gitStackVars(body.Vars)
	if err != nil {
		return nil, err
	}
	if err := d.stackNameFree(ctx, env.ID, env.Name, target, body.Name); err != nil {
		return nil, err
	}

	kind := gitx.RefKind(body.RefKind)
	imp, err := g.Sync.Import(ctx, gitsync.ImportRequest{
		EnvID: env.ID, RepoID: repo.ID, RefKind: kind, Ref: body.Ref, Folder: body.Folder, ComposeFiles: body.ComposeFiles,
		Name: body.Name, Target: target, Vars: vars,
	})
	if err != nil {
		return nil, g.importProblem(repo, kind, body, err)
	}

	st, snap, err := d.repo().CreateGitStack(ctx, stackops.CreateGitStackInput{
		EnvID: env.ID, Name: body.Name, Target: target, Actor: p.Name, Copy: imp.Copy(vars), Rev: imp.Revision(),
	})
	if errors.Is(err, stackops.ErrNameTaken) {
		return nil, stackNameTakenProblem(body.Name, env.Name)
	}
	if err != nil {
		return nil, stackInternal(d, "create from git", env.ID, err)
	}
	notImported := imp.NotImported
	if notImported == nil {
		notImported = []string{}
	}
	return &GitStackCreateOutput{CacheControl: "no-store", Body: GitStackCreateBody{
		StackID: st.ID, DraftVersion: snap.Version, Commit: imp.Commit.SHA, NotImported: notImported,
		NotImportedTotal: imp.NotImportedTotal, Problems: problemBodies(imp.Problems),
	}}, nil
}

// gitStackVars checks the variables of a create the way a save of a draft does.
func gitStackVars(in []DraftVarBody) ([]stacks.Var, error) {
	out := make([]stacks.Var, 0, len(in))
	for _, v := range in {
		if len(v.Value) > maxStackVarValue {
			return nil, varProblem(v.Name, "A variable value is at most 64 KiB.")
		}
		out = append(out, stacks.Var{Name: v.Name, Value: v.Value, Secret: v.Secret})
	}
	if err := stacks.CheckVars(out); err != nil {
		return nil, varRuleProblem(err)
	}
	return out, nil
}

// importProblem words a failed import: the git failures as browsing words them, the path rules as their own 422.
func (d GitDeps) importProblem(repo gitconn.Repo, kind gitx.RefKind, body GitStackCreateRequest, err error) error {
	var pe *gitsync.PathError
	var fe *stacks.FileError
	var le *gitx.LimitError
	switch {
	case errors.Is(err, gitconn.ErrNotFound):
		return d.repoFail("import a repository folder", err)
	case errors.Is(err, gitsync.ErrFolderInvalid):
		return httpx.Problem(http.StatusUnprocessableEntity, problemFolderInvalid, "The folder must be a path inside the repository: no .. or . name, no backslash or colon, no leading or doubled slash.")
	case errors.Is(err, gitsync.ErrNoComposeFiles):
		return httpx.Problem(http.StatusUnprocessableEntity, problemComposeFilesMissing, "Name at least one compose file.")
	case errors.As(err, &pe):
		typ, detail := problemComposePathInvalid, "A compose file must be a path inside the folder, named once: no .. or . name, no backslash or colon, no leading or doubled slash."
		if errors.Is(err, gitsync.ErrComposeFileMissing) {
			typ, detail = problemComposeFileMissing, pe.Path+" isn't in the folder at this commit."
		}
		return &huma.ErrorModel{
			Type: typ, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: detail, Errors: []*huma.ErrorDetail{{Location: "composeFiles", Message: pe.Path}},
		}
	case errors.As(err, &fe):
		return fileRuleProblem(err)
	case errors.As(err, &le):
		if le.Path != "" {
			return fileRuleProblem(&stacks.FileError{Path: le.Path, Err: stacks.ErrFileTooLarge})
		}
		return fileRuleProblem(&stacks.FileError{Err: stacks.ErrStackTooLarge})
	}
	return d.browseProblem(repo, kind, body.Ref, body.Folder, err)
}
