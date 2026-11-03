// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Clean now (plan 11-04, JAN-09, JAN-06, D-08, D-14, D-21). The engines are fake daemons behind the
// nodes of a swarm, reached through real agent sessions; the route, the lock, the jobs hub and the
// runner are the real ones, and the clock of the run is the fixture's, so the containers of
// janitorSwarm are two days old.

// nowClock is a stackops.Clock that tells the time a function does. It never fires After: nothing
// in a janitor run waits on the clock.
type nowClock func() time.Time

func (c nowClock) Now() time.Time                       { return c() }
func (c nowClock) After(time.Duration) <-chan time.Time { return nil }

type janitorStarted struct {
	RunID string `json:"runId"`
	JobID string `json:"jobId"`
}

// tick makes the body of a run from the refs of preview items.
func tick(items ...janitorItem) string {
	type ref struct {
		Type   string `json:"type"`
		NodeID string `json:"nodeId"`
		Ref    string `json:"ref"`
	}
	refs := make([]ref, 0, len(items))
	for _, it := range items {
		refs = append(refs, ref{it.Type, it.NodeID, it.Ref})
	}
	raw, err := json.Marshal(map[string]any{"items": refs})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// runJanitor presses Clean now as the fixture's instance admin.
func (s *swarmFix) runJanitor(t *testing.T, body string) (*envRec, janitorStarted) {
	t.Helper()
	rec := s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/janitor/runs", body)
	var got janitorStarted
	if rec.code == http.StatusAccepted {
		got = decode[janitorStarted](t, rec)
	}
	return rec, got
}

// awaitRun waits for the job of a run to end and returns it.
func (s *swarmFix) awaitRun(t *testing.T, id string) jobs.Job {
	t.Helper()
	job, err := s.jobHub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatalf("the job %s never ended: %v", id, err)
	}
	return job
}

// containerDeletes lists the container deletes the worker's daemon served, as it saw them.
func (s *swarmFix) containerDeletes() []string {
	var out []string
	for _, a := range []*agentConn{s.m1, s.m2, s.w} {
		for _, w := range a.fake.Writes() {
			if strings.HasPrefix(w, "DELETE /containers/") {
				out = append(out, w)
			}
		}
	}
	return out
}

func TestJanitorRunRemovesTickedTaskContainer(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	if len(preview.Items) != 3 {
		t.Fatalf("preview items = %+v, want 3", preview.Items)
	}
	rec, started := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusAccepted || started.RunID == "" || started.JobID == "" {
		t.Fatalf("Clean now: %d %s, want 202 with a run and a job", rec.code, rec.body)
	}
	job := s.awaitRun(t, started.JobID)
	if job.Kind != "janitor_run" || job.State != jobs.JobDone || len(job.Items) != 1 || job.Items[0].State != jobs.ItemDone {
		t.Fatalf("job = %s %s items %+v, want a done janitor_run with one done item", job.Kind, job.State, job.Items)
	}
	if got := s.containerDeletes(); !slices.Equal(got, []string{"DELETE /containers/web-x1"}) {
		t.Fatalf("the daemons saw %v, want one delete of web-x1", got)
	}
	// The container is gone, so the next preview no longer lists it.
	_, after := s.previewJanitor(t)
	if len(after.Items) != 2 || after.Items[0].Ref != "web-x2" {
		t.Fatalf("preview after = %+v, want web-x2 and web-x3 left", after.Items)
	}
}

