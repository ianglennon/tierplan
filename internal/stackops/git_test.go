// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
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
