// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/updates"
)

// Updating a stack TierPlan manages (plan 12-18, IMG-06, D-06, D-08, D-09, D-15). Docker is a fake daemon behind the real monitor,
// aggregator and cluster reader; the registry is a scripted Heads; the stack store, its engine, the job hub, the operation lock,
// the router and the permission checks are the real ones. The stacks are deployed for real, so their revisions, pins and labels
// are what a deploy makes.

// shopFix is a swarm stack deployed with one service for each repository it is given, every one running digest a.
type shopFix struct {
	f        *envFix
	e        envBody
	admin    *roleCaller
	operator *roleCaller
	st       stackAnswer
	heads    *scriptedHeads
}

// shopCompose is the compose file of a stack with a service for each name: registry.test/team/<name>:1.
func shopCompose(names ...string) string {
	var b strings.Builder
	b.WriteString("services:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %s:\n    image: registry.test/team/%s:1\n", n, n)
	}
	return b.String()
}

// newShop deploys a stack named shop with the services, each running digest a of its repository, then checks the registry, which
// still says a for each.
func newShop(t *testing.T, services ...string) *shopFix {
	t.Helper()
	answers := map[string]headAnswer{}
	for _, s := range services {
		answers["team/"+s] = headAnswer{digest: updDigest('a')}
	}
	heads := newScriptedHeads(answers)
	f, e := stackEnvTuned(t, func(f *envFix) { f.updateHeads = heads })
	for _, s := range services {
		f.fake.SetDistributionDigest("registry.test/team/"+s+":1", updDigest('a'))
	}
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	f.deployGood(t, operator, st.ID, shopCompose(services...))
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	return &shopFix{f: f, e: e, admin: admin, operator: operator, st: st, heads: heads}
}

// move makes the registry say digest c for the service's tag and checks it, so the service has an update available.
func (s *shopFix) move(t *testing.T, service string, c byte) {
	t.Helper()
	s.heads.set("team/"+service, updDigest(c))
	if rec := s.f.checkNow(t, s.e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
}

// stackRunBody is the body of a request to update a stack or one of its services.
func stackRunBody(kind, name string, expect map[string]string) string {
	body := map[string]any{"kind": kind, "name": name}
	if expect != nil {
		body["expect"] = expect
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// stackRunAnswer is the answer of an update that started on a stack.
type stackRunAnswer struct {
	JobID   string `json:"jobId"`
	StackID string `json:"stackId"`
}

// updateStack updates and waits for the stack job, failing the test unless it started and ended done.
func (s *shopFix) updateStack(t *testing.T, c *roleCaller, kind, name string, expect map[string]string) (stackRunAnswer, jobAnswer) {
	t.Helper()
	rec := c.do(http.MethodPost, "updates/run", stackRunBody(kind, name, expect))
	if rec.Code != http.StatusOK {
		t.Fatalf("update %s %s: %d %s", kind, name, rec.Code, rec.Body)
	}
	got := answerOf[stackRunAnswer](t, rec)
	if got.JobID == "" || got.StackID != s.st.ID {
		t.Fatalf("answer = %+v, want the stack's deploy job of stack %s", got, s.st.ID)
	}
	job := s.f.awaitDeploy(t, got.JobID)
	if job.State != "done" {
		t.Fatalf("job = %+v, want it done", job)
	}
	return got, job
}

// pinsOf are the pins stored with a revision, by service.
func pinsOf(t *testing.T, row revisionRow) map[string]string {
	t.Helper()
	var pins map[string]string
	if err := json.Unmarshal([]byte(row.Pins), &pins); err != nil {
		t.Fatalf("pins %q: %v", row.Pins, err)
	}
	return pins
}

// runByJob is the run that the job of an update recorded.
func (f *envFix) runByJob(t *testing.T, jobID string) updates.Run {
	t.Helper()
	var id string
	if err := f.st.Read.QueryRow(`SELECT id FROM image_update_runs WHERE job_id = ?`, jobID).Scan(&id); err != nil {
		t.Fatalf("no run recorded for job %s: %v", jobID, err)
	}
	run, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// The tracer (plan 12-18, IMG-06, D-06): updating one service of a managed swarm stack writes revision 2, an update of its own
// whose pins differ from revision 1's in that service alone, and deploys it in the stack's console.
func TestManagedStackServiceUpdate(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web", "api")
	s.move(t, "web", 'b')
	before := len(s.f.fake.ServiceUpdates())

	got, job := s.updateStack(t, s.operator, "service", "shop_web", map[string]string{"shop_web": updDigest('b')})

	// The deploy was given the new pin for web and the old one for api: only the chosen service's pin changed.
	sent := map[string]string{}
	for _, u := range s.f.fake.ServiceUpdates()[before:] {
		sent[u.Spec.Name] = u.Spec.TaskTemplate.ContainerSpec.Image
	}
	if sent["shop_web"] != "registry.test/team/web:1@"+updDigest('b') || sent["shop_api"] != "registry.test/team/api:1@"+updDigest('a') {
		t.Errorf("services sent = %v, want web at b and api still at a", sent)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 2 || revs[1].Outcome != "good" {
		t.Fatalf("revisions = %+v, want a good revision 2", revs)
	}
	old, now := pinsOf(t, revs[0]), pinsOf(t, revs[1])
	if now["shop_web"] != "registry.test/team/web:1@"+updDigest('b') || now["shop_api"] != old["shop_api"] || old["shop_web"] != "registry.test/team/web:1@"+updDigest('a') {
		t.Errorf("pins were %v and are %v, want only shop_web moved to b", old, now)
	}
	if origin, from := s.f.revisionOrigin(t, s.st.ID, 2); origin != "update" || from != 0 {
		t.Errorf("revision 2 = %q restored from %d, want an update that restores nothing", origin, from)
	}

	// It is the stack's own job, in its console, and the run records the revision it made.
	detail := answerOf[struct {
		Mode  string `json:"mode"`
		State string `json:"state"`
	}](t, s.operator.do(http.MethodGet, "stacks/"+s.st.ID+"/deploys/"+got.JobID, ""))
	if detail.Mode != "update" || detail.State != "done" {
		t.Errorf("stack job = %+v, want an update that is done", detail)
	}
	run := s.f.runByJob(t, job.ID)
	if run.Kind != "stack" || run.StackID != s.st.ID || run.RevisionSeq != 2 || run.State != updates.RunDone || len(run.Items) != 1 ||
		run.Items[0].Service != "shop_web" || run.Items[0].FromDigest != updDigest('a') || run.Items[0].ToDigest != updDigest('b') {
		t.Errorf("run = %+v, want a done stack run of shop_web a to b that made revision 2", run)
	}
	rows := s.f.auditRows(t, "image.updated")
	if len(rows) != 1 || rows[0].Details["workload"] != "shop_web" || rows[0].Details["to_digest"] != updDigest('b') {
		t.Errorf("image.updated rows = %+v, want one for shop_web", rows)
	}
	if h, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Errorf("the lock is still held by %+v", h)
	}
}
