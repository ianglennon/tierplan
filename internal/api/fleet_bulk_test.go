// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/wire"
)

// Updating and skipping many workloads across the fleet in one action (plan 12-30, ENV-10, IMG-06, D-10, D-11, D-14, D-15). Docker is a
// fake daemon behind the real monitor, aggregator and cluster reader; the registry is a scripted Heads; the applier, its run store, the
// job hub, the operation lock, the router and the permission checks are the real ones.

// fail makes the registry unable to answer for the repository.
func (h *scriptedHeads) fail(repo string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[repo] = headAnswer{err: err}
}

// bulkItem is one workload of a request.
func bulkItem(envID, kind, name string, expect map[string]string) map[string]any {
	it := map[string]any{"envId": envID, "kind": kind, "name": name}
	if expect != nil {
		it["expect"] = expect
	}
	return it
}

type bulkSkipped struct {
	EnvID  string `json:"envId"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Code   string `json:"code"`
}

type bulkAnswer struct {
	BulkID string `json:"bulkId"`
	Jobs   []struct {
		EnvID string `json:"envId"`
		JobID string `json:"jobId"`
	} `json:"jobs"`
	Skipped []bulkSkipped `json:"skipped"`
}

// reasons are the reasons of the skipped items, by "kind/name".
func (a bulkAnswer) reasons() map[string]string {
	out := map[string]string{}
	for _, s := range a.Skipped {
		out[s.Kind+"/"+s.Name] = s.Reason
	}
	return out
}

func (a bulkAnswer) jobOf(t *testing.T, envID string) string {
	t.Helper()
	for _, j := range a.Jobs {
		if j.EnvID == envID {
			return j.JobID
		}
	}
	t.Fatalf("no job for environment %s in %+v", envID, a)
	return ""
}

func bulkBody(items ...map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"items": items})
	return string(raw)
}

// bulkRunAs posts a bulk update as the admin and answers the recorder.
func (f *envFix) bulkRun(t *testing.T, items ...map[string]any) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/fleet/updates/run", bulkBody(items...))
}

// bulkSwarm is one swarm manager running the services, whose registry says what heads says, whose tasks follow the writes made to it
// and whose applier waits on a clock that answers at once.
func bulkSwarm(t *testing.T, heads *scriptedHeads, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(services...)
	cfg.TasksFollowWrites = true
	cfg.SwarmNodeID = "node-1"
	if tune != nil {
		tune(&cfg)
	}
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.updateApplier = func(a *updates.Applier) { a.Clock = &instantClock{now: time.Unix(1_700_000_000, 0)} }
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	return f, e
}

// bulkStates are the states of a bulk job's items by ID.
func bulkItemStates(job jobs.Job) map[string]string {
	out := map[string]string{}
	for _, it := range job.Items {
		out[it.ID] = it.State
	}
	return out
}

// runsOfBulk are the runs a bulk update recorded, oldest first.
func (f *envFix) runsOfBulk(t *testing.T, bulkID string) []updates.Run {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT id FROM image_update_runs WHERE bulk_id = ? ORDER BY started_at, id`, bulkID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	var out []updates.Run
	for _, id := range ids {
		run, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, run)
	}
	return out
}

// bulkFleet is two environments: the local host, a standalone engine running the container app, and a swarm reached through its
// agent that runs the services web and api. Their registry says what heads says, and each has been checked.
func bulkFleet(t *testing.T, heads *scriptedHeads) (f *agentFix, local, swarm envBody) {
	t.Helper()
	f = newAgentFixHost(t, "unix://"+dockertest.NewFake(t, containerEngine(plainContainer())).SocketPath())
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.updateApplier = func(a *updates.Applier) { a.Clock = &instantClock{now: time.Unix(1_700_000_000, 0)} }
	f.h = f.build(t)
	local = f.add(t)
	f.awaitFirstProbe(t, local.ID)

	env := f.srv.NewEnvironment(t, "Swarm")
	cfg := swarmManagerEngine(plainService("web", 'a'), plainService("api", 'a'))
	cfg.TasksFollowWrites, cfg.SwarmNodeID, cfg.SwarmClusterID = true, "node-1", "cluster-1"
	f.connect(t, env, cfg, &wire.Facts{
		EngineID: "ENG:mgr-1", Hostname: "mgr-1", EngineReachable: true, EngineVersion: "29.1.0",
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-1", ClusterID: "cluster-1", Control: true},
	})
	f.whenState(t, env.ID, "online")
	swarm = envBody{ID: env.ID}
	for _, id := range []string{local.ID, swarm.ID} {
		if rec := f.checkNow(t, envBody{ID: id}); rec.code != http.StatusOK {
			t.Fatalf("check now %s: %d %s", id, rec.code, rec.body)
		}
	}
	return f, local, swarm
}

