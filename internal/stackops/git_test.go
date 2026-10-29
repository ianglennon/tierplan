// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Stacks made from a repository folder (plan 09-10, GIT-02, D-06, D-08, D-14, D-18): the draft holds the
// repository's files and what they were made from, beside a stack_git row and a state row.

const (
	gitRepoID = "repo-1"
	gitCommit = "0123456789abcdef0123456789abcdef01234567"
	gitTree   = "89abcdef0123456789abcdef0123456789abcdef"
)

// addGitRepo stores a repository row for a stack to point at.
func (f repoFixture) addGitRepo(t *testing.T, id, name string) {
	t.Helper()
	if _, err := f.repo.Store.Write.ExecContext(context.Background(),
		`INSERT INTO git_repos (id, env_id, name, url, created_at, updated_at, created_by) VALUES (?, NULL, ?, 'https://git.example/acme/stacks.git', 1, 1, 'u')`,
		id, name); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
}

func gitSource() stackops.GitSource {
	return stackops.GitSource{
		RepoID: gitRepoID, RepoName: "acme", RefKind: "branch", Ref: "main", Folder: "stacks/web",
		ComposeFiles: []string{"compose.yaml", "compose.prod.yaml"},
	}
}

func gitRevision() stackops.GitRevision {
	return stackops.GitRevision{Commit: gitCommit, Tree: gitTree, Source: gitSource()}
}

func repoFile(path, content string) stacks.File {
	return stacks.File{Path: path, Content: []byte(content), Origin: stacks.OriginRepo}
}

func gitInput(f repoFixture, name string) stackops.CreateGitStackInput {
	return stackops.CreateGitStackInput{
		EnvID: f.envID, Name: name, Target: stacks.TargetStandalone, Actor: "ada",
		Copy: stacks.WorkingCopy{
			Files: []stacks.File{
				repoFile("compose.yaml", "services: {}\n"), repoFile("compose.prod.yaml", "services: {}\n"), repoFile(".env", "TAG=1\n"),
			},
			Vars:         []stacks.Var{{Name: "TAG", Value: "2"}},
			ComposeFiles: []string{"compose.yaml", "compose.prod.yaml"},
		},
		Rev: gitRevision(),
	}
}

