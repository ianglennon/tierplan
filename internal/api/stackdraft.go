// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

const (
	// maxStackVars bounds the variables of one draft. The files are bounded by stacks.CheckFiles.
	maxStackVars = 500
	// maxStackVarValue bounds the value of one variable; the name is bounded by stacks.MaxVarName.
	maxStackVarValue = 64 << 10
	// maxStackSaveBody is the largest body of a save. A save may hold stacks.MaxTotalBytes of
	// file content, and JSON writes some of it as escapes (a newline is two bytes, a control
	// character six), so the body is allowed twice that. A stack over the limit is then refused
	// by CheckFiles as 422 stack_too_large, not cut off by the body limit as a bare 413.
	maxStackSaveBody = 2 * stacks.MaxTotalBytes
)

// DraftFileBody is one file of a stack's draft.
type DraftFileBody struct {
	Path    string `json:"path" doc:"The file's path inside the stack: relative, at most 255 characters, no .. or . name, no leading or doubled slash, no backslash or colon, no control character. compose.yaml is the stack's compose file. A path that breaks the rule is 422 file_path_invalid"`
	Content string `json:"content" doc:"The file's text. A secret file's content is <hidden> on a read; sending <hidden> back for a secret file that is already saved keeps the saved content"`
	Secret  bool   `json:"secret" doc:"True when the file is stored sealed and never shown"`
	Origin  string `json:"origin,omitempty" enum:"repo,overlay" doc:"Left out for a stack that is not from git. For a stack from a git repository: repo for a file of the repository (read-only) and overlay for a file TierPlan holds for a path the repository lacks. On a save the server decides the origin from the path (a path the draft holds keeps its origin, any other path is an overlay), so what is sent here is ignored"`
}

// DraftGitBody is where the repository files of a git stack's draft come from.
type DraftGitBody struct {
	Commit   string `json:"commit" doc:"The full ID of the commit the draft's repository files are from"`
	RepoName string `json:"repoName" doc:"The repository's name when the draft was made from it"`
	Ref      string `json:"ref" doc:"The branch, tag or commit ID the draft follows"`
}

// DraftVarBody is one variable of a stack's draft.
type DraftVarBody struct {
	Name   string `json:"name" maxLength:"128" doc:"The variable's name: letters, digits and underscore, not starting with a digit"`
	Value  string `json:"value" doc:"The variable's value. A secret variable's value is <hidden> on a read; sending <hidden> back for a secret variable that is already saved keeps the saved value"`
	Secret bool   `json:"secret" doc:"True when the value is stored sealed and shown as <hidden>"`
}

// StackDraftBody is a stack's draft.
type StackDraftBody struct {
	IsDraft bool            `json:"isDraft" doc:"True when this is a saved draft. False when the stack has none (it was discarded): the files and variables are then the last deployed definition, or the starter example for a stack never deployed, and version is 0"`
	Version int64           `json:"version" doc:"The draft's version: send it back with a save, and the save is refused with 409 draft_conflict if the draft moved. 0 when there is no saved draft: a save at version 0 then makes one"`
	SavedBy string          `json:"savedBy" doc:"Who saved the draft last, or created the stack"`
	SavedAt string          `json:"savedAt" doc:"When, RFC 3339 UTC"`
	BaseSeq int64           `json:"baseSeq" doc:"The number of the deployed revision the draft started from, 0 for none"`
	Files   []DraftFileBody `json:"files" doc:"The draft's files by path"`
	Vars    []DraftVarBody  `json:"vars" doc:"The draft's variables by name"`
	// ComposeFiles and Git are for a stack from a git repository.
	ComposeFiles []string      `json:"composeFiles,omitempty" doc:"Left out for a stack with the one compose.yaml. For a stack from a git repository, the paths of its compose files in merge order"`
	Git          *DraftGitBody `json:"git,omitempty" doc:"Left out unless set: set for the draft of a stack made from a git repository, and says where its repository files are from"`
}

// StackDraftOutput wraps StackDraftBody for huma.
type StackDraftOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackDraftBody
}