// The tracer (plan 12-30, IMG-06, D-11): "Update 3 selected" across two environments makes two jobs, one in each, which update their
// items one at a time under one bulk ID. A swarm reached through its agent has two services; the local host has one container. Every
// item is done, every audit row of the selection carries the bulk ID, and each environment's job ends with a summary row.
func TestBulkUpdateTwoEnvironments(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/web": {digest: updDigest('b')}, "team/api": {digest: updDigest('b')}, "team/app": {digest: updDigest('b')},
	})
	f, local, swarm := bulkFleet(t, heads)

	rec := f.bulkRun(t,
		bulkItem(swarm.ID, "service", "web", map[string]string{"web": updDigest('b')}),
		bulkItem(local.ID, "container", "app", map[string]string{"app": updDigest('b')}),
		bulkItem(swarm.ID, "service", "api", nil),
	)
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	if len(ans.Jobs) != 2 || len(ans.Skipped) != 0 || ans.BulkID == "" {
		t.Fatalf("answer = %+v, want two jobs, nothing skipped and a bulk ID", ans)
	}

	swarmJob := f.awaitUpdate(t, ans.jobOf(t, swarm.ID))
	localJob := f.awaitUpdate(t, ans.jobOf(t, local.ID))
	for name, job := range map[string]jobs.Job{"swarm": swarmJob, "local": localJob} {
		if job.State != jobs.JobDone || job.Kind != updates.JobKind || job.Meta[updates.MetaBulk] != ans.BulkID {
			t.Errorf("%s job = %s %s bulk %q: %+v, want a done update job of the bulk", name, job.State, job.Kind, job.Meta[updates.MetaBulk], job.Items)
		}
	}
	if s := bulkItemStates(swarmJob); s["i0"] != "done" || s["i1"] != "done" || s["summary"] != "done" {
		t.Errorf("swarm job items = %v, want both services and the summary done", s)
	}
	if s := bulkItemStates(localJob); s["i0"] != "done" || s["summary"] != "done" {
		t.Errorf("local job items = %v, want the container and the summary done", s)
	}

	// The runs of the selection are done, one after the other, each tied to its own environment's job.
	runs := f.runsOfBulk(t, ans.BulkID)
	if len(runs) != 3 {
		t.Fatalf("runs = %+v, want three", runs)
	}
	for _, run := range runs {
		want := swarmJob.ID
		if run.EnvID == local.ID {
			want = localJob.ID
		}
		if run.State != updates.RunDone || run.JobID != want || len(run.Items) != 1 || run.Items[0].ToDigest != updDigest('b') || run.Items[0].FromDigest != updDigest('a') {
			t.Errorf("run = %+v, want a done run of one item a to b in job %s", run, want)
		}
	}

	// Every audit row of the selection carries the bulk ID, and each environment has one summary row.
	rows := f.auditRows(t, "image.updated")
	if len(rows) != 3 {
		t.Fatalf("image.updated rows = %+v, want three", rows)
	}
	for _, r := range rows {
		if r.Details["bulk"] != ans.BulkID {
			t.Errorf("row %+v does not carry the bulk ID", r)
		}
	}
	sums := f.auditRows(t, "image.bulk_updated")
	if len(sums) != 2 {
		t.Fatalf("image.bulk_updated rows = %+v, want one for each environment", sums)
	}
	got := map[string]string{}
	for _, r := range sums {
		if r.Details["bulk"] != ans.BulkID || r.Details["rolled_back"] != "0" || r.Details["skipped"] != "0" || r.Result != "success" {
			t.Errorf("summary row = %+v", r)
		}
		got[r.EnvID] = r.Details["done"] + " of " + r.Details["total"]
	}
	if got[swarm.ID] != "2 of 2" || got[local.ID] != "1 of 1" {
		t.Errorf("summaries = %v, want 2 of 2 in the swarm and 1 of 1 locally", got)
	}
	for _, id := range []string{swarm.ID, local.ID} {
		if h, held := f.stackLocks.Held(id); held {
			t.Errorf("the lock of %s is still held by %+v", id, h)
		}
	}
}

// rejectNewVersionOf makes every task of the service fail while it holds the digest c, as a swarm does when it cannot start an image.
func rejectNewVersionOf(f *envFix, service string, c byte) {
	var script []dockertest.FakeServiceTask
	for slot := 1; slot <= 3; slot++ {
		script = append(script, dockertest.FakeServiceTask{Slot: slot, State: "rejected", Desired: "shutdown", Err: "No such image"})
	}
	f.fake.SetServiceTasksWhileImage(service, updDigest(c), script...)
}

// threeServices are the services the single-environment bulk tests update.
func threeServices() []dockertest.FakeService {
	return []dockertest.FakeService{plainService("web", 'a'), plainService("api", 'a'), plainService("db", 'a')}
}

func threeAnswers(c byte) map[string]headAnswer {
	return map[string]headAnswer{"team/web": {digest: updDigest(c)}, "team/api": {digest: updDigest(c)}, "team/db": {digest: updDigest(c)}}
}

// The job of one item failing does not take the others with it (plan 12-30, D-11): the second of three services is rejected by the
// swarm, so it is put back and ends rolled back, and the first and the third are done. The job's summary reads 2 of 3 with one rolled
// back, and every audit row carries the bulk ID.
func TestBulkFailureIsolated(t *testing.T) {
	t.Parallel()
	f, e := bulkSwarm(t, newScriptedHeads(threeAnswers('b')), nil, threeServices()...)
	rejectNewVersionOf(f, "api", 'b')

	rec := f.bulkRun(t, bulkItem(e.ID, "service", "web", nil), bulkItem(e.ID, "service", "api", nil), bulkItem(e.ID, "service", "db", nil))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	job := f.awaitUpdate(t, ans.jobOf(t, e.ID))
	if s := bulkItemStates(job); s["i0"] != "done" || s["i1"] != "failed" || s["i2"] != "done" || s["summary"] != "done" {
		t.Fatalf("items = %v, want web and db done, api failed and the summary done", s)
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job = %s, want partial", job.State)
	}
	// api's own steps are under its item, with the rollback added to them.
	steps := bulkItemStates(job)
	if steps["i1/wait"] != "failed" || steps["i1/rollback"] != "done" || steps["i1/rollback_wait"] != "done" {
		t.Errorf("api's steps = %v, want the wait failed and the rollback done", steps)
	}

	runs := map[string]updates.Run{}
	for _, run := range f.runsOfBulk(t, ans.BulkID) {
		runs[run.Workload] = run
	}
	if runs["web"].State != updates.RunDone || runs["db"].State != updates.RunDone || runs["api"].State != updates.RunRolledBack || runs["api"].Reason != updates.ReasonDidntStart {
		t.Errorf("runs = web %s, api %s %q, db %s, want done, rolled_back (didnt_start), done", runs["web"].State, runs["api"].State, runs["api"].Reason, runs["db"].State)
	}
	// One at a time, in the order they were named: the update of api and its rollback come between web's and db's.
	var order []string
	for _, u := range f.fake.ServiceUpdates() {
		order = append(order, u.Spec.Name)
	}
	if want := []string{"web", "api", "api", "db"}; !slices.Equal(order, want) {
		t.Errorf("services written = %v, want %v", order, want)
	}
	for action, want := range map[string]int{"image.updated": 2, "image.update_failed": 1, "image.update_rolled_back": 1} {
		rows := f.auditRows(t, action)
		if len(rows) != want {
			t.Errorf("%s rows = %d, want %d", action, len(rows), want)
		}
		for _, r := range rows {
			if r.Details["bulk"] != ans.BulkID {
				t.Errorf("%s row %+v does not carry the bulk ID", action, r)
			}
		}
	}
	sums := f.auditRows(t, "image.bulk_updated")
	if len(sums) != 1 || sums[0].Details["done"] != "2" || sums[0].Details["total"] != "3" || sums[0].Details["rolled_back"] != "1" || sums[0].Details["skipped"] != "0" || sums[0].Result != "failure" {
		t.Errorf("summary rows = %+v, want 2 of 3 with 1 rolled back, a failure", sums)
	}
	if u := f.heldOf(t, e, "api"); u.State != "rolled_back" {
		t.Errorf("api reads %s, want rolled_back", u.State)
	}
	if _, held := f.stackLocks.Held(e.ID); held {
		t.Errorf("the lock is still held after the job ended")
	}
}

