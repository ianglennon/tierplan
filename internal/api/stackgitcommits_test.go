// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The commits a git update brings (plan 09-20, GIT-04, GIT-09): the ones that changed the stack's folder, newest first,
// ties by ID.

type commitsAnswer struct {
	Commits []struct {
		SHA     string `json:"sha"`
		Subject string `json:"subject"`
		Author  string `json:"author"`
		When    string `json:"when"`
	} `json:"commits"`
	Total     int  `json:"total"`
	Capped    bool `json:"capped"`
	Available bool `json:"available"`
}

func (g *gitStackFix) readCommits(t *testing.T, stackID, head string) (commitsAnswer, int) {
	t.Helper()
	rec := g.f.credCall(t, g.admin, http.MethodGet, g.gitRoute(stackID)+"/commits?head="+head, "")
	var out commitsAnswer
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out, rec.Code
}

func TestStackGitCommitsOrder(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)

	// Four commits in a row land in the same second, so their times tie. Two of them touch the folder, two do not.
	a := g.repo.Commit(t, map[string]string{"stacks/web/a.env": "A=1\n"}, "env a <b>bold</b>")
	g.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {}\n# 1\n"}, "elsewhere 1")
	b := g.repo.Commit(t, map[string]string{"stacks/web/b.env": "B=1\n"}, "env b")
	head := g.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {}\n# 2\n"}, "elsewhere 2")
	if rec := operator.pullGit(stackID, draftVersion(t, operator, stackID)); rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}

	got, code := g.readCommits(t, stackID, head)
	if code != http.StatusOK || !got.Available || got.Total != 2 || got.Capped || len(got.Commits) != 2 {
		t.Fatalf("commits = %+v (%d), want the 2 that changed the folder", got, code)
	}
	gotSHAs := []string{got.Commits[0].SHA, got.Commits[1].SHA}
	slices.Sort(gotSHAs)
	if want := []string{a, b}; !slices.Equal(gotSHAs, slices.Sorted(slices.Values(want))) {
		t.Errorf("commits = %v, want only %v", gotSHAs, want)
	}
	// Newest first, and where the times are equal by ID, the same for every read.
	first, second := got.Commits[0], got.Commits[1]
	if first.When < second.When || (first.When == second.When && first.SHA > second.SHA) {
		t.Errorf("order = %+v then %+v, want newest first with ties by ID", first, second)
	}
	again, _ := g.readCommits(t, stackID, head)
	if again.Commits[0].SHA != first.SHA {
		t.Errorf("a second read begins with %s, the first with %s", again.Commits[0].SHA, first.SHA)
	}
	for _, c := range got.Commits {
		if c.Author == "" || c.When == "" || c.Subject == "" {
			t.Errorf("commit %+v lacks a field", c)
		}
	}
	// The subject is data: markup stays as typed.
	for _, c := range got.Commits {
		if c.SHA == a && c.Subject != "env a <b>bold</b>" {
			t.Errorf("subject = %q, want the text as committed", c.Subject)
		}
	}
}

func TestStackGitCommitsUnavailable(t *testing.T) {
	t.Parallel()
	g, _, stackID := deployedGitStack(t)
	head := g.repo.Commit(t, map[string]string{"stacks/web/a.env": "A=1\n"}, "env a")

	// The mirror has not seen the head yet: no list, and no error.
	got, code := g.readCommits(t, stackID, head)
	if code != http.StatusOK || got.Available || len(got.Commits) != 0 {
		t.Errorf("commits of a head the mirror lacks = %+v (%d), want unavailable", got, code)
	}
	if _, code := g.readCommits(t, stackID, "xyz"); code != http.StatusUnprocessableEntity {
		t.Errorf("a head that is not 40 hex = %d, want 422", code)
	}
	if _, code := g.readCommits(t, stackID, strings.Repeat("A", 40)); code != http.StatusUnprocessableEntity {
		t.Errorf("an upper case head = %d, want 422", code)
	}
}

func TestStackGitCommitsOfAnInlineStack(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "plain")
	rec := admin.do(http.MethodGet, "stacks/"+st.ID+"/git/commits?head="+strings.Repeat("a", 40), "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stack_not_git") {
		t.Errorf("commits of an inline stack = %d %s, want 409 stack_not_git", rec.Code, rec.Body)
	}
}

