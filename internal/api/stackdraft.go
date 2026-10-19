// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

const (
	// maxStackFiles and maxStackVars bound one draft.
	maxStackFiles = 100
	maxStackVars  = 500
	// maxStackPath is the longest file path of a stack, in bytes.
	maxStackPath = 255
	// maxStackVarName and maxStackVarValue bound one variable.
	maxStackVarName  = 128
	maxStackVarValue = 64 << 10
)

var varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DraftFileBody is one file of a stack's draft.
type DraftFileBody struct {
	Path    string `json:"path" maxLength:"255" doc:"The file's path inside the stack: relative, with no .. and no leading slash. compose.yaml is the stack's compose file"`
	Content string `json:"content" doc:"The file's text. A secret file's content is <hidden> on a read; sending <hidden> back for a secret file that is already saved keeps the saved content"`
	Secret  bool   `json:"secret" doc:"True when the file is stored sealed and never shown"`
}

// DraftVarBody is one variable of a stack's draft.
type DraftVarBody struct {
	Name   string `json:"name" maxLength:"128" doc:"The variable's name: letters, digits and underscore, not starting with a digit"`
	Value  string `json:"value" doc:"The variable's value. A secret variable's value is <hidden> on a read; sending <hidden> back for a secret variable that is already saved keeps the saved value"`
	Secret bool   `json:"secret" doc:"True when the value is stored sealed and shown as <hidden>"`
}

// StackDraftBody is a stack's draft.
type StackDraftBody struct {
	Version int64           `json:"version" doc:"The draft's version: send it back with a save, and the save is refused with 409 draft_conflict if the draft moved"`
	SavedBy string          `json:"savedBy" doc:"Who saved the draft last, or created the stack"`
	SavedAt string          `json:"savedAt" doc:"When, RFC 3339 UTC"`
	BaseSeq int64           `json:"baseSeq" doc:"The number of the deployed revision the draft started from, 0 for none"`
	Files   []DraftFileBody `json:"files" doc:"The draft's files by path"`
	Vars    []DraftVarBody  `json:"vars" doc:"The draft's variables by name"`
}

// StackDraftOutput wraps StackDraftBody for huma.
type StackDraftOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackDraftBody
}

// StackDraftSaveRequest is the body of PUT .../draft.
type StackDraftSaveRequest struct {
	Version int64           `json:"version" minimum:"1" doc:"The version of the draft you read"`
	Files   []DraftFileBody `json:"files" maxItems:"100" doc:"Every file of the draft. compose.yaml must be one of them"`
	Vars    []DraftVarBody  `json:"vars" maxItems:"500" doc:"Every variable of the draft"`
}

// StackDraftSaveInput is the path and body of the save route.
type StackDraftSaveInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      StackDraftSaveRequest
}

// StackDraftSaveBody says what a save did.
type StackDraftSaveBody struct {
	Changed bool   `json:"changed" doc:"False when the draft already held exactly this content: nothing was written, the version did not move and no audit row was made"`
	Version int64  `json:"version" doc:"The draft's version now"`
	SavedBy string `json:"savedBy" doc:"Who saved the draft last"`
	SavedAt string `json:"savedAt" doc:"When, RFC 3339 UTC"`
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
		OperationID: "saveStackDraft", Method: http.MethodPut, Path: "/api/env/{envId}/stacks/{stackId}/draft",
		Summary: "Save a stack's draft",
		Description: "Replaces the draft's files and variables when the draft is still at the version you read; if someone saved in between the answer is 409 draft_conflict, naming who saved and when, and nothing is written. " +
			"Saving content equal to the saved draft changes nothing: changed is false, the version does not move and no audit row is written. " +
			"A save never sends anything to an engine: only a deploy does. " +
			"compose.yaml must be among the files (422 stack_file_invalid, as for a path that is absolute, has .., is repeated, or a file over 1 MiB); a variable name must be letters, digits and underscore (422 stack_var_invalid). " +
			"A secret file or variable sent as <hidden> keeps its saved value; with none saved it is 422 stack_placeholder_without_value. Audited as stack.draft_saved when it changed. Needs stacks.update (Operator).",
		Tags: []string{"stacks"}, MaxBodyBytes: maxStackBody,
	}, rbac.Requirement{Perm: rbac.PermStacksUpdate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackDraftSaveInput) (*StackDraftSaveOutput, error) {
			return saveStackDraft(ctx, d, in)
		})
}