// An environment another operation holds has its items skipped as locked, and the environment that is free is updated (plan 12-30, D-10).
func TestBulkLockedEnvironmentSkipped(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/web": {digest: updDigest('b')}, "team/api": {digest: updDigest('b')}, "team/app": {digest: updDigest('b')},
	})
	f, local, swarm := bulkFleet(t, heads)
	if _, _, ok := f.stackLocks.TryAcquire(swarm.ID, stackops.Holder{StackID: "stk-1", StackName: "shop", Kind: "deploy", By: "ada", Started: time.Now()}); !ok {
		t.Fatal("could not take the lock for a stack deploy")
	}

	rec := f.bulkRun(t, bulkItem(swarm.ID, "service", "web", nil), bulkItem(local.ID, "container", "app", nil), bulkItem(swarm.ID, "service", "api", nil))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	if len(ans.Jobs) != 1 || ans.Jobs[0].EnvID != local.ID {
		t.Fatalf("jobs = %+v, want only the local host's", ans.Jobs)
	}
	if got := ans.reasons(); len(got) != 2 || got["service/web"] != "locked" || got["service/api"] != "locked" {
		t.Errorf("skipped = %v, want web and api locked", got)
	}
	if job := f.awaitUpdate(t, ans.Jobs[0].JobID); job.State != jobs.JobDone {
		t.Errorf("the free environment's job = %s, want done", job.State)
	}
	if h, held := f.stackLocks.Held(swarm.ID); !held || h.Kind != "deploy" {
		t.Errorf("holder of the swarm = %+v %v, want the deploy still holding it", h, held)
	}
	if runs := f.runsOfBulk(t, ans.BulkID); len(runs) != 1 || runs[0].EnvID != local.ID {
		t.Errorf("runs = %+v, want only the local container's", runs)
	}
}

// An item whose newest version moved since it was confirmed is skipped at its turn and the others run (plan 12-30, D-05): the job
// names it, its run is refused with the reason, nothing is written to its service, and the summary counts it skipped.
func TestBulkMovedItemSkipped(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(threeAnswers('b'))
	f, e := bulkSwarm(t, heads, nil, threeServices()...)
	// The registry has moved on since the person looked.
	heads.set("team/api", updDigest('c'))

	rec := f.bulkRun(t,
		bulkItem(e.ID, "service", "web", map[string]string{"web": updDigest('b')}),
		bulkItem(e.ID, "service", "api", map[string]string{"api": updDigest('b')}),
		bulkItem(e.ID, "service", "db", map[string]string{"db": updDigest('b')}),
	)
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	job := f.awaitUpdate(t, ans.jobOf(t, e.ID))
	s := bulkItemStates(job)
	if s["i0"] != "done" || s["i1"] != "skipped" || s["i2"] != "done" || s["summary"] != "done" {
		t.Fatalf("items = %v, want web and db done and api skipped", s)
	}
	for _, it := range job.Items {
		if it.ID == "i1" && it.Code != "moved" {
			t.Errorf("api's item code = %q, want moved", it.Code)
		}
	}
	runs := map[string]updates.Run{}
	for _, run := range f.runsOfBulk(t, ans.BulkID) {
		runs[run.Workload] = run
	}
	if runs["api"].State != updates.RunRefused || runs["api"].Reason != "moved" || runs["web"].State != updates.RunDone || runs["db"].State != updates.RunDone {
		t.Errorf("runs = %+v, want api refused as moved and the others done", runs)
	}
	for _, u := range f.fake.ServiceUpdates() {
		if u.Spec.Name == "api" {
			t.Errorf("api was written although its version moved")
		}
	}
	sums := f.auditRows(t, "image.bulk_updated")
	if len(sums) != 1 || sums[0].Details["done"] != "2" || sums[0].Details["total"] != "3" || sums[0].Details["skipped"] != "1" {
		t.Errorf("summary rows = %+v, want 2 of 3 with 1 skipped", sums)
	}
	if rows := f.auditRows(t, "image.updated"); len(rows) != 2 {
		t.Errorf("image.updated rows = %d, want two", len(rows))
	}
}

