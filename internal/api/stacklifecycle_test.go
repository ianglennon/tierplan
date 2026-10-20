// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
)

// Stop and Start of a stack (plan 07-26, STK-11, D-19): a job on the operation lock, audited, that parks
// what runs and brings back the last deployed version.

func stopPath(stackID string) string  { return "stacks/" + stackID + "/stop" }
func startPath(stackID string) string { return "stacks/" + stackID + "/start" }

const composeWebAndApi = "services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      replicas: 3\n  api:\n    image: nginx:1.28-alpine\n"

type lifecycleStarted struct {
	JobID     string `json:"jobId"`
	Recreates bool   `json:"recreates"`
}

// lifecycle posts a stop or a start, expects 202 and waits for the job.
func (f *envFix) lifecycle(t *testing.T, c *roleCaller, path string) (lifecycleStarted, jobAnswer) {
	t.Helper()
	rec := c.do(http.MethodPost, path, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body)
	}
	started := answerOf[lifecycleStarted](t, rec)
	return started, f.awaitDeploy(t, started.JobID)
}

func TestStopAndStartAStackKeepEveryServicesDigestAndReplicaCount(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "pause")
	f.deployGood(t, operator, st.ID, composeWebAndApi)
	updates := len(f.fake.ServiceUpdates())

	started, job := f.lifecycle(t, operator, stopPath(st.ID))

	if started.Recreates || job.State != "done" {
		t.Fatalf("stop = %+v %+v, want a job that is done and recreates nothing", started, job)
	}
	stopped := readStackAnswer(t, operator, st.ID)
	if stopped.State != "stopped" || !stopped.Stopped {
		t.Errorf("stack after a stop = %+v, want it Stopped", stopped)
	}
	ups := f.fake.ServiceUpdates()[updates:]
	if len(ups) != 2 {
		t.Fatalf("a stop wrote %d updates, want one for each of the two services", len(ups))
	}
	for _, u := range ups {
		rep := u.Spec.Mode.Replicated
		if rep == nil || *rep.Replicas != 0 {
			t.Errorf("stop update for %s: mode %+v, want 0 replicas", u.Spec.Name, u.Spec.Mode)
		}
		want := map[string]string{"pause_web": "3", "pause_api": "1"}[u.Spec.Name]
		if got := u.Spec.Labels["bayplan.stack.stopped-replicas"]; got != want {
			t.Errorf("%s remembers %q, want %q", u.Spec.Name, got, want)
		}
	}
	images := map[string]string{}
	for _, u := range ups {
		images[u.Spec.Name] = u.Spec.TaskTemplate.ContainerSpec.Image
		if !strings.Contains(images[u.Spec.Name], "@sha256:") {
			t.Errorf("%s image %q lost its digest", u.Spec.Name, images[u.Spec.Name])
		}
	}

	started, job = f.lifecycle(t, operator, startPath(st.ID))

	if started.Recreates || job.State != "done" {
		t.Fatalf("start = %+v %+v, want a job that is done and recreates nothing", started, job)
	}
	back := f.fake.ServiceUpdates()[updates+2:]
	if len(back) != 2 {
		t.Fatalf("a start wrote %d updates, want two", len(back))
	}
	for _, u := range back {
		want := map[string]uint64{"pause_web": 3, "pause_api": 1}[u.Spec.Name]
		rep := u.Spec.Mode.Replicated
		if rep == nil || *rep.Replicas != want {
			t.Errorf("start update for %s: mode %+v, want %d replicas", u.Spec.Name, u.Spec.Mode, want)
		}
		if _, left := u.Spec.Labels["bayplan.stack.stopped-replicas"]; left {
			t.Errorf("%s still carries the stopped-replicas label after a start", u.Spec.Name)
		}
		if got := u.Spec.TaskTemplate.ContainerSpec.Image; got != images[u.Spec.Name] {
			t.Errorf("%s image %q after a start, want the digest it had, %q", u.Spec.Name, got, images[u.Spec.Name])
		}
	}
	running := readStackAnswer(t, operator, st.ID)
	if running.State != "running" || running.Stopped {
		t.Errorf("stack after a start = %+v, want it Running", running)
	}

	var stoppedRow, startedRow *auditRowFull
	for _, r := range f.stackAudits(t) {
		switch r.Action {
		case "stack.stopped":
			stoppedRow = &r
		case "stack.started":
			startedRow = &r
		}
	}
	for name, row := range map[string]*auditRowFull{"stack.stopped": stoppedRow, "stack.started": startedRow} {
		if row == nil || row.Result != "success" || row.TargetID != st.ID || row.Details["stack"] != "pause" || row.Details["services"] != "2" || row.Details["recreated"] != "0" || row.Details["result"] != "done" {
			t.Errorf("%s = %+v, want a successful row naming 2 services and none recreated", name, row)
		}
	}
}

