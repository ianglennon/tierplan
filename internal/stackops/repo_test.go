// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

type repoFixture struct {
	repo  stackops.Repo
	envID string
}

func newRepo(t *testing.T) repoFixture {
	t.Helper()
	s := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	const envID = "env-1"
	for _, id := range []string{envID, "env-2"} {
		if _, err := s.Write.ExecContext(context.Background(),
			`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES (?, 'agent', ?, '', 1, 1, 1)`, id, "env "+id); err != nil {
			t.Fatalf("insert environment: %v", err)
		}
	}
	tick := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	return repoFixture{
		repo: stackops.Repo{Store: s, Vault: v, Now: func() time.Time {
			tick = tick.Add(time.Second)
			return tick
		}},
		envID: envID,
	}
}

func compose(content string) stacks.File {
	return stacks.File{Path: stacks.ComposeFile, Content: []byte(content)}
}

func TestCreateStackStoresTheStackAndItsFirstDraft(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, draft, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	if st.Name != "web" || st.Target != stacks.TargetSwarm || st.Source.Kind != stacks.SourceInline || st.CreatedBy != "ada" {
		t.Errorf("stack = %+v", st)
	}
	if draft.Kind != stackops.KindDraft || draft.Version != 1 || draft.CreatedBy != "ada" {
		t.Errorf("draft = %+v", draft)
	}
	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil || string(got.Copy.Compose()) != "services: {}\n" {
		t.Fatalf("Draft = %+v, %v", got, err)
	}
	read, err := f.repo.Stack(ctx, f.envID, st.ID)
	if err != nil || read.ID != st.ID {
		t.Errorf("Stack = %+v, %v", read, err)
	}
}

func TestCreateStackRefusesATakenName(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	if _, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{}); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "bob", stacks.WorkingCopy{})
	if !errors.Is(err, stackops.ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	list, err := f.repo.Stacks(ctx, f.envID)
	if err != nil || len(list) != 1 {
		t.Errorf("Stacks = %v, %v; want the one stack", list, err)
	}
}

func TestAStackIsOnlyFoundInItsOwnEnvironment(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	st, _, err := f.repo.CreateStack(context.Background(), f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Stack(context.Background(), "env-2", st.ID); !errors.Is(err, stackops.ErrStackNotFound) {
		t.Fatalf("err = %v, want ErrStackNotFound", err)
	}
}

func TestSaveDraftBumpsTheVersionAndKeepsTheContent(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	saved, changed, err := f.repo.SaveDraft(ctx, st.ID, 1, "bob", stacks.WorkingCopy{
		Files: []stacks.File{compose("b"), {Path: "nginx.conf", Content: []byte("worker_processes 1;")}},
		Vars:  []stacks.Var{{Name: "PORT", Value: "80"}},
	})
	if err != nil || !changed || saved.Version != 2 || saved.CreatedBy != "bob" {
		t.Fatalf("SaveDraft = %+v, %v, %v", saved, changed, err)
	}
	got, _ := f.repo.Draft(ctx, st.ID)
	if got.Version != 2 || string(got.Copy.Compose()) != "b" || len(got.Copy.Files) != 2 || len(got.Copy.Vars) != 1 || got.Copy.Vars[0].Value != "80" {
		t.Errorf("draft = %+v", got)
	}
}

func TestSavingTheSameContentAgainChangesNothing(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	wc := stacks.WorkingCopy{Files: []stacks.File{compose("a")}, Vars: []stacks.Var{{Name: "K", Value: "v", Secret: true}}}
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", wc)
	draft, changed, err := f.repo.SaveDraft(ctx, st.ID, 1, "bob", wc)
	if err != nil || changed {
		t.Fatalf("SaveDraft = %+v, %v, %v; want changed false", draft, changed, err)
	}
	if draft.Version != 1 || draft.CreatedBy != "ada" {
		t.Errorf("draft = %+v; the version and the saver must not move", draft)
	}
}

func TestSaveDraftAtAStaleVersionIsAConflictNamingTheSaver(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	if _, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "bob", stacks.WorkingCopy{Files: []stacks.File{compose("b")}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "cy", stacks.WorkingCopy{Files: []stacks.File{compose("c")}})
	var conflict *stackops.DraftConflictError
	if !errors.Is(err, stackops.ErrDraftConflict) || !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want a draft conflict", err)
	}
	if conflict.SavedBy != "bob" || conflict.Version != 2 || conflict.SavedAt.IsZero() {
		t.Errorf("conflict = %+v, want bob at version 2", conflict)
	}
	if got, _ := f.repo.Draft(ctx, st.ID); string(got.Copy.Compose()) != "b" {
		t.Errorf("the refused save changed the draft to %q", got.Copy.Compose())
	}
}