// heldJob starts a bulk update of the three services whose first pull is held, and answers the job and the function that lets it go on.
func heldJob(t *testing.T) (f *envFix, e envBody, ans bulkAnswer, release func()) {
	t.Helper()
	hold := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	f, e = bulkSwarm(t, newScriptedHeads(threeAnswers('b')), func(c *dockertest.FakeConfig) { c.PullHold = hold }, threeServices()...)
	rec := f.bulkRun(t, bulkItem(e.ID, "service", "web", nil), bulkItem(e.ID, "service", "api", nil), bulkItem(e.ID, "service", "db", nil))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans = decode[bulkAnswer](t, rec)
	// The pull reaches the engine only after web's run has begun, and it is held there.
	awaitFake(t, f.fake, "web's pull to reach the engine", func() bool {
		return slices.ContainsFunc(f.fake.Writes(), func(w string) bool { return strings.HasPrefix(w, "POST /images/create") })
	})
	return f, e, ans, release
}

// While a bulk job waits, its items read queued in every badge, then updating while each runs (plan 12-30, UI-SPEC "Queued").
func TestBulkQueuedStates(t *testing.T) {
	t.Parallel()
	f, e, ans, release := heldJob(t)
	jobID := ans.jobOf(t, e.ID)

	web, api, db := f.heldOf(t, e, "web"), f.heldOf(t, e, "api"), f.heldOf(t, e, "db")
	if web.State != "updating" || api.State != "queued" || db.State != "queued" {
		t.Fatalf("states = web %s, api %s, db %s, want updating, queued, queued", web.State, api.State, db.State)
	}
	if web.JobID != jobID || api.JobID != jobID || db.JobID != jobID {
		t.Errorf("jobs = %q %q %q, want the bulk job %s on each", web.JobID, api.JobID, db.JobID, jobID)
	}
	if runs, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Running(t.Context(), e.ID); err != nil || len(runs) != 3 {
		t.Errorf("runs = %+v %v, want the three on record, one running and two queued", runs, err)
	}

	release()
	if job := f.awaitUpdate(t, jobID); job.State != jobs.JobDone {
		t.Fatalf("job = %s %+v, want done", job.State, job.Items)
	}
	for _, name := range []string{"web", "api", "db"} {
		if u := f.heldOf(t, e, name); u.State == "queued" || u.State == "updating" || u.State == "busy" {
			t.Errorf("%s reads %s after the job, want none of queued, updating or busy", name, u.State)
		}
	}
}

// A cancelled bulk job stops after the item in progress has ended and the rest are refused as cancelled; nothing of theirs is written
// (plan 12-30, D-11).
func TestBulkCancel(t *testing.T) {
	t.Parallel()
	f, e, ans, release := heldJob(t)
	jobID := ans.jobOf(t, e.ID)

	if err := f.jobHub.Cancel(jobID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	job := f.awaitUpdate(t, jobID)
	release()
	runs := map[string]updates.Run{}
	for _, run := range f.runsOfBulk(t, ans.BulkID) {
		runs[run.Workload] = run
	}
	if runs["web"].State == updates.RunQueued || runs["web"].State == updates.RunRunning || runs["web"].State == updates.RunDone {
		t.Errorf("web = %s, want the item in progress ended, not done", runs["web"].State)
	}
	for _, name := range []string{"api", "db"} {
		if r := runs[name]; r.State != updates.RunRefused || r.Reason != updates.ReasonCancelled {
			t.Errorf("%s = %s %q, want refused as cancelled", name, r.State, r.Reason)
		}
	}
	if s := bulkItemStates(job); s["i1"] != "cancelled" || s["i2"] != "cancelled" {
		t.Errorf("items = %v, want api and db cancelled", s)
	}
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Errorf("services written = %d, want none: the first pull was cancelled and the rest never started", n)
	}
	if _, held := f.stackLocks.Held(e.ID); held {
		t.Errorf("the lock is still held after the job ended")
	}
	sums := f.auditRows(t, "image.bulk_updated")
	if len(sums) != 1 || sums[0].Details["done"] != "0" || sums[0].Details["total"] != "3" || sums[0].Details["skipped"] != "2" {
		t.Errorf("summary rows = %+v, want 0 of 3 with 2 skipped", sums)
	}
}