func TestJanitorRunNeedsEnvAdmin(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	path := "/api/env/" + s.env.ID + "/janitor/runs"
	body := tick(preview.Items[0])
	as := func(user, role string) *httptest.ResponseRecorder {
		s.addUser(t, user, false)
		s.addEnvRole(t, user, s.env.ID, role)
		cookie, csrf := s.signIn(t, user, false)
		return s.call(t, authReq{method: http.MethodPost, path: path, body: body, cookie: cookie, csrf: csrf})
	}
	if rec := as("olga", "operator"); rec.Code != http.StatusForbidden {
		t.Fatalf("an Operator: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := as("vera", "viewer"); rec.Code != http.StatusForbidden {
		t.Fatalf("a Viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	if got := s.containerDeletes(); len(got) != 0 {
		t.Fatalf("a refused request reached the daemons: %v", got)
	}
	rec := as("ada", "admin")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("an Env Admin: %d %s, want 202", rec.Code, rec.Body)
	}
	var started janitorStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if job := s.awaitRun(t, started.JobID); job.State != jobs.JobDone {
		t.Fatalf("the Env Admin's job = %s, want done", job.State)
	}
	if rec := s.call(t, authReq{method: http.MethodPost, path: path, body: body}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d, want 401", rec.Code)
	}
}

func TestJanitorRunLocked(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	holder := stackops.Holder{StackID: "stk-1", StackName: "shop", JobID: "job-1", Kind: "deploy", By: "ada", Started: janitorNow}
	release, _, ok := s.stackLocks.TryAcquire(s.env.ID, holder)
	if !ok {
		t.Fatal("the lock was not free")
	}
	rec, _ := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusConflict || rec.ptyp != "operation_locked" {
		t.Fatalf("Clean now under a deploy: %d %s (%s), want 409 operation_locked", rec.code, rec.body, rec.ptyp)
	}
	if !strings.Contains(string(rec.body), "shop") {
		t.Errorf("body = %s, want the stack that holds the lock named", rec.body)
	}
	if got := s.containerDeletes(); len(got) != 0 {
		t.Fatalf("a refused run removed %v", got)
	}
	if held, ok := s.stackLocks.Held(s.env.ID); !ok || held != holder {
		t.Fatalf("held = %+v %v, want the deploy still holding the lock", held, ok)
	}
	// Once the deploy is done the same request is accepted.
	release()
	rec, started := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now after the deploy: %d %s, want 202", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)
	if _, held := s.stackLocks.Held(s.env.ID); held {
		t.Fatal("the lock is still held after the run's job finished")
	}
}

func TestJanitorRunRemovesOnlyTickedAndRemovable(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	if len(preview.Items) != 3 {
		t.Fatalf("preview items = %+v, want web-x1 x2 x3 removable", preview.Items)
	}
	// Tick the first and the last, a container of the two newest (which the janitor keeps), one nobody has,
	// and the first again on a node it is not on.
	newest := janitorItem{Type: "container", NodeID: preview.Items[0].NodeID, Ref: "web-x5"}
	missing := janitorItem{Type: "container", NodeID: preview.Items[0].NodeID, Ref: "web-nobody"}
	elsewhere := janitorItem{Type: "container", NodeID: "not-a-node", Ref: "web-x2"}
	rec, started := s.runJanitor(t, tick(preview.Items[2], newest, preview.Items[0], missing, elsewhere, preview.Items[0]))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	job := s.awaitRun(t, started.JobID)
	got := s.containerDeletes()
	slices.Sort(got)
	if want := []string{"DELETE /containers/web-x1", "DELETE /containers/web-x3"}; !slices.Equal(got, want) {
		t.Fatalf("the daemons saw %v, want %v: the ticked items that are removable, each once", got, want)
	}
	if job.State != jobs.JobDone || len(job.Items) != 2 {
		t.Fatalf("job = %s items %+v, want done with the two removals", job.State, job.Items)
	}
	// web-x2 was never ticked and is still there.
	if _, after := s.previewJanitor(t); len(after.Items) != 1 || after.Items[0].Ref != "web-x2" {
		t.Fatalf("preview after = %+v, want only web-x2 left", after.Items)
	}
}

func TestJanitorRunRejectsMalformedBodies(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	big := make([]string, 0, 5001)
	for range 5001 {
		big = append(big, `{"type":"container","nodeId":"n","ref":"a"}`)
	}
	for name, body := range map[string]string{
		"no items":     `{"items":[]}`,
		"too many":     `{"items":[` + strings.Join(big, ",") + `]}`,
		"bad type":     `{"items":[{"type":"volume","nodeId":"n","ref":"a"}]}`,
		"bad ref":      `{"items":[{"type":"container","nodeId":"n","ref":"../etc"}]}`,
		"empty ref":    `{"items":[{"type":"container","nodeId":"n","ref":""}]}`,
		"long node ID": `{"items":[{"type":"container","nodeId":"` + strings.Repeat("n", 129) + `","ref":"a"}]}`,
	} {
		if rec, _ := s.runJanitor(t, body); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, rec.code, rec.body)
		}
	}
	if got := s.containerDeletes(); len(got) != 0 {
		t.Fatalf("a refused body removed %v", got)
	}
}

