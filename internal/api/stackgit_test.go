// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// Where a git stack stands against its repository (plan 09-14, GIT-03, GIT-09, D-13, D-16): a check records what it
// saw, and every body of the stack says it.

type stackGitAnswer struct {
	RepoID       string `json:"repoId"`
	RepoName     string `json:"repoName"`
	RepoHost     string `json:"repoHost"`
	RepoPath     string `json:"repoPath"`
	RefKind      string `json:"refKind"`
	Ref          string `json:"ref"`
	Folder       string `json:"folder"`
	DeployedSha  string `json:"deployedSha"`
	HeadSha      string `json:"headSha"`
	Behind       int    `json:"behind"`
	BehindCapped bool   `json:"behindCapped"`
	Status       string `json:"status"`
	ReasonCode   string `json:"reasonCode"`
	ReasonDetail string `json:"reasonDetail"`
	CheckedAt    string `json:"checkedAt"`
	Checking     bool   `json:"checking"`
	AutoDeploy   bool   `json:"autoDeploy"`
	Paused       bool   `json:"autoDeployPaused"`
}

// checkEnded is told by the server when a check a request started has ended.
type checkEnded chan gitsync.Outcome

func (c checkEnded) tell(_ string, out gitsync.Outcome, err error) {
	if err != nil {
		out = "error: " + gitsync.Outcome(err.Error())
	}
	c <- out
}

