// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// A git stack's auto-deploy (plan 09-25, GIT-07, D-18 to D-21, D-29): off until a user turns it on, and then a commit
// that changes the folder deploys by itself on the owner's authority, through the real deploy path. The router, policy,
// vault, store, access service and deployer are the real ones; the repository is a real git server and the engine a
// fake.

// autoFix is a git stack that was deployed once, with a channel that says when a check the server started on its own
// (turning auto-deploy on) is over, and one for the checks a request started.
type autoFix struct {
	*gitStackFix
	stackID   string
	ended     checkEnded
	triggered chan bool
}

func newAutoFix(t *testing.T) *autoFix {
	t.Helper()
	a := &autoFix{ended: make(checkEnded, 8), triggered: make(chan bool, 8)}
	a.gitStackFix = newGitStackFixWith(t, func(f *envFix) {
		f.gitOnCheck = a.ended.tell
		f.gitTriggered = func(_ string, _ gitsync.Outcome, _ error, more bool) { a.triggered <- more }
	})
	rec := a.create(t, a.admin, a.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var got gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	a.stackID = got.StackID
	repo := a.stackRepo()
	ctx := context.Background()
	rev, err := repo.NewRevision(ctx, a.stackID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	// A deploy of the draft rebases it on the revision it took, as the deploy route does.
	if err := repo.RebaseDraft(ctx, a.stackID, rev.Seq); err != nil {
		t.Fatalf("RebaseDraft: %v", err)
	}
	return a
}

func (a *autoFix) route() string { return a.gitRoute(a.stackID) + "/auto-deploy" }

func (a *autoFix) put(t *testing.T, u credentialUser, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"enabled":false}`
	if enabled {
		body = `{"enabled":true}`
	}
	return a.f.credCall(t, u, http.MethodPut, a.route(), body)
}

// turnOn turns auto-deploy on as u and waits for the check that follows.
func (a *autoFix) turnOn(t *testing.T, u credentialUser) {
	t.Helper()
	if rec := a.put(t, u, true); rec.Code != http.StatusOK {
		t.Fatalf("turn on: %d %s", rec.Code, rec.Body)
	}
	testsupport.Await(t, "the check turning auto-deploy on started", a.triggered)
}

type ownedAutoDeploy struct {
	owner, state string
	needsAdmin   int
}

func (a *autoFix) owned(t *testing.T) (ownedAutoDeploy, bool) {
	t.Helper()
	var o ownedAutoDeploy
	err := a.f.st.Read.QueryRow(`SELECT owner_id, state, needs_env_admin FROM owned_resources WHERE kind = ? AND resource_id = ?`,
		gitsync.KindGitAutoDeploy, a.stackID).Scan(&o.owner, &o.state, &o.needsAdmin)
	if err != nil {
		return ownedAutoDeploy{}, false
	}
	return o, true
}

func (a *autoFix) userID(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := a.f.st.Read.QueryRow(`SELECT id FROM users WHERE username = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAutoDeployToggle(t *testing.T) {
	t.Parallel()
	a := newAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	viewer := a.f.asRoleOn(t, "u-viewer", a.env, "viewer")

	// Off by default, and nobody owns an automation that is not there.
	if got := a.readGit(t, a.admin, a.stackID); got.AutoDeploy || got.Paused {
		t.Fatalf("a new git stack = %+v, want auto-deploy off", got)
	}
	if _, ok := a.owned(t); ok {
		t.Fatal("an owned auto-deploy exists before anyone turned it on")
	}

	// A Viewer may read but not change it (stacks.deploy).
	if rec := a.put(t, viewer, true); rec.Code != http.StatusForbidden {
		t.Errorf("Viewer turning it on: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := a.f.credCall(t, operator, http.MethodPut, a.route(), `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a body that does not say: %d %s, want 422", rec.Code, rec.Body)
	}

	// An Operator turns it on: the summary says so, the owned row names them and the audit log has it.
	rec := a.put(t, operator, true)
	var answer stackGitAnswer
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &answer) != nil || !answer.AutoDeploy || answer.Paused {
		t.Fatalf("turn on: %d %s, want 200 with auto-deploy on", rec.Code, rec.Body)
	}
	testsupport.Await(t, "the check turning auto-deploy on started", a.triggered)
	o, ok := a.owned(t)
	if !ok || o.owner != a.userID(t, "u-operator") || o.state != "active" || o.needsAdmin != 0 {
		t.Errorf("owned auto-deploy = %+v (found %v), want active, owned by the Operator", o, ok)
	}
	enabled := a.f.auditRowsOf(t, audit.ActionGitAutoDeployEnabled)
	if len(enabled) != 1 || enabled[0].TargetID != a.stackID || enabled[0].EnvID != a.env || enabled[0].Result != "success" || enabled[0].Details["stack"] != "web" {
		t.Errorf("git.autodeploy_enabled rows = %+v, want one for the stack", enabled)
	}
	if got := a.readGit(t, a.admin, a.stackID); !got.AutoDeploy {
		t.Errorf("git after turning it on = %+v, want auto-deploy on", got)
	} else if got.Owner == nil || got.Owner.ID != a.userID(t, "u-operator") || got.Owner.Name == "" {
		t.Errorf("auto-deploy owner = %+v, want the Operator who turned it on", got.Owner)
	}

	// Asking again changes nothing, records nothing and does not hand it to someone else.
	if rec := a.put(t, a.admin, true); rec.Code != http.StatusOK {
		t.Errorf("turn on again: %d %s, want 200", rec.Code, rec.Body)
	}
	if o2, _ := a.owned(t); o2 != o {
		t.Errorf("owned auto-deploy after asking again = %+v, want %+v", o2, o)
	}
	if n := len(a.f.auditRowsOf(t, audit.ActionGitAutoDeployEnabled)); n != 1 {
		t.Errorf("git.autodeploy_enabled rows = %d after asking again, want 1", n)
	}

	// A Viewer can't turn it off; an Operator can, and the owner is forgotten.
	if rec := a.put(t, viewer, false); rec.Code != http.StatusForbidden {
		t.Errorf("Viewer turning it off: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := a.put(t, operator, false); rec.Code != http.StatusOK {
		t.Fatalf("turn off: %d %s", rec.Code, rec.Body)
	}
	if _, ok := a.owned(t); ok {
		t.Errorf("the owned auto-deploy is still recorded after it was turned off")
	}
	if got := a.readGit(t, a.admin, a.stackID); got.AutoDeploy {
		t.Errorf("git after turning it off = %+v, want auto-deploy off", got)
	}
	disabled := a.f.auditRowsOf(t, audit.ActionGitAutoDeployDisabled)
	if len(disabled) != 1 || disabled[0].TargetID != a.stackID || disabled[0].Details["stack"] != "web" {
		t.Errorf("git.autodeploy_disabled rows = %+v, want one for the stack", disabled)
	}
	if rec := a.put(t, operator, false); rec.Code != http.StatusOK || len(a.f.auditRowsOf(t, audit.ActionGitAutoDeployDisabled)) != 1 {
		t.Errorf("turn off again: %d %s, want 200 and no second entry", rec.Code, rec.Body)
	}

	// An Admin turning on a stack whose auto-deploy a roll back paused takes it over and resumes it.
	if err := a.stackRepo().SetAutoDeploy(context.Background(), a.stackID, true, stackops.PausedRollback, 2, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.readGit(t, a.admin, a.stackID); !got.AutoDeploy || !got.Paused || got.PausedReason != stackops.PausedRollback {
		t.Fatalf("git after a roll back pause = %+v, want on and paused by the roll back", got)
	}
	a.turnOn(t, a.admin)
	if got := a.readGit(t, a.admin, a.stackID); !got.AutoDeploy || got.Paused || got.PausedReason != "" {
		t.Errorf("git after turning it on again = %+v, want on and running", got)
	}
	if o, ok := a.owned(t); !ok || o.owner == a.userID(t, "u-operator") || o.state != "active" {
		t.Errorf("owned auto-deploy = %+v (found %v), want it handed to the person who turned it on", o, ok)
	}
}

func TestAutoDeployToggleOfAStackNotFromGit(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "plain")
	rec := admin.do(http.MethodPut, "stacks/"+st.ID+"/git/auto-deploy", `{"enabled":true}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_git" {
		t.Errorf("turn on for an inline stack: %d %s, want 409 stack_not_git", rec.Code, rec.Body)
	}
}

// poll runs the check a poller would: Check now (the route) is a person asking again, which tries a held back commit
// once more.
func (a *autoFix) poll(t *testing.T) gitsync.Outcome {
	t.Helper()
	out, err := a.f.gitSync.CheckWith(context.Background(), a.env, a.stackID, gitsync.CheckOptions{Trigger: gitsync.TriggerPoll})
	if err != nil {
		t.Fatalf("CheckWith: %v", err)
	}
	return out
}

// revisionTrigger is what the revision of a stack started by: the sync trigger column.
func (a *autoFix) revisionTrigger(t *testing.T, seq int64) (trigger, commit string) {
	t.Helper()
	if err := a.f.st.Read.QueryRow(`SELECT sync_trigger, git_commit FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND seq = ?`,
		a.stackID, seq).Scan(&trigger, &commit); err != nil {
		t.Fatal(err)
	}
	return trigger, commit
}

// lastJob is the job of the newest deploy of the stack.
func (a *autoFix) lastJob(t *testing.T) string {
	t.Helper()
	var id string
	if err := a.f.st.Read.QueryRow(`SELECT job_id FROM stack_deploys WHERE stack_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, a.stackID).Scan(&id); err != nil {
		t.Fatalf("no deploy was recorded: %v", err)
	}
	return id
}

func (a *autoFix) commitCompose(t *testing.T, compose, msg string) string {
	t.Helper()
	return a.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": compose}, msg)
}

const gitBindCompose = "services:\n  web:\n    image: nginx:1.27\n    env_file: web.env\n    volumes:\n      - /etc:/host-etc\n"

func TestAutoDeployEndToEnd(t *testing.T) {
	t.Parallel()
	a := newAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)
	head := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")

	// Checking now finds the stack behind and deploys the head on the Operator's authority, through the real deploy.
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	job := a.f.awaitDeploy(t, a.lastJob(t))
	if job.State != "done" {
		t.Fatalf("job = %+v, want it done", job)
	}
	if trigger, commit := a.revisionTrigger(t, 2); trigger != stackops.SyncPoll || commit != head {
		t.Errorf("revision #2 = trigger %q commit %s, want a poll at %s", trigger, commit, head)
	}
	if got := a.readGit(t, a.admin, a.stackID); got.Status != "in_sync" || got.DeployedSha != head || got.HeadSha != head || got.Behind != 0 {
		t.Errorf("git after the deploy = %+v, want in sync at %s", got, head)
	}

	// The job is the system's share of the owner's slots, and the audit trail is the one of an automatic deploy.
	hubJob, ok := a.f.jobHub.Get(job.ID)
	opID := a.userID(t, "u-operator")
	if !ok || hubJob.Owner != "system:gitsync:"+opID {
		t.Errorf("job owner = %q (found %v), want system:gitsync:%s", hubJob.Owner, ok, opID)
	}
	auto := a.f.auditRowsOf(t, audit.ActionGitAutoDeployed)
	if len(auto) != 1 || auto[0].Details["commit"] != head || auto[0].Details["trigger"] != "poll" || auto[0].Details["owner"] != opID {
		t.Errorf("git.autodeployed rows = %+v, want one for %s as the Operator's poll", auto, head)
	}
	deploys := a.f.deployAudits(t, a.stackID)
	if len(deploys) != 1 || deploys[0].ActorID != "system:gitsync" || deploys[0].Details["onBehalfOf"] != opID || deploys[0].Details["commit"] != head {
		t.Errorf("stack.deployed rows = %+v, want the system actor on behalf of the Operator at %s", deploys, head)
	}
}

// TestStackJobBodyAutoDeploy: the job of an automatic deploy says what started it, whom it ran for and which commit it
// deploys, so the console and the jobs indicator can tell it from a deploy a person pressed (T-09-82).
func TestStackJobBodyAutoDeploy(t *testing.T) {
	t.Parallel()
	a := newAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)
	head := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	jobID := a.lastJob(t)
	if job := a.f.awaitDeploy(t, jobID); job.State != "done" {
		t.Fatalf("job = %+v, want it done", job)
	}

	var body struct {
		Trigger    string `json:"trigger"`
		OnBehalfOf string `json:"onBehalfOf"`
		Commit     string `json:"commit"`
		StartedBy  string `json:"startedBy"`
	}
	route := "/api/env/" + a.env + "/stacks/" + a.stackID + "/deploys/" + jobID
	rec := a.f.credCall(t, a.admin, http.MethodGet, route, "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("GET job: %d %s", rec.Code, rec.Body)
	}
	if body.Trigger != "poll" || body.Commit != head[:7] || body.OnBehalfOf == "" {
		t.Errorf("job body = %+v, want a poll of %s for the Operator", body, head[:7])
	}
	// The lists read "auto-deploy" under Last deployed, from the revision behind it.
	var detail struct {
		LastDeployedTrigger string `json:"lastDeployedTrigger"`
	}
	rec = a.f.credCall(t, a.admin, http.MethodGet, "/api/env/"+a.env+"/stacks/"+a.stackID, "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &detail) != nil || detail.LastDeployedTrigger != "poll" {
		t.Errorf("stack detail: %d %s, want lastDeployedTrigger poll", rec.Code, rec.Body)
	}
	// History shows the revision as an Auto-deploy for that same person, so it is recorded as made for them.
	var madeFor string
	if err := a.f.st.Read.QueryRow(`SELECT created_by FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND seq = 2`, a.stackID).Scan(&madeFor); err != nil {
		t.Fatal(err)
	}
	if madeFor != body.OnBehalfOf {
		t.Errorf("revision #2 created by %q, want %q (the person the automatic deploy ran for)", madeFor, body.OnBehalfOf)
	}
	var list struct {
		Jobs []struct {
			ID      string `json:"id"`
			Trigger string `json:"trigger"`
		} `json:"jobs"`
	}
	rec = a.f.credCall(t, a.admin, http.MethodGet, "/api/stack-jobs", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("GET jobs: %d %s", rec.Code, rec.Body)
	}
	found := false
	for _, j := range list.Jobs {
		if j.ID == jobID {
			found = true
			if j.Trigger != "poll" {
				t.Errorf("jobs list entry = %+v, want trigger poll", j)
			}
		}
	}
	if !found {
		t.Errorf("jobs list = %+v, want the auto-deploy's job", list.Jobs)
	}
}

