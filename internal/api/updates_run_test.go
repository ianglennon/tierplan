// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Updating a service in one click (plan 12-15, IMG-06, D-05, D-07, D-10, D-13). Docker is a fake daemon behind the real monitor,
// aggregator and cluster reader; the registry is a scripted Heads; the applier, its run store, the job hub, the operation lock,
// the router and the permission checks are the real ones.

// plainSpec is the spec of a service the tests update: replicas, environment, labels and an update configuration the update
// must keep, running digest a of registry.test/team/<name>:1.
func plainSpec(name string, running byte) swarm.ServiceSpec {
	replicas := uint64(3)
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{"team": "core", "tier": "web"}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image: fmt.Sprintf("registry.test/team/%s:1@%s", name, updDigest(running)), Env: []string{"MODE=prod", "WORKERS=4"},
		}},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig: &swarm.UpdateConfig{Parallelism: 2, Order: swarm.UpdateOrderStartFirst, FailureAction: swarm.UpdateFailureActionRollback},
	}
}

// plainService is a service made of plainSpec.
func plainService(name string, running byte) dockertest.FakeService {
	spec := plainSpec(name, running)
	return dockertest.FakeService{
		ID: "svc-" + name, Name: name, Mode: "replicated", Running: 3, Desired: 3, Version: 1, Image: spec.TaskTemplate.ContainerSpec.Image,
		Labels: spec.Annotations.Labels, Spec: &spec,
	}
}

// updateEnvOf is an environment of one swarm manager whose tasks follow the writes made to it.
func updateEnvOf(t *testing.T, heads *scriptedHeads, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(services...)
	cfg.TasksFollowWrites = true
	cfg.SwarmNodeID = "node-1"
	if tune != nil {
		tune(&cfg)
	}
	return updatesEnvOf(t, heads, cfg)
}

type updateRunAnswer struct {
	JobID string `json:"jobId"`
}

func updateRunBody(name string, expect map[string]string) string {
	body := map[string]any{"kind": "service", "name": name}
	if expect != nil {
		body["expect"] = expect
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// runUpdate asks for the update of a service.
func (f *envFix) runUpdate(t *testing.T, e envBody, name string, expect map[string]string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/updates/run", updateRunBody(name, expect))
}

// awaitUpdate waits for the update's job to end and answers it as the hub holds it.
func (f *envFix) awaitUpdate(t *testing.T, jobID string) jobs.Job {
	t.Helper()
	job, err := f.jobHub.Wait(testsupport.Context(t), jobID)
	if err != nil {
		t.Fatalf("waiting for the update job: %v", err)
	}
	return job
}

// runOf is the run row the update's job recorded.
func (f *envFix) runOf(t *testing.T, job jobs.Job) updates.Run {
	t.Helper()
	run, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Get(context.Background(), job.Meta[updates.MetaRunID])
	if err != nil {
		t.Fatalf("read the run of job %s: %v", job.ID, err)
	}
	return run
}

// stepStates are the states of a job's steps by ID.
func stepStates(job jobs.Job) map[string]string {
	out := map[string]string{}
	for _, it := range job.Items {
		out[it.ID] = it.State
	}
	return out
}

// jsonOf encodes a spec the way the fake decodes it, to compare two of them.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The tracer (plan 12-15, IMG-06): an Operator updates a plain service to the digest the registry has, the job reports it done,
// the run is on record and one audit row says who updated what.
func TestPlainServiceUpdateEndToEnd(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'))
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	rec := f.runUpdate(t, e, "web", map[string]string{"web": updDigest('b')})
	if rec.code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.code, rec.body)
	}
	job := f.awaitUpdate(t, decode[updateRunAnswer](t, rec).JobID)
	if job.State != jobs.JobDone || job.Kind != updates.JobKind || job.EnvID != e.ID {
		t.Fatalf("job = %s %s %s, want a done update job of this environment: %+v", job.State, job.Kind, job.EnvID, job.Items)
	}

	sent := f.fake.ServiceUpdates()
	if len(sent) != 1 {
		t.Fatalf("service updates = %d, want 1", len(sent))
	}
	want := plainSpec("web", 'a')
	want.TaskTemplate.ContainerSpec.Image = "registry.test/team/web:1@" + updDigest('b')
	if got := sent[0].Spec; jsonOf(t, got) != jsonOf(t, want) {
		t.Errorf("the spec sent = %s\nwant the old spec with only the image changed: %s", jsonOf(t, got), jsonOf(t, want))
	}
	if sent[0].Version != "1" {
		t.Errorf("the update carried version %q, want the version it read, 1", sent[0].Version)
	}

	run := f.runOf(t, job)
	if run.State != updates.RunDone || run.Kind != "service" || run.Workload != "web" || run.EnvID != e.ID || run.JobID != job.ID ||
		len(run.Items) != 1 || run.Items[0].FromDigest != updDigest('a') || run.Items[0].ToDigest != updDigest('b') || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v, want a done run of web from a to b, on this environment and job", run)
	}
	if s := stepStates(job); !reflect.DeepEqual(s, map[string]string{"check": "done", "pull": "done", "update": "done", "wait": "done"}) {
		t.Errorf("steps = %v, want every step done", s)
	}
	rows := f.auditRows(t, "image.updated")
	if len(rows) != 1 || rows[0].EnvID != e.ID || rows[0].Result != "success" || rows[0].Details["workload"] != "web" || rows[0].Details["kind"] != "service" ||
		rows[0].Details["from_digest"] != updDigest('a') || rows[0].Details["to_digest"] != updDigest('b') || rows[0].Details["image"] != "registry.test/team/web:1" {
		t.Errorf("audit rows = %+v, want one naming the workload, the image and both digests", rows)
	}
	if rows := f.auditRows(t, "image.update_failed"); len(rows) != 0 {
		t.Errorf("failure rows = %+v, want none", rows)
	}
}

