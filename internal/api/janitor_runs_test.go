// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
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
	eng := api.NewJanitorEngine(s.agg, s.clusters, nil)
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
