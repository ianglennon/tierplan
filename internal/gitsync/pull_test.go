// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Pulling a git stack's newest commit into its draft and learning that it was deployed (plan 09-19, GIT-04, D-17): a
// pull refreshes the draft and deploys nothing; the stack reads in sync once the revision made from it is good.

func (f *checkFixture) pull(t *testing.T, version int64) (gitsync.PullResult, error) {
	t.Helper()
	return f.svc.Pull(context.Background(), "env-1", f.stackID, "bob", version)
}

func (f *checkFixture) draft(t *testing.T) stackops.Snapshot {
	t.Helper()
	d, err := f.rr.Draft(context.Background(), f.stackID)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	return d
}

func fileContent(files []stacks.File, path string) (string, bool) {
	for _, f := range files {
		if f.Path == path {
			return string(f.Content), true
		}
	}
	return "", false
}

func TestPullThenDeployed(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	ctx := context.Background()
	first := f.draft(t)
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("first check = %s, want in_sync", out)
	}

	// The repository moves: the folder changes twice, with a commit elsewhere between them.
	f.changeWeb(t, "1")
	f.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {}\n# edited\n"}, "elsewhere")
	head := f.changeWeb(t, "2")
	if out := f.check(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}

	res, err := f.pull(t, first.Version)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Commit.SHA != head || res.DraftVersion != first.Version+1 || res.Ref != "main" || res.DeployedCommit != f.sha || res.Touched != 2 {
		t.Errorf("pull = %+v, want the head %s, version %d, deployed %s and 2 commits that touched the folder", res, head, first.Version+1, f.sha)
	}
	d := f.draft(t)
	if got, _ := fileContent(d.Copy.Files, "web.env"); got != "A=2\n" {
		t.Errorf("web.env = %q, want the head's", got)
	}
	if d.Git.Commit != head || d.Git.Tree == "" || d.Git.Tree == first.Git.Tree || d.Git.Source.Ref != "main" || d.Version != res.DraftVersion {
		t.Errorf("draft git = %+v version %d, want the head's commit and tree", d.Git, d.Version)
	}

	// A pull deploys nothing and makes no revision; the state learns the head and is behind until the deploy.
	if revs, _, err := f.rr.ListRevisions(ctx, f.stackID); err != nil || len(revs) != 1 {
		t.Errorf("revisions = %d, %v, want only the one that was deployed", len(revs), err)
	}
	if s := f.state(t); s.Status != stackops.GitStatusBehind || s.HeadCommit != head || s.DeployedCommit != f.sha || s.Behind != 2 {
		t.Errorf("state after the pull = %+v, want behind 2 at %s", s, head)
	}

	// The revision the deploy makes records the commit, tree and source, and the hook sets the state.
	rev, err := f.rr.NewRevision(ctx, f.stackID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Git.Commit != head || rev.Git.Tree != d.Git.Tree || rev.Git.Source.Folder != "stacks/web" {
		t.Errorf("revision git = %+v, want the pulled commit, tree and source", rev.Git)
	}
	if err := f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	f.svc.Deployed(ctx, f.stackID, rev)
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.Behind != 0 || s.DeployedCommit != head || s.DeployedTree != rev.Git.Tree || s.HeadCommit != head || s.ReasonCode != "" {
		t.Errorf("state after the deploy = %+v, want in sync with %s deployed", s, head)
	}
	// A check agrees.
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Errorf("check after the deploy = %s, want in_sync", out)
	}
}

