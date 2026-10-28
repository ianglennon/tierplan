// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// Checking a repository folder before a stack exists (plan 09-12, GIT-08, D-09). The router, policy, CSRF guard,
// vault and store are the real ones; the repository is a real git server and the mirror a real bare repository.

const gitCheckCompose = `services:
  web:
    image: nginx:1.27
    environment:
      FROM_TABLE: ${FROM_TABLE}
      FROM_ENV: ${FROM_ENV}
      WITH_DEFAULT: ${WITH_DEFAULT:-dflt}
      NEEDED: ${NEEDED}
      DB_PASSWORD: ${DB_PASSWORD}
  worker:
    image: busybox:1.36
`

const gitCheckSecret = "committed-secret-9f3a"

type gitCheckVar struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	Source     string `json:"source"`
	Required   bool   `json:"required"`
	Secret     bool   `json:"secret"`
	SecretLike bool   `json:"secretLike"`
	Unused     bool   `json:"unused"`
}

type gitCheckAnswer struct {
	Commit struct {
		SHA  string `json:"sha"`
		When string `json:"when"`
	} `json:"commit"`
	Services []string `json:"services"`
	Problems []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		File     string `json:"file"`
		Line     int    `json:"line"`
	} `json:"problems"`
	Vars             []gitCheckVar `json:"vars"`
	Missing          []string      `json:"missing"`
	NotImported      []string      `json:"notImported"`
	NotImportedTotal int           `json:"notImportedTotal"`
}

