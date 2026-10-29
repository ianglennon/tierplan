// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The background poller (plan 09-18, GIT-06, D-14): it finds changes on its own, on a clock the test moves, and
// only records what it sees.

// pollClock is the clock of the poller and of the checks it makes. After arms a timer and announces it, so that a test
// that moves the clock knows the poller is waiting on it and does not sleep to find out.
type pollClock struct {
	*checkClock
	tmu    sync.Mutex
	timers []pollTimer
	armed  chan struct{}
}

type pollTimer struct {
	at time.Time
	ch chan time.Time
}

func newPollClock(c *checkClock) *pollClock {
	return &pollClock{checkClock: c, armed: make(chan struct{}, 4096)}
}

func (c *pollClock) Now() time.Time { return c.now() }

func (c *pollClock) After(d time.Duration) <-chan time.Time {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, pollTimer{at: c.Now().Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *pollClock) Advance(d time.Duration) {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	c.advance(d)
	now := c.Now()
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(now) {
			t.ch <- now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// Tick waits for the poller to arm its next timer, which it does once a round is done, and then moves the clock on by d.
func (c *pollClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	testsupport.Await(t, "the poller to arm its next tick", c.armed)
	c.Advance(d)
}

// polled is what the poller reported of one stack.
type polled struct{ stackID, outcome string }

// pollFixture is a check fixture whose stacks are polled on a clock the test moves.
type pollFixture struct {
	*checkFixture
	pc      *pollClock
	checked chan polled
	// afterChecked, when set, runs in the poller after each report, once the test has been told of it.
	afterChecked func()
}

func newPollFixture(t *testing.T, wrap func(gitx.Client) gitx.Client) *pollFixture {
	t.Helper()
	cf := newCheckFixture(t, wrap, nil)
	cf.deploy(t)
	return &pollFixture{checkFixture: cf, pc: newPollClock(cf.clock), checked: make(chan polled, 256)}
}

// start runs the poller until the test ends. It fills in the clock and the seam that reports what was checked.
func (f *pollFixture) start(t *testing.T, d gitsync.PollerDeps) {
	t.Helper()
	d.Clock = f.pc
	d.Checked = func(stackID, outcome string) {
		f.checked <- polled{stackID, outcome}
		if f.afterChecked != nil {
			f.afterChecked()
		}
	}
	if d.Rand == nil {
		d.Rand = func() float64 { return 0 }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.RunPoller(ctx, d)
	}()
	t.Cleanup(func() {
		cancel()
		testsupport.Await(t, "the poller to stop", done)
	})
}

// awaitChecked receives what the poller reported of the next n checks, in the order it reported them.
func (f *pollFixture) awaitChecked(t *testing.T, n int) []polled {
	t.Helper()
	out := make([]polled, 0, n)
	for range n {
		out = append(out, testsupport.Await(t, "the poller to check a stack", f.checked))
	}
	return out
}

// constRand is a random source that always answers v.
func constRand(v float64) func() float64 { return func() float64 { return v } }

func TestPollerFindsFolderChange(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	ctx := context.Background()
	rev, ok, err := f.rr.LatestGoodRevision(ctx, f.stackID)
	if err != nil || !ok {
		t.Fatalf("LatestGoodRevision: %v, %v", ok, err)
	}
	draftBefore, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}

	// Somebody pushes to the stack's folder; nobody presses anything.
	head := f.changeWeb(t, "1")
	f.start(t, gitsync.PollerDeps{Rand: constRand(0.5)})
	f.pc.Tick(t, 5*time.Minute)

	got := f.awaitChecked(t, 1)
	if got[0].stackID != f.stackID || got[0].outcome != string(gitsync.OutcomeBehind) {
		t.Fatalf("the poller reported %+v, want the stack behind", got)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusBehind || s.Behind != 1 || s.HeadCommit != head || s.DeployedCommit != f.sha {
		t.Errorf("state = %+v, want behind 1 at %s with the deployed commit %s", s, head, f.sha)
	}

	// A poll only records: no revision, no change to the draft (the poller never deploys).
	if _, err := f.rr.Revision(ctx, f.stackID, rev.Seq+1); err == nil {
		t.Errorf("revision %d exists after a poll, want none", rev.Seq+1)
	}
	draftAfter, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if draftAfter.Version != draftBefore.Version || draftAfter.Git.Commit != draftBefore.Git.Commit {
		t.Errorf("draft = v%d %+v, was v%d %+v: a poll must not change it", draftAfter.Version, draftAfter.Git, draftBefore.Version, draftBefore.Git)
	}
	if good, ok, err := f.rr.LatestGoodRevision(ctx, f.stackID); err != nil || !ok || good.Seq != rev.Seq {
		t.Errorf("latest good revision = %d, %v, %v, want the deployed one %d", good.Seq, ok, err, rev.Seq)
	}
}

// --- the manners of the poller (RESEARCH Pitfalls 4 and 14, T-09-54, T-09-56) ---

// addStack makes another stack from the fixture's folder, on the repository repoID, and deploys it. tune may change the
// request the stack is made from.
func (f *pollFixture) addStack(t *testing.T, name, repoID string, tune func(*gitsync.ImportRequest)) string {
	t.Helper()
	ctx := context.Background()
	req := f.request()
	req.Name, req.RepoID = name, repoID
	if tune != nil {
		tune(&req)
	}
	imp, err := f.svc.Import(ctx, req)
	if err != nil {
		t.Fatalf("Import %s: %v", name, err)
	}
	st, _, err := f.rr.CreateGitStack(ctx, stackops.CreateGitStackInput{
		EnvID: "env-1", Name: name, Target: stacks.TargetStandalone, Actor: "ada", Copy: imp.Copy(nil), Rev: imp.Revision(),
	})
	if err != nil {
		t.Fatalf("CreateGitStack %s: %v", name, err)
	}
	rev, err := f.rr.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision %s: %v", name, err)
	}
	if err := f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatalf("SetOutcome %s: %v", name, err)
	}
	return st.ID
}

// checkNow makes the check of a stack by hand, so that the poller finds it already seen.
func (f *pollFixture) checkNow(t *testing.T, stackID string) {
	t.Helper()
	if _, err := f.svc.Check(context.Background(), "env-1", stackID, gitsync.TriggerManual); err != nil {
		t.Fatalf("Check %s: %v", stackID, err)
	}
}

func (f *pollFixture) stateOf(t *testing.T, stackID string) stackops.GitState {
	t.Helper()
	s, err := f.rr.GitState(context.Background(), stackID)
	if err != nil {
		t.Fatalf("GitState %s: %v", stackID, err)
	}
	return s
}

// seq is a random source that gives out the values it was given, then zero.
func seq(vals ...float64) func() float64 {
	var mu sync.Mutex
	i := 0
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(vals) {
			return 0
		}
		i++
		return vals[i-1]
	}
}

