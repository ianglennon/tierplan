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

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
)

// Changing what a git stack follows (plan 09-29, GIT-02, GIT-04, D-11, D-12): the change writes the draft as a pending
// source and applies nothing; the review names the source it replaces; a good deploy makes it the stack's source.

type sourceAnswer struct {
	DraftVersion int64  `json:"draftVersion"`
	Commit       string `json:"commit"`
	Ref          string `json:"ref"`
}

// sourceReviewAnswer is the git part of a review that matters to a change of source.
type sourceReviewAnswer struct {
	Git *struct {
		Commit        string `json:"commit"`
		Ref           string `json:"ref"`
		RefKind       string `json:"refKind"`
		Folder        string `json:"folder"`
		RepoName      string `json:"repoName"`
		SourceChanged bool   `json:"sourceChanged"`
		HeadCommit    string `json:"headCommit"`
		Replaces      *struct {
			RepoName     string   `json:"repoName"`
			RefKind      string   `json:"refKind"`
			Ref          string   `json:"ref"`
			Folder       string   `json:"folder"`
			ComposeFiles []string `json:"composeFiles"`
		} `json:"replaces"`
	} `json:"git"`
}

// srcAdmin is the Admin of the environment, who alone may point a stack at another repository (WR-D06).
func (g *gitStackFix) srcAdmin(t *testing.T) *roleCaller {
	t.Helper()
	g.srcOnce.Do(func() { g.srcAdminC = g.f.caller(t, g.e, "source-admin", "admin") })
	return g.srcAdminC
}

func (c *roleCaller) changeSource(stackID string, version int64, mut func(map[string]any)) *httptest.ResponseRecorder {
	c.t.Helper()
	b := map[string]any{
		"draftVersion": version, "repoId": "", "refKind": "branch", "ref": "main", "folder": "stacks/web", "composeFiles": []string{"compose.yaml"},
	}
	if mut != nil {
		mut(b)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(http.MethodPost, "stacks/"+stackID+"/git/source", string(raw))
}

// sourceOf changes the source of the stack to the repository of the fixture with mut applied.
func (g *gitStackFix) sourceOf(c *roleCaller, stackID string, version int64, mut func(map[string]any)) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.changeSource(stackID, version, func(b map[string]any) {
		b["repoId"] = g.id
		if mut != nil {
			mut(b)
		}
	})
}

func (c *roleCaller) sourceReview(stackID string) sourceReviewAnswer {
	c.t.Helper()
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	return answerOf[sourceReviewAnswer](c.t, rec)
}

// devBranch commits a newer web image and makes it the branch dev, leaving main where it was.
func (g *gitStackFix) devBranch(t *testing.T, mainSHA string) string {
	t.Helper()
	dev := g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.29")}, "dev")
	g.repo.Branch(t, "dev", dev)
	g.repo.Branch(t, "main", mainSHA)
	return dev
}

