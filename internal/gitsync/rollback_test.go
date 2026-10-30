// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitsync_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/gitsync"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// A roll back of a git stack (plan 09-28, GIT-07, D-22, D-28): the repository head must not be deployed over the version
// the person chose, so the roll back pauses auto-deploy until a person turns it on again.

var rollbackActor = audit.Actor{Type: audit.ActorUser, ID: "u-ada", Name: "ada"}

// rollBackTo does what the roll back route does to the stack's records: the revision is a copy of the one restored, the
// pause is told, and when the deploy ends good the stack is told that too.
func (a *autoFixture) rollBackTo(t *testing.T, seq int64) stackops.Snapshot {
	t.Helper()
	ctx := context.Background()
	target, err := a.rr.Revision(ctx, a.stackID, seq)
	if err != nil {
		t.Fatalf("revision #%d: %v", seq, err)
	}
	st, err := a.rr.Stack(ctx, "env-1", a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := a.rr.CopyRevision(ctx, target, "ada", stackops.OriginRollback)
	if err != nil {
		t.Fatal(err)
	}
	a.svc.RolledBack(ctx, st, rev, rollbackActor)
	if err := a.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	a.svc.Deployed(ctx, a.stackID, rev)
	return rev
}

func (a *autoFixture) ownedRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := a.st.Read.QueryRow(`SELECT COUNT(*) FROM owned_resources WHERE kind = ? AND resource_id = ?`,
		gitsync.KindGitAutoDeploy, a.stackID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRollbackPausesAutoDeploy(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	second := a.changeWeb(t, "1")
	if out := a.poll(t); out != gitsync.OutcomeBehind || a.dep.count() != 1 {
		t.Fatalf("first auto-deploy: check = %s with %d deploys, want behind and one", out, a.dep.count())
	}
	third := a.changeWeb(t, "2")
	a.poll(t)
	if a.dep.count() != 2 || a.newestRevision(t).Seq != 3 {
		t.Fatalf("second auto-deploy: %d deploys, newest #%d, want two and #3", a.dep.count(), a.newestRevision(t).Seq)
	}
	if a.ownedRows(t) != 1 {
		t.Fatal("the automation is not recorded before the roll back")
	}

	rev := a.rollBackTo(t, 2)

	sg := a.settings(t)
	if sg.AutoDeploy || sg.AutoDeployPausedReason != stackops.PausedRollback || sg.AutoDeployPausedSeq != 2 {
		t.Errorf("settings = auto-deploy %v, paused %q at #%d; want off, paused for a rollback at #2", sg.AutoDeploy, sg.AutoDeployPausedReason, sg.AutoDeployPausedSeq)
	}
	if n := a.ownedRows(t); n != 0 {
		t.Errorf("owned automation rows = %d, want the automation forgotten", n)
	}
	rows := a.auditRows(t, audit.ActionGitAutoDeployPaused)
	if len(rows) != 1 {
		t.Fatalf("git.autodeploy_paused rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.ActorType != "user" || r.ActorID != rollbackActor.ID || r.Details["stack"] != "web" || r.Details["reason"] != "rollback" || r.Details["revision"] != "2" {
		t.Errorf("audit = %+v, want the person, the stack, reason rollback and revision 2", r)
	}

	// The restored revision is what is deployed, and the head waits for review: behind, not deployed.
	s := a.state(t)
	if s.DeployedCommit != rev.Git.Commit || s.DeployedCommit != second || s.HeadCommit != third || s.Status != stackops.GitStatusBehind || s.Behind < 1 {
		t.Errorf("state = %+v, want deployed at %s, head %s and behind", s, second, third)
	}
	if s.EventKind != stackops.GitEventPaused || s.EventDetail != "Rolled back to #2" {
		t.Errorf("event = %q %q, want the paused event naming revision 2", s.EventKind, s.EventDetail)
	}

	// A later commit to the folder is not deployed by itself, however many rounds go by.
	a.changeWeb(t, "3")
	for range 3 {
		a.clock.advance(1)
		a.poll(t)
	}
	if n := a.dep.count(); n != 2 {
		t.Errorf("deploys started = %d, want still two: nothing is deployed over a roll back", n)
	}
	if s := a.state(t); s.Status != stackops.GitStatusBehind {
		t.Errorf("status = %s, want behind", s.Status)
	}
}

func TestRollbackKeepsTheSourceTheStackFollows(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.changeWeb(t, "1")
	a.poll(t)

	// The folder the stack follows was changed in its settings since revision #1 was deployed.
	sg := a.settings(t)
	moved := stackops.GitSource{RepoID: sg.RepoID, RefKind: sg.RefKind, Ref: sg.Ref, Folder: "stacks/other", ComposeFiles: sg.ComposeFiles}
	if err := a.rr.SetAppliedSource(context.Background(), a.stackID, moved); err != nil {
		t.Fatal(err)
	}

	rev := a.rollBackTo(t, 1)

	after := a.settings(t)
	if after.Folder != "stacks/other" || after.Ref != sg.Ref || after.RepoID != sg.RepoID {
		t.Errorf("settings follow %q at %q, want the folder %q kept (D-28)", after.Folder, after.Ref, "stacks/other")
	}
	s := a.state(t)
	if s.DeployedCommit != rev.Git.Commit || s.Status != stackops.GitStatusBehind || s.ReasonCode != string(gitsync.ReasonSourceChanged) {
		t.Errorf("state = %+v, want deployed at the restored commit and behind with source_changed", s)
	}
}

func TestRollbackOfAStackWithoutAutoDeployChangesNothing(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.changeWeb(t, "1")
	a.poll(t)
	if err := a.rr.SetAutoDeploy(context.Background(), a.stackID, false, "", 0, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		return a.acc.DeregisterOwnedTx(ctx, tx, q, gitsync.KindGitAutoDeploy, a.stackID)
	}); err != nil {
		t.Fatal(err)
	}

	a.rollBackTo(t, 1)

	if sg := a.settings(t); sg.AutoDeploy || sg.AutoDeployPausedReason != "" {
		t.Errorf("settings = %+v, want auto-deploy off and not paused: it was never on", sg)
	}
	if rows := a.auditRows(t, audit.ActionGitAutoDeployPaused); len(rows) != 0 {
		t.Errorf("git.autodeploy_paused rows = %+v, want none", rows)
	}

	// A second roll back of a stack paused for the first names the newest one, and audits nothing more.
	a.enable(t)
	a.rollBackTo(t, 2)
	a.rollBackTo(t, 1)
	sg := a.settings(t)
	if sg.AutoDeployPausedReason != stackops.PausedRollback || sg.AutoDeployPausedSeq != 1 {
		t.Errorf("paused %q at #%d, want rollback at #1", sg.AutoDeployPausedReason, sg.AutoDeployPausedSeq)
	}
	if rows := a.auditRows(t, audit.ActionGitAutoDeployPaused); len(rows) != 1 {
		t.Errorf("git.autodeploy_paused rows = %d, want one: the second roll back found it paused already", len(rows))
	}
}

func TestRollbackOfAStackNotFromGitIsIgnored(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.svc.RolledBack(context.Background(), stackops.Stack{ID: "no-such-stack", EnvID: "env-1"}, stackops.Snapshot{RestoredFrom: 1}, rollbackActor)
	if _, err := a.rr.StackGit(context.Background(), "no-such-stack"); !errors.Is(err, stackops.ErrNoGit) {
		t.Fatalf("StackGit = %v, want ErrNoGit", err)
	}
	if rows := a.auditRows(t, audit.ActionGitAutoDeployPaused); len(rows) != 0 {
		t.Errorf("git.autodeploy_paused rows = %+v, want none", rows)
	}
}