func (a gitCheckAnswer) variable(t *testing.T, name string) gitCheckVar {
	t.Helper()
	for _, v := range a.Vars {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no variable %s in %+v", name, a.Vars)
	return gitCheckVar{}
}

// checkBody is a check of the fixture's stacks/web folder; mut changes it.
func (g *gitStackFix) checkBody(mut func(map[string]any)) string {
	b := map[string]any{
		"repoId": g.id, "refKind": "branch", "ref": "main", "folder": "stacks/web", "composeFiles": []string{"compose.yaml"},
		"vars": []map[string]any{{"name": "FROM_TABLE", "value": "from-table", "secret": false}},
	}
	if mut != nil {
		mut(b)
	}
	return gitJSON(b)
}

func (g *gitStackFix) check(t *testing.T, u credentialUser, body string) *httptest.ResponseRecorder {
	t.Helper()
	return g.f.credCall(t, u, http.MethodPost, "/api/env/"+g.env+"/git-stacks/check", body)
}

func (g *gitStackFix) checkOK(t *testing.T, body string) (gitCheckAnswer, string) {
	t.Helper()
	rec := g.check(t, g.admin, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var got gitCheckAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got, rec.Body.String()
}

func (g *gitStackFix) rows(t *testing.T) (n int) {
	t.Helper()
	if err := g.f.st.Read.QueryRow(`SELECT (SELECT count(*) FROM stacks) + (SELECT count(*) FROM stack_snapshots) +
		(SELECT count(*) FROM stack_snapshot_files) + (SELECT count(*) FROM stack_git)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGitStackCheckRoute(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	g.repo.Commit(t, map[string]string{
		"stacks/web/compose.yaml": gitCheckCompose,
		"stacks/web/.env":         "FROM_TABLE=from-env-file\nFROM_ENV=from-the-env\nDB_PASSWORD=" + gitCheckSecret + "\n",
	}, "vars")

	got, raw := g.checkOK(t, g.checkBody(nil))
	if len(got.Commit.SHA) != 40 || got.Commit.When == "" {
		t.Errorf("commit = %+v, want a full ID with its time", got.Commit)
	}
	if !slices.Equal(got.Services, []string{"web", "worker"}) {
		t.Errorf("services = %v", got.Services)
	}
	for name, want := range map[string]struct {
		source, value string
		required      bool
	}{
		"FROM_TABLE":   {"table", "from-table", false},
		"FROM_ENV":     {"repo-env", "from-the-env", false},
		"WITH_DEFAULT": {"default", "dflt", false},
		"NEEDED":       {"unset", "", true},
	} {
		if v := got.variable(t, name); v.Source != want.source || v.Value != want.value || v.Required != want.required {
			t.Errorf("%s = %+v, want %+v", name, v, want)
		}
	}
	if v := got.variable(t, "DB_PASSWORD"); v.Value != "<hidden>" || !v.SecretLike || v.Source != "repo-env" {
		t.Errorf("DB_PASSWORD = %+v, want a hidden value from the repository's .env", v)
	}
	if strings.Contains(raw, gitCheckSecret) {
		t.Errorf("the secret committed in .env is in the answer: %s", raw)
	}
	var blocked, warned bool
	for _, p := range got.Problems {
		blocked = blocked || (p.Code == "unset_variable" && p.Severity == "error" && strings.HasPrefix(p.Message, "NEEDED "))
		warned = warned || (p.Code == "env_secret_like" && p.Severity == "warning")
	}
	if !blocked || !warned {
		t.Errorf("problems = %+v, want NEEDED blocking and DB_PASSWORD warned", got.Problems)
	}
	if got.Missing == nil || got.NotImported == nil {
		t.Errorf("a list is null: %s", raw)
	}

	// Nothing was saved: no stack, draft, revision or file, and the engine saw no write.
	if n := g.rows(t); n != 0 {
		t.Errorf("a check left %d rows", n)
	}
	if n := len(g.f.fake.ServiceCreates()); n != 0 {
		t.Errorf("the engine was sent %d service creates", n)
	}

	// Only stacks.create may check.
	for role, want := range map[string]int{"viewer": http.StatusForbidden, "operator": http.StatusForbidden, "admin": http.StatusOK} {
		u := g.f.asRoleOn(t, "u-"+role, g.env, role)
		if rec := g.check(t, u, g.checkBody(nil)); rec.Code != want {
			t.Errorf("%s: %d %s, want %d", role, rec.Code, rec.Body, want)
		}
	}
	// The repository layer rule holds here as for a create.
	if rec := g.check(t, g.admin, g.checkBody(func(b map[string]any) { b["repoId"] = "nope" })); rec.Code != http.StatusNotFound || problemType(t, rec) != "repository_not_found" {
		t.Errorf("an unknown repository: %d %s", rec.Code, rec.Body)
	}
}

const gitOverlayCompose = `services:
  web:
    image: nginx:1.27
    secrets: [db_pw]
    env_file: web.env
secrets:
  db_pw:
    file: ./db_pw.txt
`

const gitOverlaySecret = "overlay-secret-4c1e"

// newOverlayFix serves a stack whose compose file names a secret file the repository does not hold.
func newOverlayFix(t *testing.T) *gitStackFix {
	t.Helper()
	g := newGitStackFix(t)
	g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitOverlayCompose}, "overlay stack")
	return g
}

func (a gitCheckAnswer) hasProblem(code, file string) bool {
	for _, p := range a.Problems {
		if p.Code == code && (file == "" || p.File == file || strings.Contains(p.Message, file)) {
			return true
		}
	}
	return false
}

func TestGitStackCreateOverlays(t *testing.T) {
	t.Parallel()
	g := newOverlayFix(t)
	ctx := context.Background()

	// Without the file the check names it, and the problem offers the overlay.
	got, _ := g.checkOK(t, g.checkBody(nil))
	if !slices.Equal(got.Missing, []string{"db_pw.txt"}) || !got.hasProblem("repo_file_missing", "db_pw.txt") {
		t.Errorf("missing = %v, problems = %+v, want db_pw.txt missing", got.Missing, got.Problems)
	}
	// With its path sent the check lists nothing missing: the contents are not needed to know that.
	got, _ = g.checkOK(t, g.checkBody(func(b map[string]any) { b["overlayPaths"] = []string{"db_pw.txt"} }))
	if len(got.Missing) != 0 || got.hasProblem("repo_file_missing", "") || got.hasProblem("overlay_clash", "") {
		t.Errorf("missing = %v, problems = %+v, want the overlay to fill db_pw.txt", got.Missing, got.Problems)
	}

	rec := g.create(t, g.admin, g.body(func(b map[string]any) {
		b["overlays"] = []map[string]any{{"path": "db_pw.txt", "content": gitOverlaySecret}}
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), gitOverlaySecret) {
		t.Errorf("the answer of the create holds the overlay's content: %s", rec.Body)
	}
	var created gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	for _, p := range created.Problems {
		if p.Code == "repo_file_missing" || p.Code == "overlay_clash" {
			t.Errorf("problem %+v after the overlay was added", p)
		}
	}

	// The overlay is a secret file with origin overlay, sealed at rest, and a read of the draft hides it.
	draft, err := g.stackRepo().Draft(ctx, created.StackID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range draft.Copy.Files {
		if f.Path != "db_pw.txt" {
			continue
		}
		found = true
		if f.Origin != stacks.OriginOverlay || !f.Secret || string(f.Content) != gitOverlaySecret {
			t.Errorf("db_pw.txt = %+v, want a secret file of origin overlay holding what was sent", f)
		}
	}
	if !found {
		t.Errorf("the draft has no db_pw.txt: %+v", draft.Copy.Files)
	}
	var plain, sealed, origin string
	if err := g.f.st.Read.QueryRow(`SELECT content, content_sealed, origin FROM stack_snapshot_files WHERE path = 'db_pw.txt'`).Scan(&plain, &sealed, &origin); err != nil {
		t.Fatal(err)
	}
	if plain != "" || sealed == "" || strings.Contains(sealed, gitOverlaySecret) || origin != "overlay" {
		t.Errorf("stored plain %q sealed %q origin %q, want an empty plain column, a sealed envelope and origin overlay", plain, sealed, origin)
	}
	read := g.f.credCall(t, g.admin, http.MethodGet, "/api/env/"+g.env+"/stacks/"+created.StackID+"/draft", "")
	if read.Code != http.StatusOK || strings.Contains(read.Body.String(), gitOverlaySecret) || !strings.Contains(read.Body.String(), "<hidden>") {
		t.Errorf("draft read: %d %s, want the overlay hidden", read.Code, read.Body)
	}

	// The rules of a file hold for an overlay, and a refusal creates nothing.
	var before int
	if err := g.f.st.Read.QueryRow(`SELECT count(*) FROM stacks`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		overlay map[string]any
		typ     string
	}{
		{"a path outside the folder", map[string]any{"path": "../x", "content": "x"}, "file_path_invalid"},
		{"an absolute path", map[string]any{"path": "/etc/x", "content": "x"}, "file_path_invalid"},
		{"over 1 MiB", map[string]any{"path": "big.txt", "content": strings.Repeat("a", 1<<20+1)}, "file_too_large"},
	} {
		rec := g.create(t, g.admin, g.body(func(b map[string]any) { b["name"] = "other"; b["overlays"] = []map[string]any{c.overlay} }))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != c.typ {
			t.Errorf("%s: %d %.200s, want 422 %s", c.name, rec.Code, rec.Body, c.typ)
		}
	}
	rec = g.check(t, g.admin, g.checkBody(func(b map[string]any) { b["overlayPaths"] = []string{"../x"} }))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "file_path_invalid" {
		t.Errorf("check with a bad overlay path: %d %s", rec.Code, rec.Body)
	}
	var after int
	if err := g.f.st.Read.QueryRow(`SELECT count(*) FROM stacks`).Scan(&after); err != nil || after != before {
		t.Errorf("a refused create changed the stacks from %d to %d, %v", before, after, err)
	}
}

func TestGitStackOverlayClash(t *testing.T) {
	t.Parallel()
	g := newOverlayFix(t)
	ctx := context.Background()
	// web.env is in the repository and referenced; README.md is in it and not referenced. Both are paths of the repository.
	for _, path := range []string{"web.env", "README.md"} {
		got, _ := g.checkOK(t, g.checkBody(func(b map[string]any) { b["overlayPaths"] = []string{"db_pw.txt", path} }))
		var blocking bool
		for _, p := range got.Problems {
			blocking = blocking || (p.Code == "overlay_clash" && p.Severity == "error" && p.File == path)
		}
		if !blocking {
			t.Errorf("%s: problems = %+v, want a blocking overlay_clash", path, got.Problems)
		}
		if len(got.Missing) != 0 {
			t.Errorf("%s: missing = %v, want none", path, got.Missing)
		}

		name := "clash-" + strings.ReplaceAll(strings.ToLower(path), ".", "-")
		rec := g.create(t, g.admin, g.body(func(b map[string]any) {
			b["name"] = name
			b["overlays"] = []map[string]any{{"path": "db_pw.txt", "content": "pw"}, {"path": path, "content": "shadow"}}
		}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: create: %d %s, want 201: a draft saves with its problems", path, rec.Code, rec.Body)
		}
		var created gitStackAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		var clash bool
		for _, p := range created.Problems {
			clash = clash || (p.Code == "overlay_clash" && p.Severity == "error" && p.File == path)
		}
		if !clash {
			t.Errorf("%s: problems = %+v, want the blocking overlay_clash", path, created.Problems)
		}
		// The repository's file stands; the overlay never shadows it.
		draft, err := g.stackRepo().Draft(ctx, created.StackID)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range draft.Copy.Files {
			if f.Path == path && (f.Origin != stacks.OriginRepo || string(f.Content) == "shadow") {
				t.Errorf("%s = %+v, want the repository's file", path, f)
			}
			if f.Path == "db_pw.txt" && f.Origin != stacks.OriginOverlay {
				t.Errorf("db_pw.txt = %+v, want the overlay that fills a missing path", f)
			}
		}
	}
}