func TestJanitorRunNeverForceNorVolumes(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	rec, started := s.runJanitor(t, tick(preview.Items...))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)
	got := s.containerDeletes()
	if len(got) != 3 {
		t.Fatalf("the daemons saw %v, want three deletes", got)
	}
	for _, w := range got {
		// The daemon records the path and the query as it was sent: a delete with force or v asked for
		// would carry them.
		if strings.Contains(w, "?") || strings.Contains(w, "force") || strings.Contains(w, "v=") {
			t.Errorf("%q asked for force or for the volumes", w)
		}
	}
}

// RemoveContainer looks at the container again on its node, whatever the plan said a moment ago.
func TestJanitorEngineRemoveContainerChecksAgain(t *testing.T) {
	t.Parallel()
	boxes := append(exitedSlot(5),
		taskBox{id: "web-live", service: "web", serviceID: "svc-web", slot: "1", state: "running", ageIdx: 9},
		taskBox{id: "web-kept", service: "web", serviceID: "svc-web", slot: "1", state: "exited", ageIdx: 0, finishedAgo: 48 * time.Hour, keep: "true"},
	)
	s := janitorSwarm(t, onWorker(boxes), nil, wantedTask("task-web-live", "web-live"))
	_, preview := s.previewJanitor(t)
	node := preview.Items[0].NodeID
	eng := api.NewJanitorEngine(s.agg, s.clusters, nil, janitor.KeepStore{}, nil)
	ctx := testsupport.Context(t)
	for id, want := range map[string]error{"web-live": janitor.ErrNoLongerStale, "web-kept": janitor.ErrNoLongerStale, "web-nobody": janitor.ErrGone} {
		if err := eng.RemoveContainer(ctx, s.env.ID, node, id); !errors.Is(err, want) {
			t.Errorf("RemoveContainer(%s) = %v, want %v", id, err, want)
		}
	}
	if err := eng.RemoveContainer(ctx, s.env.ID, "not-a-node", "web-x1"); !errors.Is(err, janitor.ErrNodeOffline) {
		t.Errorf("RemoveContainer on an unknown node = %v, want ErrNodeOffline", err)
	}
	for _, w := range s.containerDeletes() {
		if w != "DELETE /containers/web-nobody" {
			t.Errorf("a refused removal reached a daemon: %q", w)
		}
	}
	if err := eng.RemoveContainer(ctx, s.env.ID, node, "web-x1"); err != nil {
		t.Fatalf("RemoveContainer(web-x1) = %v, want it removed", err)
	}
}

// janitorRunRead is the answer of GET /api/env/{envId}/janitor/runs/{runId}, as the tests read it.
type janitorRunRead struct {
	ID         string  `json:"id"`
	Trigger    string  `json:"trigger"`
	Actor      string  `json:"actor"`
	ActorName  string  `json:"actorName"`
	State      string  `json:"state"`
	Reason     string  `json:"reason"`
	StartedAt  string  `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`
	JobID      string  `json:"jobId"`
	EstBytes   *int64  `json:"estBytes"`
	Counts     struct {
		RemovedContainers int `json:"removedContainers"`
		RemovedImages     int `json:"removedImages"`
		RemovedNetworks   int `json:"removedNetworks"`
		Kept              int `json:"kept"`
		Failed            int `json:"failed"`
		SkippedOffline    int `json:"skippedOffline"`
	} `json:"counts"`
	Items []struct {
		Seq     int    `json:"seq"`
		Type    string `json:"type"`
		NodeID  string `json:"nodeId"`
		Node    string `json:"node"`
		Ref     string `json:"ref"`
		Name    string `json:"name"`
		Outcome string `json:"outcome"`
		Code    string `json:"code"`
		// HasEvidence is true when the run saved the crash evidence of the container.
		HasEvidence bool `json:"hasEvidence"`
		Reason      struct {
			Code    string `json:"code"`
			Service string `json:"service"`
			Slot    string `json:"slot"`
			Rank    int    `json:"rank"`
		} `json:"reason"`
	} `json:"items"`
}