func TestAutoDeployEndToEndPolicyAndHostAccess(t *testing.T) {
	t.Parallel()
	t.Run("an Operator's auto-deploy holds back a bind mount", func(t *testing.T) {
		t.Parallel()
		a := newAutoFix(t)
		operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
		a.turnOn(t, operator)
		head := a.commitCompose(t, gitBindCompose, "mount /etc")

		a.checkNow(t, operator, a.stackID, a.ended)
		got := a.readGit(t, a.admin, a.stackID)
		if got.Status != "error" || got.ReasonCode != "auto_deploy_blocked" || !strings.HasPrefix(got.ReasonDetail, "This needs Admin: a bind mount of /etc") {
			t.Fatalf("git = %+v, want error auto_deploy_blocked with the policy reason, worded for an automatic deploy", got)
		}
		s, err := a.stackRepo().GitState(context.Background(), a.stackID)
		if err != nil || s.PendingCommit != head || s.EventKind != stackops.GitEventSkipped {
			t.Errorf("state = %+v, %v; want the commit pending and the skipped event", s, err)
		}
		if revs := a.f.revisions(t, a.stackID); len(revs) != 1 {
			t.Errorf("revisions = %+v, a refused auto-deploy leaves none", revs)
		}
		if w := a.f.fake.Writes(); len(w) != 0 {
			t.Errorf("writes = %v, a refused deploy writes nothing", w)
		}
		// Not tried again until the head changes or a person presses Check now: the polls that follow start no deploy.
		a.poll(t)
		a.poll(t)
		if n := len(a.f.auditRowsOf(t, audit.ActionGitAutoDeployed)); n != 0 {
			t.Errorf("git.autodeployed rows = %d, want none", n)
		}
		if got := a.readGit(t, a.admin, a.stackID); got.ReasonCode != "auto_deploy_blocked" {
			t.Errorf("git after another check = %+v, want it still held back", got)
		}
	})

	t.Run("an Admin's auto-deploy runs it and the owner must stay an Admin", func(t *testing.T) {
		t.Parallel()
		a := newAutoFix(t)
		a.turnOn(t, a.admin)
		head := a.commitCompose(t, gitBindCompose, "mount /etc")

		a.checkNow(t, a.admin, a.stackID, a.ended)
		if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
			t.Fatalf("job = %+v, want it done", job)
		}
		if o, ok := a.owned(t); !ok || o.needsAdmin != 1 || o.state != "active" {
			t.Errorf("owned auto-deploy = %+v (found %v), want active and marked as needing an env Admin", o, ok)
		}
		if got := a.readGit(t, a.admin, a.stackID); got.Status != "in_sync" || got.DeployedSha != head {
			t.Errorf("git = %+v, want in sync at %s", got, head)
		}
	})
}

