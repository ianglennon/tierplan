// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/gitx/gittest"
)

// Browsing a repository to pick a stack's folder (plan 09-09, GIT-02, D-06, D-12). The router, policy,
// CSRF guard, vault and store are the real ones; the repository is a real git server and the
// mirror a real bare repository on disk.

type browseRefsResp struct {
	DefaultBranch string `json:"defaultBranch"`
	Branches      []struct {
		Name   string `json:"name"`
		Commit string `json:"commit"`
	} `json:"branches"`
	Tags []struct {
		Name   string `json:"name"`
		Commit string `json:"commit"`
	} `json:"tags"`
}

type browseTreeResp struct {
	Commit      string `json:"commit"`
	CommittedAt string `json:"committedAt"`
	Path        string `json:"path"`
	Entries     []struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		HasCompose bool   `json:"hasCompose"`
	} `json:"entries"`
	Total  int  `json:"total"`
	Capped bool `json:"capped"`
}

// browseProblem is the problem a failed browse answers with.
type browseProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func decodeBrowseProblem(t *testing.T, rec *httptest.ResponseRecorder) browseProblem {
	t.Helper()
	var p browseProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return p
}

// browseRepoFix is a repository with folders, a tag and a second branch, served over TLS.
type browseRepoFix struct {
	srv  *gittest.HTTPServer
	repo *gittest.Repo
	sha  string
}

func newBrowseRepoFix(t *testing.T) *browseRepoFix {
	t.Helper()
	repo := gittest.NewRepo(t)
	repo.Symlink(t, "link", "stacks/a")
	repo.Submodule(t, "lib")
	first := repo.Commit(t, map[string]string{
		"compose.yaml":         "services: {}\n",
		"stacks/a/compose.yml": "services: {}\n",
		"stacks/b/notes.txt":   "not a stack\n",
	}, "first")
	repo.Tag(t, "v1", first, true)
	repo.Branch(t, "dev", first)
	return &browseRepoFix{srv: gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true}), repo: repo, sha: first}
}

func (b *browseRepoFix) save(t *testing.T, f *envFix, u credentialUser, path, name string) string {
	t.Helper()
	body := gitJSON(map[string]any{"name": name, "url": b.srv.URL, "caPem": b.srv.CAPEM})
	rec := f.credCall(t, u, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save %s: %d %s", name, rec.Code, rec.Body)
	}
	return decodeGitRepo(t, rec).ID
}

func treeURL(env, repoID, kind, ref, path string) string {
	q := url.Values{"refKind": {kind}, "ref": {ref}}
	if path != "" {
		q.Set("path", path)
	}
	return "/api/env/" + env + "/git/repos/" + repoID + "/tree?" + q.Encode()
}