func TestPollerDedup(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	ids := []string{f.stackID}
	for i := 1; i < 10; i++ {
		ids = append(ids, f.addStack(t, fmt.Sprintf("web%d", i), f.id, nil))
	}
	for _, id := range ids {
		f.checkNow(t, id)
	}
	base := f.srv.Requests()

	// The ten stacks are due at ten different moments of their interval, 30 s apart. They share one listing of the
	// repository for as long as it is good: one advertisement request for the interval, not ten.
	f.start(t, gitsync.PollerDeps{Rand: seq(0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9)})
	due := []int{2, 1, 1, 1, 1, 1, 1, 1, 1, 0, 2} // checks due at T0+30 s, T0+60 s ... T0+330 s
	for step, n := range due {
		f.pc.Tick(t, 30*time.Second)
		for _, c := range f.awaitChecked(t, n) {
			if c.outcome != string(gitsync.OutcomeInSync) {
				t.Fatalf("step %d: %s = %s, want in_sync", step+1, c.stackID, c.outcome)
			}
		}
		if step == 9 { // T0+300 s: the whole first interval is over
			if got := f.srv.Requests(); got != base+1 {
				t.Errorf("the git server saw %d advertisement requests for ten stacks in one interval, want 1", got-base)
			}
		}
	}
	if got := f.srv.Requests(); got != base+2 {
		t.Errorf("the git server saw %d advertisement requests in two intervals, want 2", got-base)
	}
}