func TestStopAndStartNeedOperatorAndChangeNothingForAViewer(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "guarded")
	f.deployGood(t, operator, st.ID, composeWeb127)
	writes := len(f.fake.Writes())

	for _, path := range []string{stopPath(st.ID), startPath(st.ID)} {
		if rec := viewer.do(http.MethodPost, path, ""); rec.Code != http.StatusForbidden {
			t.Errorf("a viewer's POST %s: %d %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("writes went from %d to %d, a refused stop or start writes nothing", writes, got)
	}
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.stopped" || r.Action == "stack.started" {
			t.Errorf("audit row %+v for a refused request", r)
		}
	}
}

func TestStopOfAStackThatIsStoppedOrWasNeverDeployedIsNothingRunning(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	never := newStack(t, admin, "never")
	st := newStack(t, admin, "twice")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.lifecycle(t, operator, stopPath(st.ID))
	writes := len(f.fake.Writes())

	for name, id := range map[string]string{"a stack never deployed": never.ID, "a stack already stopped": st.ID} {
		rec := operator.do(http.MethodPost, stopPath(id), "")
		if rec.Code != http.StatusConflict || problemType(t, rec) != "nothing_running" {
			t.Errorf("stop of %s: %d %s, want 409 nothing_running", name, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "Nothing is running.") {
			t.Errorf("stop of %s: %s, want the sentence Nothing is running.", name, rec.Body)
		}
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("a refused stop wrote %d times", got-writes)
	}
}

func TestStartOfAStackThatIsNotStoppedIsRefused(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "running")
	f.deployGood(t, operator, st.ID, composeWeb127)

	rec := operator.do(http.MethodPost, startPath(st.ID), "")

	if rec.Code != http.StatusConflict || problemType(t, rec) != "not_stopped" {
		t.Errorf("start of a running stack: %d %s, want 409 not_stopped", rec.Code, rec.Body)
	}
}

func TestStopTakesTheOperationLockAndIsRefusedWhileAnotherOperationRuns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha, beta := newStack(t, admin, "alpha"), newStack(t, admin, "beta")
	f.deployGood(t, operator, beta.ID, composeWeb127)
	jobID := startStuckDeploy(t, f, operator, alpha)

	rec := operator.do(http.MethodPost, stopPath(beta.ID), "")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Fatalf("stop while a deploy runs: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	if lock := lockOf(t, operator, beta.ID); lock == nil || lock.StackName != "alpha" || lock.Kind != "deploy" {
		t.Errorf("lock = %+v, want alpha's deploy", lock)
	}

	if rec := operator.do(http.MethodPost, cancelPath(alpha.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, jobID)
	// The lock is the stop's own while it runs, and free after: another stop of the same stack is not locked out.
	if _, job := f.lifecycle(t, operator, stopPath(beta.ID)); job.State != "done" {
		t.Fatalf("stop after the lock was freed = %+v", job)
	}
	if lock := lockOf(t, operator, beta.ID); lock != nil {
		t.Errorf("lock = %+v after the stop ended, want none", lock)
	}
}