// StackDraftSaveRequest is the body of PUT .../draft.
type StackDraftSaveRequest struct {
	Version int64           `json:"version" minimum:"0" doc:"The version of the draft you read, or 0 when you read a stack with no saved draft (isDraft false)"`
	Files   []DraftFileBody `json:"files" doc:"Every file of the draft. compose.yaml must be one of them. At most 50 files, 1 MiB each and 8 MiB in all"`
	Vars    []DraftVarBody  `json:"vars" maxItems:"500" doc:"Every variable of the draft"`
	// ComposeFiles is for a stack from a git repository.
	ComposeFiles []string `json:"composeFiles,omitempty" doc:"For a stack from a git repository, its compose files as read. Left out is the same as unchanged; a list that differs from the draft's is refused with 422 repo_file_read_only. Ignored for any other stack"`
}

// StackDraftSaveInput is the path and body of the save route.
type StackDraftSaveInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      StackDraftSaveRequest
}

// StackDraftDiscardInput is the path of the discard route.
type StackDraftDiscardInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// StackDraftSaveBody says what a save did.
type StackDraftSaveBody struct {
	Changed bool   `json:"changed" doc:"False when the draft already held exactly this content: nothing was written, the version did not move and no audit row was made"`
	Version int64  `json:"version" doc:"The draft's version now"`
	SavedBy string `json:"savedBy" doc:"Who saved the draft last"`
	SavedAt string `json:"savedAt" doc:"When, RFC 3339 UTC"`
	// SecretFiles lets the browser let go of the text it typed for a file the server sealed.
	SecretFiles []string `json:"secretFiles" doc:"Every file of the saved draft that is secret, ordered by path: the ones sent as secret, the ones the compose file uses under secrets:, and the ones that were already secret. Their content is stored sealed and is <hidden> in every read"`
}

// StackDraftSaveOutput wraps StackDraftSaveBody for huma.
type StackDraftSaveOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackDraftSaveBody
}

func registerStackDraft(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getStackDraft", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/draft",
		Summary: "A stack's draft",
		Description: "The files and variables of the stack's draft, with its version and who saved it last. The value of a secret variable and the content of a secret file are <hidden>. " +
			"Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackDraftOutput, error) {
			return getStackDraft(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "discardStackDraft", Method: http.MethodDelete, Path: "/api/env/{envId}/stacks/{stackId}/draft",
		Summary: "Discard a stack's draft",
		Description: "Deletes the saved draft with its files and variables; the editor then shows the last deployed definition, or the starter example for a stack never deployed. " +
			"Discarding when there is no draft is the same as discarding once: 204, nothing written, no audit row. A discard never sends anything to an engine. " +
			"Audited as stack.draft_discarded, with the version it deleted, when there was a draft. Needs stacks.update (Operator).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermStacksUpdate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackDraftDiscardInput) (*struct{}, error) {
			return nil, discardStackDraft(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "saveStackDraft", Method: http.MethodPut, Path: "/api/env/{envId}/stacks/{stackId}/draft",
		Summary: "Save a stack's draft",
		Description: "Replaces the draft's files and variables when the draft is still at the version you read; if someone saved in between the answer is 409 draft_conflict, naming who saved and when, and nothing is written. " +
			"Saving content equal to the saved draft changes nothing: changed is false, the version does not move and no audit row is written. " +
			"A save never sends anything to an engine: only a deploy does. " +
			"The files are held to the stack's file rules, each refused with 422 and the path in errors[].location: a path that is absolute, has .., a backslash or a colon (file_path_invalid), a path used twice (file_name_taken), a file over 1 MiB (file_too_large), more than 50 files or 8 MiB in all (stack_too_large), no compose.yaml (compose_missing). " +
			"A variable name must be letters, digits and underscore, not start with a digit and be at most 128 characters (422 variable_name_invalid), and be used once (422 variable_name_taken); " +
			"a secret variable's value must be at least 4 characters (422 secret_too_short), because shorter values cannot be hidden reliably; more than 500 variables, or a value over 64 KiB, is 422 stack_var_invalid. " +
			"A secret file or variable sent as <hidden> keeps its saved value; with none saved it is 422 stack_placeholder_without_value. " +
			"A file the compose file uses under secrets: is a secret file whatever the body says: it is stored sealed, and it stays secret until the file is deleted from the draft. The answer lists the secret files. " +
			"Audited as stack.draft_saved when it changed. Needs stacks.update (Operator).",
		Tags: []string{"stacks"}, MaxBodyBytes: maxStackSaveBody,
	}, rbac.Requirement{Perm: rbac.PermStacksUpdate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackDraftSaveInput) (*StackDraftSaveOutput, error) {
			return saveStackDraft(ctx, d, in)
		})
}