// getRun reads a run as the fixture's instance admin.
func (s *swarmFix) getRun(t *testing.T, runID string) (*envRec, janitorRunRead) {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/api/env/"+s.env.ID+"/janitor/runs/"+runID, "")
	var got janitorRunRead
	if rec.code == http.StatusOK {
		got = decode[janitorRunRead](t, rec)
	}
	return rec, got
}

// The tracer of JAN-10: Clean now writes the run and the container it removed, and the run reads
// back through the API once the job reads finished.
func TestJanitorRunRecorded(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	rec, started := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	job := s.awaitRun(t, started.JobID)

	rec, run := s.getRun(t, started.RunID)
	if rec.code != http.StatusOK {
		t.Fatalf("GET the run: %d %s, want 200", rec.code, rec.body)
	}
	if run.ID != started.RunID || run.Trigger != "manual" || run.Actor != "id-admin" || run.ActorName != "admin" ||
		run.State != "completed" || run.Reason != "" || run.JobID != job.ID || run.FinishedAt == nil || run.StartedAt == "" {
		t.Fatalf("run = %+v, want a completed manual run of the admin in job %s", run, job.ID)
	}
	if run.Counts.RemovedContainers != 1 || run.Counts.Failed != 0 || run.Counts.Kept != 0 || run.Counts.SkippedOffline != 0 || run.Counts.RemovedImages != 0 || run.Counts.RemovedNetworks != 0 {
		t.Fatalf("counts = %+v, want one container removed", run.Counts)
	}
	if run.EstBytes != nil {
		t.Errorf("estBytes = %d, want null: the daemon's size was not known", *run.EstBytes)
	}
	if len(run.Items) != 1 {
		t.Fatalf("items = %+v, want the one removal", run.Items)
	}
	it := run.Items[0]
	if it.Seq != 1 || it.Type != "container" || it.Ref != "web-x1" || it.Outcome != "removed" || it.Code != "" || it.Node != "wrk-1" || it.NodeID != preview.Items[0].NodeID ||
		it.Reason.Code != "task_beyond_keep" || it.Reason.Service != "web" || it.Reason.Slot != "1" || it.Reason.Rank != 1 {
		t.Fatalf("item = %+v, want web-x1 removed on wrk-1 as task_beyond_keep rank 1 of web slot 1", it)
	}
}