// updateProblem is the part of a problem the update tests read: its type, its detail and its errors with their values.
type updateProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string          `json:"location"`
		Message  string          `json:"message"`
		Value    json.RawMessage `json:"value"`
	} `json:"errors"`
}

// noRunsNoWrites asserts an update that was refused left no trace: no write to the engine, no lock held, no run on record and no
// audit row of an update.
func (f *envFix) noRunsNoWrites(t *testing.T, e envBody) {
	t.Helper()
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d after a refusal, want none", n)
	}
	if h, held := f.stackLocks.Held(e.ID); held {
		t.Errorf("the lock is held by %+v after a refusal", h)
	}
	if runs, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Running(context.Background(), e.ID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v after a refusal, want none", runs, err)
	}
	for _, a := range []string{"image.updated", "image.update_failed"} {
		if rows := f.auditRows(t, a); len(rows) != 0 {
			t.Errorf("%s audit rows = %+v after a refusal, want none", a, rows)
		}
	}
}

// A digest that moved since the confirm is refused with the refreshed one, before a lock, a run or a write (plan 12-15, D-05).
func TestUpdateRefusesMovedDigest(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('c')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'))

	rec := f.runUpdate(t, e, "web", map[string]string{"web": updDigest('b')})
	if rec.code != http.StatusConflict || rec.ptyp != "update_moved" {
		t.Fatalf("update = %d %s %s, want 409 update_moved", rec.code, rec.ptyp, rec.body)
	}
	p := decode[updateProblem](t, rec)
	if len(p.Errors) != 1 || p.Errors[0].Message != "web" {
		t.Fatalf("errors = %+v, want the one service that moved", p.Errors)
	}
	var item struct {
		Service, FromDigest, ToDigest string
	}
	if err := json.Unmarshal(p.Errors[0].Value, &item); err != nil || item.Service != "web" || item.ToDigest != updDigest('c') || item.FromDigest != updDigest('a') {
		t.Errorf("item = %+v %v, want web moved from a to c", item, err)
	}
	f.noRunsNoWrites(t, e)

	// Confirmed again at the refreshed digest, the update goes through and deploys that one.
	rec = f.runUpdate(t, e, "web", map[string]string{"web": updDigest('c')})
	if rec.code != http.StatusOK {
		t.Fatalf("update after the refresh: %d %s", rec.code, rec.body)
	}
	if job := f.awaitUpdate(t, decode[updateRunAnswer](t, rec).JobID); job.State != jobs.JobDone {
		t.Fatalf("job = %s, want done", job.State)
	}
	if got := f.fake.ServiceUpdates()[0].Spec.TaskTemplate.ContainerSpec.Image; got != "registry.test/team/web:1@"+updDigest('c') {
		t.Errorf("image deployed = %q, want the digest the server read", got)
	}
}

