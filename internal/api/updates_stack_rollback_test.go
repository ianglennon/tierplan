// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The automatic rollback of a failed update of a managed stack (plan 12-23, IMG-07, D-12, D-14, RESEARCH Pitfall 10). Docker is a fake
// swarm behind the real monitor, aggregator and cluster reader; the registry is a scripted Heads; the stack engine, the job hub, the
// operation lock and the run, failure and audit stores are the real ones. The stack's waits run on a clock that answers at once.

// shopQuick is newShop with waits that cost no time.
func shopQuick(t *testing.T, services ...string) *shopFix {
	t.Helper()
	return newShopWith(t, func(f *envFix) {
		f.stackClock = &instantClock{now: time.Unix(1_700_000_000, 0)}
	}, services...)
}

// rejectNewVersion makes every task of the service fail while it holds the digest c, as a swarm does when it cannot pull or start an
// image, and run once the service holds another.
func (s *shopFix) rejectNewVersion(service string, c byte) {
	var script []dockertest.FakeServiceTask
	for slot := 1; slot <= 3; slot++ {
		script = append(script, dockertest.FakeServiceTask{Slot: slot, State: "rejected", Desired: "shutdown", Err: "No such image"})
	}
	s.f.fake.SetServiceTasksWhileImage("shop_"+service, updDigest(c), script...)
}