func TestAutoDeployEndToEndJobFailed(t *testing.T) {
	t.Parallel()
	a := newAutoFix(t)
	engine := func(createError string) {
		cfg := swarmManagerEngine()
		cfg.TasksFollowWrites = true
		cfg.ServiceCreateError = createError
		a.f.fake.Reconfigure(cfg)
	}
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)
	engine("rpc error: code = InvalidArgument desc = the engine says no")
	head := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")

	a.checkNow(t, operator, a.stackID, a.ended)
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State == "done" {
		t.Fatalf("job = %+v, want the deploy to fail", job)
	}
	got := a.readGit(t, a.admin, a.stackID)
	if got.Status != "error" || got.ReasonCode != "auto_deploy_failed" || !strings.Contains(got.ReasonDetail, "web") || got.DeployedSha == head {
		t.Fatalf("git = %+v, want error auto_deploy_failed naming the service and the last good version still the deployed one", got)
	}
	if revs := a.f.revisions(t, a.stackID); len(revs) != 2 || revs[1].Outcome != "failed" {
		t.Errorf("revisions = %+v, want the first good and the second failed, and nothing rolled back", revs)
	}

	// The commit that failed is not deployed again in a loop.
	a.poll(t)
	if revs := a.f.revisions(t, a.stackID); len(revs) != 2 {
		t.Errorf("revisions after another check = %+v, want still two", revs)
	}
	if got := a.readGit(t, a.admin, a.stackID); got.ReasonCode != "auto_deploy_failed" {
		t.Errorf("git after another check = %+v, want it still failed", got)
	}

	// The next commit is tried.
	engine("")
	next := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.29\n    env_file: web.env\n", "bump nginx again")
	a.checkNow(t, operator, a.stackID, a.ended)
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("job = %+v, want the next commit to deploy", job)
	}
	if got := a.readGit(t, a.admin, a.stackID); got.Status != "in_sync" || got.DeployedSha != next {
		t.Errorf("git = %+v, want in sync at %s", got, next)
	}
}