// A managed stack's services in a selection are one revision of the stack, deployed inside the bulk job under the lock the job already
// holds: no second job, no second lock, its own history entry, and with its own automatic rollback when it fails (plan 12-30, D-06, D-12).
func TestBulkStackItemInJob(t *testing.T) {
	t.Parallel()
	bulkAs := func(t *testing.T, s *shopFix, items ...map[string]any) bulkAnswer {
		t.Helper()
		rec := s.f.call(t, authReq{method: http.MethodPost, path: "/api/fleet/updates/run", body: bulkBody(items...), cookie: s.operator.cookie, csrf: s.operator.csrf})
		if rec.Code != http.StatusOK {
			t.Fatalf("bulk update: %d %s", rec.Code, rec.Body)
		}
		return answerOf[bulkAnswer](t, rec)
	}
	t.Run("two services of one stack are one revision", func(t *testing.T) {
		t.Parallel()
		s := shopQuick(t, "web", "api")
		s.move(t, "web", 'b')
		s.move(t, "api", 'b')
		jobsBefore := len(s.f.jobHub.List(func(jobs.Job) bool { return true }))

		ans := bulkAs(t, s,
			bulkItem(s.e.ID, "service", "shop_web", map[string]string{"shop_web": updDigest('b')}),
			bulkItem(s.e.ID, "service", "shop_api", map[string]string{"shop_api": updDigest('b')}),
		)
		if len(ans.Jobs) != 1 || len(ans.Skipped) != 0 {
			t.Fatalf("answer = %+v, want one job and nothing skipped", ans)
		}
		job := s.f.awaitUpdate(t, ans.Jobs[0].JobID)
		if job.State != jobs.JobDone {
			t.Fatalf("job = %s %+v, want done", job.State, job.Items)
		}
		if n := len(s.f.jobHub.List(func(jobs.Job) bool { return true })); n != jobsBefore+1 {
			t.Errorf("jobs = %d, want %d: the bulk job and no job of the stack's own", n, jobsBefore+1)
		}
		st := bulkItemStates(job)
		if st["i0"] != "done" || st["summary"] != "done" || len(job.Items) < 4 {
			t.Errorf("items = %v, want the stack's item and the summary done, with the deploy's steps under the item", st)
		}
		for _, it := range job.Items {
			if it.ID != "i0" && it.ID != "summary" && !strings.HasPrefix(it.ID, "i0/") {
				t.Errorf("item %q (parent %q) is not under the stack's item", it.ID, it.Parent)
			}
		}
		revs := s.f.revisions(t, s.st.ID)
		if len(revs) != 2 || revs[1].Outcome != "good" {
			t.Fatalf("revisions = %+v, want one good revision 2: both services in one", revs)
		}
		old, now := pinsOf(t, revs[0]), pinsOf(t, revs[1])
		if now["shop_web"] != "registry.test/team/web:1@"+updDigest('b') || now["shop_api"] != "registry.test/team/api:1@"+updDigest('b') || old["shop_web"] == now["shop_web"] {
			t.Errorf("pins were %v and are %v, want both services at b", old, now)
		}
		run := s.f.runByJob(t, job.ID)
		if run.Kind != "stack" || run.StackID != s.st.ID || run.BulkID != ans.BulkID || run.State != updates.RunDone || run.RevisionSeq != 2 || len(run.Items) != 2 {
			t.Errorf("run = %+v, want a done stack run of two services that made revision 2 in this bulk", run)
		}
		rows := s.f.auditRows(t, "image.updated")
		if len(rows) != 2 {
			t.Fatalf("image.updated rows = %+v, want one for each service", rows)
		}
		for _, r := range rows {
			if r.Details["bulk"] != ans.BulkID {
				t.Errorf("row %+v does not carry the bulk ID", r)
			}
		}
		if sums := s.f.auditRows(t, "image.bulk_updated"); len(sums) != 1 || sums[0].Details["done"] != "2" || sums[0].Details["total"] != "2" {
			t.Errorf("summary rows = %+v, want 2 of 2", sums)
		}
		if h, held := s.f.stackLocks.Held(s.e.ID); held {
			t.Errorf("the lock is still held by %+v", h)
		}
	})
	t.Run("the audit rows of a stack in a job carry the caller's address", func(t *testing.T) {
		t.Parallel()
		s := shopQuick(t, "web", "api")
		s.move(t, "web", 'b')
		s.rejectNewVersion("web", 'b')

		ans := bulkAs(t, s, bulkItem(s.e.ID, "service", "shop_web", nil))
		s.f.awaitUpdate(t, ans.jobOf(t, s.e.ID))
		for _, action := range []string{"image.update_failed", "image.update_rolled_back", "stack.deployed"} {
			ips := s.f.auditIPs(t, action)
			if len(ips) == 0 {
				t.Errorf("no %s row, want the failed update and its rollback audited", action)
			}
			for _, ip := range ips {
				if ip != authIP {
					t.Errorf("%s recorded the address %q, want the caller's %s", action, ip, authIP)
				}
			}
		}
	})
	t.Run("a failing deploy puts the stack back inside the job", func(t *testing.T) {
		t.Parallel()
		s := shopQuick(t, "web", "api")
		s.move(t, "web", 'b')
		s.rejectNewVersion("web", 'b')

		ans := bulkAs(t, s, bulkItem(s.e.ID, "service", "shop_web", nil))
		job := s.f.awaitUpdate(t, ans.jobOf(t, s.e.ID))
		if st := bulkItemStates(job); st["i0"] != "failed" || st["i0/rollback"] != "done" || st["summary"] != "done" {
			t.Fatalf("items = %v, want the stack's item failed with its rollback done", st)
		}
		revs := s.f.revisions(t, s.st.ID)
		if len(revs) != 3 || revs[1].Outcome != "failed" || revs[2].Outcome != "good" {
			t.Fatalf("revisions = %+v, want good 1, the failed update 2 and a good rollback 3", revs)
		}
		run := s.f.runByJob(t, job.ID)
		if run.State != updates.RunRolledBack || run.BulkID != ans.BulkID || run.RollbackSeq != 3 {
			t.Errorf("run = %+v, want rolled_back by rollback 3 in this bulk", run)
		}
		if sums := s.f.auditRows(t, "image.bulk_updated"); len(sums) != 1 || sums[0].Details["done"] != "0" || sums[0].Details["rolled_back"] != "1" {
			t.Errorf("summary rows = %+v, want 0 of 1 with 1 rolled back", sums)
		}
		if _, held := s.f.stackLocks.Held(s.e.ID); held {
			t.Errorf("the lock is still held after the rollback")
		}
	})
}

// An item is judged again at its turn from what the check found, never from the queue it waits in: a registry that cannot be asked when
// the turn comes skips the item and does not update it to the digest an earlier check found (plan 12-30, D-05, T-12-90).
func TestBulkItemWhoseCheckFailsIsSkipped(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(threeAnswers('b'))
	f, e := bulkSwarm(t, heads, nil, threeServices()...)
	heads.fail("team/api", errors.New("connection refused"))
	f.clk.t = f.clk.t.Add(time.Minute) // the failed check is newer than the good one that selected it

	rec := f.bulkRun(t, bulkItem(e.ID, "service", "web", nil), bulkItem(e.ID, "service", "api", nil), bulkItem(e.ID, "service", "db", nil))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	job := f.awaitUpdate(t, ans.jobOf(t, e.ID))
	if s := bulkItemStates(job); s["i0"] != "done" || s["i1"] != "skipped" || s["i2"] != "done" {
		t.Fatalf("items = %v, want api skipped and the others done", s)
	}
	for _, u := range f.fake.ServiceUpdates() {
		if u.Spec.Name == "api" {
			t.Errorf("api was written although the registry could not be asked")
		}
	}
	for _, run := range f.runsOfBulk(t, ans.BulkID) {
		if run.Workload == "api" && (run.State != updates.RunRefused || run.Reason != updates.CodeCheckFailed) {
			t.Errorf("api's run = %s %q, want refused as check_failed", run.State, run.Reason)
		}
	}
}