// An update asked for while another operation holds the environment is refused and says who holds it (plan 12-15, IMG-06).
func TestUpdateRefusesHeldLock(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'))
	_, _, ok := f.stackLocks.TryAcquire(e.ID, stackops.Holder{StackID: "stk-1", StackName: "shop", Kind: "deploy", By: "ada", Started: time.Now()})
	if !ok {
		t.Fatal("could not take the lock for a stack deploy")
	}

	rec := f.runUpdate(t, e, "web", map[string]string{"web": updDigest('b')})
	if rec.code != http.StatusConflict || rec.ptyp != "operation_locked" {
		t.Fatalf("update = %d %s %s, want 409 operation_locked", rec.code, rec.ptyp, rec.body)
	}
	if p := decode[updateProblem](t, rec); !strings.Contains(p.Detail, "shop") || len(p.Errors) == 0 || p.Errors[0].Location != "stack" || p.Errors[0].Message != "shop" {
		t.Errorf("problem = %+v, want it to name the stack that holds the lock", p)
	}
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want none", n)
	}
	if runs, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Running(context.Background(), e.ID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v, want none", runs, err)
	}
	if h, held := f.stackLocks.Held(e.ID); !held || h.Kind != "deploy" {
		t.Errorf("holder = %+v %v, want the deploy still holding it", h, held)
	}
}

// A second update of a workload that is being updated is refused as locked, naming the running update; it is not read as
// already up to date (plan 12-15, IMG-06 idempotency).
func TestUpdateTwiceIsRefused(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f, e := updateEnvOf(t, heads, func(c *dockertest.FakeConfig) { c.PullHold = hold }, plainService("web", 'a'))

	first := f.runUpdate(t, e, "web", map[string]string{"web": updDigest('b')})
	if first.code != http.StatusOK {
		t.Fatalf("first update: %d %s", first.code, first.body)
	}
	jobID := decode[updateRunAnswer](t, first).JobID
	// The first update holds the lock from the moment it was answered, while its pull is held.
	second := f.runUpdate(t, e, "web", map[string]string{"web": updDigest('b')})
	if second.code != http.StatusConflict || second.ptyp != "operation_locked" {
		t.Fatalf("second update = %d %s %s, want 409 operation_locked", second.code, second.ptyp, second.body)
	}
	p := decode[updateProblem](t, second)
	if !strings.Contains(p.Detail, "image update of web") {
		t.Errorf("detail = %q, want it to name the running update of web", p.Detail)
	}
	got := map[string]string{}
	for _, d := range p.Errors {
		got[d.Location] = d.Message
	}
	if got["kind"] != "image_update" || got["workload"] != "web" || got["job"] != jobID || got["started"] == "" {
		t.Errorf("errors = %v, want the kind, the workload, the job and when it started", got)
	}
	close(hold)
	if job := f.awaitUpdate(t, jobID); job.State != jobs.JobDone {
		t.Fatalf("first job = %s %+v, want done", job.State, job.Items)
	}
	if n := len(f.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want only the first update's", n)
	}
	if _, held := f.stackLocks.Held(e.ID); held {
		t.Errorf("the lock is held after the job read as done")
	}
}

// A workload that runs the digest the registry has is not updated (plan 12-15, IMG-06 idempotency).
func TestNothingToUpdate(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('a')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'))

	rec := f.runUpdate(t, e, "web", map[string]string{"web": updDigest('a')})
	if rec.code != http.StatusConflict || rec.ptyp != "nothing_to_update" {
		t.Fatalf("update = %d %s %s, want 409 nothing_to_update", rec.code, rec.ptyp, rec.body)
	}
	f.noRunsNoWrites(t, e)
	if rec := f.runUpdate(t, e, "nobody", nil); rec.code != http.StatusNotFound {
		t.Errorf("update of a service that is not there = %d %s, want 404", rec.code, rec.body)
	}
}

// An Operator may update a service; a Viewer may not, and nothing is taken or written for the refusal (plan 12-15, D-10, T-12-38).
func TestUpdateRoutePermissions(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'))
	f.addUser(t, "vera", false)
	f.addEnvRole(t, "vera", e.ID, "viewer")
	f.addUser(t, "olive", false)
	f.addEnvRole(t, "olive", e.ID, "operator")

	path := "/api/env/" + e.ID + "/updates/run"
	body := updateRunBody("web", map[string]string{"web": updDigest('b')})
	cookie, csrf := f.signIn(t, "vera", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: path, body: body, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's update = %d %s, want 403", rec.Code, rec.Body)
	}
	f.noRunsNoWrites(t, e)

	cookie, csrf = f.signIn(t, "olive", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: path, body: body, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("an operator's update = %d %s, want 200", rec.Code, rec.Body)
	}
	var ans updateRunAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil {
		t.Fatal(err)
	}
	job := f.awaitUpdate(t, ans.JobID)
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s, want done", job.State)
	}

	// Anyone who may read the environment follows the job; another environment's job is not found.
	get := func(user, id string) int {
		c, s := f.signIn(t, user, false)
		return f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/update-jobs/" + id, cookie: c, csrf: s}).Code
	}
	if got := get("vera", ans.JobID); got != http.StatusOK {
		t.Errorf("a viewer reading the job = %d, want 200", got)
	}
	if got := get("vera", "no-such-job"); got != http.StatusNotFound {
		t.Errorf("reading a job that is not there = %d, want 404", got)
	}
}

