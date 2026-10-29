// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The draft of a git stack (plan 09-21, GIT-08, D-09, D-11): the repository's files are read-only whatever is sent,
// and overlay files and variables save as for an inline stack.

type gitDraftAnswer struct {
	Version int64            `json:"version"`
	Files   []map[string]any `json:"files"`
	Vars    []map[string]any `json:"vars"`
	// ComposeFiles and Git are what a git stack's draft says about where it came from.
	ComposeFiles []string `json:"composeFiles"`
	Git          *struct {
		Commit   string `json:"commit"`
		RepoName string `json:"repoName"`
		Ref      string `json:"ref"`
	} `json:"git"`
}

func (d gitDraftAnswer) file(path string) map[string]any {
	for _, f := range d.Files {
		if f["path"] == path {
			return f
		}
	}
	return nil
}

// gitDraftFix is a git stack whose compose file names a secret file the repository lacks, made with that overlay.
type gitDraftFix struct {
	*gitStackFix
	operator *roleCaller
	viewer   *roleCaller
	stackID  string
}

func newGitDraftFix(t *testing.T) *gitDraftFix {
	t.Helper()
	g := newOverlayFix(t)
	rec := g.create(t, g.admin, g.body(func(b map[string]any) {
		b["overlays"] = []map[string]any{{"path": "db_pw.txt", "content": gitOverlaySecret}}
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	made := answerOf[gitStackAnswer](t, rec)
	return &gitDraftFix{
		gitStackFix: g, stackID: made.StackID,
		operator: g.f.caller(t, g.e, "operator-1", "operator"), viewer: g.f.caller(t, g.e, "viewer-1", "viewer"),
	}
}

func (g *gitDraftFix) draft(t *testing.T, c *roleCaller) gitDraftAnswer {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks/"+g.stackID+"/draft", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET draft: %d %s", rec.Code, rec.Body)
	}
	return answerOf[gitDraftAnswer](t, rec)
}

// save sends the draft d with mut applied to its body.
func (g *gitDraftFix) save(t *testing.T, c *roleCaller, d gitDraftAnswer, mut func(body map[string]any)) (code int, raw string) {
	t.Helper()
	body := map[string]any{"version": d.Version, "files": d.Files, "vars": d.Vars}
	if mut != nil {
		mut(body)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := c.saveDraft(g.stackID, string(b))
	return rec.Code, rec.Body.String()
}

// readOnly asserts a save was refused as repo_file_read_only at location.
func readOnly(t *testing.T, name string, code int, raw, location string) {
	t.Helper()
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
		Errors []struct {
			Location string `json:"location"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Errorf("%s: %v: %s", name, err, raw)
		return
	}
	if code != http.StatusUnprocessableEntity || p.Type != "repo_file_read_only" || len(p.Errors) == 0 || p.Errors[0].Location != location {
		t.Errorf("%s: %d %s, want 422 repo_file_read_only at %q", name, code, raw, location)
	}
	if !strings.Contains(p.Detail, "Repository files can't be edited here. Change them in acme and pull.") {
		t.Errorf("%s: detail %q does not name the repository", name, p.Detail)
	}
}

func withFile(d gitDraftAnswer, path string, change func(f map[string]any)) []map[string]any {
	out := make([]map[string]any, 0, len(d.Files))
	for _, f := range d.Files {
		c := map[string]any{}
		for k, v := range f {
			c[k] = v
		}
		if f["path"] == path {
			change(c)
		}
		out = append(out, c)
	}
	return out
}

func TestGitDraftReadOnly(t *testing.T) {
	t.Parallel()
	g := newGitDraftFix(t)
	d := g.draft(t, g.operator)

	// A read says where each file came from.
	for path, origin := range map[string]string{"compose.yaml": "repo", "web.env": "repo", ".env": "repo", "db_pw.txt": "overlay"} {
		if f := d.file(path); f == nil || f["origin"] != origin {
			t.Errorf("%s = %v, want origin %s", path, f, origin)
		}
	}
	if f := d.file("db_pw.txt"); f["secret"] != true || f["content"] != "<hidden>" {
		t.Errorf("db_pw.txt = %v, want a hidden secret", f)
	}
	if !slices.Equal(d.ComposeFiles, []string{"compose.yaml"}) || d.Git == nil || len(d.Git.Commit) != 40 || d.Git.RepoName != "acme" || d.Git.Ref != "main" {
		t.Errorf("composeFiles = %v, git = %+v, want the compose files and where the files are from", d.ComposeFiles, d.Git)
	}

	auditBefore := len(g.f.stackAudits(t))
	code, raw := g.save(t, g.operator, d, func(b map[string]any) {
		b["files"] = withFile(d, "compose.yaml", func(f map[string]any) { f["content"] = gitOverlayCompose + "# edited\n" })
	})
	readOnly(t, "a repository file edited", code, raw, "compose.yaml")

	code, raw = g.save(t, g.operator, d, func(b map[string]any) {
		b["files"] = slices.DeleteFunc(slices.Clone(d.Files), func(f map[string]any) bool { return f["path"] == "web.env" })
	})
	readOnly(t, "a repository file removed", code, raw, "web.env")

	code, raw = g.save(t, g.operator, d, func(b map[string]any) { b["composeFiles"] = []string{"compose.yaml", "compose.prod.yaml"} })
	readOnly(t, "the compose files changed", code, raw, "composeFiles")

	// A repository path sent as a new overlay with other content is still the repository's file.
	code, raw = g.save(t, g.operator, d, func(b map[string]any) {
		b["files"] = withFile(d, ".env", func(f map[string]any) { f["content"] = "TAG=9\n"; f["origin"] = "overlay"; f["secret"] = true })
	})
	readOnly(t, "a repository file sent as an overlay", code, raw, ".env")

	if after := g.draft(t, g.operator); after.Version != d.Version {
		t.Errorf("version = %d after refused saves, want %d", after.Version, d.Version)
	}
	if n := len(g.f.stackAudits(t)); n != auditBefore {
		t.Errorf("audit rows %d -> %d: a refused save must write none", auditBefore, n)
	}

	// The same draft sent back unchanged is not a change, and the compose files may be named as they are.
	code, raw = g.save(t, g.operator, d, func(b map[string]any) { b["composeFiles"] = d.ComposeFiles })
	if code != http.StatusOK || changedOf(t, raw) {
		t.Errorf("unchanged save: %d %s, want 200 with changed false", code, raw)
	}

	// An overlay file added and a variable changed both save.
	code, raw = g.save(t, g.operator, d, func(b map[string]any) {
		b["files"] = append(slices.Clone(d.Files), map[string]any{"path": "extra.env", "content": "K=plain-text", "secret": false})
		b["vars"] = []map[string]any{{"name": "TAG", "value": "3", "secret": false}}
	})
	if code != http.StatusOK {
		t.Fatalf("overlay and variable: %d %s", code, raw)
	}
	var saved struct {
		Changed     bool     `json:"changed"`
		SecretFiles []string `json:"secretFiles"`
	}
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Changed || !slices.Contains(saved.SecretFiles, "extra.env") {
		t.Errorf("save = %+v, want a change that sealed extra.env", saved)
	}
	after := g.draft(t, g.operator)
	if f := after.file("extra.env"); f == nil || f["origin"] != "overlay" || f["secret"] != true || f["content"] != "<hidden>" {
		t.Errorf("extra.env = %v, want a hidden secret overlay", f)
	}
	if f := after.file("compose.yaml"); f["origin"] != "repo" {
		t.Errorf("compose.yaml = %v, want it still the repository's", f)
	}
	if len(after.Vars) != 1 || after.Vars[0]["value"] != "3" {
		t.Errorf("vars = %v, want TAG=3", after.Vars)
	}
	// The server stored the overlay sealed and never let the text out.
	rec := g.operator.do(http.MethodGet, "stacks/"+g.stackID+"/draft", "")
	if strings.Contains(rec.Body.String(), "plain-text") {
		t.Errorf("a read of the draft holds the overlay's content: %s", rec.Body)
	}

	// A Viewer may read and may not save.
	if rec := g.viewer.saveDraft(g.stackID, `{"version":1,"files":[],"vars":[]}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer save: %d, want 403", rec.Code)
	}
}

func TestGitDraftOverlayReplaceRemove(t *testing.T) {
	t.Parallel()
	g := newGitDraftFix(t)
	d := g.draft(t, g.operator)

	// Sending <hidden> for a sealed overlay keeps what is saved: nothing changes.
	if code, raw := g.save(t, g.operator, d, nil); code != http.StatusOK || changedOf(t, raw) {
		t.Fatalf("save as read: %d %s, want no change", code, raw)
	}

	// Replacing an overlay's content saves; the new text is not in the next read.
	const replaced = "replaced-secret-7d2a"
	code, raw := g.save(t, g.operator, d, func(b map[string]any) {
		b["files"] = withFile(d, "db_pw.txt", func(f map[string]any) { f["content"] = replaced })
	})
	if code != http.StatusOK || !changedOf(t, raw) {
		t.Fatalf("replace: %d %s, want a change", code, raw)
	}
	d2 := g.draft(t, g.operator)
	if d2.Version != d.Version+1 {
		t.Errorf("version = %d, want %d", d2.Version, d.Version+1)
	}
	if f := d2.file("db_pw.txt"); f["content"] != "<hidden>" || f["origin"] != "overlay" || f["secret"] != true {
		t.Errorf("db_pw.txt = %v, want a hidden secret overlay", f)
	}
	var sealedText string
	if err := g.f.st.Read.QueryRow(`SELECT content_sealed FROM stack_snapshot_files WHERE path = 'db_pw.txt' AND snapshot_id = (SELECT id FROM stack_snapshots WHERE stack_id = ? AND kind = 'draft')`, g.stackID).Scan(&sealedText); err != nil || sealedText == "" {
		t.Errorf("sealed content = %q, %v, want it stored sealed", sealedText, err)
	}

	// With the overlay in place the check finds nothing missing.
	checked, code, raw := g.operator.check(g.stackID, string(mustJSON(t, map[string]any{"files": d2.Files, "vars": d2.Vars})))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	for _, p := range checked.Problems {
		if p.Code == "repo_file_missing" {
			t.Errorf("problem %+v with the overlay in place", p)
		}
	}

	// Removing it saves, and the next check says the compose file's file is missing (the compose file names it).
	code, raw = g.save(t, g.operator, d2, func(b map[string]any) {
		b["files"] = slices.DeleteFunc(slices.Clone(d2.Files), func(f map[string]any) bool { return f["path"] == "db_pw.txt" })
	})
	if code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, raw)
	}
	d3 := g.draft(t, g.operator)
	if d3.file("db_pw.txt") != nil {
		t.Errorf("db_pw.txt is still in the draft: %v", d3.Files)
	}
	checked, code, raw = g.operator.check(g.stackID, string(mustJSON(t, map[string]any{"files": d3.Files, "vars": d3.Vars})))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	var missing bool
	for _, p := range checked.Problems {
		missing = missing || (p.Code == "repo_file_missing" && strings.Contains(p.Message, "db_pw.txt"))
	}
	if !missing {
		t.Errorf("problems = %+v, want repo_file_missing for db_pw.txt", checked.Problems)
	}
}