// bulkAsUser posts to a fleet bulk route as the named signed-in user.
func (f *authFixture) bulkAsUser(t *testing.T, user, path, body string) *envRec {
	t.Helper()
	cookie, csrf := f.signIn(t, user, false)
	rec := f.call(t, authReq{method: http.MethodPost, path: path, body: body, cookie: cookie, csrf: csrf})
	return &envRec{rec.Code, rec.Body.Bytes(), ""}
}

type bulkSkipAnswer struct {
	BulkID  string        `json:"bulkId"`
	Skipped int           `json:"skipped"`
	Refused []bulkSkipped `json:"refused"`
}

func skipBulkBody(items ...map[string]any) string { return bulkBody(items...) }

// Skipping the selected versions across environments needs updates.manage on each (plan 12-30, D-10, D-15): a person who is an Admin of
// one environment has theirs skipped, each with its own audit row carrying the one bulk ID, and the items of the environment where
// they are not are answered no_permission.
func TestFleetSkipSelected(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/web": {digest: updDigest('b')}, "team/api": {digest: updDigest('b')}, "team/app": {digest: updDigest('b')},
	})
	f, local, swarm := bulkFleet(t, heads)
	f.addUser(t, "sam", false)
	f.addEnvRole(t, "sam", swarm.ID, "admin")
	f.addEnvRole(t, "sam", local.ID, "operator")

	rec := f.bulkAsUser(t, "sam", "/api/fleet/updates/skip", skipBulkBody(
		bulkItem(swarm.ID, "service", "web", nil), bulkItem(local.ID, "container", "app", nil), bulkItem(swarm.ID, "service", "api", nil),
	))
	if rec.code != http.StatusOK {
		t.Fatalf("skip: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkSkipAnswer](t, rec)
	if ans.Skipped != 2 || len(ans.Refused) != 1 || ans.Refused[0].Name != "app" || ans.Refused[0].Reason != "no_permission" || ans.Refused[0].EnvID != local.ID || ans.BulkID == "" {
		t.Fatalf("answer = %+v, want two skipped and the container refused no_permission", ans)
	}
	rows := f.auditRows(t, "image.update_skipped")
	if len(rows) != 2 {
		t.Fatalf("image.update_skipped rows = %+v, want one for each skipped workload", rows)
	}
	got := map[string]string{}
	for _, r := range rows {
		if r.Details["bulk"] != ans.BulkID || r.Details["digest"] != updDigest('b') || r.EnvID != swarm.ID {
			t.Errorf("row %+v, want the swarm, the digest passed over and the bulk ID", r)
		}
		got[r.Details["workload"]] = r.Details["kind"]
	}
	if got["web"] != "service" || got["api"] != "service" {
		t.Errorf("rows name %v, want web and api", got)
	}
	for _, name := range []string{"web", "api"} {
		if u := f.swarmServiceUpdate(t, swarm.ID, name); u != "skipped" {
			t.Errorf("%s reads %s, want skipped", name, u)
		}
	}
	if u := f.localContainerUpdate(t, local.ID, "app"); u != "update_available" {
		t.Errorf("app reads %s, want update_available: it was refused", u)
	}
}

// brokenRepo is an update store whose reads of one repository fail while armed, the way a database error on one item would.
type brokenRepo struct {
	updates.Store
	repo  string
	armed atomic.Bool
}

func (b *brokenRepo) Checks(ctx context.Context, keys []updates.Key) (map[updates.Key]updates.Check, error) {
	if b.armed.Load() && slices.ContainsFunc(keys, func(k updates.Key) bool { return k.Repo == b.repo }) {
		return nil, errors.New("disk I/O error")
	}
	return b.Store.Checks(ctx, keys)
}

// An item of a bulk skip that fails on the server's side does not take the answer with it (WR-09): the skips made before it are saved
// and audited under the bulk ID, so the answer carries that ID, counts them and names the failed item, and the items after it go on.
func TestFleetSkipKeepsGoingAfterAServerError(t *testing.T) {
	t.Parallel()
	cfg := swarmManagerEngine(threeServices()...)
	cfg.TasksFollowWrites, cfg.SwarmNodeID = true, "node-1"
	f := newEnvFix(t, cfg)
	broken := &brokenRepo{Store: updates.SQLStore{Q: f.st.Q, W: f.st.Write}, repo: "team/api"}
	f.updateStore = broken
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = newScriptedHeads(threeAnswers('b'))
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	broken.armed.Store(true)
	rec := f.req(t, http.MethodPost, "/api/fleet/updates/skip", skipBulkBody(
		bulkItem(e.ID, "service", "web", nil), bulkItem(e.ID, "service", "api", nil), bulkItem(e.ID, "service", "db", nil),
	))
	broken.armed.Store(false)
	if rec.code != http.StatusOK {
		t.Fatalf("skip: %d %s, want 200 with the progress that was made", rec.code, rec.body)
	}
	ans := decode[bulkSkipAnswer](t, rec)
	if ans.BulkID == "" || ans.Skipped != 2 || len(ans.Refused) != 1 || ans.Refused[0].Name != "api" || ans.Refused[0].Reason != "offline" {
		t.Fatalf("answer = %+v, want a bulk ID, web and db skipped and api refused offline", ans)
	}
	rows := f.auditRows(t, "image.update_skipped")
	got := map[string]bool{}
	for _, r := range rows {
		if r.Details["bulk"] != ans.BulkID {
			t.Errorf("row %+v, want the bulk ID %s", r, ans.BulkID)
		}
		got[r.Details["workload"]] = true
	}
	if len(rows) != 2 || !got["web"] || !got["db"] {
		t.Errorf("image.update_skipped rows = %+v, want web and db", rows)
	}
}

// auditIPs are the client addresses of the audit rows of one action, oldest first.
func (f *envFix) auditIPs(t *testing.T, action string) []string {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT ip FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			t.Fatal(err)
		}
		out = append(out, ip)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// With the stacks of an environment unreadable, a container of a managed stack cannot be told from one TierPlan does not manage, and
// the bulk update must not guess (WR-14): its items are refused as offline and nothing is written.
func TestBulkWithUnreadableStacksRefusesTheEnvironmentsItems(t *testing.T) {
	t.Parallel()
	f, e := bulkSwarm(t, newScriptedHeads(threeAnswers('b')), nil, threeServices()...)
	if _, err := f.st.Write.Exec(`ALTER TABLE stacks RENAME TO stacks_unreadable`); err != nil {
		t.Fatal(err)
	}

	rec := f.bulkRun(t, bulkItem(e.ID, "service", "web", nil), bulkItem(e.ID, "service", "api", nil))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	if len(ans.Jobs) != 0 || len(ans.Skipped) != 2 || ans.Skipped[0].Reason != "offline" || ans.Skipped[1].Reason != "offline" {
		t.Errorf("answer = %+v, want no job and both items refused offline", ans)
	}
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want nothing written", n)
	}
}

