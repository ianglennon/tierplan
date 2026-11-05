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

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
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
	return newShopWith(t, func(*envFix) {}, services...)
}

// newShopWith is newShop with the fixture tuned before the server is built.
func newShopWith(t *testing.T, tune func(*envFix), services ...string) *shopFix {
	t.Helper()
	return newShopCfg(t, nil, tune, services...)
}

// newShopCfg is newShopWith with the fake daemon's configuration tuned too.
func newShopCfg(t *testing.T, tuneEngine func(*dockertest.FakeConfig), tune func(*envFix), services ...string) *shopFix {
	t.Helper()
	answers := map[string]headAnswer{}
	for _, s := range services {
		answers["team/"+s] = headAnswer{digest: updDigest('a')}
	}
	heads := newScriptedHeads(answers)
	f, e := stackEnvCfg(t, tuneEngine, func(f *envFix) { f.updateHeads = heads; tune(f) })
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

// newStandaloneShop is a standalone host with a stack named solo deployed through the Compose stand-in, a service for each name
// running digest a of its repository, as the containers a real up leaves. The registry still says a for each.
func newStandaloneShop(t *testing.T, services ...string) (*shopFix, *standaloneUp) {
	t.Helper()
	answers := map[string]headAnswer{}
	for _, s := range services {
		answers["team/"+s] = headAnswer{digest: updDigest('a')}
	}
	heads := newScriptedHeads(answers)
	up := &standaloneUp{}
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.stackUp = up.run
	f.updateHeads = heads
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	for _, s := range services {
		f.fake.SetDistributionDigest("registry.test/team/"+s+":1", updDigest('a'))
	}
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	f.deployGood(t, operator, st.ID, shopCompose(services...))

	// The containers the project would hold: created from the pinned reference, with the text the file wrote.
	cfg.Containers, cfg.Images = nil, nil
	for i, s := range services {
		id := updImage(byte('1' + i))
		cfg.Images = append(cfg.Images, dockertest.FakeImage{
			ID: id, RepoTags: []string{"registry.test/team/" + s + ":1"}, RepoDigests: []string{"registry.test/team/" + s + "@" + updDigest('a')},
		})
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
			ID: "c-" + s, Names: []string{"/solo-" + s + "-1"}, Image: "registry.test/team/" + s + ":1@" + updDigest('a'), ImageID: id,
			State: "running", Status: "Up 1 hour",
			Labels: map[string]string{
				"com.docker.compose.project": "solo", "com.docker.compose.service": s, "com.docker.compose.oneoff": "False",
				"tierplan.stack.id": st.ID, "tierplan.stack.image": "registry.test/team/" + s + ":1",
			},
		})
	}
	f.fake.Reconfigure(cfg)
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	return &shopFix{f: f, e: e, admin: admin, operator: operator, st: st, heads: heads}, up
}