// newDeployedAutoFix is a git stack of two services (web and db) that was really deployed, through the deploy route and
// the fake engine, with the hooks of newAutoFix so a check started by the server can be waited for. The deployed commit
// is the repository's second one.
func newDeployedAutoFix(t *testing.T) *autoFix {
	t.Helper()
	return newDeployedAutoFixWith(t, nil)
}

// newDeployedAutoFixWith is newDeployedAutoFix for a test that sets a field of the fixture too.
func newDeployedAutoFixWith(t *testing.T, tune func(*envFix)) *autoFix {
	t.Helper()
	a := &autoFix{ended: make(checkEnded, 8), triggered: make(chan bool, 8)}
	g, _, stackID := deployedGitStackWith(t, func(f *envFix) {
		f.gitOnCheck = a.ended.tell
		f.gitTriggered = func(_ string, _ gitsync.Outcome, _ error, more bool) { a.triggered <- more }
		if tune != nil {
			tune(f)
		}
	})
	a.gitStackFix, a.stackID = g, stackID
	return a
}

// autoBellItem is an item of the bell about automatic deploys.
type autoBellItem struct {
	ID           string `json:"id"`
	Severity     string `json:"severity"`
	EnvID        string `json:"envId"`
	StackID      string `json:"stackId"`
	StackName    string `json:"stackName"`
	ReasonCode   string `json:"reasonCode"`
	ReasonDetail string `json:"reasonDetail"`
	Commit       string `json:"commit"`
	Count        int    `json:"count"`
	Example      string `json:"example"`
	Revision     int64  `json:"revision"`
	Owner        string `json:"owner"`
	Since        string `json:"since"`
}