// maskedDraft is the draft as a browser receives it: secret values withheld.
func maskedDraft(s stackops.Snapshot) StackDraftBody {
	b := StackDraftBody{
		IsDraft: s.IsDraft,
		Version: s.Version, SavedBy: s.CreatedBy, SavedAt: rfc3339(s.CreatedAt), BaseSeq: s.BaseSeq,
		Files: make([]DraftFileBody, 0, len(s.Copy.Files)), Vars: make([]DraftVarBody, 0, len(s.Copy.Vars)),
	}
	for _, f := range s.Copy.Files {
		fb := DraftFileBody{Path: f.Path, Content: string(f.Content), Secret: f.Secret, Origin: string(f.Origin)}
		if f.Secret {
			fb.Content = redact.Placeholder
		}
		b.Files = append(b.Files, fb)
	}
	for _, v := range s.Copy.Vars {
		vb := DraftVarBody{Name: v.Name, Value: v.Value, Secret: v.Secret}
		if v.Secret {
			vb.Value = redact.Placeholder
		}
		b.Vars = append(b.Vars, vb)
	}
	if len(s.Copy.ComposeFiles) > 0 {
		b.ComposeFiles = slices.Clone(s.Copy.ComposeFiles)
	}
	if s.Git.Commit != "" {
		b.Git = &DraftGitBody{Commit: s.Git.Commit, RepoName: s.Git.Source.RepoName, Ref: s.Git.Source.Ref}
	}
	return b
}