// The update of a plain container (plan 12-20, IMG-06, D-07, D-12, D-13, D-21, RESEARCH Pitfall 11). Docker is a fake standalone host
// behind the real monitor, aggregator and cluster reader; the registry is a scripted Heads; the applier, its run store, the job hub,
// the operation lock, the router and the permission checks are the real ones. The applier waits on a clock that answers at once,
// so the wait for a container to run costs no time.

const (
	appRef = "registry.test/team/app:1"
	appID  = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// instantClock answers every wait at once and moves its time on by what was asked, so a window is reached without sleeping.
type instantClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *instantClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *instantClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// plainContainer is a container somebody made with docker run: an environment, a command, a label, a restart policy, a network
// with an alias and the old container's short ID as another, and a volume.
func plainContainer() dockertest.FakeContainer {
	doc := container.InspectResponse{
		Config: &container.Config{
			Image: appRef, Hostname: appID[:12], Env: []string{"MODE=prod"}, Cmd: []string{"serve"}, Labels: map[string]string{"team": "core"},
			Volumes: map[string]struct{}{"/cache": {}},
		},
		HostConfig: &container.HostConfig{NetworkMode: "appnet", RestartPolicy: container.RestartPolicy{Name: "always"}, Binds: []string{"appdata:/data"}},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "appdata", Destination: "/data", RW: true},
			{Type: mount.TypeVolume, Name: "anon0123", Destination: "/cache", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"appnet": {NetworkID: "net-app", Aliases: []string{"app", "web", appID[:12]}},
		}},
	}
	return dockertest.FakeContainer{
		ID: appID, Names: []string{"/app"}, Image: appRef, ImageID: updImage('1'), State: "running", Status: "Up 1 hour",
		Labels: map[string]string{"team": "core"}, Mounts: []string{"appdata", "anon0123"},
		Networks: map[string]dockertest.FakeEndpoint{"appnet": {NetworkID: "net-app", Aliases: []string{"app", "web"}}},
		Inspect:  dockertest.FakeInspect{Document: &doc, ImageID: updImage('1')},
	}
}

// containerEngine is a standalone host running containers, with the image of appRef at digest a and the network appnet.
func containerEngine(containers ...dockertest.FakeContainer) dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "host-1", Version: "29.1.0", Containers: containers,
		Images:   []dockertest.FakeImage{{ID: updImage('1'), RepoTags: []string{appRef}, RepoDigests: []string{"registry.test/team/app@" + updDigest('a')}}},
		Networks: []dockertest.FakeNetwork{{ID: "net-app", Name: "appnet", Driver: "bridge", Scope: "local"}},
	}
}

// containerEnvOf is a standalone host the caller configured, whose applier waits on an instant clock.
func containerEnvOf(t *testing.T, heads *scriptedHeads, cfg dockertest.FakeConfig) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.updateApplier = func(a *updates.Applier) { a.Clock = &instantClock{now: time.Unix(1_700_000_000, 0)} }
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