// TestPollerReportsOnceFree: the poller reports the last check of a read once the repository is free again. A test
// that moves the clock on as soon as it is told finds the next round able to read the repository, even when the
// goroutine that made the read has not run on yet.
func TestPollerReportsOnceFree(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	f.checkNow(t, f.stackID)
	hold := make(chan struct{})
	f.afterChecked = func() { <-hold }

	f.start(t, gitsync.PollerDeps{})
	f.pc.Tick(t, 15*time.Second)
	f.awaitChecked(t, 1)
	// The goroutine of the first read is held just after its report while the next round runs.
	f.pc.Tick(t, 5*time.Minute)
	testsupport.Await(t, "the second round to finish", f.pc.armed)
	close(hold)
	if got := f.awaitChecked(t, 1); got[0].outcome != string(gitsync.OutcomeInSync) {
		t.Fatalf("the check of the second round = %+v, want in_sync", got)
	}
}

func TestPollerCheapPath(t *testing.T) {
	t.Parallel()
	fetches := &countFetches{}
	f := newPollFixture(t, func(c gitx.Client) gitx.Client { fetches.Client = c; return fetches })
	f.checkNow(t, f.stackID)
	first := f.state(t)
	base, fetched := f.srv.Requests(), fetches.n.Load()

	// The head did not move: the listing is the only request, nothing is fetched and only the time changes.
	f.start(t, gitsync.PollerDeps{})
	f.pc.Tick(t, 15*time.Second)
	if got := f.awaitChecked(t, 1); got[0].outcome != string(gitsync.OutcomeInSync) {
		t.Fatalf("poll of an unchanged repository = %+v, want in_sync", got)
	}
	if got := f.srv.Requests(); got != base+1 {
		t.Errorf("the git server saw %d requests, want the one listing", got-base)
	}
	if got := fetches.n.Load(); got != fetched {
		t.Errorf("%d fetches for a head that did not move, want none", got-fetched)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusInSync || s.HeadCommit != first.HeadCommit || s.HeadTree != first.HeadTree {
		t.Errorf("state = %+v, want the answer of the first check", s)
	}
	if s.CheckedAt == nil || !s.CheckedAt.Equal(f.pc.Now()) {
		t.Errorf("checked at %v, want %v: the poll marks the stack as checked", s.CheckedAt, f.pc.Now())
	}

	// A head that moved is fetched, even when it did not touch the folder (D-13).
	head := f.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {a: {image: x}}\n"}, "another stack")
	f.pc.Tick(t, 5*time.Minute)
	if got := f.awaitChecked(t, 1); got[0].outcome != string(gitsync.OutcomeInSync) {
		t.Fatalf("poll after a commit to another folder = %+v, want in_sync", got)
	}
	if got := fetches.n.Load(); got != fetched+1 {
		t.Errorf("%d fetches for a head that moved, want one", got-fetched)
	}
	if s := f.state(t); s.HeadCommit != head || s.Status != stackops.GitStatusInSync {
		t.Errorf("state = %+v, want in sync with the new head %s recorded", s, head)
	}
}

// gateClient holds every fetch until the test lets it through, and notes how many were in progress at once.
type gateClient struct {
	gitx.Client
	on      atomic.Bool
	entered chan struct{}
	release chan struct{}

	mu        sync.Mutex
	cur, max  int
	byMirror  map[gitx.MirrorID]int
	mirrorMax int
}

func newGateClient() *gateClient {
	return &gateClient{entered: make(chan struct{}, 256), release: make(chan struct{}, 256), byMirror: map[gitx.MirrorID]int{}}
}

func (g *gateClient) FetchRef(ctx context.Context, m gitx.MirrorID, r gitx.Remote, kind gitx.RefKind, ref string) (gitx.Commit, error) {
	if !g.on.Load() {
		return g.Client.FetchRef(ctx, m, r, kind, ref)
	}
	g.mu.Lock()
	g.cur++
	g.max = max(g.max, g.cur)
	g.byMirror[m]++
	g.mirrorMax = max(g.mirrorMax, g.byMirror[m])
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.release
	c, err := g.Client.FetchRef(ctx, m, r, kind, ref)
	g.mu.Lock()
	g.cur--
	g.byMirror[m]--
	g.mu.Unlock()
	return c, err
}

func (g *gateClient) current() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cur
}

