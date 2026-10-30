// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
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

// A mirror is only a cache (plan 09-32, D-08): a roll back is made from the revisions the stack stores, so it needs
// neither the mirror nor the git server, and the sweep or a deleted repository can take the mirror away at any time.
func TestRollbackWithoutMirror(t *testing.T) {
	t.Parallel()
	mirrors := gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	a := newDeployedAutoFixWith(t, func(f *envFix) { f.gitClient = mirrors })
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)

	a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("deploy of #2 = %+v, want done", job)
	}

	ids, err := mirrors.ListMirrors()
	if err != nil || len(ids) != 1 {
		t.Fatalf("mirrors before = %v, %v, want the repository's one", ids, err)
	}
	if err := mirrors.RemoveMirror(ids[0]); err != nil {
		t.Fatalf("RemoveMirror: %v", err)
	}

	rolledBy := a.f.caller(t, a.e, "operator-2", "operator")
	rec := rolledBy.do(http.MethodPost, rollbackPath(a.stackID), `{"revision":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back without a mirror: %d %s", rec.Code, rec.Body)
	}
	if job := a.f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}
	got, err := a.stackRepo().Revision(context.Background(), a.stackID, 3)
	if err != nil || got.Origin != "rollback" || got.RestoredFrom != 1 {
		t.Errorf("revision #3 = origin %q restored from #%d (%v), want a roll back to #1", got.Origin, got.RestoredFrom, err)
	}
}

// rollBack rolls the stack back to revision seq as a second Operator and waits for the roll back to end.
func (a *autoFix) rollBack(t *testing.T, seq int) {
	t.Helper()
	rolledBy := a.f.caller(t, a.e, "operator-2", "operator")
	rec := rolledBy.do(http.MethodPost, rollbackPath(a.stackID), fmt.Sprintf(`{"revision":%d}`, seq))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	if job := a.f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}
}

// Auto-deploy after a roll back (plan 09-34, G-09-8, G-09-9, D-19, D-22, D-23): a roll back makes a new good revision and
// leaves the draft where it started, so the draft is behind the newest good revision. The automation proves the draft holds
// nothing a person left, so the head it deploys is not taken for an overwrite of a newer deploy nobody saw. The router,
// policy, store, access service and deployer are the real ones, the repository a real git server and the engine a fake.
func TestGitAutoDeployAfterRollback(t *testing.T) {
	t.Parallel()

	t.Run("turning auto-deploy on again deploys the head", func(t *testing.T) {
		t.Parallel()
		a := newDeployedAutoFix(t)
		operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
		a.turnOn(t, operator)

		// A commit deploys by itself (#2); a second Operator rolls back to #1, which makes #3 and pauses auto-deploy.
		a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")
		if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
			t.Fatalf("first check = %s, want behind", out)
		}
		if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
			t.Fatalf("deploy of #2 = %+v, want done", job)
		}
		a.rollBack(t, 1)

		// A commit that arrives while auto-deploy is paused is not deployed.
		head := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.29\n    env_file: web.env\n", "bump nginx again")
		if out := a.poll(t); out != gitsync.OutcomeBehind {
			t.Fatalf("poll = %s, want behind", out)
		}
		if n := len(a.f.auditRowsOf(t, audit.ActionGitAutoDeployed)); n != 1 {
			t.Fatalf("git.autodeployed rows = %d while paused, want only the one before the roll back", n)
		}

		// Turning it on again is the person's go-ahead (D-22): the check that follows deploys the head.
		a.turnOn(t, operator)
		if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
			t.Fatalf("deploy of the head = %+v, want done", job)
		}
		got := a.readGit(t, a.admin, a.stackID)
		if got.Status != "in_sync" || got.DeployedSha != head || got.HeadSha != head || got.ReasonCode != "" {
			t.Errorf("git = %+v, want in sync at %s with no reason", got, head)
		}
		if strings.Contains(got.ReasonDetail, "after you started editing") {
			t.Errorf("reason = %q, a person's sentence on an automatic deploy", got.ReasonDetail)
		}
		var deployed []auditRowFull
		for _, r := range a.f.auditRowsOf(t, audit.ActionGitAutoDeployed) {
			if r.TargetID == a.stackID && r.Details["commit"] == head {
				deployed = append(deployed, r)
			}
		}
		if len(deployed) != 1 {
			t.Errorf("git.autodeployed rows for %s = %+v, want one", head, deployed)
		}
	})

	// UAT test 8 as it ran: after a roll back, an Operator's auto-deploy meets a commit that adds a bind mount. The real
	// blocker is named, not the stale draft of a roll back.
	t.Run("a bind mount an Operator may not deploy is held back with the policy reason", func(t *testing.T) {
		t.Parallel()
		a := newDeployedAutoFix(t)
		operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
		a.turnOn(t, operator)
		a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")
		if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
			t.Fatalf("first check = %s, want behind", out)
		}
		if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
			t.Fatalf("deploy of #2 = %+v, want done", job)
		}
		a.rollBack(t, 1)
		revisions, writes := len(a.f.revisions(t, a.stackID)), len(a.f.fake.Writes())
		head := a.commitCompose(t, gitBindCompose, "mount /etc")

		a.turnOn(t, operator)
		got := a.readGit(t, a.admin, a.stackID)
		if got.Status != "error" || got.ReasonCode != "auto_deploy_blocked" {
			t.Fatalf("git = %+v, want error auto_deploy_blocked", got)
		}
		if !strings.HasPrefix(got.ReasonDetail, "This needs Admin: a bind mount of /etc") || strings.Contains(got.ReasonDetail, "after you started editing") {
			t.Errorf("reason = %q, want the host access that needs Admin and no sentence for a person editing", got.ReasonDetail)
		}
		if s, err := a.stackRepo().GitState(context.Background(), a.stackID); err != nil || s.PendingCommit != head {
			t.Errorf("state = %+v, %v; want the commit %s pending", s, err, head)
		}
		if n := len(a.f.revisions(t, a.stackID)); n != revisions {
			t.Errorf("revisions = %d, want still %d: a refused deploy adds none", n, revisions)
		}
		if n := len(a.f.fake.Writes()); n != writes {
			t.Errorf("engine writes = %d, want still %d: a refused deploy writes nothing", n, writes)
		}
	})

	// UAT test 9 as it ran: the person presses Review update (the pull route) while auto-deploy is paused, so the draft
	// already holds the head when auto-deploy is turned on. The pull inside the automatic deploy finds nothing to do, and
	// the draft still starts below the roll back.
	t.Run("a head a person pulled while paused deploys too", func(t *testing.T) {
		t.Parallel()
		a := newDeployedAutoFix(t)
		operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
		a.turnOn(t, operator)
		a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")
		if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
			t.Fatalf("first check = %s, want behind", out)
		}
		if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
			t.Fatalf("deploy of #2 = %+v, want done", job)
		}
		a.rollBack(t, 1)
		head := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.29\n    env_file: web.env\n", "bump nginx again")
		if out := a.poll(t); out != gitsync.OutcomeBehind {
			t.Fatalf("poll = %s, want behind", out)
		}

		person := a.f.caller(t, a.e, "operator-3", "operator")
		if rec := person.pullGit(a.stackID, draftVersion(t, person, a.stackID)); rec.Code != http.StatusOK {
			t.Fatalf("pull: %d %s", rec.Code, rec.Body)
		}
		if draft, err := a.stackRepo().Draft(context.Background(), a.stackID); err != nil || draft.BaseSeq >= 3 {
			t.Fatalf("draft base = #%d (%v), want a draft that still starts below the roll back, #3", draft.BaseSeq, err)
		}

		a.turnOn(t, operator)
		if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
			t.Fatalf("deploy of the head = %+v, want done", job)
		}
		if got := a.readGit(t, a.admin, a.stackID); got.Status != "in_sync" || got.DeployedSha != head || got.ReasonCode != "" {
			t.Errorf("git = %+v, want in sync at %s", got, head)
		}
	})
}

// A drift restore (plan 09-34, D-23) makes a new good revision too, and auto-deploy stays on: the next commit to the
// folder deploys by itself.
func TestGitAutoDeployAfterRestore(t *testing.T) {
	t.Parallel()
	a := newDeployedAutoFix(t)
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)
	a.commitCompose(t, "services:\n  web:\n    image: nginx:1.28\n    env_file: web.env\n", "bump nginx")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("first check = %s, want behind", out)
	}
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("deploy of #2 = %+v, want done", job)
	}

	restoredBy := a.f.caller(t, a.e, "operator-2", "operator")
	rec := restoredBy.do(http.MethodPost, rollbackPath(a.stackID), `{"mode":"restore","revision":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if job := a.f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Fatalf("restore job = %+v, want it done", job)
	}
	if got := a.readGit(t, a.admin, a.stackID); !got.AutoDeploy || got.Paused {
		t.Fatalf("git after a restore = %+v, want auto-deploy still running", got)
	}

	head := a.commitCompose(t, "services:\n  web:\n    image: nginx:1.29\n    env_file: web.env\n", "bump nginx again")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("check after the restore = %s, want behind", out)
	}
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("deploy of the head = %+v, want done", job)
	}
	if got := a.readGit(t, a.admin, a.stackID); got.Status != "in_sync" || got.DeployedSha != head || got.ReasonCode != "" {
		t.Errorf("git = %+v, want in sync at %s", got, head)
	}
}