func TestDeployedWithNewerHeadStaysBehind(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	ctx := context.Background()
	d := f.draft(t)
	pulled := f.changeWeb(t, "1")
	if _, err := f.pull(t, d.Version); err != nil {
		t.Fatal(err)
	}
	rev, err := f.rr.NewRevision(ctx, f.stackID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	// While it deployed, the repository moved on and a check saw it.
	newer := f.changeWeb(t, "2")
	if out := f.check(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	f.svc.Deployed(ctx, f.stackID, rev)
	if s := f.state(t); s.Status != stackops.GitStatusBehind || s.DeployedCommit != pulled || s.HeadCommit != newer || s.Behind < 1 {
		t.Errorf("state = %+v, want behind: %s deployed and the head %s newer", s, pulled, newer)
	}
}

func TestDeployedFollowsTheSourceOfTheRevision(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	ctx := context.Background()
	rev := f.deploy(t)
	// The stack's settings were changed since (a review moved the ref); a deploy of the revision made them follow it.
	other := rev.Git.Source
	other.Ref = "elsewhere"
	if err := f.rr.SetAppliedSource(ctx, f.stackID, other); err != nil {
		t.Fatal(err)
	}
	f.svc.Deployed(ctx, f.stackID, rev)
	sg, err := f.rr.StackGit(ctx, f.stackID)
	if err != nil || sg.Ref != "main" {
		t.Errorf("settings = %+v, %v, want the revision's ref main", sg, err)
	}
	if s := f.state(t); s.DeployedCommit != f.sha || s.Status != stackops.GitStatusInSync {
		t.Errorf("state = %+v, want in sync at %s", s, f.sha)
	}

	// A revision that was not made from a repository changes nothing.
	before := f.state(t)
	f.svc.Deployed(ctx, f.stackID, stackops.Snapshot{StackID: f.stackID})
	if after := f.state(t); after.DeployedCommit != before.DeployedCommit || after.CheckedAt == nil || !after.CheckedAt.Equal(*before.CheckedAt) {
		t.Errorf("state changed for a revision without git: %+v -> %+v", before, after)
	}
}

func TestPullDraftRaceService(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	ctx := context.Background()
	d := f.draft(t)
	f.changeWeb(t, "1")

	// A variable save wins the race: the pull, at the version it read, is refused and stores nothing.
	copy := d.Copy
	copy.Vars = append(slices.Clone(copy.Vars), stacks.Var{Name: "NEW", Value: "v"})
	saved, _, err := f.rr.SaveDraft(ctx, f.stackID, d.Version, "ada", copy)
	if err != nil {
		t.Fatal(err)
	}
	var conflict *stackops.DraftConflictError
	if _, err := f.pull(t, d.Version); !errors.As(err, &conflict) || conflict.Version != saved.Version {
		t.Fatalf("pull at a stale version = %v, want a draft conflict at %d", err, saved.Version)
	}
	if after := f.draft(t); after.Version != saved.Version || len(after.Copy.Vars) != 1 || after.Git.Commit != f.sha {
		t.Errorf("draft = %+v, want the saved one untouched", after)
	}

	// The pull wins: a save at the version it read is a conflict, and the pull's files are not lost.
	if _, err := f.pull(t, saved.Version); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, _, err := f.rr.SaveDraft(ctx, f.stackID, saved.Version, "ada", copy); !errors.As(err, &conflict) {
		t.Errorf("save at a stale version after a pull = %v, want a draft conflict", err)
	}
	if got := f.draft(t); len(got.Copy.Vars) != 1 || got.Copy.Vars[0].Name != "NEW" {
		t.Errorf("vars = %+v, want the variable kept by the pull", got.Copy.Vars)
	}
}

// kindFetches records the kind of each ref fetched.
type kindFetches struct {
	gitx.Client
	mu    sync.Mutex
	kinds []gitx.RefKind
}

func (c *kindFetches) FetchRef(ctx context.Context, m gitx.MirrorID, r gitx.Remote, kind gitx.RefKind, ref string) (gitx.Commit, error) {
	c.mu.Lock()
	c.kinds = append(c.kinds, kind)
	c.mu.Unlock()
	return c.Client.FetchRef(ctx, m, r, kind, ref)
}

func (c *kindFetches) fetched(kind gitx.RefKind) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, k := range c.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

func TestPullFixedCommit(t *testing.T) {
	t.Parallel()
	rec := &kindFetches{}
	f := newCheckFixtureWith(t, func(c gitx.Client) gitx.Client { rec.Client = c; return rec }, nil,
		func(f *fixture, req *gitsync.ImportRequest) { req.RefKind, req.Ref = gitx.RefCommit, f.sha })
	f.deploy(t)
	ctx := context.Background()
	d := f.draft(t)

	// The repository moves on; a stack that follows a commit does not.
	f.changeWeb(t, "1")
	first, err := f.pull(t, d.Version)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if first.Commit.SHA != f.sha || first.Touched != 0 || first.DeployedCommit != f.sha {
		t.Errorf("pull = %+v, want the pinned commit %s again", first, f.sha)
	}
	for _, kind := range []gitx.RefKind{gitx.RefBranch, gitx.RefTag} {
		if n := rec.fetched(kind); n != 0 {
			t.Errorf("%d fetches of a %s for a stack that follows a commit, want none", n, kind)
		}
	}
	// The mirror holds the commit now: a second pull asks the server nothing.
	requests := f.srv.Requests()
	if _, err := f.pull(t, f.draft(t).Version); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if got := f.srv.Requests(); got != requests {
		t.Errorf("the git server saw %d requests during the second pull (was %d), want no more", got, requests)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.HeadCommit != f.sha {
		t.Errorf("state = %+v, want in sync at the pinned commit", s)
	}
	_ = ctx
}

func TestPullFetchFails(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	d := f.draft(t)
	f.srv.Close()

	_, err := f.pull(t, d.Version)
	var ge *gitx.Error
	if !errors.As(err, &ge) || ge.Code != gitx.CodeUnreachable {
		t.Fatalf("Pull with the server down = %v, want a gitx error with code unreachable", err)
	}
	if after := f.draft(t); after.Version != d.Version || after.Git.Commit != f.sha || after.CreatedBy != d.CreatedBy {
		t.Errorf("draft = %+v, want it unchanged", after)
	}
	if s := f.state(t); s.Status == stackops.GitStatusError {
		t.Errorf("state = %+v, want a pull that failed to leave the state to the checks", s)
	}
}

func TestPullOfAStackThatIsNotFromGit(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	if _, err := f.svc.Pull(context.Background(), "env-1", "nope", "bob", 1); !errors.Is(err, stackops.ErrStackNotFound) {
		t.Errorf("Pull of an unknown stack = %v, want ErrStackNotFound", err)
	}
}

// A commit that brings a repository file to the path of an overlay file would shadow a sealed secret of the person's, so
// the pull is refused (overlay_clash) with the draft and its overlay untouched, rather than dropping the secret (WR-C03).
func TestPullRefusesACommitThatHoldsAFileAtAnOverlayPath(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	ctx := context.Background()
	d := f.draft(t)
	withOverlay := d.Copy
	withOverlay.Files = append(slices.Clone(withOverlay.Files), stacks.File{Path: "extra.env", Content: []byte("PW=sealed\n"), Secret: true, Origin: stacks.OriginOverlay})
	saved, _, err := f.rr.SaveDraft(ctx, f.stackID, d.Version, "ada", withOverlay)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	// The repository now names extra.env and holds it.
	f.repo.Commit(t, map[string]string{
		"stacks/web/compose.yaml": "services:\n  web:\n    image: nginx:1.27\n    env_file: [web.env, extra.env]\n",
		"stacks/web/extra.env":    "PW=repo\n",
	}, "add extra.env")

	_, err = f.pull(t, saved.Version)
	var clash *gitsync.OverlayClashError
	if !errors.As(err, &clash) || clash.Path != "extra.env" {
		t.Fatalf("Pull = %v, want an overlay clash for extra.env", err)
	}
	after := f.draft(t)
	if after.Version != saved.Version || after.Git.Commit != saved.Git.Commit {
		t.Errorf("draft = version %d at %s, want it untouched (version %d at %s)", after.Version, after.Git.Commit, saved.Version, saved.Git.Commit)
	}
	var overlay stacks.File
	for _, file := range after.Copy.Files {
		if file.Path == "extra.env" {
			overlay = file
		}
	}
	if overlay.Origin != stacks.OriginOverlay || string(overlay.Content) != "PW=sealed\n" || !overlay.Secret {
		t.Errorf("extra.env = %+v, want the person's sealed overlay kept", overlay)
	}
}
