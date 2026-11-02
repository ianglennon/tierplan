// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/gitconn"
	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

const (
	problemFolderInvalid       = "folder_invalid"
	problemComposeFilesMissing = "compose_files_required"
	problemComposePathInvalid  = "compose_path_invalid"
	problemComposeFileMissing  = "compose_file_missing"
	problemCommitMoved         = "commit_moved"
)

// GitStackCreateRequest is the body of the create-from-repository route.
type GitStackCreateRequest struct {
	Name         string                `json:"name" maxLength:"255" doc:"The stack's name, held to the rule of the environment's target, as for a stack made in TierPlan (422 stack_name_invalid; 409 stack_name_taken, 409 stack_name_clash)"`
	RepoID       string                `json:"repoId" maxLength:"64" doc:"The repository: one of this environment's, or an instance repository. Another environment's is 404 repository_not_found"`
	RefKind      string                `json:"refKind" enum:"branch,tag,commit" doc:"What ref names: a branch, a tag, or a commit by its ID or a prefix of 7 to 40 hex characters"`
	Ref          string                `json:"ref" maxLength:"2048" doc:"The branch, tag or commit. 422 ref_invalid for a name git would refuse"`
	Commit       string                `json:"commit,omitempty" pattern:"^[0-9a-f]{40}$" patternDescription:"a full 40-hex commit ID" doc:"The commit.sha the check answered for this ref. When it is given, the stack is created only if the ref still resolves to this commit, so what the person reviewed is what is created; otherwise 409 commit_moved and nothing is created. When it is empty, the head of the ref at the time of the request is used"`
	Folder       string                `json:"folder,omitempty" maxLength:"4096" doc:"The folder of the repository the stack lives in, without a leading or trailing slash; the root when empty (422 folder_invalid for a path outside the repository)"`
	ComposeFiles []string              `json:"composeFiles" minItems:"1" maxItems:"50" doc:"The compose files, relative to the folder, in merge order: a key of a later file wins over the same key of an earlier one. 422 compose_path_invalid for a path outside the folder or one named twice; 422 compose_file_missing for one the folder does not hold at the commit"`
	Vars         []DraftVarBody        `json:"vars,omitempty" maxItems:"500" doc:"The stack's variables. A value here beats the same name in the folder's .env"`
	Overlays     []GitStackOverlayBody `json:"overlays,omitempty" maxItems:"50" doc:"Files TierPlan holds for paths the repository does not have, such as the secret a compose file names under secrets:. Each is stored sealed as a secret file. 422 file_path_invalid for a bad path, file_name_taken for one named twice, file_too_large over 1 MiB"`
}

// GitStackCheckRequest is the body of the check route: where a stack would come from, and what the person has chosen,
// without any content of an overlay file.
type GitStackCheckRequest struct {
	Name         string         `json:"name,omitempty" maxLength:"255" doc:"The stack's name, when the person has typed one; it only matters to the swarm name rules. A name the target refuses is ignored here: the create refuses it"`
	RepoID       string         `json:"repoId" maxLength:"64" doc:"The repository: one of this environment's, or an instance repository. Another environment's is 404 repository_not_found"`
	RefKind      string         `json:"refKind" enum:"branch,tag,commit" doc:"What ref names: a branch, a tag, or a commit by its ID or a prefix of 7 to 40 hex characters"`
	Ref          string         `json:"ref" maxLength:"2048" doc:"The branch, tag or commit. 422 ref_invalid for a name git would refuse"`
	Commit       string         `json:"commit,omitempty" maxLength:"64" doc:"The commit.sha an earlier check answered for this ref. When the server's mirror holds it the check reads the mirror and fetches nothing; otherwise, and when it is not a full 40-hex commit ID, the ref is fetched"`
	Folder       string         `json:"folder,omitempty" maxLength:"4096" doc:"The folder of the repository the stack lives in, without a leading or trailing slash; the root when empty (422 folder_invalid for a path outside the repository)"`
	ComposeFiles []string       `json:"composeFiles" minItems:"1" maxItems:"50" doc:"The compose files, relative to the folder, in merge order. 422 compose_path_invalid for a path outside the folder or one named twice; 422 compose_file_missing for one the folder does not hold at the commit"`
	Vars         []DraftVarBody `json:"vars,omitempty" maxItems:"500" doc:"The variables typed so far. A value here beats the same name in the folder's .env"`
	OverlayPaths []string       `json:"overlayPaths,omitempty" maxItems:"50" doc:"The paths of the overlay files the person means to add, never their contents: those are sent once, when the stack is created. A path the compose files need that an overlay fills is not missing; one the repository holds is the blocking problem overlay_clash. 422 file_path_invalid for a path that is not a path inside the stack"`
}