func TestPollerConcurrencyCap(t *testing.T) {
	t.Parallel()
	gate := newGateClient()
	f := newPollFixture(t, func(c gitx.Client) gitx.Client { gate.Client = c; return gate })
	// Eight repositories, two stacks each, none seen yet: every stack needs a real fetch.
	repos := []string{f.id}
	for i := 1; i < 8; i++ {
		repos = append(repos, f.addRepo(t, nil, fmt.Sprintf("acme%d", i)))
	}
	total := 1
	for i, repo := range repos {
		for j := range 2 {
			if i == 0 && j == 0 {
				continue
			}
			f.addStack(t, fmt.Sprintf("web-%d-%d", i, j), repo, nil)
			total++
		}
	}
	gate.on.Store(true)

	f.start(t, gitsync.PollerDeps{})
	f.pc.Tick(t, 15*time.Second)
	for range 4 {
		testsupport.Await(t, "four fetches to begin", gate.entered)
	}
	if n := gate.current(); n != 4 {
		t.Errorf("%d fetches in progress, want 4: the cap is 4 at once", n)
	}
	// Let them through one at a time: the next ones start as places free up, never more than 4 together.
	for i := 4; i < total; i++ {
		gate.release <- struct{}{}
		testsupport.Await(t, "the next fetch to begin", gate.entered)
	}
	for range 4 {
		gate.release <- struct{}{}
	}
	f.awaitChecked(t, total)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.max != 4 {
		t.Errorf("at most %d fetches ran at once, want exactly the cap of 4 reached and never passed", gate.max)
	}
	if gate.mirrorMax != 1 {
		t.Errorf("%d fetches of one repository ran at once, want 1", gate.mirrorMax)
	}
}

func TestPollerBackoff(t *testing.T) {
	t.Parallel()
	fc := &faultyClient{}
	f := newPollFixture(t, func(c gitx.Client) gitx.Client { fc.Client = c; return fc })
	fc.err = &gitx.Error{Code: gitx.CodeUnreachable, Host: "git.example.com"}
	interval := 5 * time.Minute

	f.start(t, gitsync.PollerDeps{})
	// The first check comes when the stack's spread puts it (at once with a zero draw); each failure doubles the wait.
	wants := []time.Duration{interval, 2 * interval, 4 * interval, 8 * interval, time.Hour, time.Hour}
	wait := 15 * time.Second
	for i, want := range wants {
		f.pc.Tick(t, wait)
		if got := f.awaitChecked(t, 1); got[0].outcome != string(gitsync.OutcomeError) {
			t.Fatalf("check %d = %+v, want error", i+1, got)
		}
		s := f.state(t)
		if s.ConsecutiveFailures != i+1 || s.NextCheckAt == nil {
			t.Fatalf("after %d failures the state is %+v", i+1, s)
		}
		if got := s.NextCheckAt.Sub(f.pc.Now()); got != want {
			t.Errorf("after %d failures the next check is in %v, want %v", i+1, got, want)
		}
		wait = want
	}

	// The first success returns the stack to its interval.
	fc.err = nil
	f.pc.Tick(t, wait)
	if got := f.awaitChecked(t, 1); got[0].outcome != string(gitsync.OutcomeInSync) {
		t.Fatalf("check after the repository came back = %+v, want in_sync", got)
	}
	s := f.state(t)
	if s.ConsecutiveFailures != 0 || s.NextCheckAt == nil || s.NextCheckAt.Sub(f.pc.Now()) != interval {
		t.Errorf("state after the first success = %+v, want no failures and the next check in %v", s, interval)
	}
}

func TestPollerRestartSpread(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	ids := []string{f.stackID}
	for i := 1; i < 4; i++ {
		ids = append(ids, f.addStack(t, fmt.Sprintf("web%d", i), f.id, nil))
	}
	slices.Sort(ids)
	base := f.srv.Requests()
	start := f.pc.Now()

	// None of the stacks has been scheduled (the server just started): the poller spreads their first check over the
	// first interval instead of making them all at once.
	f.start(t, gitsync.PollerDeps{Rand: seq(0.2, 0.4, 0.7, 0.9)})
	f.pc.Tick(t, 15*time.Second) // round 1 is over; round 2 finds nothing due
	f.pc.Tick(t, 15*time.Second) // round 2 is over
	for i, frac := range []float64{0.2, 0.4, 0.7, 0.9} {
		s := f.stateOf(t, ids[i])
		want := start.Add(time.Duration(frac * float64(5*time.Minute)))
		if s.NextCheckAt == nil || !s.NextCheckAt.Equal(want) {
			t.Errorf("stack %d is next checked at %v, want %v", i, s.NextCheckAt, want)
		}
		if s.CheckedAt != nil {
			t.Errorf("stack %d was checked at %v, want not yet", i, s.CheckedAt)
		}
	}
	if got := f.srv.Requests(); got != base {
		t.Errorf("the git server saw %d requests before any stack was due, want none", got-base)
	}
	if n := len(f.checked); n != 0 {
		t.Errorf("%d stacks were checked at the start, want none", n)
	}
}

