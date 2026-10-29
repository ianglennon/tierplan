// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
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
	if got := a.readGit(t, a.admin, a.stackID); !got.AutoDeploy || !got.Paused {
		t.Fatalf("git after a roll back pause = %+v, want on and paused", got)
	}
	a.turnOn(t, a.admin)
	if got := a.readGit(t, a.admin, a.stackID); !got.AutoDeploy || got.Paused {
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