func TestSecretValuesAreSealedAtRestAndOpenedOnRead(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	wc := stacks.WorkingCopy{
		Files: []stacks.File{compose("a"), {Path: "key.pem", Content: []byte("TOP-SECRET-FILE"), Secret: true}},
		Vars:  []stacks.Var{{Name: "DB_PASSWORD", Value: "TOP-SECRET-VAR", Secret: true}, {Name: "PORT", Value: "80"}},
	}
	st, draft, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", wc)
	if err != nil {
		t.Fatal(err)
	}
	var plainVar, sealedVar, plainFile, sealedFile string
	row := f.repo.Store.Read.QueryRowContext(ctx, `SELECT value, value_sealed FROM stack_snapshot_vars WHERE snapshot_id = ? AND name = 'DB_PASSWORD'`, draft.ID)
	if err := row.Scan(&plainVar, &sealedVar); err != nil {
		t.Fatal(err)
	}
	row = f.repo.Store.Read.QueryRowContext(ctx, `SELECT content, content_sealed FROM stack_snapshot_files WHERE snapshot_id = ? AND path = 'key.pem'`, draft.ID)
	if err := row.Scan(&plainFile, &sealedFile); err != nil {
		t.Fatal(err)
	}
	if plainVar != "" || plainFile != "" || sealedVar == "" || sealedFile == "" ||
		strings.Contains(sealedVar, "TOP-SECRET") || strings.Contains(sealedFile, "TOP-SECRET") {
		t.Errorf("secret values are stored as var %q/%q file %q/%q", plainVar, sealedVar, plainFile, sealedFile)
	}
	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Copy.Vars[0]; v.Name != "DB_PASSWORD" || v.Value != "TOP-SECRET-VAR" || !v.Secret {
		t.Errorf("variable = %+v, want the opened secret", v)
	}
	if string(got.Copy.Files[1].Content) != "TOP-SECRET-FILE" {
		t.Errorf("file = %+v", got.Copy.Files[1])
	}
}

func TestASealedValueDoesNotOpenUnderAnotherRow(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, draft, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{
		Vars: []stacks.Var{{Name: "A", Value: "one", Secret: true}, {Name: "B", Value: "two", Secret: true}},
	})
	// Swap the two sealed values: each now sits under the other's name.
	if _, err := f.repo.Store.Write.ExecContext(ctx, `UPDATE stack_snapshot_vars SET value_sealed = CASE name
		WHEN 'A' THEN (SELECT value_sealed FROM stack_snapshot_vars WHERE snapshot_id = ? AND name = 'B')
		ELSE (SELECT value_sealed FROM stack_snapshot_vars WHERE snapshot_id = ? AND name = 'A') END WHERE snapshot_id = ?`,
		draft.ID, draft.ID, draft.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Draft(ctx, st.ID); err == nil {
		t.Fatal("a sealed value copied to another name opened")
	}
}