// stackPreviewAnswer is the preview of a stack as far as these tests read it.
type stackPreviewAnswer struct {
	Items []struct {
		Name       string `json:"name"`
		FromDigest string `json:"fromDigest"`
		ToDigest   string `json:"toDigest"`
	} `json:"items"`
	LeftOut []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"leftOut"`
	Offered bool   `json:"offered"`
	Refusal string `json:"refusal"`
}

// noNewRevision asserts the stack still has only the revisions it had and no run is on record.
func (s *shopFix) noNewRevision(t *testing.T, want int) {
	t.Helper()
	if revs := s.f.revisions(t, s.st.ID); len(revs) != want {
		t.Errorf("revisions = %+v, want %d", revs, want)
	}
	var n int
	if err := s.f.st.Read.QueryRow(`SELECT COUNT(*) FROM image_update_runs`).Scan(&n); err != nil || n != 0 {
		t.Errorf("runs = %d %v, want none", n, err)
	}
	if h, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Errorf("the lock is held by %+v", h)
	}
}

// Update stack (plan 12-18, D-06, D-15): every service with an update available changes, in one revision, and a skipped one
// is left out and named by the preview.
func TestUpdateStackLeavesOutSkipped(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web", "api", "db")
	s.heads.set("team/web", updDigest('b'))
	s.heads.set("team/api", updDigest('b'))
	s.heads.set("team/db", updDigest('c'))
	if rec := s.f.checkNow(t, s.e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	if rec := s.f.skip(t, s.e, http.MethodPost, "shop_db"); rec.code != http.StatusOK {
		t.Fatalf("skip: %d %s", rec.code, rec.body)
	}

	rec := s.operator.do(http.MethodGet, "updates/preview?kind=stack&name=shop", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	pre := answerOf[stackPreviewAnswer](t, rec)
	var names []string
	for _, it := range pre.Items {
		names = append(names, it.Name)
	}
	if !pre.Offered || strings.Join(names, ",") != "shop_api,shop_web" || len(pre.LeftOut) != 1 || pre.LeftOut[0].Name != "shop_db" || pre.LeftOut[0].State != "skipped" {
		t.Errorf("preview = %+v, want shop_api and shop_web changed and shop_db left out as skipped", pre)
	}

	s.updateStack(t, s.operator, "stack", "shop", map[string]string{"shop_web": updDigest('b'), "shop_api": updDigest('b')})
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 2 {
		t.Fatalf("revisions = %+v, want one new revision for the whole stack", revs)
	}
	now := pinsOf(t, revs[1])
	if now["shop_web"] != "registry.test/team/web:1@"+updDigest('b') || now["shop_api"] != "registry.test/team/api:1@"+updDigest('b') ||
		now["shop_db"] != "registry.test/team/db:1@"+updDigest('a') {
		t.Errorf("pins = %v, want web and api at b and the skipped db still at a", now)
	}
	if rows := s.f.auditRows(t, "image.updated"); len(rows) != 2 {
		t.Errorf("image.updated rows = %+v, want one for each of the two services", rows)
	}
}

// Nothing to update is refused and leaves nothing behind.
func TestUpdateStackNothing(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web", "api")
	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil))
	if rec.Code != http.StatusConflict || problemOf(t, rec).Type != "nothing_to_update" {
		t.Fatalf("update = %d %s, want 409 nothing_to_update", rec.Code, rec.Body)
	}
	s.noNewRevision(t, 1)

	// A single service with nothing newer is refused the same way.
	rec = s.operator.do(http.MethodPost, "updates/run", stackRunBody("service", "shop_web", nil))
	if rec.Code != http.StatusConflict || problemOf(t, rec).Type != "nothing_to_update" {
		t.Fatalf("service update = %d %s, want 409 nothing_to_update", rec.Code, rec.Body)
	}
	s.noNewRevision(t, 1)
}

// A moved digest is refused, and the digest deployed is the server's, never the request's.
func TestStackUpdateRefusesAMovedDigest(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	s.move(t, "web", 'b')
	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", map[string]string{"shop_web": updDigest('c')}))
	if rec.Code != http.StatusConflict || problemOf(t, rec).Type != "update_moved" {
		t.Fatalf("update = %d %s, want 409 update_moved", rec.Code, rec.Body)
	}
	s.noNewRevision(t, 1)
}

// A standalone stack is updated through the Compose engine with the changed pins.
func TestStandaloneStackUpdate(t *testing.T) {
	t.Parallel()
	s, up := newStandaloneShop(t, "web", "db")
	s.move(t, "web", 'b')
	_, calls := up.lastProject()

	got, _ := s.updateStack(t, s.operator, "stack", "solo", map[string]string{"web": updDigest('b')})
	project, after := up.lastProject()
	if after != calls+1 || project == nil || project.Services["web"].Image != "registry.test/team/web:1@"+updDigest('b') ||
		project.Services["db"].Image != "registry.test/team/db:1@"+updDigest('a') {
		t.Fatalf("the Compose engine ran %d times (was %d) with %+v, want one more run with web at b and db at a", after, calls, project)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 2 || revs[1].Outcome != "good" {
		t.Fatalf("revisions = %+v, want a good revision 2", revs)
	}
	now := pinsOf(t, revs[1])
	if now["web"] != "registry.test/team/web:1@"+updDigest('b') || now["db"] != "registry.test/team/db:1@"+updDigest('a') {
		t.Errorf("pins = %v, want web at b and db at a", now)
	}
	if origin, from := s.f.revisionOrigin(t, s.st.ID, 2); origin != "update" || from != 0 {
		t.Errorf("revision 2 = %q restored from %d, want an update", origin, from)
	}
	if run := s.f.runByJob(t, got.JobID); run.Kind != "stack" || run.RevisionSeq != 2 || run.State != updates.RunDone {
		t.Errorf("run = %+v, want a done stack run that made revision 2", run)
	}
}

// A discovered stack cannot be updated as a whole until it is adopted; its services are plain workloads (D-09).
func TestUnmanagedStackAdoptFirst(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/legacy_web": {digest: updDigest('b')}})
	web := plainService("legacy_web", 'a')
	web.Labels = map[string]string{"com.docker.stack.namespace": "legacy"}
	web.Spec.Labels = web.Labels
	f, e := updateEnvOf(t, heads, nil, web)
	operator := f.caller(t, e, "operator-1", "operator")
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	rec := operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "legacy", nil))
	if rec.Code != http.StatusConflict || problemOf(t, rec).Type != "adopt_first" {
		t.Fatalf("update of the stack = %d %s, want 409 adopt_first", rec.Code, rec.Body)
	}
	pre := answerOf[stackPreviewAnswer](t, operator.do(http.MethodGet, "updates/preview?kind=stack&name=legacy", ""))
	if pre.Offered || pre.Refusal != "adopt_first" {
		t.Errorf("preview = %+v, want adopt_first and nothing offered", pre)
	}
	if rec := operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "nowhere", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("update of a stack that is not there = %d %s, want 404", rec.Code, rec.Body)
	}
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Fatalf("service updates = %d after a refusal, want none", n)
	}

	// Its service still updates as a plain workload.
	rec = operator.do(http.MethodPost, "updates/run", stackRunBody("service", "legacy_web", map[string]string{"legacy_web": updDigest('b')}))
	if rec.Code != http.StatusOK {
		t.Fatalf("update of the service = %d %s, want it to start", rec.Code, rec.Body)
	}
	answer := answerOf[stackRunAnswer](t, rec)
	if answer.StackID != "" {
		t.Errorf("answer = %+v, want a plain update with no stack", answer)
	}
	if job := f.awaitUpdate(t, answer.JobID); job.State != jobs.JobDone || job.Kind != updates.JobKind {
		t.Errorf("job = %s %s, want a done image update job", job.State, job.Kind)
	}
}

// History tells an image update from the older Update images deploy, which has the same origin.
func TestHistoryMarksImageUpdate(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	// Revision 2: the Phase 7 Update images deploy of web.
	v := draftVersion(t, s.operator, s.st.ID)
	rec := s.operator.deploy(s.st.ID, `{"draftVersion":`+itoaT(v)+`,"mode":"update","services":["web"]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update images: %d %s", rec.Code, rec.Body)
	}
	if job := s.f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID); job.State != "done" {
		t.Fatalf("update images job = %+v", job)
	}
	// Revision 3: a one-click image update.
	s.move(t, "web", 'b')
	s.updateStack(t, s.operator, "service", "shop_web", nil)

	rec = s.operator.do(http.MethodGet, "stacks/"+s.st.ID+"/revisions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	list := answerOf[struct {
		Items []struct {
			Seq         int64  `json:"seq"`
			Origin      string `json:"origin"`
			ImageUpdate bool   `json:"imageUpdate"`
		} `json:"items"`
	}](t, rec)
	got := map[int64]string{}
	for _, it := range list.Items {
		got[it.Seq] = fmt.Sprintf("%s/%v", it.Origin, it.ImageUpdate)
	}
	if got[1] != "deploy/false" || got[2] != "update/false" || got[3] != "update/true" {
		t.Errorf("history = %v, want the deploy plain, the Update images deploy an update that is not an image update, and the one-click update flagged", got)
	}
	detail := answerOf[struct {
		Item struct {
			ImageUpdate bool `json:"imageUpdate"`
		} `json:"item"`
	}](t, s.operator.do(http.MethodGet, "stacks/"+s.st.ID+"/revisions/3", ""))
	if !detail.Item.ImageUpdate {
		t.Errorf("revision 3 detail = %+v, want it flagged an image update", detail)
	}
}

