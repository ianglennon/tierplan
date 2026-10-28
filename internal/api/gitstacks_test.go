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

	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Creating a stack from a repository folder (plan 09-10, GIT-02, D-06, D-14, D-27). The router, policy, CSRF
// guard, vault and store are the real ones; the repository is a real git server and the mirror a real bare
// repository on disk.

const gitWebCompose = "services:\n  web:\n    image: nginx:1.27\n    env_file: web.env\n"

type gitStackFix struct {
	f     *envFix
	env   string
	admin credentialUser
	srv   *gittest.HTTPServer
	repo  *gittest.Repo
	sha   string
	id    string
}

func newGitStackFix(t *testing.T) *gitStackFix {
	t.Helper()
	f, e := stackEnv(t, nil)
	admin := f.asAdmin(t)
	repo := gittest.NewRepo(t)
	sha := repo.Commit(t, map[string]string{
		"stacks/web/compose.yaml":  gitWebCompose,
		"stacks/web/.env":          "TAG=1\n",
		"stacks/web/web.env":       "A=b\n",
		"stacks/web/README.md":     "# web\n",
		"stacks/other/compose.yml": "services: {}\n",
	}, "first")
	srv := gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true})
	g := &gitStackFix{f: f, env: e.ID, admin: admin, srv: srv, repo: repo, sha: sha}
	body := gitJSON(map[string]any{"name": "acme", "url": srv.URL, "caPem": srv.CAPEM})
	rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+e.ID+"/git/repos", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save the repository: %d %s", rec.Code, rec.Body)
	}
	g.id = decodeGitRepo(t, rec).ID
	return g
}

type gitStackAnswer struct {
	StackID          string   `json:"stackId"`
	DraftVersion     int64    `json:"draftVersion"`
	Commit           string   `json:"commit"`
	NotImported      []string `json:"notImported"`
	NotImportedTotal int      `json:"notImportedTotal"`
	Problems         []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
		File     string `json:"file"`
	} `json:"problems"`
}

func (g *gitStackFix) body(mut func(map[string]any)) string {
	b := map[string]any{
		"name": "web", "repoId": g.id, "refKind": "branch", "ref": "main", "folder": "stacks/web",
		"composeFiles": []string{"compose.yaml"}, "vars": []map[string]any{{"name": "TAG", "value": "2", "secret": false}},
	}
	if mut != nil {
		mut(b)
	}
	return gitJSON(b)
}

func (g *gitStackFix) create(t *testing.T, u credentialUser, body string) *httptest.ResponseRecorder {
	t.Helper()
	return g.f.credCall(t, u, http.MethodPost, "/api/env/"+g.env+"/git-stacks", body)
}

func (g *gitStackFix) stackRepo() stackops.Repo {
	return stackops.Repo{Store: g.f.st, Vault: g.f.v}
}