// maskedDraft is the draft as a browser receives it: secret values withheld.
func maskedDraft(s stackops.Snapshot) StackDraftBody {
	b := StackDraftBody{
		Version: s.Version, SavedBy: s.CreatedBy, SavedAt: rfc3339(s.CreatedAt), BaseSeq: s.BaseSeq,
		Files: make([]DraftFileBody, 0, len(s.Copy.Files)), Vars: make([]DraftVarBody, 0, len(s.Copy.Vars)),
	}
	for _, f := range s.Copy.Files {
		fb := DraftFileBody{Path: f.Path, Content: string(f.Content), Secret: f.Secret}
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

// validStackPath reports whether p can name a file of a stack: relative, clean, no ".." and no
// control characters or backslashes. Nothing a stack stores is ever read from the server's
// own filesystem (D-05), but the path is still held to the rule so a later plan that resolves
// env_file or a secret file against it can trust it.
func validStackPath(p string) bool {
	if p == "" || len(p) > maxStackPath || strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	if path.Clean(p) != p {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// draftCopy turns a save request into a working copy, keeping the saved value of every secret
// the request sent as the placeholder. It returns a problem for anything it refuses.
func draftCopy(body StackDraftSaveRequest, saved stacks.WorkingCopy) (stacks.WorkingCopy, error) {
	if len(body.Files) > maxStackFiles {
		return stacks.WorkingCopy{}, httpx.Problem(http.StatusUnprocessableEntity, problemStackFileInvalid, "A stack holds at most 100 files.")
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
	seen := map[string]bool{}
	for _, f := range body.Files {
		if !validStackPath(f.Path) {
			return wc, fileProblem(f.Path, "A file path must be relative, with no .. and no leading slash.")
		}
		if seen[f.Path] {
			return wc, fileProblem(f.Path, "A file path is used twice.")
		}
		seen[f.Path] = true
		if len(f.Content) > stacks.MaxFileBytes {
			return wc, fileProblem(f.Path, "A file is at most 1 MiB.")
		}
		content := []byte(f.Content)
		if f.Secret && f.Content == redact.Placeholder {
			old, ok := savedFiles[f.Path]
			if !ok || !old.Secret {
				return wc, placeholderProblem("file", f.Path)
			}
			content = old.Content
		}
		wc.Files = append(wc.Files, stacks.File{Path: f.Path, Content: content, Secret: f.Secret})
	}
	if !seen[stacks.ComposeFile] {
		return wc, fileProblem(stacks.ComposeFile, "A stack needs a compose.yaml.")
	}
	seenVar := map[string]bool{}
	for _, v := range body.Vars {
		if len(v.Name) > maxStackVarName || !varNameRe.MatchString(v.Name) {
			return wc, varProblem(v.Name, "A variable name is letters, digits and underscore, and does not start with a digit.")
		}
		if seenVar[v.Name] {
			return wc, varProblem(v.Name, "A variable name is used twice.")
		}
		seenVar[v.Name] = true
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
	return wc, nil
}

func fileProblem(p, detail string) error {
	return &huma.ErrorModel{
		Type: problemStackFileInvalid, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: []*huma.ErrorDetail{{Location: "file", Message: p}},
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

// draftConflict is the 409 for a draft that moved: who saved it and when.
func draftConflict(c *stackops.DraftConflictError) error {
	return &huma.ErrorModel{
		Type: problemDraftConflict, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: "The draft was saved by " + c.SavedBy + " at " + rfc3339(c.SavedAt) + ". Reload it to see their changes.",
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
	return &StackDraftSaveOutput{CacheControl: "no-store", Body: StackDraftSaveBody{
		Changed: changed, Version: saved.Version, SavedBy: saved.CreatedBy, SavedAt: rfc3339(saved.CreatedAt),
	}}, nil
}