// GitStackCommitBody is a commit.
type GitStackCommitBody struct {
	SHA  string    `json:"sha" doc:"The full 40-hex commit ID"`
	When time.Time `json:"when" doc:"When the commit was made"`
}

// GitStackVariableBody is one row of the variables table of the From git page.
type GitStackVariableBody struct {
	Name       string           `json:"name" doc:"The variable's name"`
	Value      string           `json:"value" doc:"The value a deploy would use. <hidden> for a secret variable and for one whose name looks like a secret, whatever its source; empty when there is none"`
	Source     string           `json:"source" enum:"table,repo-env,default,unset" doc:"Where the value comes from: table (the variables sent), repo-env (the folder's .env), default (the ${NAME:-default} written in a compose file) or unset (nowhere)"`
	Required   bool             `json:"required" doc:"True when the variable has no value from the table, the .env or a default, so the page must ask for one. The deploy is refused until it has"`
	Secret     bool             `json:"secret" doc:"True when the variable is marked secret in the table"`
	SecretLike bool             `json:"secretLike" doc:"True when the name looks like a secret (PASSWORD, TOKEN, KEY and the like): its value is never shown"`
	Unused     bool             `json:"unused" doc:"True when the variable is in the table and no compose file uses it"`
	Default    string           `json:"default" doc:"The default written in a compose file, when hasDefault is true; <hidden> for a secret-like name"`
	HasDefault bool             `json:"hasDefault" doc:"True when a reference in a compose file writes a default, even an empty one"`
	Places     []StackPlaceBody `json:"places" doc:"Where the compose files use the variable, ordered by file and line"`
}

// GitStackCheckBody is what a repository folder and its compose files would give.
type GitStackCheckBody struct {
	Commit           GitStackCommitBody     `json:"commit" doc:"The commit the check read; send its sha back as commit to check again without a fetch"`
	Services         []string               `json:"services" doc:"The services the compose files declare, sorted. Empty when they did not load"`
	Problems         []StackProblemBody     `json:"problems" doc:"Everything the check found, blocking and warnings, ordered by file and line. A blocking one means the stack could not be deployed as it stands"`
	Vars             []GitStackVariableBody `json:"vars" doc:"Every variable the compose files use and every variable sent, ordered by name"`
	Missing          []string               `json:"missing" doc:"The paths the compose files need that the repository lacks and no overlay path fills. The page asks for the contents of each as an overlay file"`
	NotImported      []string               `json:"notImported" doc:"The first 50 names, relative to the folder, of the files the stack does not need: they are not stored"`
	NotImportedTotal int                    `json:"notImportedTotal" doc:"How many files of the folder are not imported"`
}

// GitStackCheckOutput wraps GitStackCheckBody for huma.
type GitStackCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitStackCheckBody
}

// GitStackCheckInput is the path and body of the check route.
type GitStackCheckInput struct {
	EnvironmentPathInput
	Body GitStackCheckRequest
}

