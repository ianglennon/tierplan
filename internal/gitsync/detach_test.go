// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Detaching a git stack (plan 09-31, GIT-02, D-25): the stack becomes an inline stack you edit, nothing that runs changes
// and no git automation is left.

func (a *autoFixture) ownedCount(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := a.st.Read.QueryRow(`SELECT count(*) FROM owned_resources WHERE kind = ? AND resource_id = ?`, kind, a.stackID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (a *autoFixture) rowCount(t *testing.T, table string) int {
	t.Helper()
	var n int
	// table is one of this test's own table names, never input.
	if err := a.st.Read.QueryRow(`SELECT count(*) FROM `+table+` WHERE stack_id = ?`, a.stackID).Scan(&n); err != nil { //nolint:gosec // G202: a literal table name from this file
		t.Fatal(err)
	}
	return n
}

// withWebhookAndOverlay gives the auto-deploy stack a webhook (owned by the same person) and a deployed overlay file, then
// edits the overlay in the draft so the draft differs from what is deployed.
func (a *autoFixture) withWebhookAndOverlay(t *testing.T) (token string, draft stackops.Snapshot) {
	t.Helper()
	ctx := context.Background()
	token = "routing-token-hash"
	err := a.rr.CreateGitWebhook(ctx, stackops.NewGitWebhook{
		StackID: a.stackID, Flavour: "github", Placement: "header", TokenHash: token, Secret: "shh", Last4: "shh", CreatedBy: a.ownerID,
	}, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		return a.acc.RegisterOwnedTx(ctx, tx, q, access.OwnedResource{
			Kind: gitsync.KindGitWebhook, ResourceID: a.stackID, EnvID: "env-1", Name: "web", OwnerID: a.ownerID,
		})
	})
	if err != nil {
		t.Fatalf("CreateGitWebhook: %v", err)
	}
	d, err := a.rr.Draft(ctx, a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	overlay := func(content string) stacks.WorkingCopy {
		files := slices.Clone(d.Copy.Files)
		files = append(files, stacks.File{Path: "secrets/pw.txt", Content: []byte(content), Origin: stacks.OriginOverlay, Secret: true})
		return stacks.WorkingCopy{Files: files, Vars: slices.Clone(d.Copy.Vars), ComposeFiles: slices.Clone(d.Copy.ComposeFiles)}
	}
	saved, _, err := a.rr.SaveDraft(ctx, a.stackID, d.Version, "ada", overlay("pw-deployed"))
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	a.deploy(t)
	saved, _, err = a.rr.SaveDraft(ctx, a.stackID, saved.Version, "ada", overlay("pw-edited-not-deployed"))
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	return token, saved
}

func TestDetach(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "admin")
	ctx := context.Background()
	token, draft := a.withWebhookAndOverlay(t)

	if a.ownedCount(t, gitsync.KindGitAutoDeploy) != 1 || a.ownedCount(t, gitsync.KindGitWebhook) != 1 {
		t.Fatal("the stack lacks its owned automations before the detach")
	}
	if polled, err := a.rr.PolledStacks(ctx); err != nil || len(polled) != 1 {
		t.Fatalf("PolledStacks before = %v, %v, want the stack", polled, err)
	}
	revsBefore, _, err := a.rr.ListRevisions(ctx, a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	good, _, err := a.rr.LatestGoodRevision(ctx, a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	calls := a.dep.count()

	res, err := a.svc.Detach(ctx, "env-1", a.stackID, "bob")
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if res.Revision != good.Seq || res.Commit != good.Git.Commit || res.Commit == "" {
		t.Errorf("result = %+v, want revision %d at commit %q", res, good.Seq, good.Git.Commit)
	}

	// The stack is inline, with no git settings, no sync state, and nothing the poller or a webhook could reach.
	st, err := a.rr.Stack(ctx, "env-1", a.stackID)
	if err != nil || st.Source.Kind != stacks.SourceInline || st.Source.Ref != "" {
		t.Errorf("stack = %+v, %v, want an inline stack", st, err)
	}
	if _, err := a.rr.StackGit(ctx, a.stackID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("StackGit = %v, want ErrNoGit", err)
	}
	if n := a.rowCount(t, "stack_git") + a.rowCount(t, "stack_git_state"); n != 0 {
		t.Errorf("git rows left = %d, want 0", n)
	}
	if polled, err := a.rr.PolledStacks(ctx); err != nil || len(polled) != 0 {
		t.Errorf("PolledStacks after = %v, %v, want none", polled, err)
	}
	if _, err := a.rr.GitWebhookByToken(ctx, token); !errors.Is(err, stackops.ErrNoWebhook) {
		t.Errorf("GitWebhookByToken = %v, want ErrNoWebhook (the address answers 404)", err)
	}
	if a.ownedCount(t, gitsync.KindGitAutoDeploy) != 0 || a.ownedCount(t, gitsync.KindGitWebhook) != 0 {
		t.Error("an owned git automation is left")
	}

	// A check that was running finds nothing to write to and leaves no state behind (T-09-89).
	if err := a.rr.WriteGitState(ctx, stackops.GitState{StackID: a.stackID, Status: stackops.GitStatusInSync}); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("WriteGitState after the detach = %v, want ErrNoGit", err)
	}
	if err := a.rr.SetNextCheck(ctx, a.stackID, a.clock.now()); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("SetNextCheck after the detach = %v, want ErrNoGit", err)
	}
	if _, err := a.svc.Pull(ctx, "env-1", a.stackID, "bob", draft.Version+1); err == nil {
		t.Error("Pull of a detached stack succeeded")
	}
	if n := a.rowCount(t, "stack_git_state"); n != 0 {
		t.Errorf("state rows after the writes = %d, want 0", n)
	}

	// Nothing that runs changed: no deploy, no revision, the deployed revision is the one it was and keeps its commit.
	if a.dep.count() != calls {
		t.Errorf("deploys = %d, want %d: a detach deploys nothing", a.dep.count(), calls)
	}
	revsAfter, _, err := a.rr.ListRevisions(ctx, a.stackID)
	if err != nil || len(revsAfter) != len(revsBefore) {
		t.Errorf("revisions = %d, %v, want the %d it had", len(revsAfter), err, len(revsBefore))
	}
	after, _, err := a.rr.LatestGoodRevision(ctx, a.stackID)
	if err != nil || after.Seq != good.Seq || after.Git.Commit != good.Git.Commit {
		t.Errorf("deployed revision = %+v, %v, want #%d at %s unchanged", after.Seq, err, good.Seq, good.Git.Commit)
	}

	// The draft is the deployed files, as inline files; the overlay stays secret, and the edit that was never deployed is
	// replaced. The draft records no commit, so the editor stops treating its files as the repository's.
	got, err := a.rr.Draft(ctx, a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsDraft || got.Version != draft.Version+1 || got.Git.Commit != "" || got.BaseSeq != good.Seq {
		t.Errorf("draft = version %d commit %q base %d draft %v, want version %d, no commit, based on #%d",
			got.Version, got.Git.Commit, got.BaseSeq, got.IsDraft, draft.Version+1, good.Seq)
	}
	if len(got.Copy.Files) != len(good.Copy.Files) {
		t.Errorf("draft files = %d, want the %d of the deployed revision", len(got.Copy.Files), len(good.Copy.Files))
	}
	for _, f := range got.Copy.Files {
		if f.Origin != stacks.OriginInline {
			t.Errorf("file %s origin = %q, want inline", f.Path, f.Origin)
		}
		if f.Path == "secrets/pw.txt" && (!f.Secret || string(f.Content) != "pw-deployed") {
			t.Errorf("overlay file = %q secret %v, want the deployed content, still secret", f.Content, f.Secret)
		}
	}
	// The variables are kept, and the repository's .env (TAG=1), which an inline stack no longer reads, is now in the table
	// so the same values resolve (WR-C01).
	if !slices.Equal(got.Copy.ComposeFiles, good.Copy.ComposeFiles) || len(got.Copy.Vars) != len(good.Copy.Vars)+1 {
		t.Errorf("draft compose files %v vars %d, want %v and %d kept plus the .env's", got.Copy.ComposeFiles, len(got.Copy.Vars), good.Copy.ComposeFiles, len(good.Copy.Vars))
	}
	if env, err := stacks.InterpolationEnv(got.Copy); err != nil || env["TAG"] != "1" {
		t.Errorf("TAG after the detach = %q, %v, want 1 from the repository's .env", env["TAG"], err)
	}

	// It is not a git stack any more.
	if _, err := a.svc.Detach(ctx, "env-1", a.stackID, "bob"); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("second Detach = %v, want ErrNoGit", err)
	}
	if _, err := a.svc.Detach(ctx, "env-2", a.stackID, "bob"); !errors.Is(err, stackops.ErrStackNotFound) {
		t.Errorf("Detach in another environment = %v, want ErrStackNotFound", err)
	}
}

// A stack that was never deployed has no revision to copy: its own draft is what becomes editable.
func TestDetachOfAStackNeverDeployed(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	ctx := context.Background()
	before, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Detach(ctx, "env-1", f.stackID, "bob")
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if res.Revision != 0 || res.Commit != before.Git.Commit {
		t.Errorf("result = %+v, want revision 0 at the draft's commit %s", res, before.Git.Commit)
	}
	got, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Git.Commit != "" || len(got.Copy.Files) != len(before.Copy.Files) {
		t.Errorf("draft = %d files commit %q, want the same %d files with no commit", len(got.Copy.Files), got.Git.Commit, len(before.Copy.Files))
	}
	for _, file := range got.Copy.Files {
		if file.Origin != stacks.OriginInline {
			t.Errorf("file %s origin = %q, want inline", file.Path, file.Origin)
		}
	}
}