// The Updates tab names an environment whose stacks cannot be read as not read, where it would list its managed containers as
// "adopt first" and not selectable (WR-14).
func TestFleetUpdatesWithUnreadableStacksNameTheEnvironment(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	alpha := f.id("Alpha")
	f.fake.online(alpha)
	f.fake.verdicts[alpha] = fleetVerdicts{list: []updates.WorkloadVerdict{verdictOf(t, "container", "cache", "", updates.StateUpdateAvailable)}}
	f.fleet.Store = f.st
	f.rebuild(t)
	if got := f.updatesList(t, "", ""); len(got.Groups) != 1 || len(got.NotIncluded) != 0 {
		t.Fatalf("answer = %+v, want Alpha listed while its stacks can be read", got)
	}
	if _, err := f.st.Write.Exec(`ALTER TABLE stacks RENAME TO stacks_unreadable`); err != nil {
		t.Fatal(err)
	}

	got := f.updatesList(t, "", "")
	want := []fleetNotIncluded{{EnvID: alpha, EnvName: "Alpha", Reason: "not_reporting"}}
	if len(got.Groups) != 0 || !slices.Equal(got.NotIncluded, want) {
		t.Errorf("answer = %+v, want Alpha named in notIncluded and no items", got)
	}
}

// swarmServiceUpdate is the state the services list of the environment gives the service.
func (f *agentFix) swarmServiceUpdate(t *testing.T, envID, name string) string {
	t.Helper()
	list := decode[heldList](t, f.req(t, http.MethodGet, "/api/env/"+envID+"/services", ""))
	for _, it := range list.Items {
		if it.Name == name && it.Update != nil {
			return it.Update.State
		}
	}
	t.Fatalf("no service %q in %+v", name, list)
	return ""
}

// localContainerUpdate is the state the containers list of the environment gives the container.
func (f *agentFix) localContainerUpdate(t *testing.T, envID, name string) string {
	t.Helper()
	list := decode[heldList](t, f.req(t, http.MethodGet, "/api/env/"+envID+"/containers", ""))
	for _, it := range list.Items {
		if it.Name == name && it.Update != nil {
			return it.Update.State
		}
	}
	t.Fatalf("no container %q in %+v", name, list)
	return ""
}

// Skipped, ignored and rolled back versions are never taken into a bulk update, nor is one that is up to date, however the client names
// them; the server judges each as it is now (plan 12-30, D-14, D-15, T-12-90). The one that can be updated is, and nothing is written for
// the others.
func TestBulkRefusesUnselectable(t *testing.T) {
	t.Parallel()
	answers := threeAnswers('b')
	answers["team/cache"] = headAnswer{digest: updDigest('b')}
	answers["team/fresh"] = headAnswer{digest: updDigest('b')}
	heads := newScriptedHeads(answers)
	f, e := bulkSwarm(t, heads, nil, plainService("web", 'a'), plainService("api", 'a'), plainService("db", 'a'), plainService("cache", 'a'), plainService("fresh", 'b'))

	// cache's update fails and is put back, so that version is rolled back; api's version is skipped; db is ignored.
	rejectNewVersionOf(f, "cache", 'b')
	rec := f.bulkRun(t, bulkItem(e.ID, "service", "cache", nil))
	if rec.code != http.StatusOK {
		t.Fatalf("setup update: %d %s", rec.code, rec.body)
	}
	if job := f.awaitUpdate(t, decode[bulkAnswer](t, rec).jobOf(t, e.ID)); bulkItemStates(job)["i0"] != "failed" {
		t.Fatalf("setup job = %+v, want the cache update failed", job.Items)
	}
	if rec := f.skip(t, e, http.MethodPost, "api"); rec.code != http.StatusOK {
		t.Fatalf("skip api: %d %s", rec.code, rec.body)
	}
	if rec := f.ignoreReq(t, e, http.MethodPost, `{"scope":"workload","kind":"service","name":"db"}`); rec.code != http.StatusOK {
		t.Fatalf("ignore db: %d %s", rec.code, rec.body)
	}
	before := len(f.fake.ServiceUpdates())

	rec = f.bulkRun(t,
		bulkItem(e.ID, "service", "web", nil), bulkItem(e.ID, "service", "api", nil), bulkItem(e.ID, "service", "db", nil),
		bulkItem(e.ID, "service", "cache", nil), bulkItem(e.ID, "service", "fresh", nil), bulkItem(e.ID, "service", "nobody", nil),
	)
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	want := map[string]string{
		"service/api": "not_selectable", "service/db": "not_selectable", "service/cache": "not_selectable",
		"service/fresh": "not_selectable", "service/nobody": "not_selectable",
	}
	if got := ans.reasons(); len(got) != len(want) || got["service/api"] != want["service/api"] || got["service/db"] != want["service/db"] ||
		got["service/cache"] != want["service/cache"] || got["service/fresh"] != want["service/fresh"] || got["service/nobody"] != want["service/nobody"] {
		t.Errorf("skipped = %v, want %v", got, want)
	}
	if job := f.awaitUpdate(t, ans.jobOf(t, e.ID)); job.State != jobs.JobDone || bulkItemStates(job)["i0"] != "done" {
		t.Errorf("job = %s %+v, want web alone, done", job.State, bulkItemStates(job))
	}
	var written []string
	for _, u := range f.fake.ServiceUpdates()[before:] {
		written = append(written, u.Spec.Name)
	}
	if !slices.Equal(written, []string{"web"}) {
		t.Errorf("services written by the bulk update = %v, want web alone", written)
	}
	if u := f.heldOf(t, e, "cache"); u.State != "rolled_back" {
		t.Errorf("cache reads %s, want rolled_back: a bulk update must not retry it", u.State)
	}
}