// newStackGitFix makes a git stack through the API, deploys it (a good revision), and has the server tell the test when
// a check it started is over.
func newStackGitFix(t *testing.T) (*gitStackFix, string, checkEnded) {
	t.Helper()
	ended := make(checkEnded, 4)
	g := newGitStackFixWith(t, func(f *envFix) { f.gitOnCheck = ended.tell })
	rec := g.create(t, g.admin, g.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var got gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	repo := g.stackRepo()
	ctx := context.Background()
	rev, err := repo.NewRevision(ctx, got.StackID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	return g, got.StackID, ended
}

func (g *gitStackFix) gitRoute(stackID string) string {
	return "/api/env/" + g.env + "/stacks/" + stackID + "/git"
}

func (g *gitStackFix) readGit(t *testing.T, u credentialUser, stackID string) stackGitAnswer {
	t.Helper()
	rec := g.f.credCall(t, u, http.MethodGet, g.gitRoute(stackID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET git: %d %s", rec.Code, rec.Body)
	}
	var out stackGitAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// checkNow asks for a check and waits for the server to say it is over.
func (g *gitStackFix) checkNow(t *testing.T, u credentialUser, stackID string, ended checkEnded) gitsync.Outcome {
	t.Helper()
	rec := g.f.credCall(t, u, http.MethodPost, g.gitRoute(stackID)+"/check", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST check: %d %s, want 202", rec.Code, rec.Body)
	}
	return <-ended
}

type listedGit struct {
	Items []struct {
		ID  string          `json:"id"`
		Git *stackGitAnswer `json:"git"`
	} `json:"items"`
}

func TestCheckNowAndSummary(t *testing.T) {
	t.Parallel()
	g, stackID, ended := newStackGitFix(t)

	before := g.readGit(t, g.admin, stackID)
	if before.Status != "unknown" || before.RepoID != g.id || before.RepoName != "acme" || before.RefKind != "branch" || before.Ref != "main" ||
		before.Folder != "stacks/web" || before.CheckedAt != "" || before.Checking || before.AutoDeploy || before.Paused {
		t.Errorf("before any check = %+v, want unknown with the settings", before)
	}
	if before.RepoHost == "" || strings.Contains(before.RepoPath, ".git") || strings.HasPrefix(before.RepoPath, "/") {
		t.Errorf("repository address = host %q path %q, want a host and a path with no leading slash or .git", before.RepoHost, before.RepoPath)
	}

	if out := g.checkNow(t, g.admin, stackID, ended); out != gitsync.OutcomeInSync {
		t.Fatalf("first check = %s, want in_sync", out)
	}
	head := g.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=c\n"}, "change web")
	if out := g.checkNow(t, g.admin, stackID, ended); out != gitsync.OutcomeBehind {
		t.Fatalf("check after a commit to the folder = %s, want behind", out)
	}

	got := g.readGit(t, g.admin, stackID)
	if got.Status != "behind" || got.Behind != 1 || got.BehindCapped || got.DeployedSha != g.sha || got.HeadSha != head ||
		got.CheckedAt == "" || got.Checking || got.ReasonCode != "" {
		t.Errorf("git = %+v, want behind 1 at %s with %s deployed", got, head, g.sha)
	}

	// The list and the stack's own page carry the same summary.
	rec := g.f.credCall(t, g.admin, http.MethodGet, "/api/env/"+g.env+"/stacks", "")
	var list listedGit
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	i := slices.IndexFunc(list.Items, func(it struct {
		ID  string          `json:"id"`
		Git *stackGitAnswer `json:"git"`
	}) bool {
		return it.ID == stackID
	})
	if i < 0 || list.Items[i].Git == nil || list.Items[i].Git.Behind != 1 || list.Items[i].Git.Status != "behind" || list.Items[i].Git.RepoName != "acme" {
		t.Errorf("list git = %+v, want behind 1", list.Items)
	}
	var detail struct {
		Git *stackGitAnswer `json:"git"`
	}
	rec = g.f.credCall(t, g.admin, http.MethodGet, "/api/env/"+g.env+"/stacks/"+stackID, "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &detail) != nil || detail.Git == nil || detail.Git.Behind != 1 || detail.Git.HeadSha != head {
		t.Errorf("detail: %d %s, want git behind 1", rec.Code, rec.Body)
	}
	// All stacks carries it too.
	rec = g.f.credCall(t, g.admin, http.MethodGet, "/api/stacks", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"behind":1`) {
		t.Errorf("all stacks: %d %s, want the git summary", rec.Code, rec.Body)
	}

	// A check made a revision of nothing and left the draft alone.
	repo := g.stackRepo()
	if revs, _, err := repo.ListRevisions(context.Background(), stackID); err != nil || len(revs) != 1 {
		t.Errorf("revisions = %d, %v, want the one that was deployed", len(revs), err)
	}
}

func TestStackGitRoutesRefuseWhatTheyShould(t *testing.T) {
	t.Parallel()
	g, stackID, _ := newStackGitFix(t)

	// A stack that is not from a repository.
	plain := g.f.credCall(t, g.admin, http.MethodPost, "/api/env/"+g.env+"/stacks", `{"name":"plain"}`)
	if plain.Code != http.StatusCreated {
		t.Fatalf("create a plain stack: %d %s", plain.Code, plain.Body)
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &made); err != nil || made.ID == "" {
		t.Fatalf("plain stack: %s", plain.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := g.gitRoute(made.ID)
		if method == http.MethodPost {
			path += "/check"
		}
		if rec := g.f.credCall(t, g.admin, method, path, ""); rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_git" {
			t.Errorf("%s of a stack not from git: %d %s, want 409 stack_not_git", method, rec.Code, rec.Body)
		}
	}
	// A stack that is not there.
	if rec := g.f.credCall(t, g.admin, http.MethodGet, g.gitRoute("nope"), ""); rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("a stack that is not there: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
	// A Viewer may read and ask: stacks.read.
	viewer := g.f.asRoleOn(t, "u-viewer", g.env, "viewer")
	if got := g.readGit(t, viewer, stackID); got.RepoID != g.id {
		t.Errorf("viewer read = %+v", got)
	}
	// Someone with no role on the environment may not.
	g.f.addEnvironmentRow(t, "env-other-none")
	none := g.f.asRoleOn(t, "u-none", "env-other-none", "viewer")
	if rec := g.f.credCall(t, none, http.MethodGet, g.gitRoute(stackID), ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("no role on the environment: %d %s, want it refused", rec.Code, rec.Body)
	}
}

func TestReasonCodesMatchEnum(t *testing.T) {
	t.Parallel()
	f, ok := reflect.TypeOf(api.StackGitSummary{}).FieldByName("ReasonCode")
	if !ok {
		t.Fatal("StackGitSummary has no ReasonCode")
	}
	tag := f.Tag.Get("enum")
	var want []string
	for _, c := range gitsync.ReasonCodes() {
		want = append(want, string(c))
	}
	if got := strings.Split(tag, ","); !slices.Equal(got, want) {
		t.Errorf("enum tag = %v, want exactly gitsync.ReasonCodes() = %v", got, want)
	}
	if !strings.Contains(f.Tag.Get("json"), "omitempty") {
		t.Errorf("json tag %q, want omitempty so no reason is an absent field", f.Tag.Get("json"))
	}
}
