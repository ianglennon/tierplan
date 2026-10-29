// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
)

// Rolling back a git stack (plan 09-28, GIT-07, D-22, D-28): the revision's files and variables come back exactly, the
// stack keeps following the repository, ref and folder it follows now, the head waits for review, and auto-deploy is
// paused so the head is not deployed over the roll back. The router, policy, store, access service and deployer are the
// real ones, the repository a real git server and the engine a fake.

func TestGitRollbackKeepsSettings(t *testing.T) {
	t.Parallel()
	a := newDeployedAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)
	before := a.readGit(t, a.admin, a.stackID)

	// Two automatic deploys: revisions #2 and #3.
	second := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("first check = %s, want behind", out)
	}
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("deploy of #2 = %+v, want done", job)
	}
	third := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.29\n    env_file: web.env\n", "bump nginx again")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("second check = %s, want behind", out)
	}
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("deploy of #3 = %+v, want done", job)
	}
	if got := a.readGit(t, a.admin, a.stackID); got.DeployedSha != third || !got.AutoDeploy || got.Paused {
		t.Fatalf("git before the roll back = %+v, want deployed at %s with auto-deploy running", got, third)
	}
	if _, ok := a.owned(t); !ok {
		t.Fatal("the automation is not recorded before the roll back")
	}

	rolledBy := a.f.caller(t, a.e, "operator-2", "operator")
	rec := rolledBy.do(http.MethodPost, rollbackPath(a.stackID), `{"revision":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	if job := a.f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}

	// The files and variables of #2 are what the roll back put back, exactly.
	ctx := context.Background()
	repo := a.stackRepo()
	want, err := repo.Revision(ctx, a.stackID, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.Revision(ctx, a.stackID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != "rollback" || got.RestoredFrom != 2 || got.Git.Commit != second {
		t.Errorf("revision #4 = origin %q restored from #%d at %s, want a roll back to #2 at %s", got.Origin, got.RestoredFrom, got.Git.Commit, second)
	}
	if len(got.Copy.Files) != len(want.Copy.Files) || len(got.Copy.Vars) != len(want.Copy.Vars) {
		t.Fatalf("revision #4 holds %d files and %d variables, #2 held %d and %d", len(got.Copy.Files), len(got.Copy.Vars), len(want.Copy.Files), len(want.Copy.Vars))
	}
	for i, f := range want.Copy.Files {
		if g := got.Copy.Files[i]; g.Path != f.Path || string(g.Content) != string(f.Content) {
			t.Errorf("file %d of #4 = %q, want %q as in #2", i, g.Path, f.Path)
		}
	}

	// The settings are the ones the stack followed, the head is pending and auto-deploy is paused for the roll back.
	after := a.readGit(t, a.admin, a.stackID)
	if after.RepoID != before.RepoID || after.RefKind != before.RefKind || after.Ref != before.Ref || after.Folder != before.Folder ||
		len(after.ComposeFiles) != len(before.ComposeFiles) {
		t.Errorf("git settings after = %+v, before = %+v; want them kept", after, before)
	}
	if after.DeployedSha != second || after.HeadSha != third || (after.Status != "behind" && after.Status != "source_changed") || after.Behind < 1 {
		t.Errorf("git after the roll back = %+v, want deployed at %s, head %s pending", after, second, third)
	}
	if after.AutoDeploy || !after.Paused || after.PausedReason != "rollback" {
		t.Errorf("auto-deploy after the roll back = on %v, paused %v (%q), want off and paused for the roll back", after.AutoDeploy, after.Paused, after.PausedReason)
	}
	if _, ok := a.owned(t); ok {
		t.Error("the automation is still recorded after the roll back")
	}
	rows := a.f.auditRowsOf(t, audit.ActionGitAutoDeployPaused)
	if len(rows) != 1 || rows[0].Details["reason"] != "rollback" || rows[0].Details["revision"] != "2" || rows[0].TargetID != a.stackID {
		t.Errorf("git.autodeploy_paused rows = %+v, want one for revision 2", rows)
	}

	// The paused automation shows on the bell for an Admin.
	var paused []autoBellItem
	for _, item := range a.autoBell(t, a.admin) {
		if item.ID == "git_autodeploy_paused" {
			paused = append(paused, item)
		}
	}
	if len(paused) != 1 || paused[0].ReasonCode != "rollback" || paused[0].Revision != 2 || paused[0].StackID != a.stackID {
		t.Errorf("bell items = %+v, want one paused for a roll back to #2", paused)
	}

	// A later commit is not deployed over the roll back, however it is looked for.
	a.commitCompose(t, "services:\n  web:\n    image: nginx:1.30\n    env_file: web.env\n", "bump nginx a third time")
	if out := a.poll(t); out != gitsync.OutcomeBehind {
		t.Errorf("poll = %s, want behind", out)
	}
	if n := len(a.f.auditRowsOf(t, audit.ActionGitAutoDeployed)); n != 2 {
		t.Errorf("git.autodeployed rows = %d, want the two before the roll back and none after", n)
	}
	if later := a.readGit(t, a.admin, a.stackID); later.DeployedSha != second {
		t.Errorf("deployed = %s after a poll, want it still %s", later.DeployedSha, second)
	}
}