func TestChangeSourceThroughReview(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	repo := g.stackRepo()
	deployed, _, err := repo.DeployedGit(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}
	dev := g.devBranch(t, deployed.Commit)

	version := draftVersion(t, operator, stackID)
	rec := g.sourceOf(g.srcAdmin(t), stackID, version, func(b map[string]any) { b["ref"] = "dev" })
	if rec.Code != http.StatusOK {
		t.Fatalf("change source: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	got := answerOf[sourceAnswer](t, rec)
	if got.DraftVersion != version+1 || got.Commit != dev || got.Ref != "dev" {
		t.Fatalf("answer = %+v, want version %d at %s on dev", got, version+1, dev)
	}

	// The draft is pending; the stack's own settings and sync state are not changed.
	if st := g.readGit(t, g.admin, stackID); st.Ref != "main" || st.Status != "in_sync" || st.DeployedSha != deployed.Commit {
		t.Fatalf("git after the change = %+v, want main, in sync, and nothing deployed from dev", st)
	}
	if revs, _, err := repo.ListRevisions(context.Background(), stackID); err != nil || len(revs) != 1 {
		t.Fatalf("revisions = %d, %v, want only the first deploy", len(revs), err)
	}
	if n := len(g.f.fake.Writes()); n == 0 {
		t.Fatal("the first deploy wrote nothing to the engine; the fixture is not what the test thinks")
	}
	writes := len(g.f.fake.Writes())

	// The review names the source it replaces.
	rv := operator.sourceReview(stackID)
	if rv.Git == nil || rv.Git.Replaces == nil || rv.Git.Ref != "dev" || rv.Git.Commit != dev || rv.Git.HeadCommit != dev || !rv.Git.SourceChanged {
		t.Fatalf("review git = %+v, want dev at %s replacing main", rv.Git, dev)
	}
	if r := rv.Git.Replaces; r.Ref != "main" || r.RefKind != "branch" || r.Folder != "stacks/web" || r.RepoName != "acme" || !slices.Equal(r.ComposeFiles, []string{"compose.yaml"}) {
		t.Errorf("replaces = %+v, want main of stacks/web in acme", r)
	}

	// The deploy is the normal one; it applies the source.
	review, code, raw := operator.reviewStack(stackID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	job := g.f.deployAndWait(t, operator, stackID, `{"draftVersion":`+itoaT(review.DraftVersion)+`,"pins":`+pinsJSON(t, review.Pins)+`}`)
	if job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}
	if len(g.f.fake.Writes()) <= writes {
		t.Error("the deploy of the new source wrote nothing to the engine")
	}
	if st := g.readGit(t, g.admin, stackID); st.Ref != "dev" || st.RefKind != "branch" || st.Status != "in_sync" || st.DeployedSha != dev || st.HeadSha != dev || st.Behind != 0 {
		t.Errorf("git after the deploy = %+v, want dev followed, in sync at %s", st, dev)
	}
	good, ok, err := repo.LatestGoodRevision(context.Background(), stackID)
	if err != nil || !ok || good.Git.Commit != dev || good.Git.Source.Ref != "dev" {
		t.Errorf("latest good revision git = %+v, %v, %v, want dev at %s", good.Git, ok, err, dev)
	}
	// Afterwards nothing is pending any more.
	if rv := operator.sourceReview(stackID); rv.Git == nil || rv.Git.Replaces != nil {
		t.Errorf("review git after the deploy = %+v, want no replaced source", rv.Git)
	}
	// The change was audited under its own action, with where the stack points now and where it pointed before.
	var saved bool
	for _, a := range g.f.auditRowsOf(t, audit.ActionGitSourceChanged) {
		if a.Action == audit.ActionGitSourceChanged && a.Details["version"] == itoaT(version+1) {
			saved = true
			d := a.Details
			if d["repository"] != "acme" || d["ref_kind"] != "branch" || d["ref"] != "dev" || d["folder"] != "stacks/web" || d["commit"] != dev ||
				d["repository_from"] != "acme" || d["ref_from"] != "main" || d["folder_from"] != "stacks/web" {
				t.Errorf("git.source_changed details = %v, want the new and the old repository, ref and folder and the commit %s", d, dev)
			}
		}
	}
	if !saved {
		t.Errorf("no git.source_changed audit row for version %d", version+1)
	}
}

func TestChangeSourceKinds(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	deployed, _, err := g.stackRepo().DeployedGit(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}
	first := deployed.Commit
	g.repo.Tag(t, "v1", first, false)
	second := g.repo.Commit(t, map[string]string{"stacks/web/override.yaml": "services:\n  web:\n    labels:\n      a: b\n"}, "an override")

	type want struct {
		kind, ref, folder string
		files             []string
		commit            string
		title             string
	}
	steps := []struct {
		name string
		mut  func(map[string]any)
		want want
	}{
		{"branch to tag", func(b map[string]any) { b["refKind"], b["ref"] = "tag", "v1" }, want{"tag", "v1", "stacks/web", []string{"compose.yaml"}, first, "ref"}},
		{"tag to a commit prefix", func(b map[string]any) { b["refKind"], b["ref"] = "commit", first[:12] }, want{"commit", first, "stacks/web", []string{"compose.yaml"}, first, "ref"}},
		{"a fixed commit to another", func(b map[string]any) { b["refKind"], b["ref"] = "commit", second }, want{"commit", second, "stacks/web", []string{"compose.yaml"}, second, "ref"}},
		{"another folder", func(b map[string]any) { b["folder"], b["composeFiles"] = "stacks/other", []string{"compose.yml"} }, want{"branch", "main", "stacks/other", []string{"compose.yml"}, second, "folder"}},
		{"more compose files", func(b map[string]any) { b["composeFiles"] = []string{"compose.yaml", "override.yaml"} }, want{"branch", "main", "stacks/web", []string{"compose.yaml", "override.yaml"}, second, "files"}},
		{"compose files in the other order", func(b map[string]any) { b["composeFiles"] = []string{"override.yaml", "compose.yaml"} }, want{"branch", "main", "stacks/web", []string{"override.yaml", "compose.yaml"}, second, "files"}},
	}
	for _, s := range steps {
		rec := g.sourceOf(g.srcAdmin(t), stackID, draftVersion(t, operator, stackID), s.mut)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", s.name, rec.Code, rec.Body)
		}
		if got := answerOf[sourceAnswer](t, rec); got.Commit != s.want.commit || got.Ref != s.want.ref {
			t.Errorf("%s: answer = %+v, want %s at %s", s.name, got, s.want.ref, s.want.commit)
		}
		rv := operator.sourceReview(stackID)
		if rv.Git == nil || rv.Git.Replaces == nil || rv.Git.RefKind != s.want.kind || rv.Git.Ref != s.want.ref || rv.Git.Folder != s.want.folder {
			t.Errorf("%s: review git = %+v, want %s %s of %s replacing main", s.name, rv.Git, s.want.kind, s.want.ref, s.want.folder)
		} else if r := rv.Git.Replaces; r.Ref != "main" || r.Folder != "stacks/web" || !slices.Equal(r.ComposeFiles, []string{"compose.yaml"}) {
			t.Errorf("%s: replaces = %+v, want the applied source", s.name, r)
		}
		// Whatever was asked, the stack keeps following what it followed.
		if st := g.readGit(t, g.admin, stackID); st.Ref != "main" || st.RefKind != "branch" || st.Folder != "stacks/web" || !slices.Equal(st.ComposeFiles, []string{"compose.yaml"}) {
			t.Errorf("%s: git = %+v, want the applied source untouched", s.name, st)
		}
	}

	// Asking for the source the stack follows is no change of source.
	if rec := g.sourceOf(g.srcAdmin(t), stackID, draftVersion(t, operator, stackID), nil); rec.Code != http.StatusOK {
		t.Fatalf("back to the applied source: %d %s", rec.Code, rec.Body)
	}
	if rv := operator.sourceReview(stackID); rv.Git == nil || rv.Git.Replaces != nil {
		t.Errorf("review git = %+v, want no replaced source for the applied source", rv.Git)
	}
}

func TestChangeSourceLayer(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	g.f.addEnvironmentRow(t, "env-other")
	foreign := decodeGitRepo(t, g.f.credCall(t, g.admin, http.MethodPost, "/api/env/env-other/git/repos",
		gitJSON(map[string]any{"name": "theirs", "url": g.srv.URL, "caPem": g.srv.CAPEM}))).ID
	instance := decodeGitRepo(t, g.f.credCall(t, g.admin, http.MethodPost, "/api/git/repos",
		gitJSON(map[string]any{"name": "shared", "url": g.srv.URL, "caPem": g.srv.CAPEM}))).ID

	version := draftVersion(t, operator, stackID)
	for name, id := range map[string]string{"another environment's repository": foreign, "a repository that is not there": "nope"} {
		rec := g.srcAdmin(t).changeSource(stackID, version, func(b map[string]any) { b["repoId"] = id })
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "repository_not_found" {
			t.Errorf("%s: %d %s, want 404 repository_not_found", name, rec.Code, rec.Body)
		}
	}
	if got := draftVersion(t, operator, stackID); got != version {
		t.Errorf("draft version = %d, want %d: a refused change stores nothing", got, version)
	}

	// An instance repository is usable here, and the review names it.
	rec := g.srcAdmin(t).changeSource(stackID, version, func(b map[string]any) { b["repoId"] = instance })
	if rec.Code != http.StatusOK {
		t.Fatalf("instance repository: %d %s", rec.Code, rec.Body)
	}
	if rv := operator.sourceReview(stackID); rv.Git == nil || rv.Git.RepoName != "shared" || rv.Git.Replaces == nil || rv.Git.Replaces.RepoName != "acme" {
		t.Errorf("review git = %+v, want shared replacing acme", rv.Git)
	}
	// A good deploy makes the other repository the stack's, and the old one the replaced one no more.
	review, code, raw := operator.reviewStack(stackID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if job := g.f.deployAndWait(t, operator, stackID, `{"draftVersion":`+itoaT(review.DraftVersion)+`,"pins":`+pinsJSON(t, review.Pins)+`}`); job.State != "done" {
		t.Fatalf("deploy = %+v", job)
	}
	if st := g.readGit(t, g.admin, stackID); st.RepoID != instance || st.RepoName != "shared" {
		t.Errorf("git after the deploy = %+v, want the instance repository", st)
	}
}

func TestChangeSourceRefusals(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	version := draftVersion(t, operator, stackID)
	viewer := g.f.caller(t, g.e, "viewer-1", "viewer")
	if rec := g.sourceOf(viewer, stackID, version, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a Viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	// Choosing the repository a stack reads is as sensitive as connecting one, so it takes the Admin's git.manage.
	if rec := g.sourceOf(operator, stackID, version, nil); rec.Code != http.StatusForbidden {
		t.Errorf("an Operator: %d %s, want 403", rec.Code, rec.Body)
	}
	cases := []struct {
		name   string
		mut    func(map[string]any)
		status int
		typ    string
	}{
		{"a ref git refuses", func(b map[string]any) { b["ref"] = "-x" }, http.StatusUnprocessableEntity, "ref_invalid"},
		{"a branch that is not there", func(b map[string]any) { b["ref"] = "gone" }, http.StatusUnprocessableEntity, "ref_missing"},
		{"a folder that is not there", func(b map[string]any) { b["folder"] = "stacks/nope" }, http.StatusUnprocessableEntity, "folder_missing"},
		{"a folder outside the repository", func(b map[string]any) { b["folder"] = "stacks/../.." }, http.StatusUnprocessableEntity, "folder_invalid"},
		{"a compose file the folder lacks", func(b map[string]any) { b["composeFiles"] = []string{"nope.yaml"} }, http.StatusUnprocessableEntity, "compose_file_missing"},
		{"a compose path outside the folder", func(b map[string]any) { b["composeFiles"] = []string{"../other/compose.yml"} }, http.StatusUnprocessableEntity, "compose_path_invalid"},
	}
	for _, c := range cases {
		rec := g.sourceOf(g.srcAdmin(t), stackID, version, c.mut)
		if rec.Code != c.status || problemType(t, rec) != c.typ {
			t.Errorf("%s: %d %s, want %d %s", c.name, rec.Code, rec.Body, c.status, c.typ)
		}
	}
	if got := draftVersion(t, operator, stackID); got != version {
		t.Errorf("draft version = %d, want %d: a refused change stores nothing", got, version)
	}

	// A stack that is not from a repository.
	inline := newStack(t, g.f.caller(t, g.e, "envadmin-1", "admin"), "inline")
	if rec := g.sourceOf(g.srcAdmin(t), inline.ID, 1, nil); rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_git" {
		t.Errorf("an inline stack: %d %s, want 409 stack_not_git", rec.Code, rec.Body)
	}
}

func TestChangeSourceConflict(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	deployed, _, err := g.stackRepo().DeployedGit(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}
	g.devBranch(t, deployed.Commit)
	version := draftVersion(t, operator, stackID)

	// Somebody else saves the draft first.
	other := g.f.caller(t, g.e, "admin-2", "admin")
	if rec := g.sourceOf(other, stackID, version, func(b map[string]any) { b["ref"] = "dev" }); rec.Code != http.StatusOK {
		t.Fatalf("first change: %d %s", rec.Code, rec.Body)
	}
	rec := g.sourceOf(g.srcAdmin(t), stackID, version, func(b map[string]any) { b["refKind"], b["ref"] = "tag", "v-none" })
	if rec.Code != http.StatusConflict || problemType(t, rec) != "draft_changed" {
		t.Fatalf("a change at a stale version: %d %s, want 409 draft_changed", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "u-other") && !strings.Contains(rec.Body.String(), "savedBy") {
		t.Errorf("conflict body = %s, want it to name who changed the draft", rec.Body)
	}
	// Nothing was stored for the loser: the draft still holds dev.
	if rv := operator.sourceReview(stackID); rv.Git == nil || rv.Git.Ref != "dev" {
		t.Errorf("review git = %+v, want the first change kept", rv.Git)
	}
}

func TestChangeSourceDiscard(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	deployed, _, err := g.stackRepo().DeployedGit(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}
	g.devBranch(t, deployed.Commit)
	if rec := g.sourceOf(g.srcAdmin(t), stackID, draftVersion(t, operator, stackID), func(b map[string]any) { b["ref"] = "dev" }); rec.Code != http.StatusOK {
		t.Fatalf("change source: %d %s", rec.Code, rec.Body)
	}
	if rv := operator.sourceReview(stackID); rv.Git == nil || rv.Git.Replaces == nil {
		t.Fatalf("review git = %+v, want a pending source", rv.Git)
	}

	// Discarding the draft drops the pending source: the stack is back to what it follows.
	if rec := operator.discardDraft(stackID); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("discard: %d %s", rec.Code, rec.Body)
	}
	rv := operator.sourceReview(stackID)
	if rv.Git == nil || rv.Git.Replaces != nil || rv.Git.Ref != "main" || rv.Git.Commit != deployed.Commit {
		t.Errorf("review git after the discard = %+v, want main at %s and no replaced source", rv.Git, deployed.Commit)
	}
	if st := g.readGit(t, g.admin, stackID); st.Ref != "main" {
		t.Errorf("git = %+v, want main", st)
	}

	// Pull & redeploy during a pending source pulls the applied source, and the pending source is gone.
	if rec := g.sourceOf(g.srcAdmin(t), stackID, draftVersion(t, operator, stackID), func(b map[string]any) { b["ref"] = "dev" }); rec.Code != http.StatusOK {
		t.Fatalf("change source again: %d %s", rec.Code, rec.Body)
	}
	head := g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.28")}, "newer web on main")
	rec := operator.pullGit(stackID, draftVersion(t, operator, stackID))
	if rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}
	if pulled := answerOf[pullAnswer](t, rec); pulled.Ref != "main" || pulled.Commit != head {
		t.Errorf("pull = %+v, want main at %s: the applied source", pulled, head)
	}
	if rv := operator.sourceReview(stackID); rv.Git == nil || rv.Git.Replaces != nil || rv.Git.Ref != "main" || rv.Git.Commit != head {
		t.Errorf("review git after the pull = %+v, want main at %s", rv.Git, head)
	}
}

func TestChangeSourceBlocksAutoDeploy(t *testing.T) {
	t.Parallel()
	a := newAutoFix(t)
	owner := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, owner)
	operator := a.f.caller(t, a.e, "operator-2", "operator")
	first := a.sha
	dev := a.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": "services:\n  web:\n    image: nginx:1.29\n    env_file: web.env\n"}, "dev")
	a.repo.Branch(t, "dev", dev)
	a.repo.Branch(t, "main", first)

	if rec := a.sourceOf(a.srcAdmin(t), a.stackID, draftVersion(t, operator, a.stackID), func(b map[string]any) { b["ref"] = "dev" }); rec.Code != http.StatusOK {
		t.Fatalf("change source: %d %s", rec.Code, rec.Body)
	}
	head := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx on main")

	// The head of main moved, but a new source waits for review: nothing is deployed and the draft is not pulled away.
	a.checkNow(t, owner, a.stackID, a.ended)
	s, err := a.stackRepo().GitState(context.Background(), a.stackID)
	if err != nil || s.PendingCommit != head || s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) || !strings.Contains(s.ReasonDetail, "new source") {
		t.Fatalf("state = %+v, %v; want the head held back for the new source", s, err)
	}
	if revs := a.f.revisions(t, a.stackID); len(revs) != 1 {
		t.Errorf("revisions = %+v, want none made by the auto-deploy", revs)
	}
	if n := len(a.f.auditRowsOf(t, audit.ActionGitAutoDeployed)); n != 0 {
		t.Errorf("git.autodeployed rows = %d, want none", n)
	}
	if rv := operator.sourceReview(a.stackID); rv.Git == nil || rv.Git.Ref != "dev" || rv.Git.Replaces == nil {
		t.Errorf("review git = %+v, want the pending source still there", rv.Git)
	}
}