// runsOnRecord counts the janitor runs on record, in every environment.
func (s *swarmFix) runsOnRecord(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.st.Read.QueryRowContext(testsupport.Context(t), `SELECT COUNT(*) FROM janitor_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// D-08: a Clean now refused because a deploy holds the lock is not a run, so it leaves no row.
func TestJanitorRunLockedWritesNoRun(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	release, _, ok := s.stackLocks.TryAcquire(s.env.ID, stackops.Holder{StackID: "stk-1", StackName: "shop", JobID: "job-1", Kind: "deploy", By: "ada", Started: janitorNow})
	if !ok {
		t.Fatal("the lock was not free")
	}
	rec, _ := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusConflict || rec.ptyp != "operation_locked" {
		t.Fatalf("Clean now under a deploy: %d %s (%s), want 409 operation_locked", rec.code, rec.body, rec.ptyp)
	}
	if n := s.runsOnRecord(t); n != 0 {
		t.Fatalf("%d janitor runs on record after a refused Clean now, want none", n)
	}
	release()
	rec, started := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now after the deploy: %d %s, want 202", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)
	if n := s.runsOnRecord(t); n != 1 {
		t.Fatalf("%d janitor runs on record after one Clean now, want 1", n)
	}
}

// D-15: whoever can read the environment can read its runs.
func TestJanitorRunReadableByViewer(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	s.awaitRun(t, started.JobID)

	s.addUser(t, "vera", false)
	s.addEnvRole(t, "vera", s.env.ID, "viewer")
	cookie, csrf := s.signIn(t, "vera", false)
	path := "/api/env/" + s.env.ID + "/janitor/runs/" + started.RunID
	rec := s.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("a Viewer reads the run: %d %s, want 200", rec.Code, rec.Body)
	}
	var run janitorRunRead
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.ID != started.RunID || run.State != "completed" || len(run.Items) != 1 {
		t.Fatalf("run = %+v, want the completed run with its one item", run)
	}
	// Signed out, and a user with no role on the environment, read nothing.
	if rec := s.call(t, authReq{method: http.MethodGet, path: path}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d, want 401", rec.Code)
	}
	s.addUser(t, "nell", false)
	cookie, csrf = s.signIn(t, "nell", false)
	if rec := s.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf}); rec.Code == http.StatusOK {
		t.Fatalf("a user with no role on the environment read the run: %d %s", rec.Code, rec.Body)
	}
}

// D-15, T-11-80: a run is looked up by its environment and its ID, so another environment's run
// answers the same 404 as one that does not exist.
func TestJanitorRunOtherEnvNotFound(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	s.awaitRun(t, started.JobID)
	other := s.srv.NewEnvironment(t, "Other")

	for name, path := range map[string]string{
		"another environment's run": "/api/env/" + other.ID + "/janitor/runs/" + started.RunID,
		"an unknown run":            "/api/env/" + s.env.ID + "/janitor/runs/no-such-run",
		"an unknown run elsewhere":  "/api/env/" + other.ID + "/janitor/runs/no-such-run",
	} {
		rec := s.req(t, http.MethodGet, path, "")
		if rec.code != http.StatusNotFound || rec.ptyp != "janitor_run_not_found" {
			t.Errorf("%s: %d %s (%s), want 404 janitor_run_not_found", name, rec.code, rec.body, rec.ptyp)
		}
		if strings.Contains(string(rec.body), started.RunID) && !strings.HasSuffix(path, started.RunID) {
			t.Errorf("%s: the body %s names a run of another environment", name, rec.body)
		}
	}
	if rec, run := s.getRun(t, started.RunID); rec.code != http.StatusOK || run.ID != started.RunID {
		t.Fatalf("the run in its own environment: %d %s, want 200", rec.code, rec.body)
	}
}

// refusing makes the daemon of the worker refuse the DELETE of the containers given, as it answers: a
// container that came into use after the preview is 409, one that is gone is 404, an agent whose policy
// refuses is 403.
func refusing(refusals map[string]dockertest.FakeRefusal) func(string, *dockertest.FakeConfig) {
	return func(host string, cfg *dockertest.FakeConfig) {
		if host == "wrk-1" {
			cfg.RefuseRemove = refusals
		}
	}
}

// deletesOf counts the DELETEs of one container the daemons saw.
func (s *swarmFix) deletesOf(id string) int {
	n := 0
	for _, w := range s.containerDeletes() {
		if w == "DELETE /containers/"+id {
			n++
		}
	}
	return n
}

// The tracer of JAN-06 and D-16: a container that something started using between the preview and the
// delete is refused by the engine. That is TierPlan doing its job: the item is kept and recorded as
// such, asked of the engine once, and the rest of the run goes on.
func TestJanitorRunInUseIsKeptOnce(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), refusing(map[string]dockertest.FakeRefusal{
		"web-x1": {Status: http.StatusConflict, Message: "cannot remove container \"web-x1\": container is in use"},
	}))
	_, preview := s.previewJanitor(t)
	rec, started := s.runJanitor(t, tick(preview.Items[0], preview.Items[1]))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	job := s.awaitRun(t, started.JobID)
	if len(job.Items) != 2 {
		t.Fatalf("job items = %+v, want one for each ticked item", job.Items)
	}
	if n := s.deletesOf("web-x1"); n != 1 {
		t.Fatalf("the daemon saw %d deletes of web-x1, want exactly one: a refusal is never retried", n)
	}
	if n := s.deletesOf("web-x2"); n != 1 {
		t.Fatalf("the daemon saw %d deletes of web-x2, want the run to go on to it", n)
	}
	_, run := s.getRun(t, started.RunID)
	if run.State != "completed" || run.Reason != "" || run.Counts.Kept != 1 || run.Counts.RemovedContainers != 1 || run.Counts.Failed != 0 {
		t.Fatalf("run = %+v, want completed with 1 kept and 1 removed: a refusal because it is in use is not a failure", run)
	}
	if len(run.Items) != 2 || run.Items[0].Ref != "web-x1" || run.Items[0].Outcome != "refused_in_use" || run.Items[0].Code != "in_use" ||
		run.Items[1].Ref != "web-x2" || run.Items[1].Outcome != "removed" {
		t.Fatalf("items = %+v, want web-x1 refused_in_use and web-x2 removed", run.Items)
	}
}

// startOn starts the container on the daemon of the node, as someone does at the command line between the preview
// and the run.
func startOn(t *testing.T, a *agentConn, id string) {
	t.Helper()
	cli := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", a.fake.SocketPath())
	}}}
	t.Cleanup(cli.CloseIdleConnections)
	req, err := http.NewRequestWithContext(testsupport.Context(t), http.MethodPost, "http://docker/containers/"+id+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("starting %s on the daemon: %d", id, resp.StatusCode)
	}
}

// runItems summarises a run's items as ref, outcome and code.
func runItems(run janitorRunRead) []string {
	var out []string
	for _, it := range run.Items {
		out = append(out, it.Ref+" "+it.Outcome+" "+it.Code)
	}
	return out
}

// D-14: a ticked container that was started after the preview is not stale any more. The run does not
// ask any engine to remove it, records it as skipped_protected no_longer_stale and goes on.
func TestJanitorRunNoLongerStale(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	startOn(t, s.w, "web-x1")
	rec, started := s.runJanitor(t, tick(preview.Items[0], preview.Items[1]))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)
	if n := s.deletesOf("web-x1"); n != 0 {
		t.Fatalf("the daemon saw %d deletes of web-x1, want none: it runs again", n)
	}
	_, run := s.getRun(t, started.RunID)
	if want := []string{"web-x1 skipped_protected no_longer_stale", "web-x2 removed "}; !slices.Equal(runItems(run), want) {
		t.Fatalf("items = %v, want %v", runItems(run), want)
	}
	if run.Items[0].Name == "" || run.Items[0].Name == "web-x1" {
		t.Errorf("the skipped item's name = %q, want the container's name from the fresh read", run.Items[0].Name)
	}
	if run.State != "completed" || run.Counts.Kept != 1 || run.Counts.RemovedContainers != 1 || run.Counts.Failed != 0 {
		t.Fatalf("run = %+v, want completed with 1 kept and 1 removed", run)
	}
}

// D-16: a container someone else removed in the meantime is not a failure of the run's: the engine says
// it is not there, and the run records it as gone and counts it as neither a removal nor a failure.
func TestJanitorRunGone(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), refusing(map[string]dockertest.FakeRefusal{
		"web-x1": {Status: http.StatusNotFound, Message: "No such container: web-x1"},
	}))
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0], preview.Items[1]))
	s.awaitRun(t, started.JobID)
	_, run := s.getRun(t, started.RunID)
	if want := []string{"web-x1 skipped_protected gone", "web-x2 removed "}; !slices.Equal(runItems(run), want) {
		t.Fatalf("items = %v, want %v", runItems(run), want)
	}
	if run.State != "completed" || run.Counts.RemovedContainers != 1 || run.Counts.Kept != 0 || run.Counts.Failed != 0 {
		t.Fatalf("run = %+v, want completed with 1 removed and nothing kept or failed", run)
	}
	if n := s.deletesOf("web-x1"); n != 1 {
		t.Fatalf("the daemon saw %d deletes of web-x1, want one", n)
	}
}

// D-08: a node that stops reporting between the preview and the run has its items skipped and named; the other
// nodes' items are removed, and the run is partial.
func TestJanitorRunNodeOffline(t *testing.T) {
	t.Parallel()
	var y []taskBox
	for i := range 5 {
		y = append(y, taskBox{id: "web-y" + string(rune('1'+i)), service: "web", serviceID: "svc-web", slot: "2", state: "exited", ageIdx: i, finishedAgo: 48 * time.Hour})
	}
	s := janitorSwarm(t, map[string][]taskBox{"wrk-1": exitedSlot(5), "mgr-1": y}, nil)
	_, preview := s.previewJanitor(t)
	if len(preview.Items) != 6 {
		t.Fatalf("preview items = %+v, want three on each of two nodes", preview.Items)
	}
	s.kickWorker(t)
	rec, started := s.runJanitor(t, tick(preview.Items...))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)
	_, run := s.getRun(t, started.RunID)
	if run.State != "partial" || run.Counts.RemovedContainers != 3 || run.Counts.SkippedOffline != 3 || run.Counts.Failed != 0 {
		t.Fatalf("run = %+v, want partial with 3 removed and 3 skipped offline", run)
	}
	for _, it := range run.Items {
		onWorker := strings.HasPrefix(it.Ref, "web-x")
		if onWorker && (it.Outcome != "skipped_node_offline" || it.Code != "node_offline" || it.Node != "wrk-1") {
			t.Errorf("%s = %+v, want skipped_node_offline on wrk-1", it.Ref, it)
		}
		if !onWorker && it.Outcome != "removed" {
			t.Errorf("%s = %+v, want removed: the other node answered", it.Ref, it)
		}
	}
	if got := s.deletesOf("web-x1") + s.deletesOf("web-x2") + s.deletesOf("web-x3"); got != 0 {
		t.Fatalf("%d deletes reached the node that did not report, want none", got)
	}
}

// janitorAudits are the janitor.cleaned rows of the audit log with the ID of the actor of each.
func (s *swarmFix) janitorAudits(t *testing.T) ([]auditRowFull, []string) {
	t.Helper()
	rows := queryAudits(t, s.st.Read, audit.ActionJanitorCleaned)
	var actors []string
	q, err := s.st.Read.QueryContext(testsupport.Context(t), `SELECT actor_type || ':' || actor_id FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionJanitorCleaned)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	for q.Next() {
		var a string
		if err := q.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actors = append(actors, a)
	}
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	return rows, actors
}

