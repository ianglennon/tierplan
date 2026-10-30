// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitsync_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/gitconn"
	"github.com/ianglennon/coaming/internal/gitsync"
	"github.com/ianglennon/coaming/internal/gitx"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
)

// Changing what a git stack follows (plan 09-29, GIT-02, GIT-04, D-11, D-12): the new source is imported into the draft as
// a pending source and applied by nothing but a good deploy of that draft.

func (f *checkFixture) change(t *testing.T, version int64, c gitsync.SourceChange) (gitsync.PullResult, error) {
	t.Helper()
	return f.svc.ChangeSource(context.Background(), "env-1", f.stackID, "bob", version, c)
}

// branchSource is the stack's own source with another branch.
func (f *checkFixture) branchSource(branch string) gitsync.SourceChange {
	return gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefBranch, Ref: branch, Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}}
}

func (f *checkFixture) applied(t *testing.T) stackops.StackGit {
	t.Helper()
	sg, err := f.rr.StackGit(context.Background(), f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

// devBranch commits a change to the folder and moves the branch dev to it, leaving main where it was.
func (f *checkFixture) devBranch(t *testing.T, tag string) string {
	t.Helper()
	main := f.sha
	sha := f.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=" + tag + "\n"}, "dev "+tag)
	f.repo.Branch(t, "dev", sha)
	f.repo.Branch(t, "main", main)
	return sha
}

func TestChangeSourceKeepsTheAppliedSourceUntilADeploy(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	ctx := context.Background()
	dev := f.devBranch(t, "dev")
	before := f.draft(t)
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check = %s, want in_sync", out)
	}
	stateBefore := f.state(t)

	// A variable and an overlay file the person holds stay.
	copy := before.Copy
	copy.Vars = append(slices.Clone(copy.Vars), stacks.Var{Name: "KEEP", Value: "1"})
	copy.Files = append(slices.Clone(copy.Files), stacks.File{Path: "extra.env", Content: []byte("X=1\n"), Origin: stacks.OriginOverlay})
	saved, _, err := f.rr.SaveDraft(ctx, f.stackID, before.Version, "ada", copy)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.change(t, saved.Version, f.branchSource("dev"))
	if err != nil {
		t.Fatalf("ChangeSource: %v", err)
	}
	if res.Commit.SHA != dev || res.DraftVersion != saved.Version+1 || res.Ref != "dev" || res.DeployedCommit != f.sha || res.Touched != 0 {
		t.Errorf("result = %+v, want the commit %s of dev, version %d, deployed %s and no count", res, dev, saved.Version+1, f.sha)
	}

	// The draft is the pending source: its files are dev's, the overlay and the variable are kept.
	d := f.draft(t)
	if d.Git.Commit != dev || d.Git.Source.Ref != "dev" || d.Git.Source.RefKind != "branch" || d.Git.Source.Folder != "stacks/web" {
		t.Errorf("draft git = %+v, want the commit and source of dev", d.Git)
	}
	if got, _ := fileContent(d.Copy.Files, "web.env"); got != "A=dev\n" {
		t.Errorf("web.env = %q, want dev's", got)
	}
	if got, ok := fileContent(d.Copy.Files, "extra.env"); !ok || got != "X=1\n" {
		t.Errorf("extra.env = %q, %v, want the overlay kept", got, ok)
	}
	if !slices.ContainsFunc(d.Copy.Vars, func(v stacks.Var) bool { return v.Name == "KEEP" }) {
		t.Errorf("vars = %+v, want KEEP kept", d.Copy.Vars)
	}

	// Nothing is applied: the settings and the sync state are as they were, and nothing was deployed.
	if sg := f.applied(t); sg.Ref != "main" {
		t.Errorf("settings ref = %q, want main until a deploy", sg.Ref)
	}
	if after := f.state(t); after.HeadCommit != stateBefore.HeadCommit || after.Status != stateBefore.Status || after.DeployedCommit != stateBefore.DeployedCommit {
		t.Errorf("state = %+v, want %+v untouched", after, stateBefore)
	}
	if revs, _, err := f.rr.ListRevisions(ctx, f.stackID); err != nil || len(revs) != 1 {
		t.Errorf("revisions = %d, %v, want only the first", len(revs), err)
	}
	sg := f.applied(t)
	if !gitsync.PendingSource(sg, d) {
		t.Error("PendingSource = false, want true for a draft that holds dev")
	}

	// A good deploy of the draft makes dev the stack's source, and the stack reads in sync with it.
	rev := f.deploy(t)
	f.svc.Deployed(ctx, f.stackID, rev)
	if sg := f.applied(t); sg.Ref != "dev" || sg.RefKind != "branch" {
		t.Errorf("settings after the deploy = %+v, want dev", sg)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.DeployedCommit != dev || s.HeadCommit != dev || s.Behind != 0 {
		t.Errorf("state after the deploy = %+v, want in sync at %s", s, dev)
	}
	if gitsync.PendingSource(f.applied(t), f.draft(t)) {
		t.Error("PendingSource = true after the deploy, want false")
	}
	// The next check follows dev.
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Errorf("check after the deploy = %s, want in_sync with dev", out)
	}
}

func TestChangeSourceThenPullFollowsTheAppliedSource(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.devBranch(t, "dev")
	head := f.changeWeb(t, "main-2")
	f.repo.Branch(t, "main", head)

	res, err := f.change(t, f.draft(t).Version, f.branchSource("dev"))
	if err != nil {
		t.Fatal(err)
	}
	// A pull during the wait pulls the applied source: main's head, and the pending source is gone.
	pulled, err := f.pull(t, res.DraftVersion)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulled.Commit.SHA != head || pulled.Ref != "main" {
		t.Errorf("pull = %+v, want main's head %s", pulled, head)
	}
	if d := f.draft(t); d.Git.Source.Ref != "main" || gitsync.PendingSource(f.applied(t), d) {
		t.Errorf("draft source = %+v, want main and no pending source", d.Git.Source)
	}
}

func TestChangeSourceStaleVersion(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.devBranch(t, "dev")
	d := f.draft(t)
	var conflict *stackops.DraftConflictError
	if _, err := f.change(t, d.Version+1, f.branchSource("dev")); !errors.As(err, &conflict) || conflict.Version != d.Version {
		t.Fatalf("ChangeSource at a stale version = %v, want a draft conflict at %d", err, d.Version)
	}
	if after := f.draft(t); after.Version != d.Version || after.Git.Source.Ref != "main" {
		t.Errorf("draft = %+v, want it untouched", after)
	}
}

func TestChangeSourceLayerRule(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES ('env-2', 'agent', 'two', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	other := "env-2"
	foreign := f.addRepo(t, &other, "foreign")
	c := f.branchSource("main")
	c.RepoID = foreign
	version := f.draft(t).Version
	if _, err := f.change(t, version, c); !errors.Is(err, gitconn.ErrNotFound) {
		t.Fatalf("ChangeSource to the repository of another environment = %v, want not found", err)
	}
	if after := f.draft(t); after.Version != version {
		t.Errorf("draft version = %d, want %d untouched", after.Version, version)
	}

	// An instance repository is usable anywhere, and so is one of this environment.
	mine := "env-1"
	for _, scope := range []*string{nil, &mine} {
		id := f.addRepo(t, scope, "more")
		c.RepoID = id
		res, err := f.change(t, f.draft(t).Version, c)
		if err != nil {
			t.Fatalf("ChangeSource to a repository of scope %v: %v", scope, err)
		}
		if d := f.draft(t); d.Git.Source.RepoID != id || res.Commit.SHA != f.sha {
			t.Errorf("draft source = %+v, want repository %s at %s", d.Git.Source, id, f.sha)
		}
	}
}

func TestChangeSourceKinds(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.repo.Tag(t, "v1", f.sha, false)
	second := f.repo.Commit(t, map[string]string{"stacks/web/override.yaml": "services:\n  web:\n    labels:\n      a: b\n"}, "an override")

	steps := []struct {
		name string
		c    gitsync.SourceChange
		want stackops.GitSource
		sha  string
	}{
		{"branch to tag", gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefTag, Ref: "v1", Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}},
			stackops.GitSource{RefKind: "tag", Ref: "v1", Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}}, f.sha},
		{"tag to a commit", gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefCommit, Ref: f.sha[:10], Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}},
			stackops.GitSource{RefKind: "commit", Ref: f.sha, Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}}, f.sha},
		{"one commit to another", gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefCommit, Ref: second, Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}},
			stackops.GitSource{RefKind: "commit", Ref: second, Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}}, second},
		{"another folder", gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefBranch, Ref: "main", Folder: "stacks/other", ComposeFiles: []string{"compose.yml"}},
			stackops.GitSource{RefKind: "branch", Ref: "main", Folder: "stacks/other", ComposeFiles: []string{"compose.yml"}}, second},
	}
	for _, s := range steps {
		res, err := f.change(t, f.draft(t).Version, s.c)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		d := f.draft(t)
		got := d.Git.Source
		if got.RefKind != s.want.RefKind || got.Ref != s.want.Ref || got.Folder != s.want.Folder || !slices.Equal(got.ComposeFiles, s.want.ComposeFiles) ||
			d.Git.Commit != s.sha || res.Commit.SHA != s.sha {
			t.Errorf("%s: draft source = %+v at %s, want %+v at %s", s.name, got, d.Git.Commit, s.want, s.sha)
		}
		if !gitsync.PendingSource(f.applied(t), d) {
			t.Errorf("%s: no pending source", s.name)
		}
	}
	if sg := f.applied(t); sg.Ref != "main" || sg.Folder != "stacks/web" || sg.RefKind != "branch" {
		t.Errorf("settings = %+v, want the applied source untouched", sg)
	}

	// Only the choice or the order of the compose files changes: still a pending source, and the files are the new ones.
	if _, err := f.change(t, f.draft(t).Version, f.branchSource("main")); err != nil {
		t.Fatal(err)
	}
	if gitsync.PendingSource(f.applied(t), f.draft(t)) {
		t.Error("back to the applied source: PendingSource = true, want false")
	}
	for _, files := range [][]string{{"compose.yaml", "override.yaml"}, {"override.yaml", "compose.yaml"}} {
		c := f.branchSource("main")
		c.ComposeFiles = files
		if _, err := f.change(t, f.draft(t).Version, c); err != nil {
			t.Fatalf("compose files %v: %v", files, err)
		}
		d := f.draft(t)
		if !slices.Equal(d.Git.Source.ComposeFiles, files) || !slices.Equal(d.Copy.ComposeFiles, files) || !gitsync.PendingSource(f.applied(t), d) {
			t.Errorf("compose files %v: draft source %+v, copy %v, want them pending in that order", files, d.Git.Source, d.Copy.ComposeFiles)
		}
	}
}