func containerRunBody(name string, expect map[string]string) string {
	body := map[string]any{"kind": "container", "name": name}
	if expect != nil {
		body["expect"] = expect
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// writeKinds names the writes the engine served in a word each, leaving out the ones a test does not care about.
func writeKinds(writes []string) []string {
	var out []string
	for _, w := range writes {
		method, path, _ := strings.Cut(w, " ")
		path, _, _ = strings.Cut(path, "?")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case path == "/images/create":
			out = append(out, "pull")
		case path == "/containers/create":
			out = append(out, "create")
		case method == http.MethodDelete && len(parts) == 2 && parts[0] == "containers":
			out = append(out, "remove")
		case len(parts) == 3 && parts[0] == "containers":
			out = append(out, parts[2])
		}
	}
	return out
}

// An Operator updates a plain docker run container in one click: the replacement is created beside it, the old one is stopped and
// renamed, the replacement takes its name and starts, and only then is the old one removed, without its volumes (D-07, D-13).
func TestPlainContainerUpdateEndToEnd(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/app": {digest: updDigest('b')}})
	f, e := containerEnvOf(t, heads, containerEngine(plainContainer()))
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/updates/run", containerRunBody("app", map[string]string{"app": updDigest('b')}))
	if rec.code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.code, rec.body)
	}
	job := f.awaitUpdate(t, decode[updateRunAnswer](t, rec).JobID)
	if job.State != jobs.JobDone || job.Kind != updates.JobKind || job.EnvID != e.ID {
		t.Fatalf("job = %s %s %s, want a done update job of this environment: %+v", job.State, job.Kind, job.EnvID, job.Items)
	}

	if got, want := writeKinds(f.fake.Writes()), []string{"pull", "create", "stop", "rename", "rename", "start", "remove"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the engine was asked %v, want %v: the replacement before anything stops, the old container last", got, want)
	}
	creates := f.fake.ContainerCreates()
	if len(creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(creates))
	}
	c := creates[0]
	if c.Name != "app-tierplan-new-"+appID[:12] || c.Config.Image != appRef+"@"+updDigest('b') {
		t.Errorf("created %q from %q, want a temporary name and the image pinned to the new digest", c.Name, c.Config.Image)
	}
	if !reflect.DeepEqual(c.Config.Env, []string{"MODE=prod"}) || !reflect.DeepEqual(c.Config.Cmd, []string{"serve"}) || c.Config.Labels["team"] != "core" ||
		c.HostConfig.RestartPolicy.Name != "always" || c.Config.Hostname != "" {
		t.Errorf("replacement config = %+v host %+v, want the environment, command, label and restart policy kept and the hostname reset", c.Config, c.HostConfig)
	}
	ep := c.NetworkingConfig.EndpointsConfig["appnet"]
	if ep == nil || !reflect.DeepEqual(ep.Aliases, []string{"app", "web"}) {
		t.Errorf("replacement endpoint = %+v, want appnet with its aliases but not the old short ID", ep)
	}
	if !reflect.DeepEqual(c.HostConfig.Binds, []string{"appdata:/data"}) ||
		!reflect.DeepEqual(c.HostConfig.Mounts, []mount.Mount{{Type: mount.TypeVolume, Source: "anon0123", Target: "/cache"}}) || len(c.Config.Volumes) != 0 {
		t.Errorf("volumes: binds %v mounts %+v config volumes %v, want the named volume kept and the anonymous one attached again by name, not asked for as a new one", c.HostConfig.Binds, c.HostConfig.Mounts, c.Config.Volumes)
	}
	if got, want := f.fake.ContainerRenames(), []dockertest.FakeRename{
		{ID: appID, From: "app", To: "app-tierplan-old-" + appID[:12]}, {ID: c.ID, From: c.Name, To: "app"},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("renames = %+v, want %+v", got, want)
	}
	for _, w := range f.fake.Writes() {
		if strings.HasPrefix(w, "DELETE ") && strings.Contains(w, "v=") {
			t.Errorf("the old container was removed with %q, want its volumes left", w)
		}
	}

	run := f.runOf(t, job)
	if run.State != updates.RunDone || run.Kind != "container" || run.Workload != "app" || run.WorkloadID != appID || run.OldContainer != appID ||
		len(run.Items) != 1 || run.Items[0].FromDigest != updDigest('a') || run.Items[0].ToDigest != updDigest('b') || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v, want a done run of app from a to b that recorded the old container", run)
	}
	if s := stepStates(job); !reflect.DeepEqual(s, map[string]string{"check": "done", "pull": "done", "create": "done", "swap": "done", "start": "done", "remove_old": "done"}) {
		t.Errorf("steps = %v, want every step done", s)
	}
	rows := f.auditRows(t, "image.updated")
	if len(rows) != 1 || rows[0].TargetType != "container" || rows[0].Result != "success" || rows[0].Details["workload"] != "app" || rows[0].Details["kind"] != "container" ||
		rows[0].Details["from_digest"] != updDigest('a') || rows[0].Details["to_digest"] != updDigest('b') || rows[0].Details["image"] != appRef {
		t.Errorf("audit rows = %+v, want one naming the container, the image and both digests", rows)
	}
	if h, held := f.stackLocks.Held(e.ID); held {
		t.Errorf("the lock is held by %+v after the update", h)
	}
}
