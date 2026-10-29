// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
	e     envBody
	env   string
	admin credentialUser
	srv   *gittest.HTTPServer
	repo  *gittest.Repo
	sha   string
	id    string
	// srcOnce and srcAdminC hold the Admin of the environment who changes the source of a stack (srcAdmin).
	srcOnce   sync.Once
	srcAdminC *roleCaller
}

func newGitStackFix(t *testing.T) *gitStackFix {
	t.Helper()
	return newGitStackFixWith(t, nil)
}

// newGitStackFixWith is newGitStackFix for a test that sets a field of the fixture before the router is built.
func newGitStackFixWith(t *testing.T, tune func(*envFix)) *gitStackFix {
	t.Helper()
	var f *envFix
	var e envBody
	if tune == nil {
		f, e = stackEnv(t, nil)
	} else {
		f, e = stackEnvTuned(t, tune)
	}
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
	g := &gitStackFix{f: f, e: e, env: e.ID, admin: admin, srv: srv, repo: repo, sha: sha}
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

func TestGitStackCreateSymlinkOutside(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	ctx := context.Background()
	const marker = "OUTSIDE-MARKER-77c1"
	// web.env becomes a link to a file of the same repository outside the stack folder.
	g.repo.Commit(t, map[string]string{"stacks/web/web.env": gittest.Delete, "stacks/secret/key.env": marker + "\n"}, "remove")
	g.repo.Symlink(t, "stacks/web/web.env", "../secret/key.env")
	g.repo.Commit(t, nil, "link")

	rec := g.create(t, g.admin, g.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Errorf("the answer holds the content of a file outside the folder: %s", rec.Body)
	}
	var got gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, p := range got.Problems {
		if p.Code == stacks.CodeSymlinkOutside && p.File == "web.env" && p.Severity == "error" {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("problems = %+v, want a blocking git_symlink_outside on web.env", got.Problems)
	}
	draft, err := g.stackRepo().Draft(ctx, got.StackID)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range draft.Copy.Files {
		if file.Path == "web.env" || strings.Contains(string(file.Content), marker) {
			t.Errorf("the draft holds %s, which is or holds what a link led to outside the folder", file.Path)
		}
	}
}

func TestGitStackCreateRefusalsOfSizeAndCommit(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	// A stack that names 60 files, and one whose files are 9 MiB together (each under the 1 MiB file limit).
	many := map[string]string{}
	var refs, heavyRefs []string
	for i := range 60 {
		name := fmt.Sprintf("e%02d.env", i)
		refs = append(refs, name)
		many["stacks/many/"+name] = "A=b\n"
	}
	many["stacks/many/compose.yaml"] = "services:\n  web:\n    image: nginx:1.27\n    env_file: [" + strings.Join(refs, ", ") + "]\n"
	for i := range 9 {
		name := fmt.Sprintf("h%d.env", i)
		heavyRefs = append(heavyRefs, name)
		many["stacks/heavy/"+name] = strings.Repeat("A=b\n", 262000)
	}
	many["stacks/heavy/compose.yaml"] = "services:\n  web:\n    image: nginx:1.27\n    env_file: [" + strings.Join(heavyRefs, ", ") + "]\n"
	g.repo.Commit(t, many, "big")

	cases := []struct {
		name   string
		mut    func(map[string]any)
		status int
		typ    string
	}{
		{"60 files", func(b map[string]any) { b["folder"] = "stacks/many" }, http.StatusUnprocessableEntity, "stack_too_large"},
		{"9 MiB", func(b map[string]any) { b["folder"] = "stacks/heavy" }, http.StatusUnprocessableEntity, "stack_too_large"},
		{"a commit the repository does not hold", func(b map[string]any) {
			b["refKind"], b["ref"] = "commit", strings.Repeat("ab", 20)
		}, http.StatusUnprocessableEntity, "commit_missing"},
		{"a tag that is not there", func(b map[string]any) { b["refKind"], b["ref"] = "tag", "v9" }, http.StatusUnprocessableEntity, "ref_missing"},
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

func TestGitStackCreateRefKinds(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	ctx := context.Background()
	g.repo.Tag(t, "v1", g.sha, true)

	for _, c := range []struct {
		name, kind, ref, wantKind, wantRef string
	}{
		{"by-branch", "branch", "main", "branch", "main"},
		{"by-tag", "tag", "v1", "tag", "v1"},
		{"by-prefix", "commit", g.sha[:7], "commit", g.sha},
		{"by-full-sha", "commit", g.sha, "commit", g.sha},
	} {
		rec := g.create(t, g.admin, g.body(func(b map[string]any) { b["name"], b["refKind"], b["ref"] = c.name, c.kind, c.ref }))
		if rec.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
			continue
		}
		var got gitStackAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		gs, err := g.stackRepo().StackGit(ctx, got.StackID)
		if err != nil || gs.RefKind != c.wantKind || gs.Ref != c.wantRef || got.Commit != g.sha {
			t.Errorf("%s: stack_git = %+v, %v, commit %s, want %s %s at %s", c.name, gs, err, got.Commit, c.wantKind, c.wantRef, g.sha)
		}
		draft, err := g.stackRepo().Draft(ctx, got.StackID)
		if err != nil || draft.Git.Source.RefKind != c.wantKind || draft.Git.Source.Ref != c.wantRef {
			t.Errorf("%s: draft source = %+v, %v", c.name, draft.Git.Source, err)
		}
	}
}

func TestGitStackCreateDuplicate(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	if rec := g.create(t, g.admin, g.body(nil)); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	count := func() (n int) {
		if err := g.f.st.Read.QueryRow(`SELECT (SELECT count(*) FROM stacks) * 100 + (SELECT count(*) FROM stack_git) * 10 + (SELECT count(*) FROM stack_git_state)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	for range 2 {
		rec := g.create(t, g.admin, g.body(nil))
		if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_name_taken" {
			t.Errorf("a name already used: %d %s, want 409 stack_name_taken", rec.Code, rec.Body)
		}
	}
	if after := count(); after != before {
		t.Errorf("a refused create changed the rows from %d to %d", before, after)
	}
}

func TestGitStackCreateAudited(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	rec := g.create(t, g.admin, g.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var got gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	rows := g.f.auditRows(t, "stack.created_from_git")
	if len(rows) != 1 {
		t.Fatalf("%d audit rows, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.EnvID != g.env || r.Result != "success" || r.TargetID != got.StackID {
		t.Errorf("row = %+v, want a success on the new stack in the environment", r)
	}
	for k, want := range map[string]string{"stack": "web", "repository": "acme", "ref": "main", "folder": "stacks/web"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q", k, r.Details[k], want)
		}
	}
	// A refused create is not a creation.
	if rec := g.create(t, g.admin, g.body(nil)); rec.Code != http.StatusConflict {
		t.Fatalf("again: %d", rec.Code)
	}
	if rows := g.f.auditRows(t, "stack.created_from_git"); len(rows) != 1 {
		t.Errorf("%d audit rows after a refused create, want still 1", len(rows))
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

type gitRepoListResp struct {
	Items []struct {
		ID     string `json:"id"`
		UsedBy int    `json:"usedBy"`
	} `json:"items"`
}

func TestGitRepoInUseByStacks(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	for _, name := range []string{"one", "two"} {
		if rec := g.create(t, g.admin, g.body(func(b map[string]any) { b["name"] = name })); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// The repository says how many stacks use it.
	rec := g.f.credCall(t, g.admin, http.MethodGet, "/api/env/"+g.env+"/git/repos/"+g.id, "")
	if rec.Code != http.StatusOK || decodeGitRepo(t, rec).UsedBy != 2 {
		t.Errorf("read: %d %s, want usedBy 2", rec.Code, rec.Body)
	}
	rec = g.f.credCall(t, g.admin, http.MethodGet, "/api/git/repos", "")
	var list gitRepoListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(list.Items) != 1 || list.Items[0].UsedBy != 2 {
		t.Errorf("list: %d %s, want usedBy 2", rec.Code, rec.Body)
	}

	// Deleting it is 409 repo_in_use with the count, and the repository stays.
	rec = g.f.credCall(t, g.admin, http.MethodDelete, "/api/env/"+g.env+"/git/repos/"+g.id, "")
	var p browseProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || p.Type != "repo_in_use" || len(p.Errors) != 1 || p.Errors[0].Location != "conflict.count" || p.Errors[0].Message != "2" {
		t.Errorf("delete: %d %s, want 409 repo_in_use with a count of 2", rec.Code, rec.Body)
	}
	if rec := g.f.credCall(t, g.admin, http.MethodGet, "/api/env/"+g.env+"/git/repos/"+g.id, ""); rec.Code != http.StatusOK {
		t.Errorf("the repository is gone after a refused delete: %d", rec.Code)
	}

	// Removing a stack frees one; removing both lets the repository go.
	for _, st := range g.f.stackIDs(t) {
		if _, err := g.f.st.Write.Exec(`DELETE FROM stacks WHERE id = ?`, st); err != nil {
			t.Fatal(err)
		}
	}
	if rec := g.f.credCall(t, g.admin, http.MethodDelete, "/api/env/"+g.env+"/git/repos/"+g.id, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete after the stacks went: %d %s, want 204", rec.Code, rec.Body)
	}
}

func (f *envFix) stackIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT id FROM stacks`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func TestEnvDeleteWithGitStack(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	ctx := context.Background()
	// An environment of its own holds a repository and a stack that uses it, and a stack that uses the instance's.
	g.f.addEnvironmentRow(t, "env-del")
	own := decodeGitRepo(t, g.f.credCall(t, g.admin, http.MethodPost, "/api/env/env-del/git/repos",
		gitJSON(map[string]any{"name": "own", "url": g.srv.URL, "caPem": g.srv.CAPEM}))).ID
	instance := decodeGitRepo(t, g.f.credCall(t, g.admin, http.MethodPost, "/api/git/repos",
		gitJSON(map[string]any{"name": "shared", "url": g.srv.URL, "caPem": g.srv.CAPEM}))).ID
	repo := g.stackRepo()
	for i, repoID := range []string{own, instance} {
		in := stackops.CreateGitStackInput{
			EnvID: "env-del", Name: []string{"a", "b"}[i], Target: stacks.TargetStandalone, Actor: "ada",
			Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: "compose.yaml", Content: []byte("services: {}\n"), Origin: stacks.OriginRepo}}, ComposeFiles: []string{"compose.yaml"}},
			Rev: stackops.GitRevision{Commit: g.sha, Tree: "t", Source: stackops.GitSource{
				RepoID: repoID, RepoName: "r", RefKind: "branch", Ref: "main", ComposeFiles: []string{"compose.yaml"},
			}},
		}
		if _, _, err := repo.CreateGitStack(ctx, in); err != nil {
			t.Fatalf("CreateGitStack: %v", err)
		}
	}

	if rec := g.f.credCall(t, g.admin, http.MethodDelete, "/api/environments/env-del", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove the environment: %d %s, want 204", rec.Code, rec.Body)
	}
	var n int
	for _, table := range []string{"stacks", "stack_git", "stack_git_state"} {
		if err := g.f.st.Read.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE ` + map[string]string{
			"stacks": "env_id = 'env-del'", "stack_git": "1 = 1", "stack_git_state": "1 = 1"}[table]).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s holds %d rows after the environment went, %v", table, n, err)
		}
	}
	// Its repository went with it and the instance's is still there, no longer in use.
	if rec := g.f.credCall(t, g.admin, http.MethodGet, "/api/git/repos", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), own) || !strings.Contains(rec.Body.String(), instance) {
		t.Errorf("repositories after the removal: %d %s", rec.Code, rec.Body)
	}
	if rec := g.f.credCall(t, g.admin, http.MethodDelete, "/api/git/repos/"+instance, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete the instance repository: %d %s, want 204: no stack uses it any more", rec.Code, rec.Body)
	}
}

// CR-C02: what runs can't be copied into the draft of a stack that deploys from git: its files are the repository's and
// read-only, so the change belongs in the repository. The draft is left as it was.
func TestAcceptLiveIsRefusedForAGitStack(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	rec := g.create(t, g.admin, g.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var made gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil {
		t.Fatal(err)
	}

	rec = g.f.credCall(t, g.admin, http.MethodPost, "/api/env/"+g.env+"/stacks/"+made.StackID+"/drift/accept", `{"draftVersion":1}`)

	if rec.Code != http.StatusConflict || problemType(t, rec) != "git_stack" || !strings.Contains(rec.Body.String(), "repository") {
		t.Fatalf("accept on a git stack: %d %s, want 409 git_stack pointing at the repository", rec.Code, rec.Body)
	}
	draft, err := g.stackRepo().Draft(context.Background(), made.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Version != made.DraftVersion || draft.Git.Commit != g.sha {
		t.Errorf("draft = version %d at %q, want it untouched at version %d", draft.Version, draft.Git.Commit, made.DraftVersion)
	}
}
