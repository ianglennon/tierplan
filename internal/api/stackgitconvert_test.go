// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Pointing an inline or adopted stack at a repository (plan 09-30, GIT-02, D-24): the conversion writes the draft and
// applies nothing; the review says it replaces the inline files; a good deploy makes the stack a git stack and keeps its
// history; discarding the draft leaves the stack exactly as it was.

type connectAnswer struct {
	DraftVersion int64  `json:"draftVersion"`
	Commit       string `json:"commit"`
	Ref          string `json:"ref"`
}

// legacyCompose is the inline stack's compose file; the repository's replaces it.
const legacyCompose = "services:\n  web:\n    image: nginx:%s\n"

// inlineWithHistory makes an inline stack named legacy as the environment admin, with a plain file, a secret file and a
// variable, and deploys it twice, so that it has revisions #1 and #2.
func inlineWithHistory(t *testing.T, g *gitStackFix) (*roleCaller, string) {
	t.Helper()
	admin := g.f.caller(t, g.e, "envadmin-1", "admin")
	st := newStack(t, admin, "legacy")
	save := func(tag string) {
		v := draftVersion(t, admin, st.ID)
		body, err := json.Marshal(map[string]any{
			"version": v,
			"files": []map[string]any{
				{"path": "compose.yaml", "content": strings.Replace(legacyCompose, "%s", tag, 1), "secret": false},
				{"path": "notes.txt", "content": "hello\n", "secret": false},
				{"path": "web.env", "content": "OLD=1\n", "secret": false},
				{"path": "secrets/db_pw.txt", "content": "hunter2-sealed", "secret": true},
			},
			"vars": []map[string]any{{"name": "TAG", "value": "9", "secret": false}, {"name": "TOKEN", "value": "s3cret-token", "secret": true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec := admin.saveDraft(st.ID, string(body)); rec.Code != http.StatusOK {
			t.Fatalf("save: %d %s", rec.Code, rec.Body)
		}
	}
	for _, tag := range []string{"1.25", "1.26"} {
		save(tag)
		if job := g.f.deployAndWait(t, admin, st.ID, `{"draftVersion":`+itoaT(draftVersion(t, admin, st.ID))+`}`); job.State != "done" {
			t.Fatalf("deploy of %s = %+v, want done", tag, job)
		}
	}
	return admin, st.ID
}

func (c *roleCaller) connectGit(stackID string, version int64, repoID string, mut func(map[string]any)) *httptest.ResponseRecorder {
	c.t.Helper()
	b := map[string]any{
		"draftVersion": version, "repoId": repoID, "refKind": "branch", "ref": "main", "folder": "stacks/web",
		"composeFiles": []string{"compose.yaml"},
		"files": []map[string]any{
			{"path": "notes.txt", "keep": false},
			{"path": "secrets/db_pw.txt", "keep": true},
		},
	}
	if mut != nil {
		mut(b)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(http.MethodPost, "stacks/"+stackID+"/connect-git", string(raw))
}

func (c *roleCaller) stackOf(stackID string) stackAnswer {
	c.t.Helper()
	rec := c.do(http.MethodGet, "stacks/"+stackID, "")
	if rec.Code != http.StatusOK {
		c.t.Fatalf("stack: %d %s", rec.Code, rec.Body)
	}
	return answerOf[stackAnswer](c.t, rec)
}

// convertReviewAnswer is the git part of a review that matters to a conversion.
type convertReviewAnswer struct {
	Git *struct {
		Commit     string `json:"commit"`
		Ref        string `json:"ref"`
		RepoName   string `json:"repoName"`
		Converts   bool   `json:"converts"`
		HeadCommit string `json:"headCommit"`
		Replaces   any    `json:"replaces"`
	} `json:"git"`
}

func TestConvertInlineThroughReview(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	admin, stackID := inlineWithHistory(t, g)
	repo := g.stackRepo()
	ctx := context.Background()
	writes := len(g.f.fake.Writes())

	before, _, err := repo.ListRevisions(ctx, stackID)
	if err != nil || len(before) != 2 {
		t.Fatalf("revisions before = %d, %v, want 2", len(before), err)
	}

	version := draftVersion(t, admin, stackID)
	rec := admin.connectGit(stackID, version, g.id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	got := answerOf[connectAnswer](t, rec)
	if got.DraftVersion != version+1 || got.Commit != g.sha || got.Ref != "main" {
		t.Fatalf("answer = %+v, want version %d at %s on main", got, version+1, g.sha)
	}

	// Nothing is applied: the stack is still inline, is not a git stack and wrote nothing to the engine.
	if s := admin.stackOf(stackID); s.Source != "inline" {
		t.Errorf("source after the conversion = %q, want inline until a deploy", s.Source)
	}
	if rec := admin.do(http.MethodGet, "stacks/"+stackID+"/git", ""); rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_git" {
		t.Errorf("git of a stack being converted: %d %s, want 409 stack_not_git", rec.Code, rec.Body)
	}
	if n := len(g.f.fake.Writes()); n != writes {
		t.Errorf("the conversion wrote %d times to the engine, want none", n-writes)
	}

	// The draft holds the repository's files, the secret file kept as an overlay and the variables; the plain file is gone.
	draft := answerOf[draftAnswer](t, admin.do(http.MethodGet, "stacks/"+stackID+"/draft", ""))
	paths := map[string]bool{}
	for _, f := range draft.Files {
		paths[f.Path] = f.Secret
	}
	for _, want := range []string{"compose.yaml", "web.env"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("draft files = %v, want the repository's %s", paths, want)
		}
	}
	if secret, ok := paths["secrets/db_pw.txt"]; !ok || !secret {
		t.Errorf("draft files = %v, want secrets/db_pw.txt kept as a secret file", paths)
	}
	if _, ok := paths["notes.txt"]; ok {
		t.Errorf("draft files = %v, want notes.txt dropped", paths)
	}
	vars := map[string]string{}
	for _, v := range draft.Vars {
		vars[v.Name] = v.Value
	}
	if vars["TAG"] != "9" || vars["TOKEN"] == "" {
		t.Errorf("draft vars = %v, want TAG and TOKEN kept", vars)
	}
	snap, err := repo.Draft(ctx, stackID)
	if err != nil {
		t.Fatal(err)
	}
	if kept, ok := fileText(snap.Copy.Files, "secrets/db_pw.txt"); !ok || kept != "hunter2-sealed" {
		t.Errorf("the kept overlay file = %q, %v, want its content", kept, ok)
	}
	if v := varValue(snap.Copy.Vars, "TOKEN"); v != "s3cret-token" {
		t.Errorf("the secret variable = %q, want its value kept", v)
	}

	// The review says it is the first sync with the repository, replacing the inline files.
	rv := answerOf[convertReviewAnswer](t, admin.do(http.MethodPost, "stacks/"+stackID+"/review", `{}`))
	if rv.Git == nil || !rv.Git.Converts || rv.Git.Commit != g.sha || rv.Git.HeadCommit != g.sha || rv.Git.RepoName != "acme" || rv.Git.Replaces != nil {
		t.Fatalf("review git = %+v, want a conversion to acme at %s", rv.Git, g.sha)
	}

	// The deploy is the normal one; it makes the stack a git stack, and the history stays.
	review, code, raw := admin.reviewStack(stackID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	job := g.f.deployAndWait(t, admin, stackID, `{"draftVersion":`+itoaT(review.DraftVersion)+`,"pins":`+pinsJSON(t, review.Pins)+`}`)
	if job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}
	if s := admin.stackOf(stackID); s.Source != "git" {
		t.Errorf("source after the deploy = %q, want git", s.Source)
	}
	if st := g.readGit(t, g.admin, stackID); st.Ref != "main" || st.RefKind != "branch" || st.Folder != "stacks/web" || st.Status != "in_sync" || st.DeployedSha != g.sha || st.HeadSha != g.sha {
		t.Errorf("git after the deploy = %+v, want main of stacks/web in sync at %s", st, g.sha)
	}
	after, _, err := repo.ListRevisions(ctx, stackID)
	if err != nil || len(after) != 3 {
		t.Fatalf("revisions after = %d, %v, want 3", len(after), err)
	}
	if after[0].Seq != 3 || after[0].Git.Commit != g.sha || after[0].Outcome != stackops.OutcomeGood {
		t.Errorf("revision #3 = %+v, want a good one made from %s", after[0], g.sha)
	}
	for i, old := range []int64{2, 1} {
		if r := after[i+1]; r.Seq != old || r.Git.Commit != "" || r.Outcome != stackops.OutcomeGood {
			t.Errorf("revision #%d = %+v, want the inline revision kept", old, r)
		}
	}
	// The stack is a git stack for every part of the system from now on, and nothing is pending.
	if _, ok, err := repo.PendingGit(ctx, stackID); err != nil || ok {
		t.Errorf("PendingGit after the deploy = %v, %v, want none", ok, err)
	}
	if rv := answerOf[convertReviewAnswer](t, admin.do(http.MethodPost, "stacks/"+stackID+"/review", `{}`)); rv.Git == nil || rv.Git.Converts {
		t.Errorf("review git after the deploy = %+v, want no conversion", rv.Git)
	}

	// The conversion was audited, with counts and no path or content.
	var rows []auditRowFull
	for _, a := range g.f.stackAudits(t) {
		if a.Action == audit.ActionStackPointedAtGit {
			rows = append(rows, a)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("stack.pointed_at_git rows = %d, want 1", len(rows))
	}
	d := rows[0].Details
	if rows[0].TargetID != stackID || d["stack"] != "legacy" || d["repository"] != "acme" || d["ref"] != "main" || d["folder"] != "stacks/web" || d["kept"] != "1" || d["dropped"] != "3" {
		t.Errorf("audit row = %+v, want legacy -> acme main stacks/web, 1 kept and 3 dropped", rows[0])
	}
	for k, v := range d {
		if strings.Contains(v, "hunter2") || strings.Contains(v, "db_pw") {
			t.Errorf("audit detail %s = %q names a kept file or its content", k, v)
		}
	}
}

func TestConvertDiscard(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	admin, stackID := inlineWithHistory(t, g)
	repo := g.stackRepo()
	ctx := context.Background()

	if rec := admin.connectGit(stackID, draftVersion(t, admin, stackID), g.id, nil); rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	if _, ok, err := repo.PendingGit(ctx, stackID); err != nil || !ok {
		t.Fatalf("PendingGit after the conversion = %v, %v, want a pending source", ok, err)
	}

	if rec := admin.do(http.MethodDelete, "stacks/"+stackID+"/draft", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("discard: %d %s", rec.Code, rec.Body)
	}

	// The stack is exactly as it was: inline, no git settings, its two revisions, and a draft that is its last revision.
	if _, ok, err := repo.PendingGit(ctx, stackID); err != nil || ok {
		t.Errorf("PendingGit after the discard = %v, %v, want none", ok, err)
	}
	if _, err := repo.StackGit(ctx, stackID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("StackGit after the discard = %v, want ErrNoGit", err)
	}
	if s := admin.stackOf(stackID); s.Source != "inline" || s.HasDraft {
		t.Errorf("stack after the discard = %+v, want inline with no draft", s)
	}
	if revs, _, err := repo.ListRevisions(ctx, stackID); err != nil || len(revs) != 2 {
		t.Errorf("revisions after the discard = %d, %v, want the 2 there were", len(revs), err)
	}
	draft := answerOf[draftAnswer](t, admin.do(http.MethodGet, "stacks/"+stackID+"/draft", ""))
	back := false
	for _, f := range draft.Files {
		back = back || f.Path == "notes.txt"
	}
	if !back {
		t.Errorf("draft after the discard = %+v, want the inline files back", draft.Files)
	}

	// The stack can be pointed at the repository again.
	if rec := admin.connectGit(stackID, draftVersion(t, admin, stackID), g.id, nil); rec.Code != http.StatusOK {
		t.Errorf("connect after the discard: %d %s", rec.Code, rec.Body)
	}
}

func TestConvertClash(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	admin, stackID := inlineWithHistory(t, g)
	repo := g.stackRepo()
	version := draftVersion(t, admin, stackID)

	// web.env is a file of the stack and also of the repository's folder: keeping it would shadow the repository's.
	rec := admin.connectGit(stackID, version, g.id, func(b map[string]any) {
		b["files"] = []map[string]any{{"path": "web.env", "keep": true}, {"path": "notes.txt", "keep": false}}
	})
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "overlay_clash" {
		t.Fatalf("keep a clashing file: %d %s, want 422 overlay_clash", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "web.env exists in the repository. Drop it or rename it first.") {
		t.Errorf("the refusal does not say which file and what to do: %s", rec.Body)
	}
	if got := draftVersion(t, admin, stackID); got != version {
		t.Errorf("draft version = %d, want %d: a refused conversion stores nothing", got, version)
	}
	if _, ok, err := repo.PendingGit(context.Background(), stackID); err != nil || ok {
		t.Errorf("PendingGit after a refusal = %v, %v, want none", ok, err)
	}
	if rows := g.f.stackAudits(t); slices.ContainsFunc(rows, func(a auditRowFull) bool { return a.Action == audit.ActionStackPointedAtGit }) {
		t.Error("a refused conversion was audited as done")
	}

	// Dropping it is the way out.
	rec = admin.connectGit(stackID, version, g.id, func(b map[string]any) {
		b["files"] = []map[string]any{{"path": "web.env", "keep": false}}
	})
	if rec.Code != http.StatusOK {
		t.Errorf("drop the clashing file: %d %s, want 200", rec.Code, rec.Body)
	}

	// A file that is not one of the draft's, and one named twice, are refused too.
	version = draftVersion(t, admin, stackID)
	if rec := admin.connectGit(stackID, version, g.id, func(b map[string]any) {
		b["files"] = []map[string]any{{"path": "nope.txt", "keep": true}}
	}); rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "file_path_invalid" {
		t.Errorf("an unknown file: %d %s, want 422 file_path_invalid", rec.Code, rec.Body)
	}
}

func fileText(files []stacks.File, path string) (string, bool) {
	for _, f := range files {
		if f.Path == path {
			return string(f.Content), true
		}
	}
	return "", false
}

func varValue(vars []stacks.Var, name string) string {
	for _, v := range vars {
		if v.Name == name {
			return v.Value
		}
	}
	return ""
}

func TestConvertPermissions(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	admin, stackID := inlineWithHistory(t, g)
	operator := g.f.caller(t, g.e, "operator-1", "operator")
	viewer := g.f.caller(t, g.e, "viewer-1", "viewer")
	repo := g.stackRepo()
	ctx := context.Background()
	version := draftVersion(t, admin, stackID)

	// Pointing a stack at a repository is for an Admin of the environment (git.manage), whatever a person may deploy.
	for name, who := range map[string]*roleCaller{"an operator": operator, "a viewer": viewer} {
		if rec := who.connectGit(stackID, version, g.id, nil); rec.Code != http.StatusForbidden {
			t.Errorf("connect as %s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	if _, ok, err := repo.PendingGit(ctx, stackID); err != nil || ok {
		t.Errorf("PendingGit after refused calls = %v, %v, want none", ok, err)
	}
	if got := draftVersion(t, admin, stackID); got != version {
		t.Errorf("draft version = %d, want %d: a refused call stores nothing", got, version)
	}
	for _, a := range g.f.stackAudits(t) {
		if a.Action == audit.ActionStackPointedAtGit {
			t.Errorf("a refused call was audited: %+v", a)
		}
	}

	// An adopted stack converts like an inline one.
	if _, err := g.f.st.Write.ExecContext(ctx, `UPDATE stacks SET source_kind = 'adopted' WHERE id = ?`, stackID); err != nil {
		t.Fatal(err)
	}
	if s := admin.stackOf(stackID); s.Source != "adopted" {
		t.Fatalf("source = %q, want adopted", s.Source)
	}
	if rec := admin.connectGit(stackID, version, g.id, nil); rec.Code != http.StatusOK {
		t.Fatalf("connect an adopted stack: %d %s", rec.Code, rec.Body)
	}
	review, code, raw := admin.reviewStack(stackID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if job := g.f.deployAndWait(t, admin, stackID, `{"draftVersion":`+itoaT(review.DraftVersion)+`,"pins":`+pinsJSON(t, review.Pins)+`}`); job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}
	if s := admin.stackOf(stackID); s.Source != "git" {
		t.Errorf("source after the deploy = %q, want git", s.Source)
	}

	// A git stack is not converted again.
	rec := admin.connectGit(stackID, draftVersion(t, admin, stackID), g.id, nil)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "already_git" {
		t.Errorf("connect a git stack: %d %s, want 409 already_git", rec.Code, rec.Body)
	}
	// A stack of another environment, or one that is not there, is not found.
	if rec := admin.do(http.MethodPost, "stacks/nope/connect-git", `{"draftVersion":0,"repoId":"`+g.id+`","refKind":"branch","ref":"main","composeFiles":["compose.yaml"]}`); rec.Code != http.StatusNotFound {
		t.Errorf("connect a stack that is not there: %d %s, want 404", rec.Code, rec.Body)
	}
}

func TestAConvertDraftIsEditedByTheRulesOfAGitStack(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	admin, stackID := inlineWithHistory(t, g)
	if rec := admin.connectGit(stackID, draftVersion(t, admin, stackID), g.id, nil); rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	draft := answerOf[draftAnswer](t, admin.do(http.MethodGet, "stacks/"+stackID+"/draft", ""))

	// A repository file can't be edited in the draft, as in any git stack's.
	files := make([]map[string]any, 0, len(draft.Files))
	for _, f := range draft.Files {
		content := f.Content
		if f.Path == "compose.yaml" {
			content = "services:\n  web:\n    image: nginx:9\n"
		}
		files = append(files, map[string]any{"path": f.Path, "content": content, "secret": f.Secret})
	}
	body, err := json.Marshal(map[string]any{"version": draft.Version, "files": files, "vars": []map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := admin.saveDraft(stackID, string(body))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("edit a repository file of a convert draft: %d %s, want 422", rec.Code, rec.Body)
	}
	if got := draftVersion(t, admin, stackID); got != draft.Version {
		t.Errorf("draft version = %d, want %d: a refused edit stores nothing", got, draft.Version)
	}
}
