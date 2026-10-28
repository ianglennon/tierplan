// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Checking a git stack against its repository (plan 09-14, GIT-03, GIT-09, D-13, D-16): what was deployed, what the
// repository holds now for the stack's folder, and nothing changed by looking.

type checkClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *checkClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *checkClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type checkFixture struct {
	*fixture
	rr      stackops.Repo
	svc     *gitsync.Service
	clock   *checkClock
	stackID string
}

// newCheckFixture makes a stack from the fixture's folder and deploys it (a good revision), so the repository's first
// commit is the deployed one.
func newCheckFixture(t *testing.T, wrap func(gitx.Client) gitx.Client, joined func(string, int)) *checkFixture {
	t.Helper()
	return newCheckFixtureWith(t, wrap, joined, nil)
}

// newCheckFixtureWith is newCheckFixture for a stack that follows something else than the branch main: tune may change
// the request the stack is made from, and make the refs it names, once the repository has its first commit.
func newCheckFixtureWith(t *testing.T, wrap func(gitx.Client) gitx.Client, joined func(string, int), tune func(f *fixture, req *gitsync.ImportRequest)) *checkFixture {
	t.Helper()
	f := newFixture(t, webFiles(), nil)
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES ('env-1', 'agent', 'one', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	rr := stackops.Repo{Store: f.st, Vault: v}
	var client gitx.Client = gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	if wrap != nil {
		client = wrap(client)
	}
	clock := &checkClock{t: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}
	svc := gitsync.New(gitsync.Deps{
		Git: client, Conns: f.conns, Repo: rr, Audit: audit.NewWriter(f.st, v, nil, clock.now), Now: clock.now, Joined: joined,
	})

	ctx := context.Background()
	req := f.request()
	if tune != nil {
		tune(f, &req)
	}
	imp, err := f.svc.Import(ctx, req)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	st, _, err := rr.CreateGitStack(ctx, stackops.CreateGitStackInput{
		EnvID: "env-1", Name: "web", Target: stacks.TargetStandalone, Actor: "ada", Copy: imp.Copy(nil), Rev: imp.Revision(),
	})
	if err != nil {
		t.Fatalf("CreateGitStack: %v", err)
	}
	return &checkFixture{fixture: f, rr: rr, svc: svc, clock: clock, stackID: st.ID}
}

// deploy records a good revision from the stack's draft, as a deploy that succeeded would.
func (f *checkFixture) deploy(t *testing.T) stackops.Snapshot {
	t.Helper()
	ctx := context.Background()
	rev, err := f.rr.NewRevision(ctx, f.stackID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if err := f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	return rev
}

func (f *checkFixture) check(t *testing.T) gitsync.Outcome {
	t.Helper()
	out, err := f.svc.Check(context.Background(), "env-1", f.stackID, gitsync.TriggerManual)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return out
}

func (f *checkFixture) state(t *testing.T) stackops.GitState {
	t.Helper()
	s, err := f.rr.GitState(context.Background(), f.stackID)
	if err != nil {
		t.Fatalf("GitState: %v", err)
	}
	return s
}

// syncFailure is one git.sync_failed row of the audit log.
type syncFailure struct {
	ActorID, TargetType, TargetID, EnvID, Result string
	Details                                      map[string]string
}

// syncFailures are the git.sync_failed entries so far, oldest first.
func (f *checkFixture) syncFailures(t *testing.T) []syncFailure {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_id, target_type, target_id, COALESCE(env_id, ''), result, details
		FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionGitSyncFailed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []syncFailure
	for rows.Next() {
		var e syncFailure
		var details string
		if err := rows.Scan(&e.ActorID, &e.TargetType, &e.TargetID, &e.EnvID, &e.Result, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &e.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// changeWeb commits a change to the stack's folder.
func (f *checkFixture) changeWeb(t *testing.T, tag string) string {
	t.Helper()
	return f.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=" + tag + "\n"}, "change web "+tag)
}

func TestCheckBehindAfterFolderCommit(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	rev := f.deploy(t)
	ctx := context.Background()
	draftBefore, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}

	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("first check = %s, want in_sync", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.HeadCommit != f.sha || s.DeployedCommit != f.sha || s.Behind != 0 || s.CheckedAt == nil {
		t.Fatalf("state after the first check = %+v", s)
	}

	head := f.changeWeb(t, "1")
	if out := f.check(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check after a commit to the folder = %s, want behind", out)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusBehind || s.Behind != 1 || s.HeadCommit != head || s.DeployedCommit != f.sha || s.DeployedTree != rev.Git.Tree {
		t.Errorf("state = %+v, want behind 1 at %s with the deployed commit %s", s, head, f.sha)
	}
	if want := f.repo.Tree(t, head, "stacks/web"); s.HeadTree != want {
		t.Errorf("head tree = %s, want the folder's %s", s.HeadTree, want)
	}

	// A check only records: no revision, no change to the draft (GIT-03).
	if _, err := f.rr.Revision(ctx, f.stackID, rev.Seq+1); !errors.Is(err, stackops.ErrRevisionNotFound) {
		t.Errorf("revision %d: err = %v, want none", rev.Seq+1, err)
	}
	draftAfter, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if draftAfter.Version != draftBefore.Version || draftAfter.Git.Commit != draftBefore.Git.Commit || draftAfter.Git.Tree != draftBefore.Git.Tree {
		t.Errorf("draft = v%d %+v, was v%d %+v: a check must not change it", draftAfter.Version, draftAfter.Git, draftBefore.Version, draftBefore.Git)
	}
	if good, ok, err := f.rr.LatestGoodRevision(ctx, f.stackID); err != nil || !ok || good.Seq != rev.Seq || good.Git.Commit != f.sha {
		t.Errorf("latest good revision = %+v, %v, %v, want the deployed one", good.Seq, ok, err)
	}
}

func TestCheckCountsOnlyCommitsThatChangeTheFolder(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.changeWeb(t, "1")
	f.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {a: {image: x}}\n"}, "other stack")
	f.changeWeb(t, "2")
	f.repo.Commit(t, map[string]string{"README.md": "readme\n"}, "readme")

	if out := f.check(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if s := f.state(t); s.Behind != 2 {
		t.Errorf("behind = %d, want 2: only the commits that changed the folder count", s.Behind)
	}
}

func TestCheckHeadMovedWithoutTouchingTheFolderStaysInSync(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	head := f.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {a: {image: x}}\n"}, "another stack")

	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check = %s, want in_sync (D-13)", out)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusInSync || s.Behind != 0 || s.HeadCommit != head || s.DeployedCommit != f.sha {
		t.Errorf("state = %+v, want in sync with the new head recorded and the deployed commit unchanged", s)
	}
}

func TestCheckAgainOnlyMovesTheTime(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.check(t)
	first := f.state(t)
	f.clock.advance(5 * time.Minute)
	f.check(t)
	second := f.state(t)
	if second.CheckedAt == nil || first.CheckedAt == nil || !second.CheckedAt.After(*first.CheckedAt) {
		t.Fatalf("checked at %v then %v, want it to move on", first.CheckedAt, second.CheckedAt)
	}
	second.CheckedAt = first.CheckedAt
	if second.Status != first.Status || second.HeadCommit != first.HeadCommit || second.Behind != first.Behind || second.HeadTree != first.HeadTree {
		t.Errorf("state changed between two identical checks: %+v then %+v", first, second)
	}
}

func TestCheckOfAStackNeverDeployedIsSkipped(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	if out := f.check(t); out != gitsync.OutcomeSkipped {
		t.Fatalf("check = %s, want skipped: there is nothing deployed to compare", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusUnknown || s.HeadCommit != f.sha || s.Behind != 0 {
		t.Errorf("state = %+v, want unknown with the head recorded", s)
	}
}

func TestCheckComparesWithTheDeployedSourceNotTheSettings(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	if _, err := f.st.Write.Exec(`UPDATE stack_git SET folder = 'stacks/other' WHERE stack_id = ?`, f.stackID); err != nil {
		t.Fatal(err)
	}
	f.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {a: {image: x}}\n"}, "other")

	if out := f.check(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if s := f.state(t); s.Behind != 1 || s.ReasonCode != string(gitsync.ReasonSourceChanged) {
		t.Errorf("state = %+v, want behind 1 with source_changed and no commits counted", s)
	}
}

func TestCheckRecordsAFailureAndRecovers(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.check(t)
	f.repo.DeleteRef(t, "refs/heads/main")

	f.clock.advance(time.Minute)
	if out := f.check(t); out != gitsync.OutcomeError {
		t.Fatalf("check of a branch that is gone = %s, want error", out)
	}
	first := f.state(t)
	if first.Status != stackops.GitStatusError || first.ReasonCode != string(gitsync.ReasonRefGone) || first.ReasonDetail == "" ||
		first.ErrorSince == nil || first.ConsecutiveFailures != 1 || first.HeadCommit != f.sha {
		t.Fatalf("state = %+v, want error ref_gone from now, one failure, the last head kept", first)
	}

	f.clock.advance(time.Minute)
	f.check(t)
	again := f.state(t)
	if again.ConsecutiveFailures != 2 || again.ErrorSince == nil || !again.ErrorSince.Equal(*first.ErrorSince) {
		t.Errorf("second failure = %+v, want 2 failures since %v", again, first.ErrorSince)
	}

	f.repo.Commit(t, map[string]string{"README.md": "back\n"}, "back")
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check once the branch is back = %s, want in_sync", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.ReasonCode != "" || s.ReasonDetail != "" || s.ErrorSince != nil || s.ConsecutiveFailures != 0 {
		t.Errorf("state = %+v, want the error cleared", s)
	}
}

func TestCheckRefGoneThenClears(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.check(t)
	if got := f.syncFailures(t); len(got) != 0 {
		t.Fatalf("audit rows before any error = %+v", got)
	}
	f.repo.DeleteRef(t, "refs/heads/main")

	f.clock.advance(time.Minute)
	if out := f.check(t); out != gitsync.OutcomeError {
		t.Fatalf("check of a branch that is gone = %s, want error", out)
	}
	first := f.state(t)
	if first.Status != stackops.GitStatusError || first.ReasonCode != string(gitsync.ReasonRefGone) || first.ErrorSince == nil ||
		first.ConsecutiveFailures != 1 || first.DeployedCommit != f.sha {
		t.Fatalf("state = %+v, want error ref_gone from now, one failure, the deployed commit kept", first)
	}
	rows := f.syncFailures(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows after the first error = %+v, want exactly one", rows)
	}
	if got := rows[0]; got.ActorID != "system:gitsync" || got.TargetType != "stack" || got.TargetID != f.stackID || got.EnvID != "env-1" || got.Result != "failure" {
		t.Errorf("audit row = %+v, want the system's failure on the stack", got)
	}
	if d := rows[0].Details; d["stack"] != "web" || d["reason_code"] != "ref_gone" || d["commit"] != f.sha[:7] || d["reason"] != first.ReasonDetail {
		t.Errorf("audit details = %v, want the stack, ref_gone, the short SHA %s and the sentence", d, f.sha[:7])
	}

	// The same error again adds to the run and writes nothing more.
	f.clock.advance(time.Minute)
	f.check(t)
	again := f.state(t)
	if again.ConsecutiveFailures != 2 || again.ErrorSince == nil || !again.ErrorSince.Equal(*first.ErrorSince) {
		t.Errorf("second failure = %+v, want 2 failures since %v", again, first.ErrorSince)
	}
	if rows := f.syncFailures(t); len(rows) != 1 {
		t.Errorf("audit rows after a repeated error = %d, want still one", len(rows))
	}

	// The branch comes back: the status clears with its reason and its start.
	f.repo.Commit(t, map[string]string{"README.md": "back\n"}, "back")
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check once the branch is back = %s, want in_sync", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.ReasonCode != "" || s.ReasonDetail != "" || s.ErrorSince != nil || s.ConsecutiveFailures != 0 {
		t.Errorf("state = %+v, want the error cleared", s)
	}
	// An error after the clear is a new error again.
	f.repo.DeleteRef(t, "refs/heads/main")
	f.check(t)
	if rows := f.syncFailures(t); len(rows) != 2 {
		t.Errorf("audit rows after an error that follows a good check = %d, want 2", len(rows))
	}
}

func TestCheckRefusesAStackThatIsNotAGitStackOfTheEnvironment(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	ctx := context.Background()
	if _, err := f.svc.Check(ctx, "env-2", f.stackID, gitsync.TriggerManual); !errors.Is(err, stackops.ErrStackNotFound) {
		t.Errorf("another environment's stack: err = %v, want ErrStackNotFound", err)
	}
	plain, _, err := f.rr.CreateStack(ctx, "env-1", "plain", stacks.TargetStandalone, "ada", stacks.WorkingCopy{})
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	if _, err := f.svc.Check(ctx, "env-1", plain.ID, gitsync.TriggerManual); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("a stack that is not from git: err = %v, want ErrNoGit", err)
	}
}

// gatedClient holds every fetch until release is closed and counts them.
type gatedClient struct {
	gitx.Client
	release chan struct{}
	mu      sync.Mutex
	fetches int
}

func (g *gatedClient) FetchRef(ctx context.Context, m gitx.MirrorID, r gitx.Remote, kind gitx.RefKind, ref string) (gitx.Commit, error) {
	g.mu.Lock()
	g.fetches++
	g.mu.Unlock()
	<-g.release
	return g.Client.FetchRef(ctx, m, r, kind, ref)
}

func TestCheckJoinsACheckThatRuns(t *testing.T) {
	t.Parallel()
	gate := &gatedClient{release: make(chan struct{})}
	waiting := make(chan int, 8)
	f := newCheckFixture(t, func(c gitx.Client) gitx.Client { gate.Client = c; return gate }, func(_ string, n int) { waiting <- n })
	f.deploy(t)

	type result struct {
		out gitsync.Outcome
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			out, err := f.svc.Check(context.Background(), "env-1", f.stackID, gitsync.TriggerManual)
			results <- result{out, err}
		}()
	}
	for n := 0; n < 2; n = <-waiting {
	}
	if !f.svc.InFlight(f.stackID) {
		t.Error("InFlight = false while a check runs")
	}
	close(gate.release)
	for range 2 {
		if r := <-results; r.err != nil || r.out != gitsync.OutcomeInSync {
			t.Errorf("joined check = %s, %v, want in_sync", r.out, r.err)
		}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.fetches != 1 {
		t.Errorf("%d fetches for two requests at once, want 1", gate.fetches)
	}
	if f.svc.InFlight(f.stackID) {
		t.Error("InFlight = true once the check ended")
	}
}

// --- the state machine and the behind edges (plan 09-15, GIT-09, GIT-03, D-12, D-13, D-16) ---

// faultyClient fails every fetch with err while err is set.
type faultyClient struct {
	gitx.Client
	err error
}

func (c *faultyClient) FetchRef(ctx context.Context, m gitx.MirrorID, r gitx.Remote, kind gitx.RefKind, ref string) (gitx.Commit, error) {
	if c.err != nil {
		return gitx.Commit{}, c.err
	}
	return c.Client.FetchRef(ctx, m, r, kind, ref)
}

func newFaultyFixture(t *testing.T) (*checkFixture, *faultyClient) {
	t.Helper()
	fc := &faultyClient{}
	f := newCheckFixture(t, func(c gitx.Client) gitx.Client { fc.Client = c; return fc }, nil)
	f.deploy(t)
	return f, fc
}

func TestCheckMonorepo(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)

	other := f.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {a: {image: x}}\n"}, "another stack")
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check after a commit to another folder = %s, want in_sync", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.Behind != 0 || s.HeadCommit != other || s.DeployedCommit != f.sha {
		t.Errorf("state = %+v, want in sync with the head %s recorded", s, other)
	}

	// A different commit with the same tree for the folder (GIT-09 adjacency).
	empty := f.repo.Commit(t, nil, "nothing changed")
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check after an empty commit = %s, want in_sync", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.Behind != 0 || s.HeadCommit != empty || s.HeadTree != s.DeployedTree {
		t.Errorf("state = %+v, want in sync at %s with the deployed tree", s, empty)
	}
}

// countFetches counts the fetches of a ref.
type countFetches struct {
	gitx.Client
	n atomic.Int32
}

func (c *countFetches) FetchRef(ctx context.Context, m gitx.MirrorID, r gitx.Remote, kind gitx.RefKind, ref string) (gitx.Commit, error) {
	c.n.Add(1)
	return c.Client.FetchRef(ctx, m, r, kind, ref)
}

func TestCheckFixedCommit(t *testing.T) {
	t.Parallel()
	count := &countFetches{}
	f := newCheckFixtureWith(t, func(c gitx.Client) gitx.Client { count.Client = c; return count }, nil,
		func(f *fixture, req *gitsync.ImportRequest) { req.RefKind, req.Ref = gitx.RefCommit, f.sha })
	ctx := context.Background()
	if sg, err := f.rr.StackGit(ctx, f.stackID); err != nil || sg.RefKind != "commit" {
		t.Fatalf("the stack follows %+v, %v, want a commit", sg, err)
	}

	// Never deployed: nothing to compare with.
	if out := f.check(t); out != gitsync.OutcomeSkipped {
		t.Fatalf("check before any deploy = %s, want skipped", out)
	}
	f.deploy(t)
	f.changeWeb(t, "1") // the repository moves on; the stack doesn't care
	requests := f.srv.Requests()
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check of a fixed commit = %s, want in_sync", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.Behind != 0 || s.HeadCommit != f.sha || s.ReasonCode != "" {
		t.Errorf("state = %+v, want in sync at the fixed commit, never behind", s)
	}
	if n := count.n.Load(); n != 0 {
		t.Errorf("%d fetches for a fixed commit, want none", n)
	}
	if got := f.srv.Requests(); got != requests {
		t.Errorf("the git server saw %d requests during the check (was %d), want no more", got, requests)
	}
}

func TestCheckMovedTagAndForcePush(t *testing.T) {
	t.Parallel()
	t.Run("a tag moved to a later commit", func(t *testing.T) {
		t.Parallel()
		f := newCheckFixtureWith(t, nil, nil, func(f *fixture, req *gitsync.ImportRequest) {
			f.repo.Tag(t, "v1", f.sha, false)
			req.RefKind, req.Ref = gitx.RefTag, "v1"
		})
		f.deploy(t)
		if out := f.check(t); out != gitsync.OutcomeInSync {
			t.Fatalf("check with the tag in place = %s, want in_sync", out)
		}
		moved := f.changeWeb(t, "1")
		f.repo.Tag(t, "v1", moved, false)
		if out := f.check(t); out != gitsync.OutcomeBehind {
			t.Fatalf("check after the tag moved = %s, want behind", out)
		}
		if s := f.state(t); s.Status != stackops.GitStatusBehind || s.Behind != 1 || s.HeadCommit != moved {
			t.Errorf("state = %+v, want behind 1 at %s", s, moved)
		}
	})
	t.Run("a branch force-pushed to an unrelated history", func(t *testing.T) {
		t.Parallel()
		f := newCheckFixture(t, nil, nil)
		f.deploy(t)
		head := f.repo.Orphan(t, map[string]string{"stacks/web/compose.yaml": webCompose, "stacks/web/web.env": "A=z\n"}, "rewritten")
		if out := f.check(t); out != gitsync.OutcomeBehind {
			t.Fatalf("check after a force-push = %s, want behind", out)
		}
		if s := f.state(t); s.Status != stackops.GitStatusBehind || s.Behind < 1 || s.HeadCommit != head || s.DeployedCommit != f.sha {
			t.Errorf("state = %+v, want behind at least 1 at %s with %s still deployed", s, head, f.sha)
		}
	})
	t.Run("a force-push that keeps the folder as it was", func(t *testing.T) {
		t.Parallel()
		f := newCheckFixture(t, nil, nil)
		f.deploy(t)
		head := f.repo.Orphan(t, webFiles(), "rewritten, same folder")
		if out := f.check(t); out != gitsync.OutcomeInSync {
			t.Fatalf("check after a force-push that changed nothing here = %s, want in_sync", out)
		}
		if s := f.state(t); s.Status != stackops.GitStatusInSync || s.Behind != 0 || s.HeadCommit != head {
			t.Errorf("state = %+v, want in sync at %s: the folder is the deployed one", s, head)
		}
	})
}

func TestCheckCap(t *testing.T) {
	t.Parallel()
	t.Run("2100 commits that change the folder", func(t *testing.T) {
		t.Parallel()
		f := newCheckFixture(t, nil, nil)
		f.deploy(t)
		f.repo.CommitMany(t, 2100, func(int) string { return "stacks/web/web.env" })
		if out := f.check(t); out != gitsync.OutcomeBehind {
			t.Fatalf("check = %s, want behind", out)
		}
		if s := f.state(t); s.Status != stackops.GitStatusBehind || s.Behind != gitsync.BehindCap || gitsync.BehindCap != 2000 {
			t.Errorf("state = %+v, want behind %d (capped)", s, gitsync.BehindCap)
		}
	})
	t.Run("a walk that stops at the cap reads as the cap even when fewer commits changed the folder", func(t *testing.T) {
		t.Parallel()
		f := newCheckFixture(t, nil, nil)
		f.deploy(t)
		f.repo.CommitMany(t, 2100, func(i int) string {
			if i%2 == 0 {
				return "stacks/web/web.env"
			}
			return "stacks/other/n.env"
		})
		f.check(t)
		if s := f.state(t); s.Status != stackops.GitStatusBehind || s.Behind != gitsync.BehindCap {
			t.Errorf("state = %+v, want behind %d: the walk stopped, so the count is a floor the screens mark with a plus", s, gitsync.BehindCap)
		}
	})
}

func TestCheckErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		code   gitsync.ReasonCode
		detail string
	}{
		{"the server is down", &gitx.Error{Code: gitx.CodeUnreachable}, gitsync.ReasonFetchFailed, "The server couldn't be reached."},
		{"the credential is refused", &gitx.Error{Code: gitx.CodeAuthRefused}, gitsync.ReasonFetchFailed, "The server refused the credential."},
		{"the host key changed", &gitx.Error{Code: gitx.CodeHostKeyChanged}, gitsync.ReasonHostKeyChanged, "The server's host key changed."},
		{"the host key is not trusted", &gitx.Error{Code: gitx.CodeHostKeyUntrusted}, gitsync.ReasonHostKeyUntrusted, "The server's host key isn't trusted yet."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, fc := newFaultyFixture(t)
			if out := f.check(t); out != gitsync.OutcomeInSync {
				t.Fatalf("first check = %s, want in_sync", out)
			}
			fc.err = tc.err
			if out := f.check(t); out != gitsync.OutcomeError {
				t.Fatalf("check = %s, want error", out)
			}
			s := f.state(t)
			if s.Status != stackops.GitStatusError || s.ReasonCode != string(tc.code) || s.ReasonDetail != tc.detail || s.ErrorSince == nil ||
				s.DeployedCommit != f.sha {
				t.Errorf("state = %+v, want error %s %q with the deployed commit kept", s, tc.code, tc.detail)
			}
		})
	}

	t.Run("the git host is down", func(t *testing.T) {
		t.Parallel()
		f := newCheckFixture(t, nil, nil)
		f.deploy(t)
		f.srv.Close()
		if out := f.check(t); out != gitsync.OutcomeError {
			t.Fatalf("check with the server down = %s, want error", out)
		}
		if s := f.state(t); s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonFetchFailed) || s.ReasonDetail == "" {
			t.Errorf("state = %+v, want error fetch_failed with a sentence", s)
		}
	})

	t.Run("the folder is removed", func(t *testing.T) {
		t.Parallel()
		f := newCheckFixture(t, nil, nil)
		f.deploy(t)
		gone := map[string]string{}
		for p := range webFiles() {
			if strings.HasPrefix(p, "stacks/web/") {
				gone[p] = gittest.Delete
			}
		}
		f.repo.Commit(t, gone, "remove the stack")
		if out := f.check(t); out != gitsync.OutcomeError {
			t.Fatalf("check after the folder went = %s, want error", out)
		}
		if s := f.state(t); s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonFolderGone) {
			t.Errorf("state = %+v, want error folder_gone", s)
		}
	})
}