// JAN-06, D-16: a removal that fails is recorded with a code, asked of the engine once and never again, and the run
// goes on to the next item. The run is partial and its audit row says so.
func TestJanitorRunErrorNotRetried(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), refusing(map[string]dockertest.FakeRefusal{
		"web-x1": {Status: http.StatusForbidden, Message: "agent_policy_denied: DELETE /containers/web-x1"},
		"web-x2": {Status: http.StatusInternalServerError, Message: "the engine could not remove web-x2"},
	}))
	_, preview := s.previewJanitor(t)
	rec, started := s.runJanitor(t, tick(preview.Items...))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	job := s.awaitRun(t, started.JobID)
	for _, id := range []string{"web-x1", "web-x2", "web-x3"} {
		if n := s.deletesOf(id); n != 1 {
			t.Errorf("the daemon saw %d deletes of %s, want exactly one: nothing is retried", n, id)
		}
	}
	_, run := s.getRun(t, started.RunID)
	want := []string{"web-x1 error agent_policy_denied", "web-x2 error action_failed", "web-x3 removed "}
	if !slices.Equal(runItems(run), want) {
		t.Fatalf("items = %v, want %v", runItems(run), want)
	}
	if run.State != "partial" || run.Counts.Failed != 2 || run.Counts.RemovedContainers != 1 || run.Counts.Kept != 0 {
		t.Fatalf("run = %+v, want partial with 2 failed and 1 removed", run)
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job = %s, want partial", job.State)
	}
	rows, actors := s.janitorAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want exactly one janitor.cleaned for one run", rows)
	}
	r := rows[0]
	if r.TargetType != "environment" || r.TargetID != s.env.ID || r.Result != "failure" || actors[0] != "user:id-admin" ||
		r.Details["run"] != started.RunID || r.Details["trigger"] != "manual" || r.Details["outcome"] != "partial" ||
		r.Details["failed"] != "2" || r.Details["removed_containers"] != "1" || r.Details["kept"] != "0" {
		t.Fatalf("audit = %+v actor %v, want the admin's janitor.cleaned on the environment: partial, 2 failed, 1 removed", r, actors)
	}
	for k, v := range r.Details {
		if strings.Contains(k+v, "web-x") {
			t.Errorf("audit details %s=%s name an item", k, v)
		}
	}
}