func TestGitStackCreate(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	ctx := context.Background()

	rec := g.create(t, g.admin, g.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var got gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.StackID == "" || got.DraftVersion != 1 || got.Commit != g.sha {
		t.Errorf("answer = %+v, want a stack, draft version 1 and the commit %s", got, g.sha)
	}
	if !slices.Equal(got.NotImported, []string{"README.md"}) || got.NotImportedTotal != 1 {
		t.Errorf("notImported = %v (%d), want README.md alone", got.NotImported, got.NotImportedTotal)
	}
	for _, p := range got.Problems {
		if p.Severity == "error" {
			t.Errorf("problem %+v blocks a fixture that is fine", p)
		}
	}

	repo := g.stackRepo()
	st, err := repo.Stack(ctx, g.env, got.StackID)
	if err != nil || st.Name != "web" || st.Source.Kind != stacks.SourceGit || st.Source.Ref != g.id {
		t.Fatalf("stack = %+v, %v, want a git stack of the repository", st, err)
	}
	// The draft holds the three repository files, the variable, the commit and the folder's tree.
	draft, err := repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range draft.Copy.Files {
		paths = append(paths, file.Path)
		if file.Origin != stacks.OriginRepo {
			t.Errorf("file %s has origin %q, want repo", file.Path, file.Origin)
		}
	}
	if !slices.Equal(paths, []string{".env", "compose.yaml", "web.env"}) {
		t.Errorf("draft files = %v, want .env, compose.yaml and web.env: README.md is never stored", paths)
	}
	if len(draft.Copy.Vars) != 1 || draft.Copy.Vars[0].Name != "TAG" || draft.Copy.Vars[0].Value != "2" {
		t.Errorf("vars = %+v", draft.Copy.Vars)
	}
	if draft.Git.Commit != g.sha || draft.Git.Tree != g.repo.Tree(t, g.sha, "stacks/web") ||
		draft.Git.Source.RepoName != "acme" || draft.Git.Source.Folder != "stacks/web" || draft.Git.Source.Ref != "main" {
		t.Errorf("git = %+v, want the commit, the folder's tree and the source", draft.Git)
	}
	if !slices.Equal(draft.Copy.ComposeFiles, []string{"compose.yaml"}) {
		t.Errorf("compose files = %v", draft.Copy.ComposeFiles)
	}
	gs, err := repo.StackGit(ctx, st.ID)
	if err != nil || !gs.PollEnabled || gs.PollIntervalSeconds != 300 || gs.AutoDeploy || gs.WebhookFlavour != "" {
		t.Errorf("stack_git = %+v, %v, want polling every 5 minutes, auto-deploy off and no webhook", gs, err)
	}
	if state, err := repo.GitState(ctx, st.ID); err != nil || state.Status != stackops.GitStatusUnknown {
		t.Errorf("state = %+v, %v, want unknown", state, err)
	}

	// Nothing was deployed: no revision, no operation, and the engine saw no write.
	if rows, _, err := repo.ListRevisions(ctx, st.ID); err != nil || len(rows) != 0 {
		t.Errorf("revisions = %v, %v, want none", rows, err)
	}
	if _, ok, err := repo.LatestDeploy(ctx, st.ID); err != nil || ok {
		t.Errorf("an operation exists for a stack that was only created: %v, %v", ok, err)
	}
	if n := len(g.f.fake.ServiceCreates()); n != 0 {
		t.Errorf("the engine was sent %d service creates", n)
	}

	// The stack is a stack of the environment: it is listed with its source and the same name cannot be taken twice.
	if rec := g.create(t, g.admin, g.body(nil)); rec.Code != http.StatusConflict || problemType(t, rec) != "stack_name_taken" {
		t.Errorf("the same name again: %d %s, want 409 stack_name_taken", rec.Code, rec.Body)
	}
	if rec := g.f.credCall(t, g.admin, http.MethodGet, "/api/env/"+g.env+"/stacks/"+st.ID, ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"source":"git"`) {
		t.Errorf("stack read: %d %s, want source git", rec.Code, rec.Body)
	}
}

func TestGitStackCreateRefusals(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	g.f.addEnvironmentRow(t, "env-other")
	foreign := decodeGitRepo(t, g.f.credCall(t, g.admin, http.MethodPost, "/api/env/env-other/git/repos",
		gitJSON(map[string]any{"name": "theirs", "url": g.srv.URL, "caPem": g.srv.CAPEM}))).ID

	cases := []struct {
		name   string
		mut    func(map[string]any)
		status int
		typ    string
	}{
		{"a name the target refuses", func(b map[string]any) { b["name"] = "-bad name" }, http.StatusUnprocessableEntity, "stack_name_invalid"},
		{"a repository that is not there", func(b map[string]any) { b["repoId"] = "nope" }, http.StatusNotFound, "repository_not_found"},
		{"another environment's repository", func(b map[string]any) { b["repoId"] = foreign }, http.StatusNotFound, "repository_not_found"},
		{"a ref git refuses", func(b map[string]any) { b["ref"] = "-x" }, http.StatusUnprocessableEntity, "ref_invalid"},
		{"a branch that is not there", func(b map[string]any) { b["ref"] = "gone" }, http.StatusUnprocessableEntity, "ref_missing"},
		{"a folder that is not there", func(b map[string]any) { b["folder"] = "stacks/nope" }, http.StatusUnprocessableEntity, "folder_missing"},
		{"a folder outside the repository", func(b map[string]any) { b["folder"] = "stacks/../.." }, http.StatusUnprocessableEntity, "folder_invalid"},
		{"a compose file the folder lacks", func(b map[string]any) { b["composeFiles"] = []string{"compose.prod.yaml"} }, http.StatusUnprocessableEntity, "compose_file_missing"},
		{"a compose path outside the folder", func(b map[string]any) { b["composeFiles"] = []string{"../other/compose.yml"} }, http.StatusUnprocessableEntity, "compose_path_invalid"},
		{"a variable name that is not one", func(b map[string]any) { b["vars"] = []map[string]any{{"name": "1BAD", "value": "x", "secret": false}} }, http.StatusUnprocessableEntity, "variable_name_invalid"},
	}
	for _, c := range cases {
		rec := g.create(t, g.admin, g.body(c.mut))
		if rec.Code != c.status || problemType(t, rec) != c.typ {
			t.Errorf("%s: %d %s, want %d %s", c.name, rec.Code, rec.Body, c.status, c.typ)
		}
	}
	var n int
	if err := g.f.st.Read.QueryRow(`SELECT (SELECT count(*) FROM stacks) + (SELECT count(*) FROM stack_git)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a refused create left %d rows, %v", n, err)
	}
}

func TestGitStackCreateNeedsTheCreatePermission(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	for role, want := range map[string]int{"viewer": http.StatusForbidden, "operator": http.StatusForbidden, "admin": http.StatusCreated} {
		u := g.f.asRoleOn(t, "u-"+role, g.env, role)
		if rec := g.create(t, u, g.body(func(b map[string]any) { b["name"] = "s-" + role })); rec.Code != want {
			t.Errorf("%s: %d %s, want %d", role, rec.Code, rec.Body, want)
		}
	}
}