func TestNewRevisionCopiesTheDraftAndSealsSecretsUnderTheNewSnapshot(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	wc := stacks.WorkingCopy{Files: []stacks.File{compose("a")}, Vars: []stacks.Var{{Name: "K", Value: "secret", Secret: true}}}
	st, draft, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", wc)
	rev1, err := f.repo.NewRevision(ctx, st.ID, "bob")
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	rev2, err := f.repo.NewRevision(ctx, st.ID, "bob")
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if rev1.Seq != 1 || rev2.Seq != 2 || rev1.ID == draft.ID || rev1.Outcome != stackops.OutcomePending || rev1.Kind != stackops.KindRevision {
		t.Errorf("revisions = %+v / %+v", rev1, rev2)
	}
	if err := f.repo.SetOutcome(ctx, rev1.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	good, found, err := f.repo.LatestGoodRevision(ctx, st.ID)
	if err != nil || !found || good.ID != rev1.ID {
		t.Fatalf("LatestGoodRevision = %+v, %v, %v", good, found, err)
	}
	if v := good.Copy.Vars[0]; v.Value != "secret" {
		t.Errorf("revision variable = %+v, want the secret opened under the revision's own context", v)
	}
	// The draft is untouched by taking revisions.
	if d, _ := f.repo.Draft(ctx, st.ID); d.ID != draft.ID || d.Version != 1 {
		t.Errorf("draft = %+v", d)
	}
}

func TestADeployRowIsRunningUntilItIsFinished(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{})
	row, err := f.repo.StartDeploy(ctx, st.ID, f.envID, stackops.DeployKindDeploy, 1, "job-1", "bob")
	if err != nil || row.State != stackops.DeployRunning {
		t.Fatalf("StartDeploy = %+v, %v", row, err)
	}
	latest, found, _ := f.repo.LatestDeploy(ctx, st.ID)
	if !found || latest.ID != row.ID || latest.State != stackops.DeployRunning || latest.JobID != "job-1" {
		t.Errorf("LatestDeploy = %+v, %v", latest, found)
	}
	if err := f.repo.FinishDeploy(ctx, row.ID, stackops.DeployFailed, "web_api", "service_rejected"); err != nil {
		t.Fatal(err)
	}
	latest, _, _ = f.repo.LatestDeploy(ctx, st.ID)
	if latest.State != stackops.DeployFailed || latest.FailedService != "web_api" || latest.Code != "service_rejected" || latest.EndedAt.IsZero() {
		t.Errorf("finished deploy = %+v", latest)
	}
	if err := f.repo.FinishDeploy(ctx, row.ID, stackops.DeployDone, "", ""); err == nil {
		t.Error("a finished deploy was finished again")
	}
	all, err := f.repo.LatestDeploys(ctx, f.envID)
	if err != nil || all[st.ID].ID != row.ID {
		t.Errorf("LatestDeploys = %v, %v", all, err)
	}
}

func TestDraftStatesSeeASaveAfterTheLastDeploy(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	states := func() bool {
		m, err := f.repo.DraftStates(ctx, f.envID)
		if err != nil {
			t.Fatal(err)
		}
		return m[st.ID]
	}
	if states() {
		t.Error("a stack nobody has saved reads as having a draft")
	}
	if _, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("b")}}); err != nil {
		t.Fatal(err)
	}
	if !states() {
		t.Error("a saved, undeployed draft does not read as a draft")
	}
	if _, err := f.repo.NewRevision(ctx, st.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	if states() {
		t.Error("a deployed draft still reads as a draft")
	}
	if _, _, err := f.repo.SaveDraft(ctx, st.ID, 2, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("c")}}); err != nil {
		t.Fatal(err)
	}
	if !states() {
		t.Error("a save after the deploy does not read as a draft")
	}
}

// Discarding a draft (plan 07-03, STK-05, D-08): the draft goes, and the editor falls back to what
// is deployed.