// autoBell are the git_autodeploy_* items the bell shows a user.
func (a *autoFix) autoBell(t *testing.T, u credentialUser) []autoBellItem {
	t.Helper()
	rec := a.f.credCall(t, u, http.MethodGet, "/api/environments/warnings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("warnings: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []autoBellItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var out []autoBellItem
	for _, it := range body.Items {
		if strings.HasPrefix(it.ID, "git_autodeploy_") {
			out = append(out, it)
		}
	}
	return out
}

func TestAutoDeployRevertedBellItem(t *testing.T) {
	t.Parallel()
	a := newDeployedAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	other := a.f.asRoleOn(t, "u-operator-2", a.env, "operator")
	viewer := a.f.asRoleOn(t, "u-viewer", a.env, "viewer")
	envAdmin := a.f.asRoleOn(t, "u-env-admin", a.env, "admin")
	a.turnOn(t, operator)

	// Somebody scales web outside Bayplan; the commit then changes the image of web and leaves its replicas alone.
	a.f.editLiveService(t, a.e, "web", "web", scaleTo(5))
	head := a.commitCompose(t, gitCompose("1.28"), "newer web")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("job = %+v, want the deploy to go ahead and finish", job)
	}
	const line = "web replicas 5 → 1"

	// The revision, the audit entry and the state all carry what was put back.
	repo := a.stackRepo()
	ctx := context.Background()
	good, ok, err := repo.LatestGoodRevision(ctx, a.stackID)
	if err != nil || !ok || good.Git.Commit != head || good.SyncTrigger != stackops.SyncPoll {
		t.Fatalf("newest good revision = %+v, %v, %v; want an automatic one at %s", good, ok, err, head)
	}
	if lines, err := repo.RevertedDrift(ctx, a.stackID, good.Seq); err != nil || len(lines) != 1 || lines[0] != line {
		t.Errorf("reverted drift of revision #%d = %q, %v; want %q", good.Seq, lines, err, line)
	}
	if rows := a.f.auditRowsOf(t, audit.ActionGitAutoDeployed); len(rows) != 1 || rows[0].Details["reverted"] != line || rows[0].Details["revertedCount"] != "1" {
		t.Errorf("git.autodeployed rows = %+v, want the reverted line in the details", rows)
	}
	if s, err := repo.GitState(ctx, a.stackID); err != nil || s.EventKind != stackops.GitEventReverted {
		t.Errorf("state = %+v, %v; want the %s event", s, err, stackops.GitEventReverted)
	}

	// The bell tells the owner and the environment's Admins, once, with the first change and where to open it.
	for name, u := range map[string]credentialUser{"owner": operator, "environment admin": envAdmin, "instance admin": a.admin} {
		items := a.autoBell(t, u)
		if len(items) != 1 {
			t.Errorf("%s: items = %+v, want one", name, items)
			continue
		}
		it := items[0]
		if it.ID != "git_autodeploy_reverted" || it.Severity != "warning" || it.EnvID != a.env || it.StackID != a.stackID || it.StackName != "web" ||
			it.Count != 1 || it.Example != line || it.Revision != good.Seq || it.Since == "" {
			t.Errorf("%s: item = %+v, want the reverted notice of web's revision #%d", name, it, good.Seq)
		}
	}
	for name, u := range map[string]credentialUser{"another operator": other, "viewer": viewer} {
		if items := a.autoBell(t, u); len(items) != 0 {
			t.Errorf("%s was shown %+v, want nothing", name, items)
		}
	}
}