func TestCreateGitStackRows(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()

	st, draft, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatalf("CreateGitStack: %v", err)
	}
	if st.Source.Kind != stacks.SourceGit || st.Source.Ref != gitRepoID || st.Target != stacks.TargetStandalone || st.CreatedBy != "ada" {
		t.Errorf("stack = %+v, want source git with the repository ID as its ref", st)
	}
	read, err := f.repo.Stack(ctx, f.envID, st.ID)
	if err != nil || read.Source.Kind != stacks.SourceGit || read.Source.Ref != gitRepoID {
		t.Fatalf("Stack = %+v, %v", read, err)
	}

	// The draft holds the repository files, the variables, the ordered compose files and the revision fields.
	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsDraft || got.Version != 1 || draft.Version != 1 {
		t.Errorf("draft = %+v", got)
	}
	var paths []string
	for _, file := range got.Copy.Files {
		paths = append(paths, file.Path)
		if file.Origin != stacks.OriginRepo {
			t.Errorf("file %s has origin %q, want repo", file.Path, file.Origin)
		}
	}
	if !slices.Equal(paths, []string{".env", "compose.prod.yaml", "compose.yaml"}) {
		t.Errorf("files = %v", paths)
	}
	if !slices.Equal(got.Copy.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) || len(got.Copy.Vars) != 1 || got.Copy.Vars[0].Value != "2" {
		t.Errorf("copy = %+v, want the compose files in order and the variable", got.Copy)
	}
	if got.Git.Commit != gitCommit || got.Git.Tree != gitTree || got.Git.Source.Folder != "stacks/web" || got.Git.Source.RepoName != "acme" ||
		!slices.Equal(got.Git.Source.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) || got.SyncTrigger != "" {
		t.Errorf("git = %+v", got.Git)
	}

	// The settings: polling every 5 minutes, auto-deploy off, no webhook.
	g, err := f.repo.StackGit(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.RepoID != gitRepoID || g.RefKind != "branch" || g.Ref != "main" || g.Folder != "stacks/web" ||
		!slices.Equal(g.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) ||
		!g.PollEnabled || g.PollIntervalSeconds != 300 || g.AutoDeploy || g.WebhookFlavour != "" || g.WebhookTokenHash != "" || g.PendingConvert {
		t.Errorf("stack_git = %+v", g)
	}
	// The state: not checked yet.
	state, err := f.repo.GitState(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != stackops.GitStatusUnknown || state.Behind != 0 || state.CheckedAt != nil || state.HeadCommit != "" {
		t.Errorf("state = %+v, want unknown and never checked", state)
	}

	// Nothing is deployed: no revision, no deploy row.
	if rows, _, err := f.repo.ListRevisions(ctx, st.ID); err != nil || len(rows) != 0 {
		t.Errorf("revisions = %v, %v, want none", rows, err)
	}
	if _, ok, err := f.repo.LatestDeploy(ctx, st.ID); err != nil || ok {
		t.Errorf("LatestDeploy = %v, %v, want no operation", ok, err)
	}
}

func TestCreateGitStackRefusesATakenNameAndStoresNothing(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	if _, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetStandalone, "ada", stacks.WorkingCopy{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web")); !errors.Is(err, stackops.ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	var n int
	if err := f.repo.Store.Read.QueryRow(`SELECT (SELECT count(*) FROM stack_git) + (SELECT count(*) FROM stack_git_state)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("git rows = %d, %v, want none", n, err)
	}
}

func TestCreateGitStackNeedsComposeFilesAndARepository(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	in := gitInput(f, "web")
	in.Copy.ComposeFiles, in.Rev.Source.ComposeFiles = nil, nil
	if _, _, err := f.repo.CreateGitStack(ctx, in); !errors.Is(err, stackops.ErrNoComposeFiles) {
		t.Errorf("no compose files: err = %v, want ErrNoComposeFiles", err)
	}
	// The repository row is missing: the foreign key refuses it and nothing is stored.
	if _, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web")); err == nil {
		t.Error("a stack of a repository that does not exist was stored")
	}
	if stacksOf, err := f.repo.Stacks(ctx, f.envID); err != nil || len(stacksOf) != 0 {
		t.Errorf("stacks = %v, %v, want none", stacksOf, err)
	}
}

func TestAStackThatIsNotFromGitHasNoGitSettings(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "plain", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.StackGit(ctx, st.ID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("StackGit: err = %v, want ErrNoGit", err)
	}
	if _, err := f.repo.GitState(ctx, st.ID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("GitState: err = %v, want ErrNoGit", err)
	}
}

func TestAGitStateThatWasNotRestoredReadsAsUnknown(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	// A backup leaves the state rows out: the stack must still read.
	if _, err := f.repo.Store.Write.ExecContext(ctx, `DELETE FROM stack_git_state`); err != nil {
		t.Fatal(err)
	}
	state, err := f.repo.GitState(ctx, st.ID)
	if err != nil || state.Status != stackops.GitStatusUnknown || state.StackID != st.ID {
		t.Errorf("state = %+v, %v, want unknown", state, err)
	}
}

// A person's save of the draft cannot see where a file came from or the order of the compose files, so it keeps them.
func TestSavingAGitDraftKeepsItsComposeFilesAndOrigins(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	// The editor sends the files it was given, with no origin and no compose file list.
	edited := stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: "compose.yaml", Content: []byte("services: {a: {}}\n")},
			{Path: "compose.prod.yaml", Content: []byte("services: {}\n")},
			{Path: ".env", Content: []byte("TAG=1\n")},
		},
	}
	saved, changed, err := f.repo.SaveDraft(ctx, st.ID, 1, "bob", edited)
	if err != nil || !changed {
		t.Fatalf("SaveDraft: %v, changed %v", err, changed)
	}
	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Copy.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) || !slices.Equal(saved.Copy.ComposeFiles, got.Copy.ComposeFiles) {
		t.Errorf("compose files = %v, want the order kept", got.Copy.ComposeFiles)
	}
	for _, file := range got.Copy.Files {
		if file.Origin != stacks.OriginRepo {
			t.Errorf("file %s has origin %q after a save, want repo", file.Path, file.Origin)
		}
	}
	if got.Git.Commit != gitCommit || got.Git.Tree != gitTree {
		t.Errorf("git = %+v, want the commit and tree the files were read at", got.Git)
	}
}

// An inline stack's snapshots keep the defaults of the git columns.
func TestInlineSnapshotsHaveNoGitColumns(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "plain", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	var commit, tree, source, files, trigger string
	if err := f.repo.Store.Read.QueryRow(`SELECT git_commit, git_tree, git_source, compose_files, sync_trigger FROM stack_snapshots WHERE stack_id = ?`, st.ID).
		Scan(&commit, &tree, &source, &files, &trigger); err != nil {
		t.Fatal(err)
	}
	if commit != "" || tree != "" || source != "{}" || files != "[]" || trigger != "" {
		t.Errorf("columns = %q %q %q %q %q, want the defaults", commit, tree, source, files, trigger)
	}
	got, err := f.repo.Draft(ctx, st.ID)
	if err != nil || len(got.Copy.ComposeFiles) != 0 || got.Git.Commit != "" || got.Git.Source.RepoID != "" {
		t.Errorf("draft = %+v, %v", got, err)
	}
}

// moveDraftTo makes the draft of a stack stand at another commit, as a sync of the repository will.
func (f repoFixture) moveDraftTo(t *testing.T, stackID, commit, tree string) {
	t.Helper()
	source := `{"repoId":"` + gitRepoID + `","repoName":"acme","refKind":"branch","ref":"main","folder":"stacks/web","composeFiles":["compose.yaml"]}`
	if _, err := f.repo.Store.Write.ExecContext(context.Background(),
		`UPDATE stack_snapshots SET git_commit = ?, git_tree = ?, git_source = ?, compose_files = '["compose.yaml"]' WHERE stack_id = ? AND kind = 'draft'`,
		commit, tree, source, stackID); err != nil {
		t.Fatalf("move the draft: %v", err)
	}
}

func TestGitRevisionRoundTrip(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.repo.NewRevision(ctx, st.ID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if rev.Git.Commit != gitCommit || rev.Git.Tree != gitTree || rev.Git.Source.Folder != "stacks/web" {
		t.Errorf("new revision git = %+v, want the draft's commit, tree and source", rev.Git)
	}

	// Read back from the store, the revision is the draft's content with its origins and compose files.
	got, err := f.repo.Revision(ctx, st.ID, rev.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if got.Git.Commit != gitCommit || got.Git.Tree != gitTree || got.Git.Source.RepoID != gitRepoID || got.Git.Source.RepoName != "acme" ||
		got.Git.Source.RefKind != "branch" || got.Git.Source.Ref != "main" || got.Git.Source.Folder != "stacks/web" ||
		!slices.Equal(got.Git.Source.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) {
		t.Errorf("revision git = %+v", got.Git)
	}
	if !slices.Equal(got.Copy.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) {
		t.Errorf("revision compose files = %v", got.Copy.ComposeFiles)
	}
	for _, file := range got.Copy.Files {
		if file.Origin != stacks.OriginRepo {
			t.Errorf("revision file %s has origin %q, want repo", file.Path, file.Origin)
		}
	}
	// It restores without the mirror and survives a force-push: nothing about it is read from the repository, so
	// moving the draft on leaves the revision as it was.
	f.moveDraftTo(t, st.ID, "ffffffffffffffffffffffffffffffffffffffff", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	again, err := f.repo.Revision(ctx, st.ID, rev.Seq)
	if err != nil || again.Git.Commit != gitCommit || again.Git.Tree != gitTree {
		t.Errorf("revision after the draft moved = %+v, %v", again.Git, err)
	}
}

func TestGitRollbackCopiesRestoredSource(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.repo.NewRevision(ctx, st.ID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, first.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	// A later sync moved the draft to another commit and folder; its revision is good too.
	const newCommit, newTree = "ffffffffffffffffffffffffffffffffffffffff", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	f.moveDraftTo(t, st.ID, newCommit, newTree)
	second, err := f.repo.NewRevision(ctx, st.ID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if second.Git.Commit != newCommit {
		t.Fatalf("second revision git = %+v, want the moved draft's", second.Git)
	}

	// A roll back to the first revision copies the first revision's source, not the draft's.
	from, err := f.repo.Revision(ctx, st.ID, first.Seq)
	if err != nil {
		t.Fatal(err)
	}
	back, err := f.repo.CopyRevision(ctx, from, "carol", stackops.OriginRollback)
	if err != nil {
		t.Fatalf("CopyRevision: %v", err)
	}
	if back.Git.Commit != gitCommit || back.Git.Tree != gitTree {
		t.Errorf("rollback git = %+v, want the restored revision's commit and tree", back.Git)
	}
	got, err := f.repo.Revision(ctx, st.ID, back.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if got.Git.Commit != gitCommit || got.Git.Tree != gitTree || !slices.Equal(got.Git.Source.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) ||
		!slices.Equal(got.Copy.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) || got.RestoredFrom != first.Seq {
		t.Errorf("stored rollback = %+v, want the first revision's source and compose files, restored from %d", got, first.Seq)
	}
	for _, file := range got.Copy.Files {
		if file.Origin != stacks.OriginRepo {
			t.Errorf("rollback file %s has origin %q, want repo", file.Path, file.Origin)
		}
	}
	// A roll back is not a sync: it records no trigger.
	if got.SyncTrigger != "" {
		t.Errorf("rollback trigger = %q", got.SyncTrigger)
	}
}

func TestInlineRevisionUnchanged(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "plain", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.repo.NewRevision(ctx, st.ID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := f.repo.CopyRevision(ctx, rev, "bob", stackops.OriginRollback)
	if err != nil {
		t.Fatal(err)
	}
	for _, snap := range []stackops.Snapshot{rev, copied} {
		var commit, tree, source, files, trigger string
		if err := f.repo.Store.Read.QueryRow(`SELECT git_commit, git_tree, git_source, compose_files, sync_trigger FROM stack_snapshots WHERE id = ?`, snap.ID).
			Scan(&commit, &tree, &source, &files, &trigger); err != nil {
			t.Fatal(err)
		}
		if commit != "" || tree != "" || source != "{}" || files != "[]" || trigger != "" {
			t.Errorf("revision %d columns = %q %q %q %q %q, want empty git columns and '[]' compose files", snap.Seq, commit, tree, source, files, trigger)
		}
		got, err := f.repo.Revision(ctx, st.ID, snap.Seq)
		if err != nil || got.Git.Commit != "" || got.Git.Source.RepoID != "" || len(got.Copy.ComposeFiles) != 0 ||
			len(got.Copy.Files) != 1 || got.Copy.Files[0].Origin != stacks.OriginInline || string(got.Copy.Compose()) != "services: {}\n" {
			t.Errorf("revision %d = %+v, %v, want an inline revision as before", snap.Seq, got, err)
		}
	}
}

// Refreshing the draft of a git stack from a newer commit (plan 09-19, GIT-04, D-17): the repository's files are
// replaced, Bayplan's own files and the variables stay, and a draft that moved is not overwritten.

func TestRefreshDraftKeepsOverlaysAndVars(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, draft, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	// A person adds an overlay file and a variable.
	withOverlay := draft.Copy
	withOverlay.Files = append(slices.Clone(withOverlay.Files), stacks.File{Path: "secrets/db.txt", Content: []byte("hunter2"), Secret: true, Origin: stacks.OriginOverlay})
	withOverlay.Vars = append(slices.Clone(withOverlay.Vars), stacks.Var{Name: "EXTRA", Value: "x"})
	saved, _, err := f.repo.SaveDraft(ctx, st.ID, draft.Version, "ada", withOverlay)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	rev := gitRevision()
	rev.Commit, rev.Tree = "fedcba9876543210fedcba9876543210fedcba98", "1111111111111111111111111111111111111111"
	repoFiles := []stacks.File{repoFile("compose.yaml", "services: {web: {image: nginx}}\n"), repoFile("compose.prod.yaml", "services: {}\n")}
	got, err := f.repo.RefreshDraftFromGit(ctx, st.ID, saved.Version, "bob", repoFiles, rev)
	if err != nil {
		t.Fatalf("RefreshDraftFromGit: %v", err)
	}
	if got.Version != saved.Version+1 {
		t.Errorf("version = %d, want %d", got.Version, saved.Version+1)
	}
	read, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range read.Copy.Files {
		paths = append(paths, file.Path+":"+string(file.Origin))
	}
	// .env is gone (the commit does not have it); the overlay stays; the compose file is the new one.
	want := []string{"compose.prod.yaml:repo", "compose.yaml:repo", "secrets/db.txt:overlay"}
	if !slices.Equal(paths, want) {
		t.Errorf("files = %v, want %v", paths, want)
	}
	for _, file := range read.Copy.Files {
		if file.Path == "secrets/db.txt" && (string(file.Content) != "hunter2" || !file.Secret) {
			t.Errorf("overlay = %q secret %v, want it kept and sealed", file.Content, file.Secret)
		}
		if file.Path == "compose.yaml" && string(file.Content) != "services: {web: {image: nginx}}\n" {
			t.Errorf("compose.yaml = %q, want the new commit's", file.Content)
		}
	}
	if len(read.Copy.Vars) != 2 {
		t.Errorf("vars = %+v, want both kept", read.Copy.Vars)
	}
	if read.Git.Commit != rev.Commit || read.Git.Tree != rev.Tree || read.Version != saved.Version+1 || read.CreatedBy != "bob" {
		t.Errorf("draft = %+v, want the new commit, tree, version and saver", read)
	}

	// The same commit again changes nothing.
	again, err := f.repo.RefreshDraftFromGit(ctx, st.ID, read.Version, "bob", repoFiles, rev)
	if err != nil || again.Version != read.Version {
		t.Errorf("a refresh with nothing new = version %d, %v, want %d", again.Version, err, read.Version)
	}

	// A stale version is a conflict and the draft stays as it is.
	var conflict *stackops.DraftConflictError
	if _, err := f.repo.RefreshDraftFromGit(ctx, st.ID, saved.Version, "bob", nil, gitRevision()); !errors.As(err, &conflict) || conflict.Version != read.Version {
		t.Errorf("refresh at a stale version = %v, want a draft conflict at version %d", err, read.Version)
	}
	after, _ := f.repo.Draft(ctx, st.ID)
	if after.Version != read.Version || after.Git.Commit != rev.Commit {
		t.Errorf("draft after a refused refresh = %+v, want it unchanged", after)
	}
}

func TestRefreshDraftRepositoryFileWinsItsPath(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, draft, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	withOverlay := draft.Copy
	withOverlay.Files = append(slices.Clone(withOverlay.Files), stacks.File{Path: "extra.env", Content: []byte("A=1"), Secret: true, Origin: stacks.OriginOverlay})
	saved, _, err := f.repo.SaveDraft(ctx, st.ID, draft.Version, "ada", withOverlay)
	if err != nil {
		t.Fatal(err)
	}
	// The new commit now holds extra.env itself.
	repoFiles := []stacks.File{repoFile("compose.yaml", "services: {}\n"), repoFile("compose.prod.yaml", "services: {}\n"), repoFile("extra.env", "A=repo\n")}
	if _, err := f.repo.RefreshDraftFromGit(ctx, st.ID, saved.Version, "ada", repoFiles, gitRevision()); err != nil {
		t.Fatalf("RefreshDraftFromGit: %v", err)
	}
	read, _ := f.repo.Draft(ctx, st.ID)
	n := 0
	for _, file := range read.Copy.Files {
		if file.Path == "extra.env" {
			n++
			if file.Origin != stacks.OriginRepo || string(file.Content) != "A=repo\n" {
				t.Errorf("extra.env = %q origin %s, want the repository's", file.Content, file.Origin)
			}
		}
	}
	if n != 1 {
		t.Errorf("extra.env held %d times, want once", n)
	}
}

func TestSetAppliedSource(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	src := gitSource()
	src.Ref, src.Folder, src.ComposeFiles = "release", "stacks/web2", []string{"docker-compose.yml"}
	if err := f.repo.SetAppliedSource(ctx, st.ID, src); err != nil {
		t.Fatalf("SetAppliedSource: %v", err)
	}
	g, err := f.repo.StackGit(ctx, st.ID)
	if err != nil || g.Ref != "release" || g.Folder != "stacks/web2" || !slices.Equal(g.ComposeFiles, []string{"docker-compose.yml"}) {
		t.Errorf("StackGit = %+v, %v, want the new source", g, err)
	}
	if err := f.repo.SetAppliedSource(ctx, "nope", src); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("SetAppliedSource of an unknown stack = %v, want ErrNoGit", err)
	}

	// Following another repository moves the stack's own source ref with it (WR-C02).
	f.addGitRepo(t, "repo-2", "other")
	src.RepoID = "repo-2"
	if err := f.repo.SetAppliedSource(ctx, st.ID, src); err != nil {
		t.Fatalf("SetAppliedSource to another repository: %v", err)
	}
	if got, err := f.repo.Stack(ctx, f.envID, st.ID); err != nil || got.Source.Kind != stacks.SourceGit || got.Source.Ref != "repo-2" {
		t.Errorf("stack = %+v, %v, want the new repository as its ref", got.Source, err)
	}
}

// A git stack's draft may change its overlay files and variables and nothing that belongs to the repository (plan
// 09-21, GIT-08, D-11).

func gitDraftCopy() stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files: []stacks.File{
			repoFile("compose.yaml", "services: {}\n"), repoFile("compose.prod.yaml", "services: {}\n"),
			{Path: "web.env", Content: []byte("A=b\n"), Secret: true, Origin: stacks.OriginOverlay},
		},
		Vars:         []stacks.Var{{Name: "TAG", Value: "2"}},
		ComposeFiles: []string{"compose.yaml", "compose.prod.yaml"},
	}
}

func TestCheckGitDraftEdit(t *testing.T) {
	t.Parallel()
	prev := gitDraftCopy()
	edit := func(mut func(*stacks.WorkingCopy)) stacks.WorkingCopy {
		next := stacks.WorkingCopy{
			Files: slices.Clone(prev.Files), Vars: slices.Clone(prev.Vars), ComposeFiles: slices.Clone(prev.ComposeFiles),
		}
		mut(&next)
		return stackops.GitDraftOrigins(prev, next)
	}
	cases := map[string]struct {
		next stacks.WorkingCopy
		path string // the path the refusal names; "-" for an edit that is allowed
	}{
		"unchanged": {edit(func(*stacks.WorkingCopy) {}), "-"},
		"a repository file edited": {edit(func(w *stacks.WorkingCopy) {
			w.Files[0] = stacks.File{Path: "compose.yaml", Content: []byte("services: {a: {}}\n")}
		}), "compose.yaml"},
		"a repository file removed": {edit(func(w *stacks.WorkingCopy) { w.Files = slices.Delete(w.Files, 1, 2) }), "compose.prod.yaml"},
		"the compose files reordered": {edit(func(w *stacks.WorkingCopy) {
			w.ComposeFiles = []string{"compose.prod.yaml", "compose.yaml"}
		}), ""},
		"an overlay replaced": {edit(func(w *stacks.WorkingCopy) {
			w.Files[2] = stacks.File{Path: "web.env", Content: []byte("A=c\n"), Secret: true}
		}), "-"},
		"an overlay removed": {edit(func(w *stacks.WorkingCopy) { w.Files = w.Files[:2] }), "-"},
		"an overlay added": {edit(func(w *stacks.WorkingCopy) {
			w.Files = append(w.Files, stacks.File{Path: "db.env", Content: []byte("X=1\n")})
		}), "-"},
		"a variable changed": {edit(func(w *stacks.WorkingCopy) {
			w.Vars = []stacks.Var{{Name: "TAG", Value: "3"}, {Name: "NEW", Value: "1"}}
		}), "-"},
		"a repository path reused": {edit(func(w *stacks.WorkingCopy) {
			w.Files = append(w.Files[:1], w.Files[2:]...)
			w.Files = append(w.Files, stacks.File{Path: "compose.prod.yaml", Content: []byte("x")})
		}), "compose.prod.yaml"},
	}
	for name, tc := range cases {
		err := stackops.CheckGitDraftEdit(prev, tc.next)
		if tc.path == "-" {
			if err != nil {
				t.Errorf("%s: %v, want the edit allowed", name, err)
			}
			continue
		}
		var re *stackops.RepoFileEditError
		if !errors.Is(err, stackops.ErrRepoFileReadOnly) || !errors.As(err, &re) || re.Path != tc.path {
			t.Errorf("%s: %v, want ErrRepoFileReadOnly naming %q", name, err, tc.path)
		}
	}
}

func TestGitDraftOriginsMakesNewFilesSecretOverlays(t *testing.T) {
	t.Parallel()
	prev := gitDraftCopy()
	next := stacks.WorkingCopy{Files: []stacks.File{
		{Path: "compose.yaml", Content: []byte("services: {}\n")}, {Path: "new.env", Content: []byte("K=v\n")},
	}}
	got := stackops.GitDraftOrigins(prev, next)
	if f := got.Files[0]; f.Origin != stacks.OriginRepo || f.Secret {
		t.Errorf("compose.yaml = %+v, want a plain repository file", f)
	}
	if f := got.Files[1]; f.Origin != stacks.OriginOverlay || !f.Secret {
		t.Errorf("new.env = %+v, want a secret overlay", f)
	}
	if next.Files[1].Origin != stacks.OriginInline {
		t.Errorf("the input was changed: %+v", next)
	}
}

// The settings an automatic deploy keeps (plan 09-25, GIT-07, D-18, D-19).

func TestSetAutoDeployOnOffAndPaused(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	if sg, err := f.repo.StackGit(ctx, st.ID); err != nil || sg.AutoDeploy || sg.AutoDeployPausedReason != "" {
		t.Fatalf("a new git stack = %+v, %v; want auto-deploy off", sg, err)
	}

	ran := false
	if err := f.repo.SetAutoDeploy(ctx, st.ID, true, "", 0, func(context.Context, *sql.Tx, *sqlc.Queries) error { ran = true; return nil }); err != nil {
		t.Fatalf("SetAutoDeploy on: %v", err)
	}
	if sg, _ := f.repo.StackGit(ctx, st.ID); !sg.AutoDeploy || sg.AutoDeployPausedReason != "" || !ran {
		t.Errorf("after on = %+v (callback ran %v), want on and running", sg, ran)
	}
	if err := f.repo.SetAutoDeploy(ctx, st.ID, true, stackops.PausedRollback, 4, nil); err != nil {
		t.Fatal(err)
	}
	if sg, _ := f.repo.StackGit(ctx, st.ID); !sg.AutoDeploy || sg.AutoDeployPausedReason != stackops.PausedRollback || sg.AutoDeployPausedSeq != 4 {
		t.Errorf("after pause = %+v, want paused for a roll back at 4", sg)
	}
	if err := f.repo.SetAutoDeploy(ctx, st.ID, false, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	if sg, _ := f.repo.StackGit(ctx, st.ID); sg.AutoDeploy || sg.AutoDeployPausedReason != "" || sg.AutoDeployPausedSeq != 0 {
		t.Errorf("after off = %+v, want off and not paused", sg)
	}

	// A callback that fails undoes the setting.
	boom := errors.New("boom")
	if err := f.repo.SetAutoDeploy(ctx, st.ID, true, "", 0, func(context.Context, *sql.Tx, *sqlc.Queries) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("SetAutoDeploy with a failing callback = %v, want it", err)
	}
	if sg, _ := f.repo.StackGit(ctx, st.ID); sg.AutoDeploy {
		t.Errorf("auto-deploy is on after its transaction failed")
	}
	if err := f.repo.SetAutoDeploy(ctx, "no-such-stack", true, "", 0, nil); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("SetAutoDeploy of a stack without git = %v, want ErrNoGit", err)
	}
}

func TestSetSyncTriggerAndRevertedDriftAreForRevisions(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, draft, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := f.repo.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetSyncTrigger(ctx, rev.ID, stackops.SyncWebhook); err != nil {
		t.Fatalf("SetSyncTrigger: %v", err)
	}
	if err := f.repo.SetSyncTrigger(ctx, rev.ID, "manual"); err == nil {
		t.Errorf("SetSyncTrigger accepted a trigger no automatic deploy has")
	}
	if err := f.repo.SetSyncTrigger(ctx, draft.ID, stackops.SyncPoll); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetSyncTrigger of a draft = %v, want sql.ErrNoRows", err)
	}
	if err := f.repo.SetRevertedDrift(ctx, rev.ID, []string{"image of web: nginx:1.28 -> nginx:1.27"}); err != nil {
		t.Fatalf("SetRevertedDrift: %v", err)
	}
	var trigger, drift string
	if err := f.repo.Store.Read.QueryRow(`SELECT sync_trigger, reverted_drift FROM stack_snapshots WHERE id = ?`, rev.ID).Scan(&trigger, &drift); err != nil {
		t.Fatal(err)
	}
	if trigger != "webhook" || !strings.Contains(drift, "nginx:1.28") || !strings.HasPrefix(drift, "[") {
		t.Errorf("stored trigger %q, reverted drift %q", trigger, drift)
	}
	got, err := f.repo.Revision(ctx, st.ID, rev.Seq)
	if err != nil || got.SyncTrigger != "webhook" {
		t.Errorf("Revision = %+v, %v; want sync trigger webhook", got, err)
	}
}

func TestSetGitEventIsStoredAndClearedBesideTheState(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	if err := f.repo.SetGitEvent(ctx, st.ID, stackops.GitEventSkipped, "a bind mount", at); err != nil {
		t.Fatalf("SetGitEvent: %v", err)
	}
	s, err := f.repo.GitState(ctx, st.ID)
	if err != nil || s.EventKind != stackops.GitEventSkipped || s.EventDetail != "a bind mount" || s.EventAt == nil || !s.EventAt.Equal(at) {
		t.Fatalf("state = %+v, %v; want the event", s, err)
	}
	// A check's write of the answer leaves the event alone.
	s.Behind = 3
	if err := f.repo.WriteGitState(ctx, s); err != nil {
		t.Fatal(err)
	}
	if s, _ = f.repo.GitState(ctx, st.ID); s.EventKind != stackops.GitEventSkipped || s.Behind != 3 {
		t.Errorf("state after a write = %+v, want the event kept", s)
	}
	if err := f.repo.SetGitEvent(ctx, st.ID, "", "ignored", at); err != nil {
		t.Fatal(err)
	}
	if s, _ = f.repo.GitState(ctx, st.ID); s.EventKind != "" || s.EventDetail != "" || s.EventAt != nil {
		t.Errorf("state after clearing = %+v, want no event", s)
	}
	if err := f.repo.SetGitEvent(ctx, "no-such-stack", stackops.GitEventPaused, "x", at); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetGitEvent of an unknown stack = %v, want sql.ErrNoRows", err)
	}
}

// Pointing an inline stack at a repository (plan 09-30, GIT-02, D-24): the draft is rewritten and the source is held as
// pending settings that no reader of git settings sees, until a deploy finishes the conversion or a discard ends it.

func TestPendingGitSourceIsNotAGitStackUntilFinalized(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, draft, err := f.repo.CreateStack(ctx, f.envID, "legacy", stacks.TargetStandalone, "ada", stacks.WorkingCopy{
		Files: []stacks.File{compose("services: {}\n"), {Path: "notes.txt", Content: []byte("n")}},
		Vars:  []stacks.Var{{Name: "TAG", Value: "2"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	wc := stacks.WorkingCopy{
		Files:        []stacks.File{repoFile("compose.yaml", "services: {web: {image: nginx}}\n"), {Path: "secrets/k", Content: []byte("sealed"), Secret: true, Origin: stacks.OriginOverlay}},
		Vars:         draft.Copy.Vars,
		ComposeFiles: []string{"compose.yaml"},
	}
	rev := gitRevision()
	rev.Source.ComposeFiles = []string{"compose.yaml"}
	saved, err := f.repo.CreatePendingGitSource(ctx, st.ID, draft.Version, "ada", wc, rev)
	if err != nil {
		t.Fatalf("CreatePendingGitSource: %v", err)
	}
	if saved.Version != draft.Version+1 || saved.Git.Commit != gitCommit {
		t.Errorf("draft = version %d at %q, want version %d at %s", saved.Version, saved.Git.Commit, draft.Version+1, gitCommit)
	}
	read, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range read.Copy.Files {
		paths = append(paths, file.Path+":"+string(file.Origin))
	}
	if want := []string{"compose.yaml:repo", "secrets/k:overlay"}; !slices.Equal(paths, want) {
		t.Errorf("files = %v, want %v: the inline files are replaced", paths, want)
	}

	// The stack is not a git stack yet: no reader of git settings sees the pending source, and it is not polled.
	if _, err := f.repo.StackGit(ctx, st.ID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("StackGit = %v, want ErrNoGit while the conversion is pending", err)
	}
	if _, err := f.repo.GitState(ctx, st.ID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("GitState = %v, want ErrNoGit while the conversion is pending", err)
	}
	if all, err := f.repo.StackGitsOfEnv(ctx, f.envID); err != nil || len(all) != 0 {
		t.Errorf("StackGitsOfEnv = %v, %v, want none", all, err)
	}
	if polled, err := f.repo.PolledStacks(ctx); err != nil || len(polled) != 0 {
		t.Errorf("PolledStacks = %v, %v, want none", polled, err)
	}
	pending, ok, err := f.repo.PendingGit(ctx, st.ID)
	if err != nil || !ok || !pending.PendingConvert || pending.RepoID != gitRepoID || pending.Ref != "main" || pending.Folder != "stacks/web" || !slices.Equal(pending.ComposeFiles, []string{"compose.yaml"}) {
		t.Fatalf("PendingGit = %+v, %v, %v, want the source", pending, ok, err)
	}
	if got, err := f.repo.Stack(ctx, f.envID, st.ID); err != nil || got.Source.Kind != stacks.SourceInline {
		t.Errorf("stack = %+v, %v, want it inline until finalized", got, err)
	}

	// Nothing that writes the settings of a stack from a repository reaches a pending source (WR-C02).
	moved := gitSource()
	moved.Ref = "elsewhere"
	if err := f.repo.SetAppliedSource(ctx, st.ID, moved); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("SetAppliedSource on a pending source = %v, want ErrNoGit", err)
	}
	if err := f.repo.SetPolling(ctx, st.ID, false, stackops.MinPollInterval); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("SetPolling on a pending source = %v, want ErrNoGit", err)
	}
	if err := f.repo.SetAutoDeploy(ctx, st.ID, true, "", 0, nil); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("SetAutoDeploy on a pending source = %v, want ErrNoGit", err)
	}
	if got, err := f.repo.Stack(ctx, f.envID, st.ID); err != nil || got.Source.Kind != stacks.SourceInline || got.Source.Ref != "" {
		t.Errorf("stack = %+v, %v, want it inline with no source ref", got.Source, err)
	}
	if pending, ok, _ := f.repo.PendingGit(ctx, st.ID); !ok || pending.Ref != "main" || !pending.PollEnabled || pending.AutoDeploy {
		t.Errorf("pending source = %+v, want it as it was made", pending)
	}

	// Asking again replaces the pending source.
	other := rev
	other.Source.Ref = "dev"
	if _, err := f.repo.CreatePendingGitSource(ctx, st.ID, saved.Version, "ada", wc, other); err != nil {
		t.Fatalf("second CreatePendingGitSource: %v", err)
	}
	if pending, _, _ := f.repo.PendingGit(ctx, st.ID); pending.Ref != "dev" {
		t.Errorf("pending ref = %q, want dev", pending.Ref)
	}

	// Finalizing makes the stack a git stack, once.
	if done, err := f.repo.FinalizeConvert(ctx, st.ID); err != nil || !done {
		t.Fatalf("FinalizeConvert = %v, %v, want done", done, err)
	}
	if got, err := f.repo.Stack(ctx, f.envID, st.ID); err != nil || got.Source.Kind != stacks.SourceGit || got.Source.Ref != gitRepoID {
		t.Errorf("stack = %+v, %v, want git with the repository as its ref", got, err)
	}
	sg, err := f.repo.StackGit(ctx, st.ID)
	if err != nil || sg.PendingConvert || sg.Ref != "dev" || !sg.PollEnabled {
		t.Errorf("StackGit = %+v, %v, want the settings, polling and no pending flag", sg, err)
	}
	if state, err := f.repo.GitState(ctx, st.ID); err != nil || state.Status != stackops.GitStatusUnknown {
		t.Errorf("GitState = %+v, %v, want unknown", state, err)
	}
	if _, ok, _ := f.repo.PendingGit(ctx, st.ID); ok {
		t.Error("PendingGit after the conversion is finished, want none")
	}
	if done, err := f.repo.FinalizeConvert(ctx, st.ID); err != nil || done {
		t.Errorf("a second FinalizeConvert = %v, %v, want nothing to do", done, err)
	}
	// A git stack cannot be pointed at a repository.
	if _, err := f.repo.CreatePendingGitSource(ctx, st.ID, saved.Version+1, "ada", wc, rev); !errors.Is(err, stackops.ErrAlreadyGit) {
		t.Errorf("CreatePendingGitSource on a git stack = %v, want ErrAlreadyGit", err)
	}
}

func TestDiscardingAConvertDraftDropsThePendingSource(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, draft, err := f.repo.CreateStack(ctx, f.envID, "legacy", stacks.TargetStandalone, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	wc := stacks.WorkingCopy{Files: []stacks.File{repoFile("compose.yaml", "services: {}\n")}, ComposeFiles: []string{"compose.yaml"}}
	rev := gitRevision()
	rev.Source.ComposeFiles = []string{"compose.yaml"}
	if _, err := f.repo.CreatePendingGitSource(ctx, st.ID, draft.Version, "ada", wc, rev); err != nil {
		t.Fatal(err)
	}
	if discarded, _, err := f.repo.DiscardDraft(ctx, st.ID); err != nil || !discarded {
		t.Fatalf("DiscardDraft = %v, %v", discarded, err)
	}
	if _, ok, err := f.repo.PendingGit(ctx, st.ID); err != nil || ok {
		t.Errorf("PendingGit after a discard = %v, %v, want none", ok, err)
	}
	if dropped, err := f.repo.DropPendingConvert(ctx, st.ID); err != nil || dropped {
		t.Errorf("DropPendingConvert with nothing pending = %v, %v, want false", dropped, err)
	}
	if got, err := f.repo.Stack(ctx, f.envID, st.ID); err != nil || got.Source.Kind != stacks.SourceInline {
		t.Errorf("stack = %+v, %v, want inline", got, err)
	}
	// No git settings are left behind, so a repository only the discarded conversion pointed at is free again.
	var n int
	if err := f.repo.Store.Read.QueryRow(`SELECT COUNT(*) FROM stack_git`).Scan(&n); err != nil || n != 0 {
		t.Errorf("stack_git rows = %d, %v, want none", n, err)
	}

	// A stale version stores neither a draft nor a pending source.
	var conflict *stackops.DraftConflictError
	if _, err := f.repo.CreatePendingGitSource(ctx, st.ID, 5, "ada", wc, rev); !errors.As(err, &conflict) {
		t.Errorf("CreatePendingGitSource at a stale version = %v, want a draft conflict", err)
	}
	if _, ok, _ := f.repo.PendingGit(ctx, st.ID); ok {
		t.Error("a refused conversion left a pending source")
	}
}

// A detached stack no longer reads the repository's .env, so the values it supplied move into the variable table: a
// compose `${X:-default}` resolves the same before and after the detach (WR-C01).
func TestDetachKeepsWhatTheRepositoryEnvSupplied(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    environment:\n      TAG: ${TAG:-latest}\n      GREETING: ${GREETING:-hello}\n      DB_PASSWORD: ${DB_PASSWORD:-none}\n      BLANK: ${BLANK:-fallback}\n"
	in := gitInput(f, "web")
	in.Copy = stacks.WorkingCopy{
		Files: []stacks.File{
			repoFile("compose.yaml", compose),
			repoFile(".env", "TAG=from-env\nGREETING=hi\nDB_PASSWORD=pw\nBLANK=\n"),
		},
		Vars:         []stacks.Var{{Name: "TAG", Value: "from-table"}, {Name: "GREETING"}},
		ComposeFiles: []string{"compose.yaml"},
	}
	in.Rev.Source.ComposeFiles = []string{"compose.yaml"}
	st, draft, err := f.repo.CreateGitStack(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	before := stacks.Check(ctx, stacks.CheckInput{Name: "web", Target: stacks.TargetStandalone, Copy: draft.Copy})
	if before.Loaded == nil {
		t.Fatalf("check before: %v", before.Problems)
	}

	if err := f.repo.Detach(ctx, st.ID, draft, "bob", nil); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	read, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := stacks.Check(ctx, stacks.CheckInput{Name: "web", Target: stacks.TargetStandalone, Copy: read.Copy})
	if after.Loaded == nil {
		t.Fatalf("check after: %v", after.Problems)
	}

	beforeEnv, err := stacks.InterpolationEnv(draft.Copy)
	if err != nil {
		t.Fatal(err)
	}
	afterEnv, err := stacks.InterpolationEnv(read.Copy)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeEnv) != len(afterEnv) {
		t.Errorf("interpolation env after = %v, want %v", afterEnv, beforeEnv)
	}
	for name, want := range beforeEnv {
		if got := afterEnv[name]; got != want {
			t.Errorf("%s resolves to %q after the detach, %q before", name, got, want)
		}
	}
	for _, name := range []string{"TAG", "GREETING", "DB_PASSWORD", "BLANK"} {
		b, a := before.Loaded.Project.Services["web"].Environment[name], after.Loaded.Project.Services["web"].Environment[name]
		if b == nil || a == nil || *a != *b {
			t.Errorf("service env %s = %v after, %v before: want the same value", name, a, b)
		}
	}
	// The table still wins over the .env, a blank table entry takes the .env's value, and a secret-like name is sealed.
	byName := map[string]stacks.Var{}
	for _, v := range read.Copy.Vars {
		byName[v.Name] = v
	}
	if byName["TAG"].Value != "from-table" || byName["GREETING"].Value != "hi" {
		t.Errorf("TAG = %q, GREETING = %q, want the table's value and the .env's for the blank one", byName["TAG"].Value, byName["GREETING"].Value)
	}
	if v := byName["DB_PASSWORD"]; v.Value != "pw" || !v.Secret {
		t.Errorf("DB_PASSWORD = %+v, want the .env's value as a secret variable", v)
	}
	if _, ok := byName["BLANK"]; ok {
		t.Error("a blank .env value became a variable")
	}
}