func fileSnapshotCount(t *testing.T, f repoFixture, snapshotID string) int {
	t.Helper()
	var n int
	row := f.repo.Store.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM stack_snapshot_files WHERE snapshot_id = ?) + (SELECT COUNT(*) FROM stack_snapshot_vars WHERE snapshot_id = ?)`, snapshotID, snapshotID)
	if err := row.Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAStoredDraftIsADraft(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, created, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	if !created.IsDraft {
		t.Error("the draft CreateStack answers is not marked IsDraft")
	}
	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil || !got.IsDraft || got.Version != 1 {
		t.Errorf("Draft = %+v, %v; want a stored draft at version 1", got, err)
	}
}

func TestDiscardDraftDeletesTheDraftWithItsFilesAndVariables(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, first, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	if _, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "ada", stacks.WorkingCopy{
		Files: []stacks.File{compose("b"), {Path: "key.pem", Content: []byte("k"), Secret: true}},
		Vars:  []stacks.Var{{Name: "K", Value: "v", Secret: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if fileSnapshotCount(t, f, first.ID) == 0 {
		t.Fatal("the draft holds no content to discard")
	}
	discarded, version, err := f.repo.DiscardDraft(ctx, st.ID)
	if err != nil || !discarded || version != 2 {
		t.Fatalf("DiscardDraft = %v, %d, %v; want true at version 2", discarded, version, err)
	}
	if n := fileSnapshotCount(t, f, first.ID); n != 0 {
		t.Errorf("%d files and variables of the discarded draft are still stored", n)
	}
	var drafts int
	if err := f.repo.Store.Read.QueryRow(`SELECT COUNT(*) FROM stack_snapshots WHERE stack_id = ? AND kind = 'draft'`, st.ID).Scan(&drafts); err != nil || drafts != 0 {
		t.Errorf("%d drafts stored after a discard (%v)", drafts, err)
	}
	if _, err := f.repo.Stack(ctx, f.envID, st.ID); err != nil {
		t.Errorf("the stack is gone after its draft was discarded: %v", err)
	}
}

func TestDiscardingWhenThereIsNoDraftIsNotAnError(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	if discarded, _, err := f.repo.DiscardDraft(ctx, st.ID); err != nil || !discarded {
		t.Fatalf("first DiscardDraft = %v, %v", discarded, err)
	}
	discarded, version, err := f.repo.DiscardDraft(ctx, st.ID)
	if err != nil || discarded || version != 0 {
		t.Errorf("second DiscardDraft = %v, %d, %v; want false, 0, nil", discarded, version, err)
	}
}

func TestDraftOfAStackWithNoneFallsBackToTheLastGoodRevision(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{
		Files: []stacks.File{compose("deployed")}, Vars: []stacks.Var{{Name: "PORT", Value: "80"}},
	})
	rev, err := f.repo.NewRevision(ctx, st.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	// A later revision that failed is not what is deployed.
	failed, _ := f.repo.NewRevision(ctx, st.ID, "bob")
	if err := f.repo.SetOutcome(ctx, failed.ID, stackops.OutcomeFailed, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("edited")}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.repo.DiscardDraft(ctx, st.ID); err != nil {
		t.Fatal(err)
	}

	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatalf("Draft after a discard: %v", err)
	}
	if got.IsDraft || got.Version != 0 || got.BaseSeq != rev.Seq {
		t.Errorf("fallback = IsDraft %v, version %d, base %d; want false, 0, %d", got.IsDraft, got.Version, got.BaseSeq, rev.Seq)
	}
	if string(got.Copy.Compose()) != "deployed" || len(got.Copy.Vars) != 1 || got.Copy.Vars[0].Value != "80" {
		t.Errorf("fallback content = %+v, want the last good revision's", got.Copy)
	}
	// The revisions are untouched by the discard.
	if good, found, _ := f.repo.LatestGoodRevision(ctx, st.ID); !found || good.ID != rev.ID {
		t.Errorf("LatestGoodRevision = %+v, %v", good, found)
	}
}

func TestDraftOfAStackNeverDeployedFallsBackToTheStarter(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("mine")}})
	if _, _, err := f.repo.DiscardDraft(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil || got.IsDraft || got.Version != 0 || got.BaseSeq != 0 {
		t.Fatalf("Draft = %+v, %v; want the fallback at version 0", got, err)
	}
	if string(got.Copy.Compose()) != stacks.StarterCompose || len(got.Copy.Files) != 1 {
		t.Errorf("fallback = %+v, want only the starter compose.yaml", got.Copy)
	}
}

func TestSavingAfterADiscardStartsAFreshDraft(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	rev, _ := f.repo.NewRevision(ctx, st.ID, "ada")
	if err := f.repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.repo.DiscardDraft(ctx, st.ID); err != nil {
		t.Fatal(err)
	}

	saved, changed, err := f.repo.SaveDraft(ctx, st.ID, 0, "bob", stacks.WorkingCopy{Files: []stacks.File{compose("b")}})
	if err != nil || !changed {
		t.Fatalf("SaveDraft after a discard = %+v, %v, %v", saved, changed, err)
	}
	// Version 1 is the draft a stack is created with; every saved draft is later than that, which
	// is how the list tells a draft with changes from one nobody touched.
	if !saved.IsDraft || saved.Version != 2 || saved.CreatedBy != "bob" || saved.BaseSeq != rev.Seq {
		t.Errorf("fresh draft = %+v, want a draft at version 2 by bob based on revision %d", saved, rev.Seq)
	}
	got, _ := f.repo.Draft(ctx, st.ID)
	if !got.IsDraft || string(got.Copy.Compose()) != "b" {
		t.Errorf("Draft = %+v", got)
	}
	states, _ := f.repo.DraftStates(ctx, f.envID)
	if !states[st.ID] {
		t.Error("the fresh draft does not read as having changes")
	}
}

func TestSavingContentEqualToTheFallbackWritesNoDraft(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	if _, _, err := f.repo.DiscardDraft(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	same := stacks.WorkingCopy{Files: []stacks.File{compose(stacks.StarterCompose)}}
	saved, changed, err := f.repo.SaveDraft(ctx, st.ID, 0, "bob", same)
	if err != nil || changed || saved.IsDraft {
		t.Fatalf("SaveDraft of the fallback = %+v, %v, %v; want nothing written", saved, changed, err)
	}
	if got, _ := f.repo.Draft(ctx, st.ID); got.IsDraft {
		t.Error("a draft was written for content equal to what is deployed")
	}
}

func TestSavingAtAVersionAfterTheDraftWasDiscardedIsAConflict(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	if _, _, err := f.repo.DiscardDraft(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "bob", stacks.WorkingCopy{Files: []stacks.File{compose("b")}})
	var conflict *stackops.DraftConflictError
	if !errors.As(err, &conflict) || conflict.Version != 0 {
		t.Fatalf("err = %v, want a conflict at version 0: the draft this editor read is gone", err)
	}
	if got, _ := f.repo.Draft(ctx, st.ID); got.IsDraft {
		t.Error("a save at a stale version wrote a draft")
	}
}

func TestSavingAtVersionZeroWhenADraftExistsIsAConflict(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("a")}})
	_, _, err := f.repo.SaveDraft(ctx, st.ID, 0, "bob", stacks.WorkingCopy{Files: []stacks.File{compose("b")}})
	var conflict *stackops.DraftConflictError
	if !errors.As(err, &conflict) || conflict.SavedBy != "ada" || conflict.Version != 1 {
		t.Fatalf("err = %v, want a conflict naming ada at version 1", err)
	}
}

func TestPinsAreStoredOnARevisionAndTheLastGoodOnesReadBack(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("x")}})
	if got, err := f.repo.GoodPins(ctx, st.ID); err != nil || got != nil {
		t.Fatalf("GoodPins of a stack with no good revision = %v, %v; want none", got, err)
	}

	good, _ := f.repo.NewRevision(ctx, st.ID, "ada")
	if err := f.repo.SetPins(ctx, good.ID, map[string]string{"web_app": "nginx:1.27@sha256:aaa", "web_db": "registry.example:5000/db:2@sha256:bbb"}); err != nil {
		t.Fatalf("SetPins: %v", err)
	}
	// The outcome of the deploy does not wipe what the deploy stored.
	if err := f.repo.SetOutcome(ctx, good.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	// A later revision that failed, with its own pins, is not what runs.
	failed, _ := f.repo.NewRevision(ctx, st.ID, "ada")
	if err := f.repo.SetPins(ctx, failed.ID, map[string]string{"web_app": "nginx:1.28@sha256:ccc"}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, failed.ID, stackops.OutcomeFailed, "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := f.repo.GoodPins(ctx, st.ID)
	if err != nil {
		t.Fatalf("GoodPins: %v", err)
	}
	want := map[string]stacks.LivePin{
		"web_app": {AsWritten: "nginx:1.27", Image: "nginx:1.27@sha256:aaa"},
		"web_db":  {AsWritten: "registry.example:5000/db:2", Image: "registry.example:5000/db:2@sha256:bbb"},
	}
	if len(got) != len(want) || got["web_app"] != want["web_app"] || got["web_db"] != want["web_db"] {
		t.Errorf("GoodPins = %+v, want %+v", got, want)
	}
}

func TestSetPinsRefusesARevisionThatDoesNotExistAndADraft(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, draft, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("x")}})
	if err := f.repo.SetPins(ctx, "nope", map[string]string{}); err == nil {
		t.Error("SetPins of an unknown snapshot succeeded")
	}
	if err := f.repo.SetPins(ctx, draft.ID, map[string]string{"web_app": "nginx@sha256:aaa"}); err == nil {
		t.Errorf("SetPins of the draft of %s succeeded: only a revision has pins", st.ID)
	}
}

// Secret stack files (plan 07-13, STK-04, UI-SPEC Defaults 6): a file the compose file uses under
// secrets: is stored sealed, and stays sealed until the file is deleted.

const composeWithTokFile = "services:\n  web:\n    image: nginx:1.27\nsecrets:\n  tok:\n    file: tok.txt\n"

func filesOf(compose string, extra ...stacks.File) stacks.WorkingCopy {
	return stacks.WorkingCopy{Files: append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, extra...)}
}

// storedFile reads how a snapshot holds one file: its plain content, its sealed content and whether
// it is flagged secret.
func storedFile(t *testing.T, f repoFixture, snapshotID, path string) (plain, sealed string, secret bool) {
	t.Helper()
	var flag int
	row := f.repo.Store.Read.QueryRowContext(context.Background(),
		`SELECT content, content_sealed, secret FROM stack_snapshot_files WHERE snapshot_id = ? AND path = ?`, snapshotID, path)
	if err := row.Scan(&plain, &sealed, &flag); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return plain, sealed, flag == 1
}

func TestSavingADraftSealsAFileTheComposeFileUsesAsASecret(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, draft, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", filesOf("services: {}\n"))

	// The browser sends tok.txt as a plain file; the server decides it is secret from the reference.
	saved, changed, err := f.repo.SaveDraft(ctx, st.ID, 1, "ada", filesOf(composeWithTokFile,
		stacks.File{Path: "tok.txt", Content: []byte("TOP-SECRET-TOKEN")},
		stacks.File{Path: "notes.txt", Content: []byte("visible notes")}))
	if err != nil || !changed {
		t.Fatalf("SaveDraft = %v, %v", changed, err)
	}
	plain, sealed, secret := storedFile(t, f, saved.ID, "tok.txt")
	if !secret || plain != "" || sealed == "" || strings.Contains(sealed, "TOP-SECRET") {
		t.Errorf("tok.txt is stored secret=%v plain=%q sealed=%q, want it flagged and sealed", secret, plain, sealed)
	}
	if plain, sealed, secret := storedFile(t, f, saved.ID, "notes.txt"); secret || plain != "visible notes" || sealed != "" {
		t.Errorf("notes.txt is stored secret=%v plain=%q sealed=%q, want it plain", secret, plain, sealed)
	}
	if draft.ID != saved.ID {
		t.Errorf("the save made a new snapshot %s, want the draft %s updated", saved.ID, draft.ID)
	}

	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range got.Copy.Files {
		if file.Path == "tok.txt" && (!file.Secret || string(file.Content) != "TOP-SECRET-TOKEN") {
			t.Errorf("tok.txt read back = %+v, want the opened secret", file)
		}
	}
}

func TestAFileStaysSecretWhenTheReferenceIsRemovedUntilItIsDeleted(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", filesOf(composeWithTokFile, stacks.File{Path: "tok.txt", Content: []byte("TOP-SECRET-TOKEN")}))
	first, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "ada", filesOf(composeWithTokFile, stacks.File{Path: "tok.txt", Content: []byte("TOP-SECRET-TOKEN")}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, secret := storedFile(t, f, first.ID, "tok.txt"); !secret {
		t.Fatal("tok.txt is not secret after the first save")
	}

	// The reference goes; the browser still sends the file plain. It stays sealed.
	after, changed, err := f.repo.SaveDraft(ctx, st.ID, first.Version, "ada", filesOf("services: {}\n", stacks.File{Path: "tok.txt", Content: []byte("TOP-SECRET-TOKEN")}))
	if err != nil || !changed {
		t.Fatalf("SaveDraft = %v, %v", changed, err)
	}
	if plain, sealed, secret := storedFile(t, f, after.ID, "tok.txt"); !secret || plain != "" || sealed == "" {
		t.Errorf("tok.txt without a reference is stored secret=%v plain=%q sealed=%q, want it still sealed", secret, plain, sealed)
	}

	// Deleting it ends that: a file added again under the name is a new, plain file.
	deleted, _, err := f.repo.SaveDraft(ctx, st.ID, after.Version, "ada", filesOf("services: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	readded, _, err := f.repo.SaveDraft(ctx, st.ID, deleted.Version, "ada", filesOf("services: {}\n", stacks.File{Path: "tok.txt", Content: []byte("plain again")}))
	if err != nil {
		t.Fatal(err)
	}
	if plain, _, secret := storedFile(t, f, readded.ID, "tok.txt"); secret || plain != "plain again" {
		t.Errorf("a re-added tok.txt is stored secret=%v plain=%q, want a new plain file", secret, plain)
	}
}

func TestSavingTheSameSecretFileAgainChangesNothing(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", filesOf("services: {}\n"))
	copy := filesOf(composeWithTokFile, stacks.File{Path: "tok.txt", Content: []byte("TOP-SECRET-TOKEN")})
	first, changed, err := f.repo.SaveDraft(ctx, st.ID, 1, "ada", copy)
	if err != nil || !changed {
		t.Fatalf("first save = %v, %v", changed, err)
	}
	// The same plain file sent again is the same content, because the flag is decided on the server.
	again, changed, err := f.repo.SaveDraft(ctx, st.ID, first.Version, "ada", copy)
	if err != nil || changed || again.Version != first.Version {
		t.Errorf("second save = version %d changed %v err %v, want nothing written", again.Version, changed, err)
	}
}

func TestARevisionKeepsASecretFileSealedUnderItsOwnRow(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", filesOf("services: {}\n"))
	if _, _, err := f.repo.SaveDraft(ctx, st.ID, 1, "ada", filesOf(composeWithTokFile, stacks.File{Path: "tok.txt", Content: []byte("TOP-SECRET-TOKEN")})); err != nil {
		t.Fatal(err)
	}
	rev, err := f.repo.NewRevision(ctx, st.ID, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if plain, sealed, secret := storedFile(t, f, rev.ID, "tok.txt"); !secret || plain != "" || sealed == "" {
		t.Errorf("the revision holds tok.txt secret=%v plain=%q sealed=%q, want it sealed", secret, plain, sealed)
	}
	if err := f.repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	good, found, err := f.repo.LatestGoodRevision(ctx, st.ID)
	if err != nil || !found {
		t.Fatalf("LatestGoodRevision = %v, %v", found, err)
	}
	for _, file := range good.Copy.Files {
		if file.Path == "tok.txt" && string(file.Content) != "TOP-SECRET-TOKEN" {
			t.Errorf("revision file = %+v, want the content opened under the revision's own context", file)
		}
	}
}

// The rendering of a revision (plan 07-16, D-11): what the next review compares the draft's own
// rendering with.

func TestARevisionKeepsItsRenderingThroughItsOutcomeAndOnlyAGoodOneIsTheBase(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("x")}})
	if got, err := f.repo.GoodRendering(ctx, st.ID); err != nil || got != nil {
		t.Fatalf("GoodRendering of a stack with no good revision = %q, %v; want none", got, err)
	}

	good, _ := f.repo.NewRevision(ctx, st.ID, "ada")
	if err := f.repo.SetRendered(ctx, good.ID, []byte("services:\n  web: {}\n")); err != nil {
		t.Fatalf("SetRendered: %v", err)
	}
	// Ending the deploy does not wipe the rendering the deploy stored when it started.
	if err := f.repo.SetOutcome(ctx, good.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	failed, _ := f.repo.NewRevision(ctx, st.ID, "ada")
	if err := f.repo.SetRendered(ctx, failed.ID, []byte("services:\n  other: {}\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, failed.ID, stackops.OutcomeFailed, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.GoodRendering(ctx, st.ID)
	if err != nil || string(got) != "services:\n  web: {}\n" {
		t.Errorf("GoodRendering = %q, %v; want the good revision's, not the failed one's", got, err)
	}
}

func TestSetRenderedRefusesARevisionThatDoesNotExistAndADraft(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	_, draft, _ := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("x")}})
	if err := f.repo.SetRendered(ctx, "nope", []byte("x")); err == nil {
		t.Error("SetRendered of an unknown snapshot succeeded")
	}
	if err := f.repo.SetRendered(ctx, draft.ID, []byte("x")); err == nil {
		t.Error("SetRendered of the draft succeeded: only a revision has a rendering")
	}
}