func TestChangeSourceRefusals(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	version := f.draft(t).Version
	cases := map[string]struct {
		c    gitsync.SourceChange
		want error
	}{
		"a folder outside the repository": {gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefBranch, Ref: "main", Folder: "../x", ComposeFiles: []string{"compose.yaml"}}, gitsync.ErrFolderInvalid},
		"no compose file":                 {gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefBranch, Ref: "main", Folder: "stacks/web"}, gitsync.ErrNoComposeFiles},
		"a compose file the folder lacks": {gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefBranch, Ref: "main", Folder: "stacks/web", ComposeFiles: []string{"nope.yaml"}}, gitsync.ErrComposeFileMissing},
	}
	for name, c := range cases {
		if _, err := f.change(t, version, c.c); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if _, err := f.change(t, version, f.branchSource("nope")); err == nil || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("a branch that does not exist: err = %v, want a refusal", err)
	}
	if after := f.draft(t); after.Version != version || after.Git.Source.Ref != "main" {
		t.Errorf("draft = %+v, want it untouched by every refusal", after)
	}
}

func TestChangeSourceBlocksAutoDeploy(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	ctx := context.Background()
	a.devBranch(t, "dev")
	head := a.changeWeb(t, "main-2")
	a.repo.Branch(t, "main", head)

	d, err := a.rr.Draft(ctx, a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.svc.ChangeSource(ctx, "env-1", a.stackID, "bob", d.Version, a.branchSource("dev"))
	if err != nil {
		t.Fatalf("ChangeSource: %v", err)
	}

	// The head of the applied source moves; auto-deploy waits for a person, and the pending source is not pulled away.
	if out := a.poll(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	s := a.state(t)
	if a.dep.count() != 0 || s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) ||
		!strings.Contains(s.ReasonDetail, "new source") || s.PendingCommit != head {
		t.Fatalf("%d deploys, state %+v; want nothing deployed and the commit held back for the new source", a.dep.count(), s)
	}
	after, err := a.rr.Draft(ctx, a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != res.DraftVersion || after.Git.Source.Ref != "dev" {
		t.Errorf("draft = version %d source %+v, want the pending source untouched at version %d", after.Version, after.Git.Source, res.DraftVersion)
	}
}