func TestGitBrowse(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	env := f.add(t).ID
	f.addEnvironmentRow(t, "env-other")
	admin := f.asAdmin(t)
	b := newBrowseRepoFix(t)
	own := b.save(t, f, admin, "/api/env/"+env+"/git/repos", "own")
	instance := b.save(t, f, admin, "/api/git/repos", "shared")
	foreign := b.save(t, f, admin, "/api/env/env-other/git/repos", "foreign")

	// The refs: every branch and tag with its commit (the annotated tag peeled) and the default branch.
	rec := f.credCall(t, admin, http.MethodGet, "/api/env/"+env+"/git/repos/"+own+"/refs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refs: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("refs Cache-Control = %q, want no-store", cc)
	}
	var refs browseRefsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &refs); err != nil {
		t.Fatal(err)
	}
	if refs.DefaultBranch != "main" || len(refs.Branches) != 2 || refs.Branches[0].Name != "dev" || refs.Branches[1].Name != "main" ||
		refs.Branches[1].Commit != b.sha || len(refs.Tags) != 1 || refs.Tags[0].Name != "v1" || refs.Tags[0].Commit != b.sha {
		t.Errorf("refs = %+v, want branches dev and main, tag v1 and the default main, all at %s", refs, b.sha)
	}

	// The root of main.
	rec = f.credCall(t, admin, http.MethodGet, treeURL(env, own, "branch", "main", ""), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tree: %d %s", rec.Code, rec.Body)
	}
	var tree browseTreeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range tree.Entries {
		names = append(names, e.Kind+":"+e.Name)
	}
	if tree.Commit != b.sha || tree.CommittedAt != "2026-01-01T00:00:00Z" || tree.Path != "" || tree.Total != 4 || tree.Capped ||
		strings.Join(names, " ") != "dir:stacks file:compose.yaml submodule:lib symlink:link" {
		t.Errorf("root = %+v (%v), want commit %s and dir:stacks file:compose.yaml submodule:lib symlink:link", tree, names, b.sha)
	}

	// A folder, with the hint for the one that holds a compose file.
	rec = f.credCall(t, admin, http.MethodGet, treeURL(env, own, "branch", "main", "stacks"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tree stacks: %d %s", rec.Code, rec.Body)
	}
	tree = browseTreeResp{}
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	if tree.Path != "stacks" || len(tree.Entries) != 2 || tree.Entries[0].Name != "a" || !tree.Entries[0].HasCompose || tree.Entries[1].Name != "b" || tree.Entries[1].HasCompose {
		t.Errorf("stacks = %+v, want a (with a compose file) and b (without)", tree)
	}

	// An instance repository is usable in every environment.
	if rec := f.credCall(t, admin, http.MethodGet, treeURL(env, instance, "branch", "main", ""), ""); rec.Code != http.StatusOK {
		t.Errorf("instance repository: %d %s, want 200", rec.Code, rec.Body)
	}

	// Another environment's repository is not found, on both routes, exactly as one that does not exist.
	for _, path := range []string{treeURL(env, foreign, "branch", "main", ""), "/api/env/" + env + "/git/repos/" + foreign + "/refs", treeURL(env, "no-such-repo", "branch", "main", "")} {
		rec := f.credCall(t, admin, http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound || decodeBrowseProblem(t, rec).Type != "repository_not_found" {
			t.Errorf("%s: %d %s, want 404 repository_not_found", path, rec.Code, rec.Body)
		}
	}

	// A viewer or an operator cannot browse (stacks.create); an environment admin can, and cannot where
	// they hold no role.
	for name, u := range map[string]credentialUser{
		"viewer":   f.asRoleOn(t, "viewer", env, "viewer"),
		"operator": f.asRoleOn(t, "operator", env, "operator"),
	} {
		for _, path := range []string{treeURL(env, own, "branch", "main", ""), "/api/env/" + env + "/git/repos/" + own + "/refs"} {
			if rec := f.credCall(t, u, http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
				t.Errorf("%s on %s: %d %s, want 403", name, path, rec.Code, rec.Body)
			}
		}
	}
	envAdmin := f.asRoleOn(t, "envadmin", env, "admin")
	if rec := f.credCall(t, envAdmin, http.MethodGet, treeURL(env, own, "branch", "main", ""), ""); rec.Code != http.StatusOK {
		t.Errorf("environment admin: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestGitBrowseTagsCommitsAndMissingThings(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	env := f.add(t).ID
	admin := f.asAdmin(t)
	b := newBrowseRepoFix(t)
	id := b.save(t, f, admin, "/api/env/"+env+"/git/repos", "own")

	get := func(kind, ref, path string) *httptest.ResponseRecorder {
		return f.credCall(t, admin, http.MethodGet, treeURL(env, id, kind, ref, path), "")
	}
	// An annotated tag, a commit by prefix and by its full ID all answer the commit's folder.
	for _, tc := range []struct{ kind, ref string }{{"tag", "v1"}, {"commit", b.sha[:7]}, {"commit", b.sha}} {
		rec := get(tc.kind, tc.ref, "stacks")
		var tree browseTreeResp
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tree) != nil || tree.Commit != b.sha || len(tree.Entries) != 2 {
			t.Errorf("%s %s: %d %s, want the stacks folder of %s", tc.kind, tc.ref, rec.Code, rec.Body, b.sha)
		}
	}

	// Every way something can be missing is a 422 with its own type, worded for a person.
	for _, tc := range []struct {
		name, kind, ref, path, typ, detail string
	}{
		{"branch", "branch", "gone", "", "ref_missing", "Branch gone isn't in own."},
		{"tag", "tag", "gone", "", "ref_missing", "Tag gone isn't in own."},
		{"commit", "commit", "deadbee", "", "commit_missing", "Commit deadbee isn't in own."},
		{"folder", "branch", "main", "nope", "folder_missing", "There is no folder nope at this commit."},
		{"file", "branch", "main", "compose.yaml", "folder_missing", "There is no folder compose.yaml at this commit."},
	} {
		rec := get(tc.kind, tc.ref, tc.path)
		p := decodeBrowseProblem(t, rec)
		if rec.Code != http.StatusUnprocessableEntity || p.Type != tc.typ || p.Detail != tc.detail {
			t.Errorf("%s: %d %s, want 422 %s %q", tc.name, rec.Code, rec.Body, tc.typ, tc.detail)
		}
	}
}

func TestGitBrowseRefInvalidNeverReachesTheServer(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	env := f.add(t).ID
	admin := f.asAdmin(t)
	b := newBrowseRepoFix(t)
	id := b.save(t, f, admin, "/api/env/"+env+"/git/repos", "own")
	before := b.srv.Requests()

	for _, tc := range []struct{ kind, ref string }{
		{"branch", "a..b"}, {"branch", "-x"}, {"tag", "a\nb"}, {"branch", strings.Repeat("a", 300)}, {"commit", "abc"}, {"commit", "main"},
	} {
		rec := f.credCall(t, admin, http.MethodGet, treeURL(env, id, tc.kind, tc.ref, ""), "")
		if p := decodeBrowseProblem(t, rec); rec.Code != http.StatusUnprocessableEntity || p.Type != "ref_invalid" {
			t.Errorf("%s %q: %d %s, want 422 ref_invalid", tc.kind, tc.ref, rec.Code, rec.Body)
		}
	}
	if n := b.srv.Requests() - before; n != 0 {
		t.Errorf("the git server saw %d requests for refused names, want none", n)
	}
}