func TestCheckHeadFailsChecks(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	ctx := context.Background()
	draftBefore, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}

	// The new commit needs a variable nobody has set.
	bad := f.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": "services:\n  web:\n    image: nginx:${NEWTAG}\n    env_file: web.env\n"}, "needs NEWTAG")
	if out := f.check(t); out != gitsync.OutcomeError {
		t.Fatalf("check of a head that fails the checks = %s, want error", out)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonChecksFailed) || !strings.Contains(s.ReasonDetail, "NEWTAG") ||
		s.HeadCommit != bad || s.DeployedCommit != f.sha || s.Behind != 1 {
		t.Fatalf("state = %+v, want error checks_failed naming NEWTAG at %s, the deployed commit kept", s, bad)
	}
	// Nothing was deployed and nothing was drafted.
	draftAfter, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if draftAfter.Version != draftBefore.Version || draftAfter.Git.Commit != f.sha {
		t.Errorf("draft = v%d at %s, was v%d: a failed check must not change it", draftAfter.Version, draftAfter.Git.Commit, draftBefore.Version)
	}
	if good, ok, err := f.rr.LatestGoodRevision(ctx, f.stackID); err != nil || !ok || good.Git.Commit != f.sha {
		t.Errorf("latest good revision = %+v, %v, %v, want the one deployed before", good.Git, ok, err)
	}

	// The stack's own variable fills it: the head passes and the stack is behind.
	copy := draftAfter.Copy
	copy.Vars = append(slices.Clone(copy.Vars), stacks.Var{Name: "NEWTAG", Value: "2"})
	if _, _, err := f.rr.SaveDraft(ctx, f.stackID, draftAfter.Version, "ada", copy); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if out := f.check(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check once the variable is set = %s, want behind", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusBehind || s.Behind != 1 || s.ReasonCode != "" || s.ErrorSince != nil || s.ConsecutiveFailures != 0 {
		t.Errorf("state = %+v, want behind 1 with the error cleared", s)
	}
}