// The head a check recorded is in the review, so the page can say the branch moved on after the review was prepared
// (plan 09-20, D-17). The draft keeps the commit it was pulled at, and a deploy deploys that one.
func TestReviewHeadCommit(t *testing.T) {
	t.Parallel()
	ended := make(checkEnded, 4)
	g, operator, stackID := deployedGitStackWith(t, func(f *envFix) { f.gitOnCheck = ended.tell })

	g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.28")}, "newer web")
	if rec := operator.pullGit(stackID, draftVersion(t, operator, stackID)); rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}
	pulled := g.readGit(t, g.admin, stackID).HeadSha
	got, _ := operator.reviewGit(stackID)
	if got == nil || got.Commit != pulled || got.HeadCommit != pulled {
		t.Fatalf("review git after a pull = %+v, want the pulled commit %s as the head too", got, pulled)
	}

	// The branch moves on, and a check sees it.
	moved := g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.29")}, "newest web")
	g.checkNow(t, g.admin, stackID, ended)
	got, rv := operator.reviewGit(stackID)
	if got == nil || got.HeadCommit != moved || got.Commit != pulled {
		t.Fatalf("review git after the branch moved = %+v, want the head %s and the reviewed commit %s", got, moved, pulled)
	}

	// The deploy deploys the commit that was reviewed.
	job := g.f.deployAndWait(t, operator, stackID, `{"draftVersion":`+itoaT(rv.DraftVersion)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`)
	if job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}
	good, ok, err := g.stackRepo().LatestGoodRevision(context.Background(), stackID)
	if err != nil || !ok || good.Git.Commit != pulled {
		t.Errorf("deployed commit = %+v, %v, %v, want the reviewed %s and not the head %s", good.Git, ok, err, pulled, moved)
	}
}

func TestReviewOfAnInlineStackHasNoHeadCommit(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "plain")
	rec := admin.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "headCommit") {
		t.Errorf("review of an inline stack = %d %s, want no headCommit", rec.Code, rec.Body)
	}
}

type revisionGit struct {
	Commit      string `json:"commit"`
	Tree        string `json:"tree"`
	SyncTrigger string `json:"syncTrigger"`
	Source      *struct {
		RepoID       string   `json:"repoId"`
		RepoName     string   `json:"repoName"`
		RefKind      string   `json:"refKind"`
		Ref          string   `json:"ref"`
		Folder       string   `json:"folder"`
		ComposeFiles []string `json:"composeFiles"`
	} `json:"source"`
}

// The History of a git stack names the commit, the folder's tree and the source of every revision, and a revision of a
// stack that is not from git carries none (plan 09-20, GIT-03, D-08).
func TestRevisionGitFields(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	first, _, err := g.stackRepo().DeployedGit(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}
	head := g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.28")}, "newer web")
	if rec := operator.pullGit(stackID, draftVersion(t, operator, stackID)); rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}
	_, rv := operator.reviewGit(stackID)
	if job := g.f.deployAndWait(t, operator, stackID, `{"draftVersion":`+itoaT(rv.DraftVersion)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`); job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}

	var list struct {
		Items []struct {
			Seq int64 `json:"seq"`
			revisionGit
		} `json:"items"`
	}
	rec := operator.do(http.MethodGet, "stacks/"+stackID+"/revisions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revisions: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("revisions = %+v, want 2", list.Items)
	}
	newest, oldest := list.Items[0], list.Items[1]
	if newest.Commit != head || newest.Tree == "" || newest.Tree == oldest.Tree || newest.SyncTrigger != "" {
		t.Errorf("newest revision = %+v, want the pulled commit %s and its own tree", newest, head)
	}
	if oldest.Commit != first.Commit || oldest.Tree != first.Tree {
		t.Errorf("oldest revision = %+v, want commit %s tree %s", oldest, first.Commit, first.Tree)
	}
	for _, it := range list.Items {
		src := it.Source
		if src == nil || src.RepoID != g.id || src.RepoName != "acme" || src.RefKind != "branch" || src.Ref != "main" || src.Folder != "stacks/web" ||
			!slices.Equal(src.ComposeFiles, []string{"compose.yaml"}) {
			t.Errorf("revision %d source = %+v, want the repository, ref, folder and compose files", it.Seq, src)
		}
	}

	// The revision's own card carries the same.
	var detail struct {
		Item revisionGit `json:"item"`
	}
	rec = operator.do(http.MethodGet, "stacks/"+stackID+"/revisions/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revision 1: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Item.Commit != first.Commit || detail.Item.Tree != first.Tree || detail.Item.Source == nil || detail.Item.Source.Folder != "stacks/web" {
		t.Errorf("revision 1 item = %+v, want the first commit %s, its tree and source", detail.Item, first.Commit)
	}
}

func TestRevisionOfAnInlineStackHasNoGit(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "plain")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(draftVersion(t, operator, st.ID))+`}`); job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}
	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/revisions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revisions: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []revisionGit `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Commit != "" || list.Items[0].Tree != "" || list.Items[0].Source != nil || list.Items[0].SyncTrigger != "" {
		t.Errorf("revisions = %+v, want one with no commit, tree, source or trigger", list.Items)
	}
}