// janitorHold takes the environment's lock as a janitor run does.
func janitorHold(t *testing.T, f *envFix, envID string) (stackops.Holder, func()) {
	t.Helper()
	h := stackops.Holder{Kind: stackops.HolderKindJanitor, JobID: "job-9", RunID: "run-9", By: "ada", Started: time.Date(2026, 1, 1, 8, 30, 0, 0, time.UTC)}
	release, _, ok := f.stackLocks.TryAcquire(envID, h)
	if !ok {
		t.Fatal("the lock was not free")
	}
	return h, release
}

// RESEARCH F9, Pitfall 4: a deploy, a review and an automatic deploy refused because the janitor holds the
// lock are told the janitor has the environment, not an empty stack name, and may try again.
func TestDeployRefusedByJanitorHold(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha := newStack(t, admin, "alpha")
	h, release := janitorHold(t, f, e.ID)
	t.Cleanup(release)

	writes := len(f.fake.Writes())
	rec := operator.deploy(alpha.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Fatalf("a deploy while the janitor cleans: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	pr := problemOf(t, rec)
	if want := "The janitor is cleaning " + e.Name + ", started 2026-01-01T08:30:00Z. Try again when it finishes."; pr.Detail != want {
		t.Errorf("detail = %q, want %q", pr.Detail, want)
	}
	var named []string
	for _, er := range pr.Errors {
		named = append(named, er.Location+"="+er.Message)
	}
	for _, want := range []string{"kind=janitor", "runId=" + h.RunID} {
		if !slices.Contains(named, want) {
			t.Errorf("errors = %v, want %s", named, want)
		}
	}
	if slices.ContainsFunc(named, func(s string) bool { return strings.HasPrefix(s, "stack=") || strings.HasPrefix(s, "stackId=") }) {
		t.Errorf("errors = %v, want no empty stack named", named)
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("writes went from %d to %d, a refused deploy writes nothing", writes, got)
	}

	// The review says it as a blocking problem, in the same words.
	rv, code, raw := operator.reviewStack(alpha.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if p, ok := rv.problem("operation_locked"); !ok || p.Severity != "error" || !strings.HasPrefix(p.Message, "The janitor is cleaning "+e.Name+", started ") {
		t.Errorf("problems = %+v, want a blocking operation_locked that names the janitor", rv.Problems)
	}

	// Once the janitor is done the same deploy goes ahead.
	release()
	rec = operator.deploy(alpha.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a deploy after the janitor finished: %d %s, want 202", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID)
}

// An automatic deploy (git) refused by the janitor's hold is a retryable refusal in the same words.
func TestAutoDeployRefusedByJanitorHold(t *testing.T) {
	t.Parallel()
	r := newDeployerRig(t, "swarm")
	v := r.stack(t, "locked", composeWeb127)
	_, release := janitorHold(t, r.f, r.e.ID)
	t.Cleanup(release)
	ref := refusalOf(t, r.deploy(v, "webhook"))
	if ref.Code != "operation_locked" || !ref.Retry || !strings.HasPrefix(ref.Detail, "The janitor is cleaning "+r.e.Name+", started ") {
		t.Errorf("refusal = %+v, want a retryable operation_locked that names the janitor", ref)
	}
}

// The lock a stack's page shows while the janitor cleans: its kind, no stack, and the run.
func TestLockBodyJanitor(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	alpha := newStack(t, admin, "alpha")
	if lock := lockOf(t, viewer, alpha.ID); lock != nil {
		t.Fatalf("lock = %+v before anything holds the environment, want none", lock)
	}
	h, release := janitorHold(t, f, e.ID)
	t.Cleanup(release)
	rec := viewer.do(http.MethodGet, "stacks/"+alpha.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get stack: %d %s", rec.Code, rec.Body)
	}
	lock := answerOf[struct {
		Lock *struct {
			lockAnswer
			RunID string `json:"runId"`
		} `json:"lock"`
	}](t, rec).Lock
	if lock == nil || lock.Kind != "janitor" || lock.StackID != "" || lock.StackName != "" || lock.RunID != h.RunID ||
		lock.JobID != h.JobID || lock.StartedBy != "ada" || lock.StartedAt != "2026-01-01T08:30:00Z" {
		t.Fatalf("lock = %+v, want the janitor's, with no stack and run %s", lock, h.RunID)
	}
	// Another holder's body carries no run.
	release()
	release2, _, ok := f.stackLocks.TryAcquire(e.ID, stackops.Holder{StackID: alpha.ID, StackName: "alpha", JobID: "j", Kind: "deploy", By: "ada", Started: h.Started})
	if !ok {
		t.Fatal("the lock was not free")
	}
	t.Cleanup(release2)
	if raw := viewer.do(http.MethodGet, "stacks/"+alpha.ID, "").Body.String(); strings.Contains(raw, "runId") {
		t.Errorf("a deploy's lock body names a run: %s", raw)
	}
}
