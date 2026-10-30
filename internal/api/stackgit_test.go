// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// Where a git stack stands against its repository (plan 09-14, GIT-03, GIT-09, D-13, D-16): a check records what it
// saw, and every body of the stack says it.

type stackGitAnswer struct {
	RepoID       string   `json:"repoId"`
	RepoName     string   `json:"repoName"`
	RepoHost     string   `json:"repoHost"`
	RepoPath     string   `json:"repoPath"`
	RefKind      string   `json:"refKind"`
	Ref          string   `json:"ref"`
	Folder       string   `json:"folder"`
	ComposeFiles []string `json:"composeFiles"`
	DeployedSha  string   `json:"deployedSha"`
	HeadSha      string   `json:"headSha"`
	Behind       int      `json:"behind"`
	BehindCapped bool     `json:"behindCapped"`
	Status       string   `json:"status"`
	ReasonCode   string   `json:"reasonCode"`
	ReasonDetail string   `json:"reasonDetail"`
	CheckedAt    string   `json:"checkedAt"`
	Checking     bool     `json:"checking"`
	AutoDeploy   bool     `json:"autoDeploy"`
	Paused       bool     `json:"autoDeployPaused"`
	PausedReason string   `json:"autoDeployPausedReason"`
	Owner        *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"autoDeployOwner"`
	PollEnabled  bool     `json:"pollEnabled"`
	PollMinutes  int      `json:"pollMinutes"`
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
	if !slices.Equal(before.ComposeFiles, []string{"compose.yaml"}) {
		t.Errorf("compose files = %v, want [compose.yaml] in the order the stack merges them", before.ComposeFiles)
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

type bellItem struct {
	ID           string `json:"id"`
	Severity     string `json:"severity"`
	EnvID        string `json:"envId"`
	StackID      string `json:"stackId"`
	StackName    string `json:"stackName"`
	ReasonCode   string `json:"reasonCode"`
	ReasonDetail string `json:"reasonDetail"`
	Since        string `json:"since"`
}

// gitSyncItems are the git_sync_error items the bell shows a user.
func (g *gitStackFix) gitSyncItems(t *testing.T, u credentialUser) []bellItem {
	t.Helper()
	rec := g.f.credCall(t, u, http.MethodGet, "/api/environments/warnings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("warnings: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []bellItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var out []bellItem
	for _, it := range body.Items {
		if it.ID == "git_sync_error" {
			out = append(out, it)
		}
	}
	return out
}

func TestGitSyncBellItems(t *testing.T) {
	t.Parallel()
	g, stackID, _ := newStackGitFix(t)
	repo := g.stackRepo()
	ctx := context.Background()
	since := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	checked := since.Add(time.Hour)
	state := stackops.GitState{
		StackID: stackID, Status: stackops.GitStatusError, ReasonCode: string(gitsync.ReasonRefGone),
		ReasonDetail: "The branch or tag no longer exists.", CheckedAt: &checked, ErrorSince: &since, ConsecutiveFailures: 3,
	}
	envAdmin := g.f.asRoleOn(t, "u-env-admin", g.env, "admin")
	operator := g.f.asRoleOn(t, "u-operator", g.env, "operator")
	viewer := g.f.asRoleOn(t, "u-viewer-bell", g.env, "viewer")

	if items := g.gitSyncItems(t, g.admin); len(items) != 0 {
		t.Fatalf("items before any error = %+v, want none", items)
	}
	if err := repo.WriteGitState(ctx, state); err != nil {
		t.Fatal(err)
	}

	for name, u := range map[string]credentialUser{"instance admin": g.admin, "environment admin": envAdmin} {
		items := g.gitSyncItems(t, u)
		if len(items) != 1 {
			t.Errorf("%s: items = %+v, want one", name, items)
			continue
		}
		it := items[0]
		if it.Severity != "error" || it.EnvID != g.env || it.StackID != stackID || it.StackName != "web" ||
			it.ReasonCode != "ref_gone" || it.ReasonDetail != state.ReasonDetail || it.Since != "2026-10-28T08:00:00Z" {
			t.Errorf("%s: item = %+v, want the stack in error since 08:00 with its reason", name, it)
		}
	}
	// Only whoever manages the environment's git is told: git.manage (T-09-96).
	for name, u := range map[string]credentialUser{"operator": operator, "viewer": viewer} {
		if items := g.gitSyncItems(t, u); len(items) != 0 {
			t.Errorf("%s was shown %+v, want nothing", name, items)
		}
	}

	// Back in sync, the item is gone: nothing is stored to dismiss.
	state.Status, state.ReasonCode, state.ReasonDetail, state.ErrorSince, state.ConsecutiveFailures = stackops.GitStatusInSync, "", "", nil, 0
	if err := repo.WriteGitState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if items := g.gitSyncItems(t, g.admin); len(items) != 0 {
		t.Errorf("items once back in sync = %+v, want none", items)
	}
}

func TestStackGitErrorState(t *testing.T) {
	t.Parallel()
	g, stackID, ended := newStackGitFix(t)
	if out := g.checkNow(t, g.admin, stackID, ended); out != gitsync.OutcomeInSync {
		t.Fatalf("first check = %s, want in_sync", out)
	}
	if got := g.readGit(t, g.admin, stackID); got.Status != "in_sync" || got.ReasonCode != "" {
		t.Fatalf("git before the error = %+v, want in_sync", got)
	}

	// The branch is deleted on the git host.
	g.repo.DeleteRef(t, "refs/heads/main")
	if out := g.checkNow(t, g.admin, stackID, ended); out != gitsync.OutcomeError {
		t.Fatalf("check of a branch that is gone = %s, want error", out)
	}
	got := g.readGit(t, g.admin, stackID)
	if got.Status != "error" || got.ReasonCode != "ref_gone" || got.ReasonDetail == "" || got.CheckedAt == "" {
		t.Fatalf("git after the failure = %+v, want error ref_gone with a sentence, never in_sync (GIT-09)", got)
	}
	// The last good state keeps running: what was deployed is still what the stack says is deployed.
	if got.DeployedSha != g.sha {
		t.Errorf("deployed = %s, want %s kept", got.DeployedSha, g.sha)
	}
	// The stack list says the same.
	rec := g.f.credCall(t, g.admin, http.MethodGet, "/api/env/"+g.env+"/stacks", "")
	var list listedGit
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	for _, it := range list.Items {
		if it.ID == stackID && (it.Git == nil || it.Git.Status != "error" || it.Git.ReasonCode != "ref_gone") {
			t.Errorf("list git = %+v, want error ref_gone", it.Git)
		}
	}
	if n := len(g.f.audits(t, audit.ActionGitSyncFailed)); n != 1 {
		t.Errorf("git.sync_failed rows = %d, want 1", n)
	}
	// A second failed check changes nothing the reader sees and writes no second row.
	g.checkNow(t, g.admin, stackID, ended)
	if again := g.readGit(t, g.admin, stackID); again.Status != "error" || again.ReasonCode != "ref_gone" {
		t.Errorf("git after a repeated failure = %+v, want the same error", again)
	}
	if n := len(g.f.audits(t, audit.ActionGitSyncFailed)); n != 1 {
		t.Errorf("git.sync_failed rows after a repeat = %d, want still 1", n)
	}

	// The branch is back: the next good check clears the error.
	g.repo.Commit(t, map[string]string{"README.md": "back\n"}, "back")
	if out := g.checkNow(t, g.admin, stackID, ended); out != gitsync.OutcomeInSync {
		t.Fatalf("check once the branch is back = %s, want in_sync", out)
	}
	if back := g.readGit(t, g.admin, stackID); back.Status != "in_sync" || back.ReasonCode != "" || back.ReasonDetail != "" {
		t.Errorf("git after the good check = %+v, want in_sync with no reason", back)
	}
}

// putUpdates sets a stack's polling.
func (g *gitStackFix) putUpdates(t *testing.T, u credentialUser, stackID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return g.f.credCall(t, u, http.MethodPut, g.gitRoute(stackID)+"/updates", body)
}

func TestStackGitUpdates(t *testing.T) {
	t.Parallel()
	g, stackID, _ := newStackGitFix(t)

	if got := g.readGit(t, g.admin, stackID); !got.PollEnabled || got.PollMinutes != 5 {
		t.Fatalf("polling before any change = on %v every %d minutes, want on every 5", got.PollEnabled, got.PollMinutes)
	}

	// The two ends are accepted and a number outside them, or not whole, is not (GIT-06 adjacency).
	for _, minutes := range []string{"1", "1440"} {
		rec := g.putUpdates(t, g.admin, stackID, `{"pollEnabled":true,"pollMinutes":`+minutes+`}`)
		if rec.Code != http.StatusOK {
			t.Errorf("%s minutes: %d %s, want 200", minutes, rec.Code, rec.Body)
		}
	}
	for _, minutes := range []string{"0", "1441", "2.5", "-3"} {
		rec := g.putUpdates(t, g.admin, stackID, `{"pollEnabled":true,"pollMinutes":`+minutes+`}`)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "poll_interval" {
			t.Errorf("%s minutes: %d %s, want 422 poll_interval", minutes, rec.Code, rec.Body)
		}
	}
	if got := g.readGit(t, g.admin, stackID); !got.PollEnabled || got.PollMinutes != 1440 {
		t.Errorf("polling after refused changes = on %v every %d minutes, want the last accepted: on every 1440", got.PollEnabled, got.PollMinutes)
	}

	// A change is stored, answered with the stack's git summary and audited with the old and the new values.
	rec := g.putUpdates(t, g.admin, stackID, `{"pollEnabled":false,"pollMinutes":15}`)
	var answer stackGitAnswer
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &answer) != nil || answer.PollEnabled || answer.PollMinutes != 15 || answer.RepoID != g.id {
		t.Fatalf("change: %d %s, want 200 with polling off every 15 minutes", rec.Code, rec.Body)
	}
	if got := g.readGit(t, g.admin, stackID); got.PollEnabled || got.PollMinutes != 15 {
		t.Errorf("polling after the change = on %v every %d minutes, want off every 15", got.PollEnabled, got.PollMinutes)
	}
	rows := g.f.auditRows(t, "git.updates_changed")
	last := rows[len(rows)-1]
	if last.TargetType != "stack" || last.TargetID != stackID || last.EnvID != g.env || last.Result != "success" {
		t.Errorf("audit row = %+v, want the stack's success", last)
	}
	want := map[string]string{"stack": "web", "poll_enabled": "false", "poll_minutes": "15", "poll_enabled_from": "true", "poll_minutes_from": "1440"}
	if !reflect.DeepEqual(last.Details, want) {
		t.Errorf("audit details = %v, want %v", last.Details, want)
	}
	// The same values again change nothing and write nothing.
	before := len(rows)
	if rec := g.putUpdates(t, g.admin, stackID, `{"pollEnabled":false,"pollMinutes":15}`); rec.Code != http.StatusOK {
		t.Errorf("the same values again: %d %s, want 200", rec.Code, rec.Body)
	}
	if got := len(g.f.auditRows(t, "git.updates_changed")); got != before {
		t.Errorf("%d audit rows after a change that changed nothing, want still %d", got, before)
	}

	// A Viewer may read but not change (stacks.deploy).
	viewer := g.f.asRoleOn(t, "u-viewer", g.env, "viewer")
	if rec := g.putUpdates(t, viewer, stackID, `{"pollEnabled":true,"pollMinutes":5}`); rec.Code != http.StatusForbidden {
		t.Errorf("a Viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	// A stack that is not from a repository.
	plain := g.f.credCall(t, g.admin, http.MethodPost, "/api/env/"+g.env+"/stacks", `{"name":"plain"}`)
	var made struct {
		ID string `json:"id"`
	}
	if plain.Code != http.StatusCreated || json.Unmarshal(plain.Body.Bytes(), &made) != nil {
		t.Fatalf("create a plain stack: %d %s", plain.Code, plain.Body)
	}
	if rec := g.putUpdates(t, g.admin, made.ID, `{"pollEnabled":true,"pollMinutes":5}`); rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_git" {
		t.Errorf("a stack not from git: %d %s, want 409 stack_not_git", rec.Code, rec.Body)
	}
}