// changedOf reads the changed flag of a save's answer.
func changedOf(t *testing.T, raw string) bool {
	t.Helper()
	var a struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return a.Changed
}

const gitVarsCompose = "services:\n  web:\n    image: nginx:${TAG}\n    environment:\n      DB_PASSWORD: ${DB_PASSWORD}\n"

// The check of a git stack knows the repository's .env, so the Variables pane can say where a value comes from and
// warn about a secret committed there (D-07).
func TestGitDraftCheckSeesTheRepositoryEnv(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	g.repo.Commit(t, map[string]string{
		"stacks/web/compose.yaml": gitVarsCompose,
		"stacks/web/.env":         "TAG=1.27\nDB_PASSWORD=" + gitCheckSecret + "\n",
	}, "vars")
	rec := g.create(t, g.admin, g.body(func(b map[string]any) { b["vars"] = []map[string]any{} }))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := answerOf[gitStackAnswer](t, rec).StackID
	operator := g.f.caller(t, g.e, "operator-1", "operator")
	d := answerOf[gitDraftAnswer](t, operator.do(http.MethodGet, "stacks/"+id+"/draft", ""))

	got, code, raw := operator.check(id, string(mustJSON(t, map[string]any{"files": d.Files, "vars": d.Vars})))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	if strings.Contains(raw, gitCheckSecret) {
		t.Errorf("the secret committed in .env is in the answer: %s", raw)
	}
	var tag, pw *checkVariable
	for i := range got.Variables {
		switch got.Variables[i].Name {
		case "TAG":
			tag = &got.Variables[i]
		case "DB_PASSWORD":
			pw = &got.Variables[i]
		}
	}
	if tag == nil || tag.Source != "repo-env" || tag.Value != "1.27" {
		t.Errorf("TAG = %+v, want 1.27 from the repository's .env", tag)
	}
	if pw == nil || pw.Source != "repo-env" || pw.Value != "<hidden>" || !pw.SecretLike {
		t.Errorf("DB_PASSWORD = %+v, want a hidden, secret-like value from the repository's .env", pw)
	}
	var warned bool
	for _, p := range got.Problems {
		warned = warned || (p.Code == "env_secret_like" && p.Severity == "warning")
	}
	if !warned {
		t.Errorf("problems = %+v, want the env_secret_like warning", got.Problems)
	}

	// A value in the table beats the .env.
	withVar, code, raw := operator.check(id, string(mustJSON(t, map[string]any{
		"files": d.Files, "vars": []map[string]any{{"name": "TAG", "value": "2", "secret": false}},
	})))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	for _, v := range withVar.Variables {
		if v.Name == "TAG" && (v.Source != "table" || v.Value != "2") {
			t.Errorf("TAG = %+v, want 2 from the table", v)
		}
	}
}