// startUpdate asks for an update and waits for the stack's job to end, whatever it ended as.
func (s *shopFix) startUpdate(t *testing.T, kind, name string) (string, jobs.Job) {
	t.Helper()
	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody(kind, name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update %s %s: %d %s", kind, name, rec.Code, rec.Body)
	}
	id := answerOf[stackRunAnswer](t, rec).JobID
	job, err := s.f.jobHub.Wait(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return id, job
}

// failures are the digests the environment remembers as failed, by kind/name.
func (s *shopFix) failures(t *testing.T) map[string]updates.Failure {
	t.Helper()
	got, err := (updates.FailureStore{Q: s.f.st.Q, W: s.f.st.Write}).ForEnv(context.Background(), s.e.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The tracer (plan 12-23, IMG-07, D-12): the new tasks of the updated service are rejected, so the stack is rolled back in the same
// console to the revision before the update: a "rollback of #1" revision deployed with that revision's own pins, the run rolled back,
// the digest remembered and the badge reading rolled back.
func TestStackUpdateFailureRollsBack(t *testing.T) {
	t.Parallel()
	s := shopQuick(t, "web", "api")
	s.move(t, "web", 'b')
	s.rejectNewVersion("web", 'b')
	before := len(s.f.fake.ServiceUpdates())

	jobID, job := s.startUpdate(t, "stack", "shop")

	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 3 || revs[0].Outcome != "good" || revs[1].Outcome != "failed" || revs[2].Outcome != "good" {
		t.Fatalf("revisions = %+v, want good 1, the failed update 2 and a good rollback 3", revs)
	}
	if origin, from := s.f.revisionOrigin(t, s.st.ID, 2); origin != "update" || from != 0 {
		t.Errorf("revision 2 = %q restored from %d, want the update", origin, from)
	}
	if origin, from := s.f.revisionOrigin(t, s.st.ID, 3); origin != "rollback" || from != 1 {
		t.Errorf("revision 3 = %q restored from %d, want a rollback of #1", origin, from)
	}
	if got, want := pinsOf(t, revs[2]), pinsOf(t, revs[0]); got["shop_web"] != want["shop_web"] || got["shop_api"] != want["shop_api"] {
		t.Errorf("rollback pins = %v, want revision 1's %v", got, want)
	}
	// The last write of the rollback put web back at the digest it ran.
	writes := s.f.fake.ServiceUpdates()[before:]
	last := map[string]string{}
	for _, u := range writes {
		last[u.Spec.Name] = u.Spec.TaskTemplate.ContainerSpec.Image
	}
	if last["shop_web"] != "registry.test/team/web:1@"+updDigest('a') {
		t.Errorf("web was last written as %q, want it at the digest it ran before", last["shop_web"])
	}

	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart || run.RevisionSeq != 2 || run.RollbackSeq != 3 || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v, want rolled_back (didnt_start) of revision 2 by rollback 3", run)
	}
	steps := stepStates(job)
	if steps[updates.ItemRollback] != jobs.ItemDone || steps[updates.ItemRollbackWait] != jobs.ItemDone {
		t.Errorf("steps = %v, want both rollback steps added and done", steps)
	}
	var label string
	for _, it := range job.Items {
		if it.ID == updates.ItemRollback {
			label = it.Label
		}
	}
	if label != "Roll back to revision #1" {
		t.Errorf("rollback step = %q, want %q", label, "Roll back to revision #1")
	}

	f := s.failures(t)["service/shop_web"]
	if f.Digest != updDigest('b') || f.Reason != updates.ReasonDidntStart || f.RollbackFailed || f.RunID != run.ID {
		t.Errorf("failure of shop_web = %+v, want the new digest, rolled back", f)
	}
	list := decode[updatedList](t, s.f.get(t, s.e, "services"))
	if u := updateOf(t, list, "shop_web"); u == nil || u.State != "rolled_back" {
		t.Errorf("shop_web verdict = %+v, want rolled_back", u)
	}
	if _, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Error("the lock is still held after the rollback")
	}
	if rows := s.f.auditRows(t, "image.update_failed"); len(rows) != 1 || rows[0].Details["workload"] != "shop_web" || rows[0].Details["reason"] != updates.ReasonDidntStart {
		t.Errorf("image.update_failed rows = %+v, want one for shop_web", rows)
	}
	if rows := s.f.auditRows(t, "image.update_rolled_back"); len(rows) != 1 || rows[0].Result != "success" || rows[0].Details["workload"] != "shop_web" {
		t.Errorf("image.update_rolled_back rows = %+v, want one success for shop_web", rows)
	}
	deploys := s.f.auditRows(t, "stack.deployed")
	var rollbacks int
	for _, r := range deploys {
		if r.Details["mode"] == "rollback" && r.Details["restoredFrom"] == "1" && r.Result == "success" {
			rollbacks++
		}
	}
	if rollbacks != 1 {
		t.Errorf("stack.deployed rows = %+v, want one successful rollback of #1", deploys)
	}
	if !strings.Contains(strings.Join(outputOf(t, s.f, jobID), "\n"), "revision #1") {
		t.Errorf("the console says nothing of revision #1")
	}
}

// outputOf are the lines of a job's output log.
func outputOf(t *testing.T, f *envFix, jobID string) []string {
	t.Helper()
	lines, ok := f.jobHub.Output(jobID)
	if !ok {
		t.Fatalf("no output for job %s", jobID)
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

// updateWrites counts the service updates the fake daemon accepts once armed, and calls on with the number of each, inside the
// daemon's handler, so a test acts at an exact write of the update or of its rollback.
type updateWrites struct {
	armed atomic.Bool
	n     atomic.Int32
	on    func(n int)
}

// hook is the daemon's OnWrite.
func (u *updateWrites) hook(w string) {
	if !u.armed.Load() || !strings.HasPrefix(w, "POST /services/") || !strings.Contains(w, "/update") {
		return
	}
	n := int(u.n.Add(1))
	if u.on != nil {
		u.on(n)
	}
}

// shopWithWrites is shopQuick on a daemon that tells uw about each service update once uw is armed.
func shopWithWrites(t *testing.T, uw *updateWrites, services ...string) *shopFix {
	t.Helper()
	return newShopCfg(t, func(c *dockertest.FakeConfig) { c.OnWrite = uw.hook }, func(f *envFix) {
		f.stackClock = &instantClock{now: time.Unix(1_700_000_000, 0)}
	}, services...)
}

// lastImages are the image each service was last written with among the updates the daemon accepted from index from on.
func (s *shopFix) lastImages(from int) map[string]string {
	out := map[string]string{}
	for _, u := range s.f.fake.ServiceUpdates()[from:] {
		out[u.Spec.Name] = u.Spec.TaskTemplate.ContainerSpec.Image
	}
	return out
}

// An update of two services in which the second fails puts both back, never the second alone: a stack is not left half updated
// (plan 12-23, IMG-07, T-12-67).
func TestStackRollbackCoversEveryService(t *testing.T) {
	t.Parallel()
	s := shopQuick(t, "web", "api")
	s.move(t, "web", 'b')
	s.move(t, "api", 'b')
	s.rejectNewVersion("api", 'b')
	before := len(s.f.fake.ServiceUpdates())

	jobID, _ := s.startUpdate(t, "stack", "shop")

	// web was written with the new digest first, and written again with the old one by the rollback.
	var webImages []string
	for _, u := range s.f.fake.ServiceUpdates()[before:] {
		if u.Spec.Name == "shop_web" {
			webImages = append(webImages, u.Spec.TaskTemplate.ContainerSpec.Image)
		}
	}
	if len(webImages) != 2 || !strings.HasSuffix(webImages[0], "@"+updDigest('b')) || !strings.HasSuffix(webImages[1], "@"+updDigest('a')) {
		t.Errorf("web was written as %v, want the new digest and then the old one", webImages)
	}
	last := s.lastImages(before)
	if !strings.HasSuffix(last["shop_web"], "@"+updDigest('a')) || !strings.HasSuffix(last["shop_api"], "@"+updDigest('a')) {
		t.Errorf("last images = %v, want both services back at the old digest", last)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 3 || revs[2].Outcome != "good" {
		t.Fatalf("revisions = %+v, want one rollback revision", revs)
	}
	if got, want := pinsOf(t, revs[2]), pinsOf(t, revs[0]); got["shop_web"] != want["shop_web"] || got["shop_api"] != want["shop_api"] {
		t.Errorf("rollback pins = %v, want revision 1's %v", got, want)
	}
	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunRolledBack || len(run.Items) != 2 {
		t.Errorf("run = %+v, want one rolled-back run of both services", run)
	}
	fails := s.failures(t)
	for _, name := range []string{"service/shop_web", "service/shop_api"} {
		if f := fails[name]; f.Digest != updDigest('b') || f.RollbackFailed {
			t.Errorf("failure %s = %+v, want the new digest, rolled back", name, f)
		}
	}
	if rows := s.f.auditRows(t, "image.update_rolled_back"); len(rows) != 2 {
		t.Errorf("image.update_rolled_back rows = %+v, want one for each service", rows)
	}
}

// The environment's lock is held from the update's start to the end of the rollback: a deploy asked for while the rollback writes is
// 409 operation_locked, and the holder is still the update (plan 12-23, IMG-07, T-12-68).
func TestLockHeldThroughRollback(t *testing.T) {
	t.Parallel()
	uw := &updateWrites{}
	s := shopWithWrites(t, uw, "web")
	s.move(t, "web", 'b')
	s.rejectNewVersion("web", 'b')

	type probe struct {
		code   int
		kind   string
		held   bool
		holder string
	}
	var got atomic.Pointer[probe]
	uw.on = func(n int) {
		if n != 2 {
			return // the update's own write is 1; the rollback's is 2
		}
		rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil))
		h, held := s.f.stackLocks.Held(s.e.ID)
		got.Store(&probe{code: rec.Code, kind: problemType(t, rec), held: held, holder: h.Kind})
	}
	uw.armed.Store(true)

	s.startUpdate(t, "stack", "shop")

	p := got.Load()
	if p == nil {
		t.Fatal("the rollback never wrote")
	}
	if p.code != http.StatusConflict || p.kind != "operation_locked" || !p.held || p.holder != "update" {
		t.Errorf("an update asked for during the rollback = %+v, want 409 operation_locked with the update still the holder", *p)
	}
	if _, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Error("the lock is still held after the rollback")
	}
}

// gateClock lets the first wait of a deploy block until a test lets it go, and every later wait pass at once, so a test can change
// the daemon between the write of an update and the first read of its tasks.
type gateClock struct {
	mu      sync.Mutex
	now     time.Time
	armed   chan struct{}
	release chan struct{}
	used    bool
}

func newGateClock() *gateClock {
	return &gateClock{now: time.Unix(1_700_000_000, 0), armed: make(chan struct{}), release: make(chan struct{})}
}

func (c *gateClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gateClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	first := !c.used
	c.used = true
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	if first {
		close(c.armed)
		<-c.release
	}
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

// The person's own failure action may roll a service back before TierPlan does. TierPlan's rollback then writes the same spec the
// service already has, and still writes the one "rollback of #1" revision and one pair of audit rows for the service (plan 12-23,
// IMG-07, RESEARCH Pitfall 10).
func TestEngineRolledBackStackFirst(t *testing.T) {
	t.Parallel()
	gate := newGateClock()
	s := newShopWith(t, func(f *envFix) { f.stackClock = gate }, "web", "api")
	s.move(t, "api", 'b')
	s.f.fake.SetTaskStateWhileImage("shop_api", updDigest('b'), "preparing", "")
	before := len(s.f.fake.ServiceUpdates())

	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	jobID := answerOf[stackRunAnswer](t, rec).JobID
	<-gate.armed
	// The new tasks of api have been read starting; before the next read the swarm's failure action puts the previous spec back.
	if !s.f.fake.RollBackService("shop_api") {
		t.Fatal("the fake could not roll the service back")
	}
	close(gate.release)
	if _, err := s.f.jobHub.Wait(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}

	writes := s.f.fake.ServiceUpdates()[before:]
	var api []string
	for _, u := range writes {
		if u.Spec.Name == "shop_api" {
			api = append(api, u.Spec.TaskTemplate.ContainerSpec.Image)
		}
	}
	if len(api) != 2 || !strings.HasSuffix(api[1], "@"+updDigest('a')) {
		t.Errorf("api was written as %v, want the update and then a rollback that sends the old digest it already has", api)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 3 || revs[2].Outcome != "good" {
		t.Fatalf("revisions = %+v, want exactly one rollback revision", revs)
	}
	if origin, from := s.f.revisionOrigin(t, s.st.ID, 3); origin != "rollback" || from != 1 {
		t.Errorf("revision 3 = %q restored from %d, want a rollback of #1", origin, from)
	}
	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart || run.RollbackSeq != 3 {
		t.Errorf("run = %+v, want rolled_back (didnt_start) by revision 3", run)
	}
	if rows := s.f.auditRows(t, "image.update_failed"); len(rows) != 1 {
		t.Errorf("image.update_failed rows = %+v, want one", rows)
	}
	if rows := s.f.auditRows(t, "image.update_rolled_back"); len(rows) != 1 || rows[0].Result != "success" {
		t.Errorf("image.update_rolled_back rows = %+v, want one success", rows)
	}
	if f := s.failures(t)["service/shop_api"]; f.Digest != updDigest('b') || f.RollbackFailed {
		t.Errorf("failure = %+v, want the new digest, rolled back", f)
	}
}

// A standalone stack is rolled back through the Compose engine with the previous revision's pins (plan 12-23, IMG-07, D-12).
func TestStandaloneStackRollback(t *testing.T) {
	t.Parallel()
	s, up := newStandaloneShop(t, "web", "db")
	s.move(t, "web", 'b')
	_, calls := up.lastProject()
	up.failNext(errors.New("container solo-web-1 exited"))

	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "solo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	jobID := answerOf[stackRunAnswer](t, rec).JobID
	if _, err := s.f.jobHub.Wait(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}

	project, after := up.lastProject()
	if after != calls+2 || project == nil || project.Services["web"].Image != "registry.test/team/web:1@"+updDigest('a') ||
		project.Services["db"].Image != "registry.test/team/db:1@"+updDigest('a') {
		t.Fatalf("the Compose engine ran %d times (was %d) with %+v, want the update and a rollback with both services at the old digest", after, calls, project)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 3 || revs[1].Outcome != "failed" || revs[2].Outcome != "good" {
		t.Fatalf("revisions = %+v, want the failed update 2 and a good rollback 3", revs)
	}
	if origin, from := s.f.revisionOrigin(t, s.st.ID, 3); origin != "rollback" || from != 1 {
		t.Errorf("revision 3 = %q restored from %d, want a rollback of #1", origin, from)
	}
	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunRolledBack || run.RollbackSeq != 3 || run.RevisionSeq != 2 {
		t.Errorf("run = %+v, want rolled_back of revision 2 by revision 3", run)
	}
	if f := s.failures(t)["container/solo-web-1"]; f.Digest != updDigest('b') || f.RollbackFailed {
		t.Errorf("failures = %+v, want the new digest remembered for the container, rolled back", s.failures(t))
	}
	if rows := s.f.auditRows(t, "image.update_rolled_back"); len(rows) != 1 || rows[0].Result != "success" {
		t.Errorf("image.update_rolled_back rows = %+v, want one success", rows)
	}
}

// A git stack's rollback revision carries the commit of the revision it restores and does not touch the stack's automatic deploys:
// they are not paused, and nothing is audited as paused (plan 12-23, D-08, T-12-69).
func TestGitStackRollbackKeepsAutoDeploy(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"library/nginx": {digest: updDigest('b')}, "library/postgres": {digest: updDigest('a')}})
	a := newDeployedAutoFixWith(t, func(f *envFix) {
		f.updateHeads = heads
		f.stackClock = &instantClock{now: time.Unix(1_700_000_000, 0)}
		f.fake.SetDistributionDigest("docker.io/library/nginx:1.27", updDigest('a'))
		f.fake.SetDistributionDigest("docker.io/library/postgres:16", updDigest('a'))
	})
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)
	if rec := a.f.checkNow(t, a.e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	before := a.readGit(t, a.admin, a.stackID)
	first, err := a.stackRepo().Revision(context.Background(), a.stackID, 1)
	if err != nil || first.Git.Commit == "" {
		t.Fatalf("revision 1 = %+v, %v", first.Git, err)
	}
	var script []dockertest.FakeServiceTask
	for slot := 1; slot <= 3; slot++ {
		script = append(script, dockertest.FakeServiceTask{Slot: slot, State: "rejected", Desired: "shutdown", Err: "No such image"})
	}
	a.f.fake.SetServiceTasksWhileImage("web_web", updDigest('b'), script...)

	caller := a.f.caller(t, a.e, "operator-2", "operator")
	rec := caller.do(http.MethodPost, "updates/run", stackRunBody("stack", "web", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	jobID := answerOf[stackRunAnswer](t, rec).JobID
	if _, err := a.f.jobHub.Wait(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}

	revs := a.f.revisions(t, a.stackID)
	if len(revs) != 3 || revs[2].Outcome != "good" {
		t.Fatalf("revisions = %+v, want a good rollback revision 3", revs)
	}
	third, err := a.stackRepo().Revision(context.Background(), a.stackID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if third.Origin != "rollback" || third.RestoredFrom != 1 || third.Git.Commit != first.Git.Commit || third.Git.Tree != first.Git.Tree {
		t.Errorf("revision 3 = origin %q restored from %d at %s, want a rollback of #1 at the commit %s", third.Origin, third.RestoredFrom, third.Git.Commit, first.Git.Commit)
	}
	after := a.readGit(t, a.admin, a.stackID)
	if !after.AutoDeploy || after.Paused || after.PausedReason != "" || after.DeployedSha != before.DeployedSha || after.HeadSha != before.HeadSha {
		t.Errorf("git after = %+v, before = %+v; want automatic deploys still on, not paused, at the same commits", after, before)
	}
	if rows := a.f.auditRowsOf(t, audit.ActionGitAutoDeployPaused); len(rows) != 0 {
		t.Errorf("git.autodeploy_paused rows = %+v, want none: an automatic rollback pauses nothing", rows)
	}
	if run := a.f.runByJob(t, jobID); run.State != updates.RunRolledBack {
		t.Errorf("run = %+v, want rolled_back", run)
	}
}

// A rollback the swarm refuses ends the run rollback_failed, remembers the digest as one that failed with its rollback, tells the
// audit log the rollback failed, and leaves the way back open: the revision before the update is still the good one (plan 12-23,
// IMG-07, D-14).
func TestStackRollbackFails(t *testing.T) {
	t.Parallel()
	uw := &updateWrites{}
	s := shopWithWrites(t, uw, "web")
	s.move(t, "web", 'b')
	s.rejectNewVersion("web", 'b')
	uw.on = func(n int) {
		if n == 2 {
			s.f.fake.RefuseServiceUpdates("shop_web", "the swarm will not take this spec")
		}
	}
	uw.armed.Store(true)

	jobID, job := s.startUpdate(t, "stack", "shop")

	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunRollbackFailed || run.Reason != updates.ReasonRestoreFailed || run.RevisionSeq != 2 || run.RollbackSeq != 3 {
		t.Errorf("run = %+v, want rollback_failed (restore_failed) with revision 2 and rollback 3", run)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 3 || revs[2].Outcome != "failed" {
		t.Errorf("revisions = %+v, want the rollback revision failed", revs)
	}
	if steps := stepStates(job); steps[updates.ItemRollback] != jobs.ItemFailed || steps[updates.ItemRollbackWait] != jobs.ItemSkipped {
		t.Errorf("steps = %v, want the rollback failed and its wait skipped", steps)
	}
	if f := s.failures(t)["service/shop_web"]; f.Digest != updDigest('b') || !f.RollbackFailed {
		t.Errorf("failure = %+v, want the new digest with its rollback failed", f)
	}
	if rows := s.f.auditRows(t, "image.update_rolled_back"); len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != updates.ReasonRestoreFailed {
		t.Errorf("image.update_rolled_back rows = %+v, want one failure (restore_failed)", rows)
	}
	if rows := s.f.auditRows(t, "image.update_failed"); len(rows) != 1 || rows[0].Details["reason"] != updates.ReasonDidntStart {
		t.Errorf("image.update_failed rows = %+v, want one with the cause", rows)
	}
	if _, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Error("the lock is still held after the rollback failed")
	}
	// The revision before the update is still the one to roll back to.
	got := answerOf[struct {
		Rollback struct {
			Available bool `json:"available"`
		} `json:"rollback"`
	}](t, s.operator.do(http.MethodGet, "stacks/"+s.st.ID, ""))
	if !got.Rollback.Available {
		t.Error("the stack offers no roll back after a failed automatic one")
	}
}

// A person who presses Stop on an update decides what happens next: nothing is rolled back and nothing is remembered as a failed
// version (plan 12-23, IMG-07).
func TestCancelledUpdateNoRollback(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	s.move(t, "web", 'b')
	s.f.fake.SetTaskState("shop_web", "preparing", "")
	before := len(s.f.fake.ServiceUpdates())
	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	jobID := answerOf[stackRunAnswer](t, rec).JobID
	awaitStackJobItem(t, s.f, jobID, func(it jobs.Item) bool { return it.ID == "service:shop_web" && it.Unit == "tasks" })
	if rec := s.operator.do(http.MethodPost, cancelPath(s.st.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	job := s.f.awaitDeploy(t, jobID)
	if job.State == "done" {
		t.Fatalf("job = %+v, want it stopped", job)
	}

	if n := len(s.f.fake.ServiceUpdates()) - before; n != 1 {
		t.Errorf("service updates = %d, want only the update's own write", n)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 2 || revs[1].Outcome != "failed" {
		t.Errorf("revisions = %+v, want the cancelled update 2 and no rollback", revs)
	}
	for _, it := range job.Items {
		if it.ID == updates.ItemRollback || it.ID == updates.ItemRollbackWait {
			t.Errorf("job has a %s step after a cancel", it.ID)
		}
	}
	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunFailed || run.Reason != updates.ReasonCancelled || run.RollbackSeq != 0 {
		t.Errorf("run = %+v, want failed (cancelled) with no rollback", run)
	}
	if f := s.failures(t); len(f) != 0 {
		t.Errorf("failures = %+v, want none: a cancel says nothing of the version", f)
	}
	if rows := s.f.auditRows(t, "image.update_rolled_back"); len(rows) != 0 {
		t.Errorf("image.update_rolled_back rows = %+v, want none", rows)
	}
}
