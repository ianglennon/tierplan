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