func TestPollerOrdering(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	other := f.addRepo(t, nil, "second")
	x := f.stackID
	y := f.addStack(t, "web1", f.id, nil)
	z := f.addStack(t, "web2", other, nil)
	ctx := context.Background()
	now := f.pc.Now()
	// x and y are due together; z, on another repository, is due earlier.
	for id, at := range map[string]time.Time{x: now.Add(-10 * time.Second), y: now.Add(-10 * time.Second), z: now.Add(-20 * time.Second)} {
		if err := f.rr.SetNextCheck(ctx, id, at); err != nil {
			t.Fatal(err)
		}
	}
	pair := []string{x, y}
	slices.Sort(pair)

	f.start(t, gitsync.PollerDeps{MaxFetches: 1})
	f.pc.Tick(t, time.Second)
	got := f.awaitChecked(t, 3)
	order := []string{got[0].stackID, got[1].stackID, got[2].stackID}
	if want := []string{z, pair[0], pair[1]}; !slices.Equal(order, want) {
		t.Errorf("stacks were checked in the order %v, want %v: next check time first, then stack ID", order, want)
	}
}

func TestPollerNothingToPoll(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	other := f.addRepo(t, nil, "second")
	third := f.addRepo(t, nil, "third")
	off := f.addStack(t, "off", other, nil)
	fixed := f.addStack(t, "fixed", third, func(r *gitsync.ImportRequest) { r.RefKind, r.Ref = gitx.RefCommit, f.sha })
	// The fixture's own stack stops polling too: no repository has a polled stack.
	for _, id := range []string{f.stackID, off} {
		if _, err := f.st.Write.Exec(`UPDATE stack_git SET poll_enabled = 0 WHERE stack_id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	list, err := f.rr.PolledStacks(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("PolledStacks = %+v, %v, want none: polling off, and a fixed commit is never polled", list, err)
	}
	base := f.srv.Requests()

	f.start(t, gitsync.PollerDeps{})
	for range 4 {
		f.pc.Tick(t, time.Hour)
	}
	if got := f.srv.Requests(); got != base {
		t.Errorf("the git server saw %d requests with nothing to poll, want none", got-base)
	}
	if n := len(f.checked); n != 0 {
		t.Errorf("%d checks with nothing to poll, want none", n)
	}
	if s := f.stateOf(t, fixed); s.CheckedAt != nil {
		t.Errorf("the fixed commit stack was checked at %v, want never polled", s.CheckedAt)
	}
}

func TestPollerRefGone(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	f.checkNow(t, f.stackID)
	f.repo.DeleteRef(t, "refs/heads/main")

	f.start(t, gitsync.PollerDeps{})
	f.pc.Tick(t, 15*time.Second)
	if got := f.awaitChecked(t, 1); got[0].outcome != string(gitsync.OutcomeError) {
		t.Fatalf("poll of a branch that is gone = %+v, want error", got)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonRefGone) {
		t.Errorf("state = %+v, want error ref_gone and never in sync", s)
	}
}

func TestPollerNeverDeploys(t *testing.T) {
	t.Parallel()
	f := newPollFixture(t, nil)
	ctx := context.Background()
	good, ok, err := f.rr.LatestGoodRevision(ctx, f.stackID)
	if err != nil || !ok {
		t.Fatalf("LatestGoodRevision: %v, %v", ok, err)
	}
	draft, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if sg, err := f.rr.StackGit(ctx, f.stackID); err != nil || sg.AutoDeploy {
		t.Fatalf("StackGit = %+v, %v, want auto-deploy off", sg, err)
	}

	f.start(t, gitsync.PollerDeps{})
	for i := range 4 {
		f.changeWeb(t, fmt.Sprint(i))
		f.pc.Tick(t, 5*time.Minute)
		f.awaitChecked(t, 1)
	}
	if s := f.state(t); s.Status != stackops.GitStatusBehind || s.Behind != 4 {
		t.Errorf("state = %+v, want behind 4: the poll records the new head and the count", s)
	}
	if _, err := f.rr.Revision(ctx, f.stackID, good.Seq+1); err == nil {
		t.Errorf("revision %d exists after the polls, want none: a poll never deploys", good.Seq+1)
	}
	after, err := f.rr.Draft(ctx, f.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != draft.Version || after.Git.Commit != draft.Git.Commit {
		t.Errorf("draft = v%d %+v, was v%d %+v: a poll must not change it", after.Version, after.Git, draft.Version, draft.Git)
	}
}

// A commit an automatic deploy holds back is waiting for a person, not failing to be read: the repository answered, so
// the stack is polled at its interval and a fix pushed meanwhile is deployed soon, not after the backoff of a down
// server (plan 09-25).
func TestPollerBlockedAutoDeployKeepsItsInterval(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.dep.set(func(d *fakeDeployer) {
		d.refuse = &gitsync.DeployRefusal{Code: "policy_refused", Detail: "This stack uses settings that need Admin."}
	})
	f := &pollFixture{checkFixture: a.checkFixture, pc: newPollClock(a.clock), checked: make(chan polled, 256)}
	a.changeWeb(t, "bind")
	interval := 5 * time.Minute

	f.start(t, gitsync.PollerDeps{})
	wait := 15 * time.Second
	for i := range 4 {
		f.pc.Tick(t, wait)
		got := f.awaitChecked(t, 1)
		if want := string(gitsync.OutcomeError); i > 0 && got[0].outcome != want {
			t.Fatalf("check %d = %+v, want %s", i+1, got, want)
		}
		if next := f.state(t).NextCheckAt; next == nil || next.Sub(f.pc.Now()) != interval {
			t.Fatalf("after check %d the next check is at %v (now %v), want in %v", i+1, next, f.pc.Now(), interval)
		}
		wait = interval
	}
	if a.dep.count() != 1 {
		t.Errorf("deploys started = %d, want the one attempt", a.dep.count())
	}
	if s := f.state(t); s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) || s.ConsecutiveFailures < 2 {
		t.Errorf("state = %+v, want the commit still held back", s)
	}
}

// A repository that answered, but whose head or ref a person must fix, is polled at the stack's interval and not backed
// off like a server that is down: the push that fixes it is noticed at the next interval, not up to an hour late
// (review WR-B04).
func TestPollerAnsweredRepositoryKeepsItsInterval(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		spoil  func(t *testing.T, f *pollFixture)
		reason gitsync.ReasonCode
	}{
		"ref gone": {
			spoil:  func(t *testing.T, f *pollFixture) { f.repo.DeleteRef(t, "refs/heads/main") },
			reason: gitsync.ReasonRefGone,
		},
		"head fails the checks": {
			spoil: func(t *testing.T, f *pollFixture) {
				f.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": "services:\n  web:\n    image: nginx:${NEWTAG}\n    env_file: web.env\n"}, "needs NEWTAG")
			},
			reason: gitsync.ReasonChecksFailed,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPollFixture(t, nil)
			f.checkNow(t, f.stackID)
			tc.spoil(t, f)
			interval := 5 * time.Minute

			f.start(t, gitsync.PollerDeps{})
			wait := 15 * time.Second
			for i := range 5 {
				f.pc.Tick(t, wait)
				if got := f.awaitChecked(t, 1); got[0].outcome != string(gitsync.OutcomeError) {
					t.Fatalf("check %d = %+v, want error", i+1, got)
				}
				s := f.state(t)
				if s.ReasonCode != string(tc.reason) {
					t.Fatalf("check %d: state = %+v, want %s", i+1, s, tc.reason)
				}
				if s.NextCheckAt == nil || s.NextCheckAt.Sub(f.pc.Now()) != interval {
					t.Fatalf("after check %d the next check is at %v (now %v), want in %v", i+1, s.NextCheckAt, f.pc.Now(), interval)
				}
				wait = interval
			}
		})
	}
}