// ids are the ids of the items, in the order the bell lists them.
func ids(items []autoBellItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestAutoDeployBellItems(t *testing.T) {
	t.Parallel()
	a := newAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	other := a.f.asRoleOn(t, "u-operator-2", a.env, "operator")
	viewer := a.f.asRoleOn(t, "u-viewer", a.env, "viewer")
	envAdmin := a.f.asRoleOn(t, "u-env-admin", a.env, "admin")
	a.turnOn(t, operator)

	// Nothing is wrong, so nobody is told anything.
	for name, u := range map[string]credentialUser{"owner": operator, "environment admin": envAdmin} {
		if items := a.autoBell(t, u); len(items) != 0 {
			t.Fatalf("%s: items before anything happened = %+v, want none", name, items)
		}
	}

	// A commit that asks for host access is held back for an Operator's automation.
	head := a.commitCompose(t, gitBindCompose, "mount /etc")
	a.checkNow(t, operator, a.stackID, a.ended)
	for name, u := range map[string]credentialUser{"owner": operator, "environment admin": envAdmin, "instance admin": a.admin} {
		items := a.autoBell(t, u)
		if len(items) != 1 {
			t.Errorf("%s: items = %v, want one skipped notice", name, ids(items))
			continue
		}
		it := items[0]
		if it.ID != "git_autodeploy_skipped" || it.Severity != "warning" || it.StackID != a.stackID || it.StackName != "web" ||
			it.Commit != head[:7] || it.ReasonDetail == "" || it.Since == "" {
			t.Errorf("%s: item = %+v, want web's commit %s held back with the reason", name, it, head[:7])
		}
	}
	// T-09-100: someone else who can deploy, and a Viewer, are told nothing about another person's automation.
	for name, u := range map[string]credentialUser{"another operator": other, "viewer": viewer} {
		if items := a.autoBell(t, u); len(items) != 0 {
			t.Errorf("%s was shown %v, want nothing", name, ids(items))
		}
	}

	// A fix that deploys ends the wait, and the notice goes with it.
	a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "no mount")
	a.checkNow(t, operator, a.stackID, a.ended)
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("job = %+v, want the fix to deploy", job)
	}
	if items := a.autoBell(t, operator); len(items) != 0 {
		t.Errorf("items after the fix deployed = %v, want none", ids(items))
	}

	// The owner loses the right to deploy (a Viewer still reads the environment): the automation pauses, and the owner
	// and the Admins are told, apart from the stack's own alert.
	if _, err := a.f.st.Write.Exec(`UPDATE role_assignments SET role_id = 'viewer' WHERE principal_id = ?`, a.userID(t, "u-operator")); err != nil {
		t.Fatal(err)
	}
	a.commitCompose(t, "services:\n  web:\n    image: nginx:1.29\n    env_file: web.env\n", "bump")
	a.poll(t)
	for name, u := range map[string]credentialUser{"owner": operator, "environment admin": envAdmin} {
		items := a.autoBell(t, u)
		if len(items) != 1 {
			t.Errorf("%s: items = %v, want one paused notice", name, ids(items))
			continue
		}
		it := items[0]
		if it.ID != "git_autodeploy_paused" || it.ReasonCode != "owner" || it.StackID != a.stackID || it.Owner != "u-operator" || it.Since == "" {
			t.Errorf("%s: item = %+v, want the pause for the owner, who is u-operator", name, it)
		}
	}
	for name, u := range map[string]credentialUser{"another operator": other, "viewer": viewer} {
		if items := a.autoBell(t, u); len(items) != 0 {
			t.Errorf("%s was shown %v of a pause, want nothing", name, ids(items))
		}
	}

	// A roll back pauses it too, with the revision it went back to; nobody owns it then, so only Admins are told.
	if _, err := a.f.st.Write.Exec(`DELETE FROM owned_resources WHERE kind = ? AND resource_id = ?`, gitsync.KindGitAutoDeploy, a.stackID); err != nil {
		t.Fatal(err)
	}
	if err := a.stackRepo().SetAutoDeploy(context.Background(), a.stackID, false, stackops.PausedRollback, 1, nil); err != nil {
		t.Fatal(err)
	}
	items := a.autoBell(t, envAdmin)
	if len(items) != 1 || items[0].ID != "git_autodeploy_paused" || items[0].ReasonCode != "rollback" || items[0].Revision != 1 || items[0].Owner != "" {
		t.Errorf("environment admin: items = %+v, want the pause by a roll back to revision 1 with no owner", items)
	}
	if items := a.autoBell(t, operator); len(items) != 0 {
		t.Errorf("the former owner was shown %v, want nothing once the automation has no owner", ids(items))
	}

	// Turned on again by a person, the pause and its notice are gone.
	a.turnOn(t, a.admin)
	if items := a.autoBell(t, envAdmin); len(items) != 0 {
		t.Errorf("items after resuming = %v, want none", ids(items))
	}
}