func TestCheckSourceChanged(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	f.deploy(t)
	f.repo.Branch(t, "dev", f.sha)
	if _, err := f.st.Write.Exec(`UPDATE stack_git SET ref = 'dev' WHERE stack_id = ?`, f.stackID); err != nil {
		t.Fatal(err)
	}
	if out := f.check(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check after the ref changed = %s, want behind", out)
	}
	if s := f.state(t); s.Status != stackops.GitStatusBehind || s.ReasonCode != string(gitsync.ReasonSourceChanged) || s.Behind != 0 && s.Behind != 1 {
		t.Errorf("state = %+v, want behind with source_changed", s)
	}
	// The ref is put back: the note goes.
	if _, err := f.st.Write.Exec(`UPDATE stack_git SET ref = 'main' WHERE stack_id = ?`, f.stackID); err != nil {
		t.Fatal(err)
	}
	if out := f.check(t); out != gitsync.OutcomeInSync {
		t.Fatalf("check with the ref back = %s, want in_sync", out)
	}
	if s := f.state(t); s.ReasonCode != "" || s.ReasonDetail != "" {
		t.Errorf("state = %+v, want no reason", s)
	}
}

func TestCheckAuditOnce(t *testing.T) {
	t.Parallel()
	f, fc := newFaultyFixture(t)
	fc.err = &gitx.Error{Code: gitx.CodeUnreachable}
	f.check(t)
	f.clock.advance(time.Minute)
	f.check(t)
	if s := f.state(t); s.ConsecutiveFailures != 2 {
		t.Errorf("consecutive failures = %d, want 2", s.ConsecutiveFailures)
	}
	if rows := f.syncFailures(t); len(rows) != 1 {
		t.Fatalf("audit rows after two identical errors = %d, want 1", len(rows))
	}

	// Another reason is a new error: a second row, and the run starts again.
	fc.err = &gitx.Error{Code: gitx.CodeHostKeyChanged}
	f.clock.advance(time.Minute)
	f.check(t)
	s := f.state(t)
	if s.ReasonCode != string(gitsync.ReasonHostKeyChanged) || s.ConsecutiveFailures != 1 {
		t.Errorf("state = %+v, want host_key_changed on its first failure", s)
	}
	rows := f.syncFailures(t)
	if len(rows) != 2 || rows[1].Details["reason_code"] != "host_key_changed" {
		t.Fatalf("audit rows = %+v, want a second one for host_key_changed", rows)
	}

	// A good check clears everything.
	fc.err = nil
	f.check(t)
	if s := f.state(t); s.Status != stackops.GitStatusInSync || s.ReasonCode != "" || s.ErrorSince != nil || s.ConsecutiveFailures != 0 {
		t.Errorf("state = %+v, want the error cleared", s)
	}
}

func TestCheckEmptyStates(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	s := f.state(t)
	if s.Status != stackops.GitStatusUnknown || s.CheckedAt != nil || s.DeployedCommit != "" || s.Behind != 0 {
		t.Errorf("state before any check = %+v, want unknown, never checked, nothing deployed", s)
	}
	// Checked but never deployed: the head is known, the deployed commit is empty and the stack is not behind.
	f.check(t)
	s = f.state(t)
	if s.Status != stackops.GitStatusUnknown || s.CheckedAt == nil || s.DeployedCommit != "" || s.HeadCommit != f.sha || s.Behind != 0 {
		t.Errorf("state after a check of a stack never deployed = %+v, want unknown with only the head", s)
	}
	// Whenever the status is behind, it says by how much or why.
	f.deploy(t)
	f.changeWeb(t, "1")
	f.check(t)
	if s := f.state(t); s.Status != stackops.GitStatusBehind || (s.Behind < 1 && s.ReasonCode != string(gitsync.ReasonSourceChanged)) {
		t.Errorf("state = %+v, want behind >= 1 or source_changed", s)
	}
}