func TestStopAndStartAreStackJobsOfTheJobsList(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "listed")
	f.deployGood(t, operator, st.ID, composeWeb127)
	stop, _ := f.lifecycle(t, operator, stopPath(st.ID))
	start, _ := f.lifecycle(t, operator, startPath(st.ID))

	list := answerOf[struct {
		Jobs []struct {
			ID, Kind, State string
		} `json:"jobs"`
	}](t, f.call(t, authReq{method: http.MethodGet, path: "/api/stack-jobs", cookie: operator.cookie, csrf: operator.csrf}))
	kinds := map[string]string{}
	for _, j := range list.Jobs {
		kinds[j.ID] = j.Kind + ":" + j.State
	}
	if kinds[stop.JobID] != "stop:done" || kinds[start.JobID] != "start:done" {
		t.Errorf("jobs = %v, want the stop and the start listed as done", kinds)
	}
	job := answerOf[struct {
		Mode, State string
	}](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+stop.JobID, ""))
	if job.Mode != "stop" || job.State != "done" {
		t.Errorf("job = %+v, want mode stop, readable at the console route", job)
	}
}

func TestADeployBringsAStoppedStackBackAndClearsItsStoppedRecord(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "redeploy")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.lifecycle(t, operator, stopPath(st.ID))
	if got := readStackAnswer(t, operator, st.ID); !got.Stopped {
		t.Fatalf("stack = %+v, want it stopped", got)
	}

	f.deployGood(t, operator, st.ID, composeWeb128)

	if got := readStackAnswer(t, operator, st.ID); got.Stopped || got.State != "running" {
		t.Errorf("stack after a deploy = %+v, want it running and no longer recorded as stopped", got)
	}
}

func TestAStopAfterAFailedDeployDoesNotHideTheFailure(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "failed-then-stopped")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployThenCancel(t, operator, st, composeWeb128)
	if got := readStackAnswer(t, operator, st.ID); got.State != "deploy_failed" {
		t.Fatalf("state = %q, want deploy_failed", got.State)
	}

	f.lifecycle(t, operator, stopPath(st.ID))
	f.lifecycle(t, operator, startPath(st.ID))

	if got := listedStack(t, operator, st.ID); got.State != "deploy_failed" {
		t.Errorf("state after a stop and a start = %q, want deploy_failed kept until a deploy succeeds", got.State)
	}
}

func TestStartRecreatesAGlobalServiceAndTheAnswerSaysSo(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "withglobal")
	f.deployGood(t, operator, st.ID, "services:\n  web:\n    image: nginx:1.27-alpine\n  agent:\n    image: nginx:1.28-alpine\n    deploy:\n      mode: global\n")
	creates := len(f.fake.ServiceCreates())

	f.lifecycle(t, operator, stopPath(st.ID))
	started, job := f.lifecycle(t, operator, startPath(st.ID))

	if !started.Recreates || job.State != "done" {
		t.Fatalf("start = %+v %+v, want a job that recreates and is done", started, job)
	}
	made := f.fake.ServiceCreates()[creates:]
	if len(made) != 1 || made[0].Spec.Name != "withglobal_agent" || !strings.Contains(made[0].Spec.TaskTemplate.ContainerSpec.Image, "@sha256:") {
		t.Errorf("created again = %+v, want withglobal_agent with its pinned image", made)
	}
	for _, it := range job.Items {
		if it.State != jobs.ItemDone {
			t.Errorf("item %s is %s, want done", it.ID, it.State)
		}
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.started" {
			row = &r
		}
	}
	if row == nil || row.Details["recreated"] != "1" {
		t.Errorf("stack.started = %+v, want one service recreated", row)
	}
}

// hostCalls records the stop and the start the Compose SDK's stand-in was asked for.
type hostCalls struct {
	mu    sync.Mutex
	calls []string
	err   error
	last  *types.Project
}

func (h *hostCalls) record(call string) func(context.Context, *dockerx.Client, *types.Project) error {
	return func(_ context.Context, _ *dockerx.Client, p *types.Project) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls, h.last = append(h.calls, call+" "+p.Name), p
		return h.err
	}
}