func TestAutoDeployKeepsUnchangedDigests(t *testing.T) {
	t.Parallel()
	a := newDeployedAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)

	dbDigest := dockertest.FakeDigestOf("docker.io/library/postgres:16")
	webDigest := dockertest.FakeDigestOf("docker.io/library/nginx:1.28")
	// The tag the database runs moves on the registry; nothing in the commit asks for it.
	a.f.fake.SetDistributionDigest("docker.io/library/postgres:16", "sha256:"+strings.Repeat("c", 64))
	looked := len(a.f.fake.DistributionRefs())

	// The commit changes the text of the image of web only.
	a.commitCompose(t, gitCompose("1.28"), "newer web")
	a.checkNow(t, operator, a.stackID, a.ended)
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("job = %+v, want the deploy to finish", job)
	}
	for _, ref := range a.f.fake.DistributionRefs()[looked:] {
		if strings.Contains(ref, "postgres") {
			t.Errorf("the registry was asked about %s, an image the commit did not change", ref)
		}
	}

	repo := a.stackRepo()
	good, ok, err := repo.LatestGoodRevision(context.Background(), a.stackID)
	if err != nil || !ok || good.SyncTrigger != stackops.SyncPoll {
		t.Fatalf("newest good revision = %+v, %v, %v; want an automatic one", good, ok, err)
	}
	pins, err := repo.RevisionPins(context.Background(), good.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pins["web_db"], "@"+dbDigest) {
		t.Errorf("pin of db = %q, want it to keep the digest it runs (%s) although its tag moved", pins["web_db"], dbDigest)
	}
	if !strings.HasSuffix(pins["web_web"], "@"+webDigest) {
		t.Errorf("pin of web = %q, want a fresh lookup of nginx:1.28 (%s)", pins["web_web"], webDigest)
	}
}