// GitStackOverlayBody is an overlay file of a create: a file TierPlan holds for a path the repository does not have.
type GitStackOverlayBody struct {
	Path    string `json:"path" doc:"The file's path inside the stack folder, as a path in a compose file writes it: relative, no .. or . name, no leading or doubled slash, no backslash or colon. 422 file_path_invalid otherwise. A path the repository holds is not stored: it is the blocking problem overlay_clash and the repository's file stands"`
	Content string `json:"content" writeOnly:"true" doc:"The file's text, at most 1 MiB (422 file_too_large). It is stored sealed as a secret file and never shown again"`
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
	registerStackGit(r, s, g)
	limiter := &checkLimiter{}
	httpx.Register(r, huma.Operation{
		OperationID: "checkGitStackSource", Method: http.MethodPost, Path: "/api/env/{envId}/git-stacks/check",
		Summary: "Check what a repository folder would give as a stack",
		Description: "Reads the folder at the ref the way creating a stack from it would, and answers, without saving anything: the commit the ref resolved to, the services found, every problem (blocking ones and warnings, with file and line), " +
			"the variables with the value a deploy would use and where it comes from (table, repo-env, default or unset) and whether it is required, the paths the compose files need that the repository lacks (missing), and the files of the folder that are not imported. " +
			"A secret variable, and a value whose name looks like a secret, is <hidden>. A variable that is required has no value from the variables sent, the folder's .env or a default: the page asks for it. " +
			"Overlay files are sent as paths only, never their contents. When commit is the sha an earlier check answered and the server's mirror holds it, nothing is fetched. " +
			"At most 60 checks a minute per user: 429 rate_limited. Fails like creating a stack from the folder does: 404 repository_not_found; 422 ref_invalid, ref_missing, commit_missing, commit_ambiguous, folder_missing, folder_invalid, compose_path_invalid, compose_file_missing, file_path_invalid, file_too_large or stack_too_large. Needs stacks.create (Admin).",
		Tags: []string{"stacks", "git"}, MaxBodyBytes: maxStackBody,
	}, rbac.Requirement{Perm: rbac.PermStacksCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *GitStackCheckInput) (*GitStackCheckOutput, error) {
			return checkGitStackSource(ctx, s, g, limiter, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "createGitStack", Method: http.MethodPost, Path: "/api/env/{envId}/git-stacks",
		Summary: "Create a stack from a folder of a git repository",
		Description: "Fetches the ref into the server's mirror of the repository and creates a stack whose draft holds the files at the commit it resolved to: the compose files, the folder's .env when it has one, " +
			"and every env_file, secrets and configs file the compose files name. The other files of the folder are not stored; the first 50 names and the total come back as notImported. " +
			"The stack's source is git, it polls the repository every 5 minutes, auto-deploy is off and it has no webhook. Nothing is deployed and nothing is sent to any engine. Audited as stack.created_from_git with the stack, the repository, the ref and the folder. " +
			"The name follows the rules of creating a stack in TierPlan (422 stack_name_invalid, 409 stack_name_taken, 409 stack_name_clash). The repository must be one of this environment or an instance repository (404 repository_not_found). " +
			"422 ref_invalid for a ref name git would refuse; 422 ref_missing, commit_missing, commit_ambiguous or folder_missing when the ref, commit or folder is not there; 422 folder_invalid, compose_path_invalid or compose_file_missing for the paths; " +
			"422 file_too_large (a file over 1 MiB) or stack_too_large (more than 50 files or 8 MiB); otherwise 422 with the code of the git failure as its type and the host in its errors, as for browsing. " +
			"When commit is sent and the ref no longer resolves to it, 409 commit_moved and nothing is created. Needs stacks.create (Admin).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxStackSaveBody,
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
		Name: body.Name, Target: target, Vars: vars, Overlays: overlayFiles(body.Overlays),
	})
	if err != nil {
		return nil, g.importProblem(repo, kind, body.Ref, body.Folder, err)
	}
	// The commit the person checked is the one to create from: a ref that has moved since is refused, not followed.
	if body.Commit != "" && imp.Commit.SHA != body.Commit {
		return nil, httpx.Problem(http.StatusConflict, problemCommitMoved, "The branch or tag has moved since it was checked. Check it again.")
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
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackCreatedFromGit, st.ID, audit.ResultSuccess, "",
		map[string]string{"stack": body.Name, "repository": imp.Source.RepoName, "ref": imp.Source.Ref, "folder": imp.Source.Folder})
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
func (d GitDeps) importProblem(repo gitconn.Repo, kind gitx.RefKind, ref, folder string, err error) error {
	var pe *gitsync.PathError
	var fe *stacks.FileError
	var le *gitx.LimitError
	var clash *gitsync.OverlayClashError
	switch {
	case errors.Is(err, gitconn.ErrNotFound):
		return d.repoFail("import a repository folder", err)
	case errors.As(err, &clash):
		// A pull or a change of source found a file of the repository at the path of an overlay file: nothing is stored.
		return &huma.ErrorModel{
			Type: problemOverlayClash, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: clash.Path + " is now in the repository and also an overlay file here. Remove the overlay file, then try again.",
			Errors: []*huma.ErrorDetail{{Location: "files", Message: clash.Path}},
		}
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
	return d.browseProblem(repo, kind, ref, folder, err)
}

// overlayFiles turns the overlay files of a create into the importer's.
func overlayFiles(in []GitStackOverlayBody) []gitsync.Overlay {
	out := make([]gitsync.Overlay, 0, len(in))
	for _, o := range in {
		out = append(out, gitsync.Overlay{Path: o.Path, Content: []byte(o.Content)})
	}
	return out
}

const (
	// checksPerMinute is how many checks of a repository folder one user may make in a minute: the page checks as a
	// person types, so ordinary use is far under it, and a check that finds the commit in the mirror costs no fetch.
	checksPerMinute = 60
	// checkIdle is how long a user's limiter is kept after their last check.
	checkIdle = 5 * time.Minute
)

// checkLimiter is the per-user limit on checks of a repository folder. It forgets a user who has been idle.
type checkLimiter struct {
	mu    sync.Mutex
	by    map[string]*checkEntry
	swept time.Time
	// perMinute is the rate; zero is checksPerMinute.
	perMinute int
}

type checkEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

func (l *checkLimiter) allow(user string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string]*checkEntry{}
	}
	if now.Sub(l.swept) > time.Minute {
		for id, e := range l.by {
			if now.Sub(e.seen) > checkIdle {
				delete(l.by, id)
			}
		}
		l.swept = now
	}
	e, ok := l.by[user]
	if !ok {
		n := cmp.Or(l.perMinute, checksPerMinute)
		e = &checkEntry{lim: rate.NewLimiter(rate.Every(time.Minute/time.Duration(n)), n)}
		l.by[user] = e
	}
	e.seen = now
	return e.lim.AllowN(now, 1)
}

// checkGitStackSource answers what a repository folder and its compose files would give. It writes nothing.
func checkGitStackSource(ctx context.Context, d StacksDeps, g GitDeps, lim *checkLimiter, in *GitStackCheckInput) (*GitStackCheckOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if g.Sync == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	if !lim.allow(string(p.Type)+":"+p.ID, now()) {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many checks. Wait a moment and try again.")
	}
	body := in.Body
	target, err := d.stackTargetOf(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	repo, err := g.browseRepo(ctx, env.ID, body.RepoID)
	if err != nil {
		return nil, err
	}
	vars, err := gitStackVars(body.Vars)
	if err != nil {
		return nil, err
	}
	// The name only matters to the swarm name rules; a name the target refuses is the create's to refuse.
	name := "stack"
	if stacks.NameValid(target, body.Name) {
		name = body.Name
	}
	kind := gitx.RefKind(body.RefKind)
	got, err := g.Sync.CheckSource(ctx, gitsync.CheckSourceRequest{
		ImportRequest: gitsync.ImportRequest{
			EnvID: env.ID, RepoID: repo.ID, RefKind: kind, Ref: body.Ref, Folder: body.Folder, ComposeFiles: body.ComposeFiles,
			Name: name, Target: target, Vars: vars,
		},
		Commit: body.Commit, OverlayPaths: body.OverlayPaths,
	})
	if err != nil {
		return nil, g.importProblem(repo, kind, body.Ref, body.Folder, err)
	}
	services, missing, notImported := got.Services, got.Missing, got.NotImported
	if services == nil {
		services = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	if notImported == nil {
		notImported = []string{}
	}
	return &GitStackCheckOutput{CacheControl: "no-store", Body: GitStackCheckBody{
		Commit:   GitStackCommitBody{SHA: got.Commit.SHA, When: got.Commit.When},
		Services: services, Problems: problemBodies(got.Problems), Vars: gitVariableBodies(got.Vars),
		Missing: missing, NotImported: notImported, NotImportedTotal: got.NotImportedTotal,
	}}, nil
}

// gitVariableBodies turns the variables of a check into their answer. A secret, and a value whose name looks like a
// secret, is never in it.
func gitVariableBodies(vars []stacks.ResolvedVar) []GitStackVariableBody {
	out := make([]GitStackVariableBody, 0, len(vars))
	for _, v := range vars {
		secretLike := v.SecretLike || stacks.SecretLikeName(v.Name)
		hide := v.Secret || secretLike
		value, def := v.Value, v.Default
		if hide && value != "" {
			value = stacks.Hidden
		}
		if hide && def != "" {
			def = stacks.Hidden
		}
		places := make([]StackPlaceBody, 0, len(v.Places))
		for _, pl := range v.Places {
			places = append(places, StackPlaceBody{File: pl.File, Line: pl.Line})
		}
		out = append(out, GitStackVariableBody{
			Name: v.Name, Value: value, Source: v.Source, Required: v.Required, Secret: v.Secret, SecretLike: secretLike,
			Unused: v.Unused, Default: def, HasDefault: v.HasDefault, Places: places,
		})
	}
	return out
}