func (h *hostCalls) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// standaloneHost is a standalone host with a stack deployed to it (the SDK's up is a stand-in that makes
// nothing) and the containers of the project put on the engine, running, as a real up would have left them.
func standaloneHost(t *testing.T, state string) (*envFix, *roleCaller, stackAnswer, *hostCalls) {
	t.Helper()
	f, e, _ := standaloneEnv(t, nil)
	calls := &hostCalls{}
	f.stackStop, f.stackStart = calls.record("stop"), calls.record("start")
	f.h = f.build(t)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy = %+v", job)
	}
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	cfg.Containers = []dockertest.FakeContainer{{
		ID: "c1", Names: []string{"/solo-web-1"}, Image: "nginx", State: state,
		Labels: map[string]string{
			"com.docker.compose.project": "solo", "com.docker.compose.service": "web",
			"com.docker.compose.oneoff": "False", "bayplan.stack.id": st.ID,
		},
	}}
	f.fake.Reconfigure(cfg)
	return f, operator, st, calls
}

func TestStopAndStartAStandaloneStackRunTheComposeSDKOverItsLastRevision(t *testing.T) {
	t.Parallel()
	f, operator, st, calls := standaloneHost(t, "running")

	started, job := f.lifecycle(t, operator, stopPath(st.ID))
	if started.Recreates || job.State != "done" {
		t.Fatalf("stop = %+v %+v, want a job that is done", started, job)
	}
	if got := readStackAnswer(t, operator, st.ID); got.State != "stopped" || !got.Stopped {
		t.Errorf("stack after a stop = %+v, want Stopped", got)
	}

	started, job = f.lifecycle(t, operator, startPath(st.ID))
	if started.Recreates || job.State != "done" {
		t.Fatalf("start = %+v %+v, want a job that is done and recreates nothing", started, job)
	}
	if got := calls.seen(); len(got) != 2 || got[0] != "stop solo" || got[1] != "start solo" {
		t.Errorf("SDK calls = %v, want a stop then a start of the project solo", got)
	}
	if got := calls.last; got == nil || !strings.Contains(got.Services["web"].Image, "@sha256:") {
		t.Errorf("project = %+v, want the image the last revision pinned", got)
	}
	if got := readStackAnswer(t, operator, st.ID); got.Stopped {
		t.Errorf("stack after a start = %+v, want it no longer stopped", got)
	}
	for _, r := range f.stackAudits(t) {
		if (r.Action == "stack.stopped" || r.Action == "stack.started") && (r.Result != "success" || r.Details["services"] != "1") {
			t.Errorf("%s = %+v, want a success naming 1 service", r.Action, r)
		}
	}
}

func TestStopOfAStandaloneStackWithNoContainerIsNothingRunning(t *testing.T) {
	t.Parallel()
	f, operator, st, calls := standaloneHost(t, "running")
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	f.fake.Reconfigure(cfg)

	rec := operator.do(http.MethodPost, stopPath(st.ID), "")

	if rec.Code != http.StatusConflict || problemType(t, rec) != "nothing_running" {
		t.Errorf("stop: %d %s, want 409 nothing_running", rec.Code, rec.Body)
	}
	if got := calls.seen(); len(got) != 0 {
		t.Errorf("SDK calls = %v, want none", got)
	}
}

func TestAStandaloneStopTheSDKRefusesIsRecordedFailedAndLeavesTheStackNotStopped(t *testing.T) {
	t.Parallel()
	f, operator, st, calls := standaloneHost(t, "running")
	calls.err = errors.New("Error response from daemon: cannot stop container: permission denied")

	_, job := f.lifecycle(t, operator, stopPath(st.ID))

	if job.State == "done" {
		t.Fatalf("job = %+v, want it failed", job)
	}
	if got := readStackAnswer(t, operator, st.ID); got.Stopped {
		t.Errorf("stack = %+v, a stop that failed must not record the stack as stopped", got)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.stopped" {
			row = &r
		}
	}
	if row == nil || row.Result != "failure" || row.Details["result"] != "failed" {
		t.Errorf("stack.stopped = %+v, want a failure", row)
	}
}