// TestCheckNowByAViewerDoesNotRetryAHeldBackCommit: a commit that auto-deploy held back is tried once more by "Check now"
// of someone who can deploy, and by nobody else (WR-D03).
func TestCheckNowByAViewerDoesNotRetryAHeldBackCommit(t *testing.T) {
	t.Parallel()
	a := newAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	viewer := a.f.asRoleOn(t, "u-viewer", a.env, "viewer")
	a.turnOn(t, operator)
	head := a.commitCompose(t, gitBindCompose, "mount /etc")
	a.checkNow(t, operator, a.stackID, a.ended)
	if got := a.readGit(t, a.admin, a.stackID); got.ReasonCode != "auto_deploy_blocked" {
		t.Fatalf("git = %+v, want the commit held back", got)
	}

	// The cause goes away: the owner now holds the role a bind mount needs.
	if _, err := a.f.st.Write.Exec(`UPDATE role_assignments SET role_id = 'admin' WHERE id = ?`, "ra-u-operator-"+a.env); err != nil {
		t.Fatal(err)
	}
	a.checkNow(t, viewer, a.stackID, a.ended)
	s, err := a.stackRepo().GitState(context.Background(), a.stackID)
	if err != nil || s.PendingCommit != head || s.DeployedCommit == head {
		t.Fatalf("after a Viewer's check: state %+v, %v; want the commit still held back and not deployed", s, err)
	}
	if revs := a.f.revisions(t, a.stackID); len(revs) != 1 {
		t.Fatalf("revisions = %+v, a Viewer's check must start no deploy", revs)
	}

	a.checkNow(t, operator, a.stackID, a.ended)
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("job = %+v, want the deploy an owner who can deploy asked for", job)
	}
	if got := a.readGit(t, a.admin, a.stackID); got.Status != "in_sync" || got.DeployedSha != head {
		t.Errorf("git = %+v, want in sync at %s", got, head)
	}
}

// TestCheckNowIsRateLimitedPerUser: "Check now" starts a fetch with the repository's credential, so one user may start
// only so many a minute (WR-D03).
func TestCheckNowIsRateLimitedPerUser(t *testing.T) {
	t.Parallel()
	g, stackID, ended := newStackGitFix(t)
	viewer := g.f.asRoleOn(t, "u-viewer", g.env, "viewer")
	for i := 1; i <= 12; i++ {
		g.checkNow(t, viewer, stackID, ended)
	}
	rec := g.f.credCall(t, viewer, http.MethodPost, g.gitRoute(stackID)+"/check", "")
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
		t.Errorf("13th check in a minute: %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	// Another user has a budget of their own.
	g.checkNow(t, g.admin, stackID, ended)
}
