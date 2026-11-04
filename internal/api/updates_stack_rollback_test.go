// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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