// A Viewer who posts a bulk update gets every item answered no_permission and nothing is started, taken or written; an Operator of one
// environment has theirs updated and the other's refused (plan 12-30, D-10, T-12-89).
func TestBulkViewerStartsNothing(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/web": {digest: updDigest('b')}, "team/api": {digest: updDigest('b')}, "team/app": {digest: updDigest('b')},
	})
	f, local, swarm := bulkFleet(t, heads)
	f.addUser(t, "vera", false)
	f.addEnvRole(t, "vera", swarm.ID, "viewer")
	f.addEnvRole(t, "vera", local.ID, "viewer")
	f.addUser(t, "olive", false)
	f.addEnvRole(t, "olive", swarm.ID, "operator")
	f.addEnvRole(t, "olive", local.ID, "viewer")
	f.addUser(t, "nina", false) // no access to either

	body := bulkBody(bulkItem(swarm.ID, "service", "web", nil), bulkItem(local.ID, "container", "app", nil), bulkItem(swarm.ID, "service", "api", nil))
	for _, user := range []string{"vera", "nina"} {
		rec := f.bulkAsUser(t, user, "/api/fleet/updates/run", body)
		if rec.code != http.StatusOK {
			t.Fatalf("%s's bulk update: %d %s", user, rec.code, rec.body)
		}
		ans := decode[bulkAnswer](t, rec)
		if len(ans.Jobs) != 0 || len(ans.Skipped) != 3 {
			t.Fatalf("%s's answer = %+v, want no job and three skipped", user, ans)
		}
		for _, s := range ans.Skipped {
			if s.Reason != "no_permission" {
				t.Errorf("%s: %s/%s = %s, want no_permission", user, s.Kind, s.Name, s.Reason)
			}
		}
	}
	if runs := f.runsOfBulk(t, ""); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
	for _, id := range []string{swarm.ID, local.ID} {
		if h, held := f.stackLocks.Held(id); held {
			t.Errorf("the lock of %s is held by %+v after refusals", id, h)
		}
	}
	if rows := f.auditRows(t, "image.bulk_updated"); len(rows) != 0 {
		t.Errorf("image.bulk_updated rows = %+v, want none", rows)
	}
	// An unnamed caller is refused before anything.
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/fleet/updates/run", body: body}); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("an anonymous bulk update = %d, want 401 or 403", rec.Code)
	}

	rec := f.bulkAsUser(t, "olive", "/api/fleet/updates/run", body)
	if rec.code != http.StatusOK {
		t.Fatalf("olive's bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	if len(ans.Jobs) != 1 || ans.Jobs[0].EnvID != swarm.ID || len(ans.Skipped) != 1 || ans.Skipped[0].Name != "app" || ans.Skipped[0].Reason != "no_permission" {
		t.Fatalf("olive's answer = %+v, want the swarm's job and the container refused no_permission", ans)
	}
	if job := f.awaitUpdate(t, ans.Jobs[0].JobID); job.State != jobs.JobDone {
		t.Errorf("olive's job = %s, want done", job.State)
	}
}

// At most 500 workloads in a request: 501 is 422 before anything is read, 500 is judged, one by one (plan 12-30, T-12-91).
func TestBulkMaxItems(t *testing.T) {
	t.Parallel()
	f, e := bulkSwarm(t, newScriptedHeads(threeAnswers('b')), nil, threeServices()...)
	items := make([]map[string]any, 0, 501)
	for i := range 501 {
		items = append(items, bulkItem(e.ID, "service", "svc-"+strconv.Itoa(i), nil))
	}
	for _, path := range []string{"/api/fleet/updates/run", "/api/fleet/updates/skip"} {
		if rec := f.req(t, http.MethodPost, path, bulkBody(items...)); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("501 items to %s = %d %s, want 422", path, rec.code, rec.body)
		}
		if rec := f.req(t, http.MethodPost, path, `{"items":[]}`); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("no items to %s = %d %s, want 422", path, rec.code, rec.body)
		}
	}
	rec := f.req(t, http.MethodPost, "/api/fleet/updates/run", bulkBody(items[:500]...))
	if rec.code != http.StatusOK {
		t.Fatalf("500 items: %d %s", rec.code, rec.body)
	}
	if ans := decode[bulkAnswer](t, rec); len(ans.Jobs) != 0 || len(ans.Skipped) != 500 {
		t.Errorf("answer: %d jobs, %d skipped, want none and 500 not selectable", len(ans.Jobs), len(ans.Skipped))
	}
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Errorf("services written = %d, want none", n)
	}
}