func getStackDraft(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackDraftOutput, error) {
	repo := d.repo()
	if _, err := repo.Stack(ctx, in.EnvID, in.StackID); err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	draft, err := repo.Draft(ctx, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	return &StackDraftOutput{CacheControl: "no-store", Body: maskedDraft(draft)}, nil
}

// stackLookupProblem maps the errors of reading a stack or its draft.
func stackLookupProblem(d StacksDeps, envID string, err error) error {
	if errors.Is(err, stackops.ErrStackNotFound) || errors.Is(err, stackops.ErrNoDraft) {
		return stackNotFound()
	}
	return stackInternal(d, "read", envID, err)
}

// draftCopy turns a save request into a working copy, keeping the saved value of every secret
// the request sent as the placeholder. It returns a problem for anything it refuses.
func draftCopy(body StackDraftSaveRequest, saved stacks.WorkingCopy) (stacks.WorkingCopy, error) {
	if len(body.Files) > stacks.MaxFiles {
		return stacks.WorkingCopy{}, fileRuleProblem(&stacks.FileError{Err: stacks.ErrStackTooLarge})
	}
	if len(body.Vars) > maxStackVars {
		return stacks.WorkingCopy{}, httpx.Problem(http.StatusUnprocessableEntity, problemStackVarInvalid, "A stack holds at most 500 variables.")
	}
	savedFiles := make(map[string]stacks.File, len(saved.Files))
	for _, f := range saved.Files {
		savedFiles[f.Path] = f
	}
	savedVars := make(map[string]stacks.Var, len(saved.Vars))
	for _, v := range saved.Vars {
		savedVars[v.Name] = v
	}

	var wc stacks.WorkingCopy
	files := make([]stacks.File, 0, len(body.Files))
	for _, f := range body.Files {
		// The path is judged before it is used to find a saved secret, so a bad path is named as
		// one and not as a secret with no value to keep.
		if err := stacks.ValidPath(f.Path); err != nil {
			return wc, fileRuleProblem(err)
		}
		content := []byte(f.Content)
		if f.Secret && f.Content == redact.Placeholder {
			old, ok := savedFiles[f.Path]
			if !ok || !old.Secret {
				return wc, placeholderProblem("file", f.Path)
			}
			content = old.Content
		}
		files = append(files, stacks.File{Path: f.Path, Content: content, Secret: f.Secret})
	}
	// The whole set is judged on what will be stored, a kept secret at its real size.
	if err := stacks.CheckFiles(files); err != nil {
		return wc, fileRuleProblem(err)
	}
	wc.Files = files
	for _, v := range body.Vars {
		if len(v.Value) > maxStackVarValue {
			return wc, varProblem(v.Name, "A variable value is at most 64 KiB.")
		}
		value := v.Value
		if v.Secret && v.Value == redact.Placeholder {
			old, ok := savedVars[v.Name]
			if !ok || !old.Secret {
				return wc, placeholderProblem("variable", v.Name)
			}
			value = old.Value
		}
		wc.Vars = append(wc.Vars, stacks.Var{Name: v.Name, Value: value, Secret: v.Secret})
	}
	// The names are judged on what will be stored. A secret that is too short is judged only when
	// the draft is saved: the check runs as a person types, and a half-typed secret is not yet a
	// mistake.
	if err := stacks.CheckVarNames(wc.Vars); err != nil {
		return wc, varRuleProblem(err)
	}
	return wc, nil
}

// fileRuleProblem is the 422 for a rule the files of a stack break: its own problem type, and the
// path of the file at fault in errors[].location ("files" when the rule is about the whole set,
// "file" when the path itself is empty).
func fileRuleProblem(err error) error {
	var fe *stacks.FileError
	loc := ""
	if errors.As(err, &fe) {
		loc = fe.Path
	}
	typ, detail := problemFilePathInvalid, "A file path must be relative and inside the stack: no .. or . name, no leading or doubled slash, no backslash or colon, no control character, at most 255 characters and 100 in a name."
	switch {
	case errors.Is(err, stacks.ErrPathTaken):
		typ, detail = problemFileNameTaken, "Two files have the same path."
	case errors.Is(err, stacks.ErrFileTooLarge):
		typ, detail = problemFileTooLarge, "A file is at most 1 MiB."
	case errors.Is(err, stacks.ErrStackTooLarge):
		typ, detail = problemStackTooLarge, "A stack holds at most 50 files and 8 MiB in all."
		if loc == "" {
			loc = "files"
		}
	case errors.Is(err, stacks.ErrComposeMissing):
		typ, detail = problemComposeMissing, "A stack needs a compose.yaml."
	}
	if loc == "" {
		loc = "file"
	}
	return &huma.ErrorModel{
		Type: typ, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: []*huma.ErrorDetail{{Location: loc, Message: detail}},
	}
}

// varRuleProblem is the 422 for a rule the variable table breaks: its own problem type, and the
// name of the variable at fault in errors[].location "variable".
func varRuleProblem(err error) error {
	var ve *stacks.VarError
	name := ""
	if errors.As(err, &ve) {
		name = ve.Name
	}
	typ, detail := problemVariableNameInvalid, "A variable name is letters, digits and underscore, does not start with a digit and is at most 128 characters."
	switch {
	case errors.Is(err, stacks.ErrVarNameTaken):
		typ, detail = problemVariableNameTaken, "A variable name is used twice."
	case errors.Is(err, stacks.ErrSecretTooShort):
		typ, detail = problemSecretTooShort, "A secret value is at least 4 characters, so that hiding it can't hide ordinary words. Use a longer value, or turn Secret off."
	}
	return &huma.ErrorModel{
		Type: typ, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: []*huma.ErrorDetail{{Location: "variable", Message: name}},
	}
}

func varProblem(name, detail string) error {
	return &huma.ErrorModel{
		Type: problemStackVarInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: []*huma.ErrorDetail{{Location: "variable", Message: name}},
	}
}

func placeholderProblem(kind, name string) error {
	return &huma.ErrorModel{
		Type: problemStackPlaceholder, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: "The " + kind + " " + name + " was sent as <hidden> but has no saved secret value to keep. Enter its value.",
		Errors: []*huma.ErrorDetail{{Location: kind, Message: name}},
	}
}

// draftConflict is the 409 for a draft that moved: who saved it and when. A draft that was
// discarded has no saver: savedBy is empty and version is 0.
func draftConflict(c *stackops.DraftConflictError) error {
	detail := "The draft was saved by " + c.SavedBy + " at " + rfc3339(c.SavedAt) + ". Reload it to see their changes."
	if c.SavedBy == "" {
		detail = "The draft was discarded. Reload it to see what is saved now."
	}
	return &huma.ErrorModel{
		Type: problemDraftConflict, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: detail,
		Errors: []*huma.ErrorDetail{
			{Location: "savedBy", Message: c.SavedBy},
			{Location: "savedAt", Message: rfc3339(c.SavedAt)},
			{Location: "version", Message: itoa(c.Version)},
		},
	}
}

func saveStackDraft(ctx context.Context, d StacksDeps, in *StackDraftSaveInput) (*StackDraftSaveOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	current, err := repo.Draft(ctx, st.ID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	wc, err := draftCopy(in.Body, current.Copy)
	if err != nil {
		return nil, err
	}
	if err := stacks.CheckVars(wc.Vars); err != nil {
		return nil, varRuleProblem(err)
	}
	// A draft that points an inline or adopted stack at a repository (D-24) holds repository files too: it is edited by the
	// rules of a git stack from the moment it is saved.
	if st.Source.Kind == stacks.SourceGit || current.Git.Commit != "" {
		wc.ComposeFiles = slices.Clone(in.Body.ComposeFiles)
		if len(wc.ComposeFiles) == 0 {
			wc.ComposeFiles = slices.Clone(current.Copy.ComposeFiles)
		}
		wc = stackops.GitDraftOrigins(current.Copy, wc)
		if err := stackops.CheckGitDraftEdit(current.Copy, wc); err != nil {
			return nil, repoFileReadOnly(current.Git.Source.RepoName, err)
		}
	}
	saved, changed, err := repo.SaveDraft(ctx, st.ID, in.Body.Version, p.Name, wc)
	var conflict *stackops.DraftConflictError
	if errors.As(err, &conflict) {
		return nil, draftConflict(conflict)
	}
	if err != nil {
		return nil, stackInternal(d, "save draft", env.ID, err)
	}
	if changed {
		recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackDraftSaved, st.ID, audit.ResultSuccess, "", map[string]string{
			"stack": st.Name, "version": itoa(saved.Version), "files": itoa(int64(len(wc.Files))), "vars": itoa(int64(len(wc.Vars))),
		})
	}
	secretFiles := []string{}
	for _, f := range saved.Copy.Files {
		if f.Secret {
			secretFiles = append(secretFiles, f.Path)
		}
	}
	slices.Sort(secretFiles)
	return &StackDraftSaveOutput{CacheControl: "no-store", Body: StackDraftSaveBody{
		Changed: changed, Version: saved.Version, SavedBy: saved.CreatedBy, SavedAt: rfc3339(saved.CreatedAt),
		SecretFiles: secretFiles,
	}}, nil
}

// repoFileReadOnly is the 422 for an edit of a git stack's draft that touches the repository's files or the list of
// compose files: the path in errors[].location, or "composeFiles" for the list.
func repoFileReadOnly(repoName string, err error) error {
	var re *stackops.RepoFileEditError
	loc := "composeFiles"
	if errors.As(err, &re) && re.Path != "" {
		loc = re.Path
	}
	where := "the repository"
	if repoName != "" {
		where = repoName
	}
	detail := "Repository files can't be edited here. Change them in " + where + " and pull."
	return &huma.ErrorModel{
		Type: problemRepoFileReadOnly, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: []*huma.ErrorDetail{{Location: loc, Message: detail}},
	}
}

// discardStackDraft deletes a stack's draft. It is idempotent: with no draft it answers 204 and
// writes neither a row nor an audit entry. It never touches an engine.
func discardStackDraft(ctx context.Context, d StacksDeps, in *StackDraftDiscardInput) error {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return stackLookupProblem(d, env.ID, err)
	}
	discarded, version, err := repo.DiscardDraft(ctx, st.ID)
	if err != nil {
		return stackInternal(d, "discard draft", env.ID, err)
	}
	if discarded {
		recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackDraftDiscarded, st.ID, audit.ResultSuccess, "", map[string]string{
			"stack": st.Name, "version": itoa(version),
		})
	}
	return nil
}