// Updating a stack deploys it, so it needs an Operator, and a Viewer changes nothing.
func TestStackUpdatePermissions(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	s.move(t, "web", 'b')
	viewer := s.f.caller(t, s.e, "viewer-1", "viewer")
	for _, body := range []string{stackRunBody("stack", "shop", nil), stackRunBody("service", "shop_web", nil)} {
		if rec := viewer.do(http.MethodPost, "updates/run", body); rec.Code != http.StatusForbidden {
			t.Errorf("a Viewer's update %s = %d %s, want 403", body, rec.Code, rec.Body)
		}
	}
	s.noNewRevision(t, 1)
	// The preview needs only to read.
	if rec := viewer.do(http.MethodGet, "updates/preview?kind=stack&name=shop", ""); rec.Code != http.StatusOK {
		t.Errorf("a Viewer's preview = %d %s, want it to read", rec.Code, rec.Body)
	}
	s.updateStack(t, s.operator, "stack", "shop", nil)
	// A stack update is refused while another operation holds the environment.
	release, _, ok := s.f.stackLocks.TryAcquire(s.e.ID, stackops.Holder{StackName: "other", Kind: "deploy", By: "ada"})
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer release()
	s.heads.set("team/web", updDigest('c'))
	if rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil)); rec.Code != http.StatusConflict || problemOf(t, rec).Type != "operation_locked" {
		t.Errorf("update under a lock = %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
}

// A git stack updates in one click with pins only (D-08): the revision records the commit the good one ran, nothing is committed
// or pulled, automatic deploys stay on, and a later commit that leaves an image alone keeps the new pin (P9 D-21).
func TestGitStackUpdateKeepsCommit(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"library/nginx": {digest: updDigest('b')}, "library/postgres": {digest: updDigest('a')}})
	a := newDeployedAutoFixWith(t, func(f *envFix) {
		f.updateHeads = heads
		f.fake.SetDistributionDigest("docker.io/library/nginx:1.27", updDigest('a'))
		f.fake.SetDistributionDigest("docker.io/library/postgres:16", updDigest('a'))
	})
	operator := a.f.asRoleOn(t, "u-operator", a.env, "operator")
	a.turnOn(t, operator)
	if rec := a.f.checkNow(t, a.e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	before := a.readGit(t, a.admin, a.stackID)
	repo := a.stackRepo()
	first, err := repo.Revision(context.Background(), a.stackID, 1)
	if err != nil || first.Git.Commit == "" {
		t.Fatalf("revision 1 = %+v, %v, want it to record its commit", first.Git, err)
	}

	caller := a.f.caller(t, a.e, "operator-2", "operator")
	rec := caller.do(http.MethodPost, "updates/run", stackRunBody("stack", "web", map[string]string{"web_web": updDigest('b')}))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if job := a.f.awaitDeploy(t, answerOf[stackRunAnswer](t, rec).JobID); job.State != "done" {
		t.Fatalf("job = %+v, want it done", job)
	}

	second, err := repo.Revision(context.Background(), a.stackID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if second.Origin != "update" || second.RestoredFrom != 0 || second.Git.Commit != first.Git.Commit || second.Git.Tree != first.Git.Tree {
		t.Errorf("revision 2 = origin %q restored from %d at %s, want an update at the commit %s", second.Origin, second.RestoredFrom, second.Git.Commit, first.Git.Commit)
	}
	after := a.readGit(t, a.admin, a.stackID)
	if !after.AutoDeploy || after.Paused || after.DeployedSha != before.DeployedSha || after.HeadSha != before.HeadSha {
		t.Errorf("git after = %+v, before = %+v; want auto-deploy still on, not paused, at the same commits", after, before)
	}
	if rows := a.f.auditRowsOf(t, audit.ActionGitAutoDeployPaused); len(rows) != 0 {
		t.Errorf("git.autodeploy_paused rows = %+v, want none: an update is not a roll back", rows)
	}
	revs := a.f.revisions(t, a.stackID)
	if pins := pinsOf(t, revs[1]); !strings.HasSuffix(pins["web_web"], "@"+updDigest('b')) || !strings.HasSuffix(pins["web_db"], "@"+updDigest('a')) {
		t.Fatalf("revision 2 pins = %v, want web at b and db at a", pins)
	}

	// A later commit changes the database image and leaves web's alone: web keeps the pin the update gave it.
	a.commitCompose(t, strings.Replace(gitCompose("1.27"), "postgres:16", "postgres:17", 1), "newer db")
	if out := a.checkNow(t, operator, a.stackID, a.ended); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if job := a.f.awaitDeploy(t, a.lastJob(t)); job.State != "done" {
		t.Fatalf("automatic deploy = %+v, want done", job)
	}
	revs = a.f.revisions(t, a.stackID)
	if len(revs) != 3 {
		t.Fatalf("revisions = %+v, want three", revs)
	}
	if pins := pinsOf(t, revs[2]); !strings.HasSuffix(pins["web_web"], "@"+updDigest('b')) {
		t.Errorf("revision 3 pins = %v, want web to keep the pin the update gave it", pins)
	}
}

// The platforms the check read are what the deploy sends: it asks the daemon for none of the new digest's, so a revision that
// predates the platform record needs no extra registry call (RESEARCH Open Question 4).
func TestUpdateRemembersCheckedPlatforms(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	s.heads.mu.Lock()
	s.heads.answers["team/web"] = headAnswer{digest: updDigest('b'), platforms: map[string]string{"linux/amd64": updDigest('1'), "linux/arm64": updDigest('2')}}
	s.heads.mu.Unlock()
	if rec := s.f.checkNow(t, s.e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	asked := len(s.f.fake.DistributionRefs())

	s.updateStack(t, s.operator, "stack", "shop", nil)
	if n := len(s.f.fake.DistributionRefs()); n != asked {
		t.Errorf("distribution checks went from %d to %d, want none for a digest whose platforms the check read", asked, n)
	}
	rev, err := (stackops.Repo{Store: s.f.st, Vault: s.f.v}).Revision(context.Background(), s.st.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := rev.Platforms["shop_web"]
	if len(got) != 2 || got[0].OS != "linux" || got[0].Architecture != "amd64" || got[1].Architecture != "arm64" {
		t.Errorf("revision 2 platforms = %+v, want linux/amd64 and linux/arm64 as the check read them", rev.Platforms)
	}
}

// draftBase is the revision the stack's stored draft starts from, and its version.
func (f *envFix) draftBase(t *testing.T, stackID string) (base, version int64) {
	t.Helper()
	if err := f.st.Read.QueryRow(`SELECT base_seq, version FROM stack_snapshots WHERE stack_id = ? AND kind = 'draft'`, stackID).Scan(&base, &version); err != nil {
		t.Fatalf("read the draft of %s: %v", stackID, err)
	}
	return base, version
}

// The draft is never shipped (T-12-49): an unsaved edit stays in the draft, untouched, and the update deploys the content of the
// newest good revision. The draft's review then shows the update as a deploy made since.
func TestStackUpdateIgnoresDraftEdits(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	edited := shopCompose("web") + "# edited, not deployed\n"
	v := saveCompose(t, s.operator, s.st.ID, edited, `[{"name":"EDITED","value":"yes","secret":false}]`)
	s.move(t, "web", 'b')

	s.updateStack(t, s.operator, "stack", "shop", nil)

	rev, err := (stackops.Repo{Store: s.f.st, Vault: s.f.v}).Revision(context.Background(), s.st.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.Copy.Vars) != 0 || len(rev.Copy.Files) != 1 || string(rev.Copy.Files[0].Content) != shopCompose("web") {
		t.Errorf("revision 2 holds %d variables and files %+v, want exactly revision 1's content without the edit", len(rev.Copy.Vars), rev.Copy.Files)
	}
	draft := answerOf[draftAnswer](t, s.operator.do(http.MethodGet, "stacks/"+s.st.ID+"/draft", ""))
	if draft.Version != v || len(draft.Vars) != 1 || draft.Vars[0].Name != "EDITED" || len(draft.Files) != 1 || draft.Files[0].Content != edited {
		t.Errorf("draft after = %+v, want version %d with the edit untouched", draft, v)
	}
	if base, _ := s.f.draftBase(t, s.st.ID); base != 1 {
		t.Errorf("the edited draft starts from revision %d, want it left at 1 so its review shows the update", base)
	}
	rec := s.operator.deploy(s.st.ID, `{"draftVersion":`+itoaT(v)+`}`)
	if rec.Code != http.StatusConflict || problemOf(t, rec).Type != "concurrent_unacknowledged" {
		t.Errorf("deploying the edited draft = %d %s, want 409 concurrent_unacknowledged: the update is a deploy made since", rec.Code, rec.Body)
	}
}

// A draft that holds what the update copied is rebased onto the update revision, so the next deploy of it asks for nothing.
func TestUnEditedDraftRebased(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	if base, _ := s.f.draftBase(t, s.st.ID); base != 1 {
		t.Fatalf("the draft starts from revision %d, want 1", base)
	}
	s.move(t, "web", 'b')
	s.updateStack(t, s.operator, "stack", "shop", nil)

	base, version := s.f.draftBase(t, s.st.ID)
	if base != 2 {
		t.Errorf("the draft starts from revision %d after a good update, want 2", base)
	}
	rec := s.operator.deploy(s.st.ID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploying the draft = %d %s, want it to need no acknowledgement", rec.Code, rec.Body)
	}
	if job := s.f.awaitDeploy(t, answerOf[stackRunAnswer](t, rec).JobID); job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}
}

// A person who cancels the image update of a stack is named in the audit row of the end of its deploy (WR-11). The update routes and the
// cancel route are registered from copies of the stack deps, so the record of who cancelled has to be one the copies share.
func TestCancelledUpdateNamesWhoCancelled(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	s.move(t, "web", 'b')
	s.f.fake.SetTaskState("shop_web", "preparing", "")
	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	jobID := answerOf[stackRunAnswer](t, rec).JobID
	awaitStackJobItem(t, s.f, jobID, func(it jobs.Item) bool { return it.ID == "service:shop_web" && it.Unit == "tasks" })
	other := s.f.caller(t, s.e, "operator-2", "operator")
	if rec := other.do(http.MethodPost, cancelPath(s.st.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if job := s.f.awaitDeploy(t, jobID); job.State == "done" {
		t.Fatalf("job = %+v, want it stopped", job)
	}
	rows := s.f.auditRows(t, "stack.deployed")
	last := rows[len(rows)-1]
	if last.Details["result"] != "cancelled" || last.Details["cancelledBy"] != "operator-2" {
		t.Errorf("stack.deployed = %+v, want result cancelled by operator-2", last)
	}
}

// A failed update leaves the draft where it was and says so in the run and the audit log.
func TestFailedUpdateNoRebase(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	s.move(t, "web", 'b')
	s.f.fake.SetTaskState("shop_web", "preparing", "")
	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "shop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	jobID := answerOf[stackRunAnswer](t, rec).JobID
	awaitStackJobItem(t, s.f, jobID, func(it jobs.Item) bool { return it.ID == "service:shop_web" && it.Unit == "tasks" })
	if rec := s.operator.do(http.MethodPost, cancelPath(s.st.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if job := s.f.awaitDeploy(t, jobID); job.State == "done" {
		t.Fatalf("job = %+v, want it stopped", job)
	}
	s.f.fake.SetTaskState("shop_web", "", "")

	if base, _ := s.f.draftBase(t, s.st.ID); base != 1 {
		t.Errorf("the draft starts from revision %d after a failed update, want it left at 1", base)
	}
	revs := s.f.revisions(t, s.st.ID)
	if len(revs) != 2 || revs[1].Outcome != "failed" {
		t.Errorf("revisions = %+v, want revision 2 failed", revs)
	}
	run := s.f.runByJob(t, jobID)
	if run.State != updates.RunFailed || run.Reason != updates.ReasonCancelled || run.RevisionSeq != 2 || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v, want a failed run that was cancelled and made revision 2", run)
	}
	if rows := s.f.auditRows(t, "image.update_failed"); len(rows) != 1 || rows[0].Details["workload"] != "shop_web" || rows[0].Result != "failure" {
		t.Errorf("image.update_failed rows = %+v, want one for shop_web", rows)
	}
	if rows := s.f.auditRows(t, "image.updated"); len(rows) != 0 {
		t.Errorf("image.updated rows = %+v, want none", rows)
	}
	if _, held := s.f.stackLocks.Held(s.e.ID); held {
		t.Error("the lock is still held after the update ended")
	}
}

// The revision's pins are the images that run, so the stack reads in sync afterwards.
func TestNoDriftAfterUpdate(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web", "api")
	s.move(t, "web", 'b')
	s.updateStack(t, s.operator, "stack", "shop", nil)

	got := answerOf[driftAnswer](t, s.operator.do(http.MethodGet, driftPath(s.st.ID, true), ""))
	if got.State != "none" || got.Revision != 2 || got.Count != 0 {
		t.Errorf("drift after the update = %+v, want none at revision 2", got)
	}
}
