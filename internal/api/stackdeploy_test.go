// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/stackstest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The deploy console follows a deploy over server-sent events (plan 07-17, STK-07, D-13). The hub is
// real; the job under test is driven by the test, so every frame is awaited in order and nothing
// depends on a clock.

// gate waits for a channel the test closes, or for the end of the job's context, so a run never
// outlives a test that failed.
func gate(ctx context.Context, ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

type stackJobFrame struct {
	ID      string `json:"id"`
	StackID string `json:"stackId"`
	State   string `json:"state"`
	Items   []struct {
		ID, Label, Parent, Kind, State, Code string
	} `json:"items"`
}

type stackItemFrame struct {
	ID, Label, Parent, Kind, State, Code string
}

// startStackJob starts a stack_deploy job in the hub for the stack, as the deploy route does, with
// a first step and a run the test moves through its gates.
func startStackJob(t *testing.T, f *envFix, envID, stackID string, run func(ctx context.Context, r *jobs.Reporter)) jobs.Job {
	t.Helper()
	job, err := f.jobHub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: envID, Owner: "user:test",
		Meta:  map[string]string{"stackId": stackID, "stackName": "web", "mode": "deploy", "startedBy": "ada"},
		Items: []jobs.ItemSpec{{ID: "check", Label: "Check the stack", Kind: "step"}, {ID: "services", Label: "Deploy services", Kind: "step"}},
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestStackJobStreamResumes(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "web")

	addRow, finishRow := make(chan struct{}), make(chan struct{})
	job := startStackJob(t, f, e.ID, st.ID, func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("check", jobs.ItemDone, "", "", nil)
		if !gate(ctx, addRow) {
			return
		}
		r.Add(jobs.ItemSpec{ID: "service:web_api", Label: "api", Parent: "services", Kind: "service"})
		if !gate(ctx, finishRow) {
			return
		}
		r.Finish("service:web_api", jobs.ItemDone, "", "", nil)
		r.Finish("services", jobs.ItemDone, "", "", nil)
	})
	path := "/api/env/" + e.ID + "/stacks/" + st.ID + "/deploys/" + job.ID + "/stream"

	// A Viewer (stacks.read) follows it: the snapshot has no id and holds the steps as they stand.
	c := openSSE(t, f.authFixture, path, viewer.cookie, "")
	if c.status0() != http.StatusOK || c.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status = %d headers = %v", c.status0(), c.header)
	}
	first := nextFrame(t, c)
	var snap stackJobFrame
	if first.event != "snapshot" || first.id != "" || json.Unmarshal([]byte(first.data), &snap) != nil || snap.ID != job.ID || snap.StackID != st.ID || len(snap.Items) != 2 {
		t.Fatalf("first frame = %+v, want the snapshot of two steps without an id", first)
	}
	if got := snap.Items[0]; got.ID != "check" || got.Kind != "step" || got.Label != "Check the stack" {
		t.Errorf("first step = %+v", got)
	}

	// An item the run adds arrives as an item frame with its parent and kind.
	close(addRow)
	var added stackItemFrame
	var addedID uint64
	for {
		fr := nextFrame(t, c)
		if fr.event != "item" {
			t.Fatalf("frame = %+v, want an item frame", fr)
		}
		if err := json.Unmarshal([]byte(fr.data), &added); err != nil {
			t.Fatal(err)
		}
		if added.ID == "service:web_api" {
			n, err := strconv.ParseUint(fr.id, 10, 64)
			if err != nil {
				t.Fatalf("item frame id %q", fr.id)
			}
			addedID = n
			break
		}
	}
	if added.Parent != "services" || added.Kind != "service" || added.State != "waiting" || added.Label != "api" {
		t.Fatalf("added item = %+v", added)
	}
	c.close()

	// The deploy goes on without the stream.
	if j, ok := f.jobHub.Get(job.ID); !ok || j.Finished() {
		t.Fatalf("closing the stream stopped the deploy: %+v", j)
	}
	close(finishRow)
	if _, err := f.jobHub.Wait(testsupport.Context(t), job.ID); err != nil {
		t.Fatal(err)
	}

	// A browser that comes back with the last id it saw is shown the whole job at once and then only
	// what happened after that event: nothing before it, nothing twice, ending with done.
	c = openSSE(t, f.authFixture, path, viewer.cookie, strconv.FormatUint(addedID, 10))
	fr := nextFrame(t, c)
	if fr.event != "snapshot" || fr.id != "" {
		t.Fatalf("first frame after resume = %+v, want a snapshot without an id", fr)
	}
	if err := json.Unmarshal([]byte(fr.data), &snap); err != nil || snap.State != "done" || len(snap.Items) != 3 {
		t.Fatalf("resumed snapshot = %s (%v), want the finished job with all three items", fr.data, err)
	}
	var ids []uint64
	var events []string
	for {
		fr := nextFrame(t, c)
		n, err := strconv.ParseUint(fr.id, 10, 64)
		if err != nil {
			t.Fatalf("frame %+v has no event id", fr)
		}
		ids = append(ids, n)
		events = append(events, fr.event)
		if fr.event == "done" {
			break
		}
	}
	if len(ids) == 0 || ids[0] <= addedID || !slices.IsSorted(ids) || len(slices.Compact(slices.Clone(ids))) != len(ids) {
		t.Errorf("resumed ids = %v after %d, want only newer events, in order, once each", ids, addedID)
	}
	if events[len(events)-1] != "done" || slices.Contains(events[:len(events)-1], "done") {
		t.Errorf("resumed events = %v, want done once, last", events)
	}
	c.awaitEnd(t, "the done frame")

	// The query carries the same id for a browser that cannot set the header.
	c = openSSE(t, f.authFixture, path+"?lastEventId="+strconv.FormatUint(addedID, 10), viewer.cookie, "")
	nextFrame(t, c) // the snapshot
	if fr := nextFrame(t, c); fr.id == "" || fr.id == strconv.FormatUint(addedID, 10) {
		t.Errorf("first frame after ?lastEventId = %+v, want an event newer than %d", fr, addedID)
	}
}

func TestStackJobStreamIsOnlyOfItsOwnStackAndEnvironment(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "web")
	other := newStack(t, admin, "other")
	job := startStackJob(t, f, e.ID, st.ID, func(ctx context.Context, r *jobs.Reporter) { <-ctx.Done() })

	// Another stack's path, and a job of some other kind, are the same 404 as a job that is gone.
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/stacks/" + other.ID + "/deploys/" + job.ID + "/stream", cookie: viewer.cookie})
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_job_not_found" {
		t.Errorf("a job of another stack: %d %s, want 404 stack_job_not_found", rec.Code, rec.Body)
	}
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/stacks/" + st.ID + "/deploys/no-such-job/stream", cookie: viewer.cookie})
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_job_not_found" {
		t.Errorf("an unknown job: %d %s, want 404 stack_job_not_found", rec.Code, rec.Body)
	}
	// A person with no role on the environment is refused by the policy, before the job is looked at.
	f.addUser(t, "nobody-1", false)
	cookie, _ := f.signIn(t, "nobody-1", false)
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/stacks/" + st.ID + "/deploys/" + job.ID + "/stream", cookie: cookie})
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("a person with no access: %d %s, want the policy's refusal", rec.Code, rec.Body)
	}
	if err := f.jobHub.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
}

// A secret never reaches the console (plan 07-17, T-07-81, STK-07): not Docker's words on a failed
// item, not the output log, not the snapshot of the stream. The engine here echoes the value of a
// secret variable in its refusal, as a real one can.
func TestConsoleNeverShowsASecret(t *testing.T) {
	t.Parallel()
	const secret = "tok-9f8e7d6c5b4a" //nolint:gosec // a made-up value the test must never find in an answer
	f, e := stackEnv(t, func(c *dockertest.FakeConfig) {
		c.ServiceCreateError = "rpc error: code = InvalidArgument desc = env TOKEN=" + secret + " is not allowed"
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      TOKEN: ${TOKEN}\n"
	version := saveCompose(t, operator, st.ID, compose, `[{"name":"TOKEN","value":"`+secret+`","secret":true}]`)
	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if job := f.awaitDeploy(t, started.JobID); job.State != "partial" {
		t.Fatalf("job = %+v, want the deploy to fail at the service", job)
	}

	// The read of the job.
	rec = operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+started.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get job: %d %s", rec.Code, rec.Body)
	}
	read := rec.Body.String()
	// The stream's snapshot, and its done frame.
	c := openSSE(t, f.authFixture, "/api/env/"+e.ID+"/stacks/"+st.ID+"/deploys/"+started.JobID+"/stream", operator.cookie, "")
	snapshot := nextFrame(t, c).data
	type consoleView struct {
		Items []struct {
			ID, Detail, Message, Code string
		} `json:"items"`
		Output []struct {
			Seq  uint64 `json:"seq"`
			Text string `json:"text"`
		} `json:"output"`
	}
	var job consoleView
	for name, body := range map[string]string{"the read": read, "the stream's snapshot": snapshot} {
		if strings.Contains(body, secret) {
			t.Fatalf("a secret reached %s: %s", name, body)
		}
		var v consoleView
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		job = v
		var failed, logged bool
		for _, it := range v.Items {
			if it.ID == "service:shop_web" && it.Code == "service_rejected" && strings.Contains(it.Detail, "TOKEN=<hidden> is not allowed") {
				failed = true
			}
			if strings.Contains(it.Message, "TOKEN") {
				t.Errorf("%s: item %s message %q holds Docker's words", name, it.ID, it.Message)
			}
		}
		for _, l := range v.Output {
			if strings.Contains(l.Text, "TOKEN=<hidden> is not allowed") {
				logged = true
			}
		}
		if !failed || !logged {
			t.Errorf("%s: want the service row failed with Docker's words (secret hidden) in its detail and in the output; items %+v, output %+v", name, v.Items, v.Output)
		}
	}
	if len(job.Output) == 0 || job.Output[0].Seq == 0 {
		t.Errorf("output = %+v, want the lines the deploy wrote, each with its sequence", job.Output)
	}
}

func TestStackJobStreamCarriesTheOutputLogAndResumesIt(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "web")
	g1, g2, g3 := make(chan struct{}), make(chan struct{}), make(chan struct{})
	job := startStackJob(t, f, e.ID, st.ID, func(ctx context.Context, r *jobs.Reporter) {
		if !gate(ctx, g1) {
			return
		}
		r.Output("Created network web_default")
		if !gate(ctx, g2) {
			return
		}
		r.Output("Created service web_api")
		if !gate(ctx, g3) {
			return
		}
		r.Finish("check", jobs.ItemDone, "", "", nil)
		r.Finish("services", jobs.ItemDone, "", "", nil)
	})
	path := "/api/env/" + e.ID + "/stacks/" + st.ID + "/deploys/" + job.ID + "/stream"
	type line struct {
		Seq  uint64 `json:"seq"`
		At   string `json:"at"`
		Text string `json:"text"`
	}
	c := openSSE(t, f.authFixture, path, viewer.cookie, "")
	var snap struct {
		Output []line `json:"output"`
	}
	if first := nextFrame(t, c); first.event != "snapshot" || json.Unmarshal([]byte(first.data), &snap) != nil || snap.Output == nil || len(snap.Output) != 0 {
		t.Fatalf("first frame = %+v, want a snapshot with an empty output list", first)
	}

	close(g1)
	fr := nextFrame(t, c)
	var one line
	if fr.event != "output" || fr.id == "" || json.Unmarshal([]byte(fr.data), &one) != nil || one.Text != "Created network web_default" || one.At == "" || strconv.FormatUint(one.Seq, 10) != fr.id {
		t.Fatalf("frame = %+v (%+v), want an output frame whose seq is its id", fr, one)
	}
	close(g2)
	fr = nextFrame(t, c)
	var two line
	if fr.event != "output" || json.Unmarshal([]byte(fr.data), &two) != nil || two.Text != "Created service web_api" || two.Seq <= one.Seq {
		t.Fatalf("frame = %+v, want the second line after the first", fr)
	}
	c.close()

	close(g3)
	if _, err := f.jobHub.Wait(testsupport.Context(t), job.ID); err != nil {
		t.Fatal(err)
	}
	// Coming back after the first line: the snapshot holds the log, and only what happened after the
	// first line is replayed, so the second line comes once, as a frame.
	c = openSSE(t, f.authFixture, path, viewer.cookie, strconv.FormatUint(one.Seq, 10))
	if first := nextFrame(t, c); first.event != "snapshot" || json.Unmarshal([]byte(first.data), &snap) != nil || len(snap.Output) != 2 || snap.Output[1].Text != two.Text {
		t.Fatalf("snapshot after resume = %+v, want the whole log", first)
	}
	var texts []string
	for {
		fr := nextFrame(t, c)
		if fr.event == "output" {
			var l line
			if err := json.Unmarshal([]byte(fr.data), &l); err != nil {
				t.Fatal(err)
			}
			texts = append(texts, l.Text)
		}
		if fr.event == "done" {
			break
		}
	}
	if !slices.Equal(texts, []string{two.Text}) {
		t.Errorf("replayed lines = %q, want only the line after the one the browser had", texts)
	}
}

// Cancel deploy (plan 07-18, STK-07, D-14): the deploy stops after the step it is on, the job ends
// cancelled, and stack.deployed records it.

// awaitStackJobItem reads the job's events until an item satisfies cond, starting with the job as it stands.
func awaitStackJobItem(t *testing.T, f *envFix, jobID string, cond func(jobs.Item) bool) {
	t.Helper()
	snap, ch, cancel, err := f.jobHub.Subscribe(testsupport.Context(t), jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for _, it := range snap.Items {
		if cond(it) {
			return
		}
	}
	ctx := testsupport.Context(t)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("the job ended before the item the test waited for")
			}
			if ev.Item != nil && cond(*ev.Item) {
				return
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the item")
		}
	}
}

// startStuckDeploy deploys the starter stack to a swarm whose service never gets past preparing,
// and returns when the deploy has begun waiting for it.
func startStuckDeploy(t *testing.T, f *envFix, operator *roleCaller, st stackAnswer) string {
	t.Helper()
	f.fake.SetTaskState(st.Name+"_web", "preparing", "")
	rec := operator.deploy(st.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	awaitStackJobItem(t, f, started.JobID, func(it jobs.Item) bool { return it.ID == "service:"+st.Name+"_web" && it.Unit == "tasks" })
	return started.JobID
}

func cancelPath(stackID, jobID string) string {
	return "stacks/" + stackID + "/deploys/" + jobID + "/cancel"
}

func TestCancelDeployStopsTheJobAndRecordsItCancelled(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	other := f.caller(t, e, "operator-2", "operator")
	st := newStack(t, admin, "tracer")
	jobID := startStuckDeploy(t, f, operator, st)

	// Someone other than the starter may cancel: it needs stacks.deploy, not the job.
	rec := other.do(http.MethodPost, cancelPath(st.ID, jobID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	job := f.awaitDeploy(t, jobID)
	if job.State == "running" {
		t.Fatalf("job = %+v, want it ended", job)
	}

	// The job reads cancelled, though services before the stop were written.
	rec = operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+jobID, "")
	if got := answerOf[struct {
		State string `json:"state"`
	}](t, rec).State; got != "cancelled" {
		t.Errorf("job state = %q, want cancelled", got)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.deployed" {
			row = &r
		}
	}
	if row == nil || row.Details["result"] != "cancelled" || row.Details["cancelledBy"] != "operator-2" {
		t.Errorf("stack.deployed = %+v, want result cancelled by operator-2", row)
	}
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, a cancel writes nothing", n)
	}
}

func TestCancelDeployOfAJobThatEndedIsRefusedAndChangesNothing(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	rec := operator.deploy(st.ID, `{"draftVersion":1}`)
	started := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	rec = operator.do(http.MethodPost, cancelPath(st.ID, started.JobID), "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "job_finished") {
		t.Fatalf("cancel of a finished job: %d %s, want 409 job_finished", rec.Code, rec.Body)
	}
}

func TestCancelDeployIsOnlyForADeployOfThatStack(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	elsewhere := newStack(t, admin, "elsewhere")
	jobID := startStuckDeploy(t, f, operator, st)
	t.Cleanup(func() { _ = f.jobHub.Cancel(jobID) })

	for name, path := range map[string]string{
		"a job of another stack":  cancelPath(elsewhere.ID, jobID),
		"a job that is not there": cancelPath(st.ID, "no-such-job"),
	} {
		rec := operator.do(http.MethodPost, path, "")
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "stack_job_not_found") {
			t.Errorf("%s: %d %s, want 404 stack_job_not_found", name, rec.Code, rec.Body)
		}
	}
	if j, ok := f.jobHub.Get(jobID); !ok || j.Finished() {
		t.Errorf("job = %+v, a refused cancel must leave it running", j)
	}
}

func TestCancelDeployNeedsOperator(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "tracer")
	jobID := startStuckDeploy(t, f, operator, st)
	t.Cleanup(func() { _ = f.jobHub.Cancel(jobID) })

	if rec := viewer.do(http.MethodPost, cancelPath(st.ID, jobID), ""); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's cancel: %d %s, want 403", rec.Code, rec.Body)
	}
	if j, ok := f.jobHub.Get(jobID); !ok || j.Finished() {
		t.Errorf("job = %+v, a refused cancel must leave it running", j)
	}
}

// One stack operation at a time on an environment (plan 07-18, STK-09 concurrency, T-07-87).

type lockAnswer struct {
	StackID   string `json:"stackId"`
	StackName string `json:"stackName"`
	JobID     string `json:"jobId"`
	Kind      string `json:"kind"`
	StartedAt string `json:"startedAt"`
	StartedBy string `json:"startedBy"`
}

// lockOf reads the operation lock a stack's page shows, nil while nothing holds the environment.
func lockOf(t *testing.T, c *roleCaller, stackID string) *lockAnswer {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks/"+stackID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get stack: %d %s", rec.Code, rec.Body)
	}
	return answerOf[struct {
		Lock *lockAnswer `json:"lock"`
	}](t, rec).Lock
}

func TestSecondOperationIsRefusedWhileOneRuns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha, beta := newStack(t, admin, "alpha"), newStack(t, admin, "beta")
	jobID := startStuckDeploy(t, f, operator, alpha)

	// Every stack page of the environment says who holds it.
	for _, id := range []string{alpha.ID, beta.ID} {
		lock := lockOf(t, operator, id)
		if lock == nil || lock.StackID != alpha.ID || lock.StackName != "alpha" || lock.JobID != jobID || lock.Kind != "deploy" || lock.StartedBy != "operator-1" || lock.StartedAt == "" {
			t.Fatalf("lock on %s = %+v, want alpha's deploy %s started by operator-1", id, lock, jobID)
		}
	}

	// Another stack's deploy, and the same stack's, are refused naming the holder.
	writes := len(f.fake.Writes())
	for name, id := range map[string]string{"another stack": beta.ID, "the same stack": alpha.ID} {
		rec := operator.deploy(id, `{"draftVersion":1}`)
		if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
			t.Fatalf("a deploy of %s: %d %s, want 409 operation_locked", name, rec.Code, rec.Body)
		}
		pr := problemOf(t, rec)
		var named []string
		for _, er := range pr.Errors {
			named = append(named, er.Location+"="+er.Message)
		}
		if !slices.Contains(named, "stack=alpha") || !slices.Contains(named, "job="+jobID) || !slices.ContainsFunc(named, func(s string) bool { return strings.HasPrefix(s, "started=20") }) {
			t.Errorf("errors = %v, want the stack, the job and when it started", named)
		}
	}
	var revisions int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision'`, beta.ID).Scan(&revisions); err != nil || revisions != 0 {
		t.Errorf("revisions of the refused deploy = %d (%v), want none", revisions, err)
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("writes went from %d to %d, a refused operation writes nothing", writes, got)
	}

	// The review says it too, as a problem, so Deploy waits.
	rv, code, raw := operator.reviewStack(beta.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if p, ok := rv.problem("operation_locked"); !ok || p.Severity != "error" {
		t.Errorf("problems = %+v, want a blocking operation_locked", rv.Problems)
	}

	// The lock goes with the job, whatever state it ends in, and the next operation can run.
	if rec := operator.do(http.MethodPost, cancelPath(alpha.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, jobID)
	if lock := lockOf(t, operator, beta.ID); lock != nil {
		t.Errorf("lock = %+v after the job ended, want none", lock)
	}
	rec := operator.deploy(beta.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy after the lock was released: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID)
}

func TestRunningDeploysAreMarkedInterruptedAtStartup(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "crash")
	ctx := testsupport.Context(t)
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	rev, err := repo.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StartDeploy(ctx, st.ID, e.ID, stackops.DeployKindDeploy, rev.Seq, "job-gone-with-the-process", "ada"); err != nil {
		t.Fatal(err)
	}
	if got := answerOf[stackAnswer](t, admin.do(http.MethodGet, "stacks/"+st.ID, "")).State; got != "deploying" {
		t.Fatalf("state before the restart = %q, want deploying", got)
	}

	// The server does this once the store is open.
	if n, err := repo.MarkInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("MarkInterrupted = %d, %v, want 1", n, err)
	}
	got := answerOf[struct {
		stackAnswer
		LastOperation *struct {
			Kind, State, Code string
		} `json:"lastOperation"`
	}](t, admin.do(http.MethodGet, "stacks/"+st.ID, ""))
	if got.State != "deploy_failed" || got.LastOperation == nil || got.LastOperation.State != "interrupted" || got.LastOperation.Code != "interrupted" || got.LastOperation.Kind != "deploy" {
		t.Errorf("stack = %+v %+v, want deploy_failed from an interrupted deploy", got.stackAnswer, got.LastOperation)
	}
	if r, err := repo.Revision(ctx, st.ID, rev.Seq); err != nil || r.Outcome != stackops.OutcomeFailed {
		t.Errorf("revision = %+v, %v, want failed", r, err)
	}
	list := answerOf[struct {
		Items []stackAnswer `json:"items"`
	}](t, admin.do(http.MethodGet, "stacks", ""))
	if len(list.Items) != 1 || list.Items[0].State != "deploy_failed" {
		t.Errorf("list = %+v", list.Items)
	}
}

const hostAccessCompose = "services:\n  web:\n    image: nginx:1.27-alpine\n    privileged: true\n"

func TestPolicyIsCheckedAgainUnderTheLock(t *testing.T) {
	t.Parallel()
	var between atomic.Pointer[func()]
	f, e := stackEnvTuned(t, func(f *envFix) {
		f.stackBeforeJob = func(context.Context) {
			if fn := between.Load(); fn != nil {
				(*fn)()
			}
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	holder := f.caller(t, e, "envadmin-2", "admin")
	st := newStack(t, admin, "esc")
	version := saveCompose(t, holder, st.ID, hostAccessCompose, `[]`)

	// holder is an env Admin when the deploy is judged, and an Operator by the time the job runs:
	// the grant is gone, so the job must not write what the request was allowed to ask for.
	demote := func() {
		if _, err := f.st.Write.Exec(`UPDATE role_assignments SET role_id = 'operator' WHERE principal_id = 'id-envadmin-2'`); err != nil {
			t.Error(err)
		}
	}
	between.Store(&demote)
	rec := holder.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s, want 202: the request itself was allowed", rec.Code, rec.Body)
	}
	jobID := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID
	job := f.awaitDeploy(t, jobID)

	byID := map[string]string{}
	for _, it := range job.Items {
		byID[it.ID] = it.State + ":" + it.Code
	}
	if byID["check"] != "failed:needs_admin" {
		t.Errorf("items = %v, want the check step failed because the policy now refuses it", byID)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a refused policy re-check writes nothing", w)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.deployed" {
			row = &r
		}
	}
	if row == nil || row.Result != "failure" || row.Details["code"] != "needs_admin" {
		t.Errorf("stack.deployed = %+v, want a failure with needs_admin", row)
	}
}

// A secret a swarm would refuse is refused by the server before anything is sent (plan 07-13 carry-over,
// D-40): the browser's Upload checks the size too, but a file typed into the editor or sent by another
// client reaches the same place.
func TestAnOversizeSecretFileIsRefusedByTheReviewAndTheDeploy(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "big")
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    secrets: [tok]\nsecrets:\n  tok:\n    file: tok.txt\n"
	// The content is varied: a file of one repeated character is slow to mask (deferred-items.md).
	body := func(size int) string {
		cb, _ := json.Marshal(compose)
		rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // a fixed seed gives the same file every run; the content is not a secret
		content := make([]byte, size)
		for i := range content {
			content[i] = "abcdefghijklmnopqrstuvwxyz0123456789"[rng.IntN(36)]
		}
		return `{"version":` + itoaT(draftVersion(t, operator, st.ID)) + `,"files":[{"path":"compose.yaml","content":` + string(cb) + `,"secret":false},` +
			`{"path":"tok.txt","content":"` + string(content) + `","secret":true}],"vars":[]}`
	}
	if rec := operator.saveDraft(st.ID, body(520_000)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %.200s", rec.Code, rec.Body)
	}
	version := draftVersion(t, operator, st.ID)

	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	p, ok := rv.problem("object_invalid")
	if !ok || p.Severity != "error" || !strings.Contains(p.Message, "The secret tok cannot be created: it is over 500 KB, the most a swarm secret holds.") {
		t.Fatalf("problems = %+v, want a blocking object_invalid naming the 500 KB limit", rv.Problems)
	}
	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "it is over 500 KB, the most a swarm secret holds") {
		t.Fatalf("deploy: %d %.300s, want 422 naming the limit", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

// composeEnvFile is a stack whose one service reads its environment from app.env, a file of the stack.
const composeEnvFile = "services:\n  web:\n    image: nginx:1.27-alpine\n    env_file: app.env\n"

// envFileFiles are the files of a stack that uses env_file: its compose file and app.env, which sets FROM_FILE.
func envFileFiles(compose string) []draftFile {
	return []draftFile{{Path: "compose.yaml", Content: compose}, {Path: "app.env", Content: "FROM_FILE=1\n"}}
}

// saveFiles saves files over the draft as c and returns the new version.
func saveFiles(t *testing.T, c *roleCaller, stackID string, files ...draftFile) int64 {
	t.Helper()
	v := draftVersion(t, c, stackID)
	if rec := c.saveDraft(stackID, filesBody(t, v, files...)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	return draftVersion(t, c, stackID)
}

// deployFilesGood saves files as the draft, deploys it and waits for it to end good.
func (f *envFix) deployFilesGood(t *testing.T, c *roleCaller, stackID string, files ...draftFile) {
	t.Helper()
	v := saveFiles(t, c, stackID, files...)
	if job := f.deployAndWait(t, c, stackID, `{"draftVersion":`+itoaT(v)+`}`); job.State != "done" {
		t.Fatalf("deploy = %+v, want it done", job)
	}
}

// hasEnv reports whether env holds the entry want.
func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// A stack that uses env_file reviews clean, so it must deploy: the deploy takes its project from the check the
// review ran, with the file's variables already in the service (review CR-05).
func TestASwarmStackThatUsesAnEnvFileReviewsCleanAndDeploysWithTheFilesValues(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "envf")
	v := saveFiles(t, operator, st.ID, envFileFiles(composeEnvFile)...)

	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			t.Fatalf("problems = %+v, want no error: the review accepts an env_file stack", rv.Problems)
		}
	}
	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %.300s, want 202: what the review accepts deploys", rec.Code, rec.Body)
	}
	jobID := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID
	if job := f.awaitDeploy(t, jobID); job.State != "done" {
		t.Fatalf("deploy job = %+v, want it done", job)
	}
	made := f.fake.ServiceCreates()
	if len(made) == 0 {
		t.Fatal("no service was created")
	}
	if env := made[len(made)-1].Spec.TaskTemplate.ContainerSpec.Env; !hasEnv(env, "FROM_FILE=1") {
		t.Errorf("env = %v, want the env file's FROM_FILE=1", env)
	}
}

// leakyValue is a secret variable's value in the tests of the masking invariant (review WR-04).
const leakyValue = "hunter2secretvalue"

// leakyVars is a variable table with one secret variable, PW, that holds leakyValue.
const leakyVars = `[{"name":"PW","value":"` + leakyValue + `","secret":true}]`

// A deploy that is refused says why without ever quoting a secret variable (review WR-04, T-07-159): the
// converter quotes the values it refuses, so its text is masked with the stack's own masker.
func TestADeployRefusalNeverQuotesASecretVariable(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		compose string
		// quoted is true when the refusal comes from the converter, which quotes the value it refuses.
		quoted bool
	}{
		"a value the converter quotes": {"services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      endpoint_mode: ${PW}\n", true},
		"a value in a cast position":   {"services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      replicas: ${PW}\n", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "leaky")
			v := saveCompose(t, operator, st.ID, tc.compose, leakyVars)

			rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("deploy: %d %.300s, want 422", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), leakyValue) {
				t.Errorf("deploy answer = %s, want no secret value", rec.Body)
			}
			if tc.quoted && !strings.Contains(rec.Body.String(), "because they could quote it.") {
				t.Errorf("deploy answer = %s, want the converter's refusal replaced by the fixed sentence", rec.Body)
			}
			if w := f.fake.Writes(); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}

// escapedLeakyValue is a secret variable's value that Go, compose-go and the schema validator quote with
// escapes: a backslash, a double quote, a single quote and line breaks (review CR-01, D-02).
const escapedLeakyValue = "hunter2-alpha\\hunter2-bravo\"hunter2-charlie'hunter2-delta\nhunter2-echo\n"

var escapedLeakyParts = []string{"hunter2-alpha", "hunter2-bravo", "hunter2-charlie", "hunter2-delta", "hunter2-echo"}

// escapedLeakyVars is a variable table with one secret variable, PW, that holds escapedLeakyValue. The value
// cannot be spliced into a JSON string by hand, so it is encoded.
func escapedLeakyVars(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal([]map[string]any{{"name": "PW", "value": escapedLeakyValue, "secret": true}})
	if err != nil {
		t.Fatalf("encode the variables: %v", err)
	}
	return string(b)
}

// A deploy refused at a cast position, a schema pattern or a value the converter quotes never carries a secret
// variable that those texts write with escapes (review CR-01, T-07-168).
func TestADeployRefusalNeverQuotesAnEscapedSecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		compose string
		// fixed is the key of the field the loader refuses, reported with the fixed sentence (07-40); empty
		// for the converter's refusal (07-41), which ends in the same words whichever field it names.
		fixed string
	}{
		"a cast position":              {head + "    deploy:\n      replicas: ${PW}\n", "services.web.deploy.replicas"},
		"a schema pattern":             {head + "    pull_policy: ${PW}\n", "services.web.pull_policy"},
		"a value the converter quotes": {head + "    deploy:\n      endpoint_mode: ${PW}\n", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "escaped")
			v := saveCompose(t, operator, st.ID, tc.compose, escapedLeakyVars(t))

			rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("deploy: %d %.300s, want 422", rec.Code, rec.Body)
			}
			for _, part := range escapedLeakyParts {
				if strings.Contains(rec.Body.String(), part) {
					t.Errorf("deploy answer = %s, want no part (%q) of the secret", rec.Body, part)
				}
			}
			if tc.fixed != "" {
				if !strings.Contains(rec.Body.String(), stacks.SecretValueMessage(tc.fixed)) {
					t.Errorf("deploy answer = %s, want the fixed sentence at %s", rec.Body, tc.fixed)
				}
			} else if !strings.Contains(rec.Body.String(), "because they could quote it.") {
				t.Errorf("deploy answer = %s, want the converter's refusal replaced by the fixed sentence", rec.Body)
			}
			if w := f.fake.Writes(); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}

// partSecretVars is a variable table with one secret variable, PW, that holds value.
func partSecretVars(t *testing.T, value string) string {
	t.Helper()
	b, err := json.Marshal([]map[string]any{{"name": "PW", "value": value, "secret": true}})
	if err != nil {
		t.Fatalf("encode the variables: %v", err)
	}
	return string(b)
}

// loaderWordsOf is the text of a 422 that is built from the loader's or the converter's words: its detail and
// every message of its errors. Locations and the raw body hold API fields and key paths, which are not scanned.
func loaderWordsOf(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	em := answerOf[huma.ErrorModel](t, rec)
	out := []string{em.Detail}
	for _, d := range em.Errors {
		out = append(out, d.Message)
	}
	return out
}

// A deploy refused at a field fed by a secret variable carries the fixed sentence and no part of the
// value, though go-units quotes a substring of it (07-40, D-02, T-07-172).
func TestADeployRefusalNeverQuotesPartOfASecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		compose, secret, path string
		parts                 []string
	}{
		"mem_limit, a number then a word": {head + "    mem_limit: ${PW}\n", "Password2024!", "services.web.mem_limit", []string{"Password2024"}},
		"mem_limit, a number then a unit": {head + "    mem_limit: ${PW}\n", "2024secretword", "services.web.mem_limit", []string{"secretword", "2024secret"}},
		"deploy.replicas":                 {head + "    deploy:\n      replicas: ${PW}\n", "Password2024!", "services.web.deploy.replicas", []string{"Password2024"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "partial")
			v := saveCompose(t, operator, st.ID, tc.compose, partSecretVars(t, tc.secret))

			rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("deploy: %d %.300s, want 422", rec.Code, rec.Body)
			}
			raw := rec.Body.String()
			if !strings.Contains(raw, "stack_invalid") || !strings.Contains(raw, stacks.SecretValueMessage(tc.path)) {
				t.Errorf("deploy answer = %s, want stack_invalid with the fixed sentence at %s", raw, tc.path)
			}
			for _, part := range tc.parts {
				if strings.Contains(raw, part) {
					t.Errorf("deploy answer = %s, want no part (%q) of the secret", raw, part)
				}
			}
			stackstest.NoSecretRun(t, tc.secret, loaderWordsOf(t, rec)...)
			if w := f.fake.Writes(); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}

// compose-go moves list items between the file and its model: it concatenates the lists of several YAML
// documents and merges repeated items even in one document, so the loader's index is not an item of the
// file. A deploy refused at a list item that uses a secret variable carries the file's sentence at the merged
// key and no run of the value (07-45, D-02, 07-REVIEW-gaps3 CR-01, T-07-189).
func TestADeployRefusalOfAListItemTheLoaderMovesNeverQuotesPartOfASecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct{ compose, secret, key string }{
		"two documents, a tmpfs size in the second": {
			head + "    volumes:\n      - {type: tmpfs, target: /a, tmpfs: {size: 1m}}\n---\nservices:\n  web:\n    volumes:\n      - {type: tmpfs, target: /b, tmpfs: {size: \"${PW}\"}}\n      - {type: tmpfs, target: /c, tmpfs: {size: 1m}}\n",
			"Password2024!", "services.web.volumes.1.tmpfs.size",
		},
		"two documents, a relative bind in the second": {
			head + "    volumes: [\"./p1:/a1\"]\n---\nservices:\n  web:\n    volumes: [\"./${PW}:/s\", \"./p2:/a2\", \"./p3:/a3\", \"./p4:/a4\"]\n",
			"Zq9xkwmpr:Vb7nm", "services.web.volumes.1",
		},
		"one document, a repeated target before the secret item": {
			head + "    volumes:\n      - {type: tmpfs, target: /a, tmpfs: {size: 1m}}\n      - {type: tmpfs, target: /a, tmpfs: {size: 2m}}\n      - {type: tmpfs, target: /b, tmpfs: {size: \"${PW}\"}}\n      - {type: tmpfs, target: /c, tmpfs: {size: 1m}}\n",
			"Password2024!", "services.web.volumes.1.tmpfs.size",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "listmoves")
			v := saveCompose(t, operator, st.ID, tc.compose, partSecretVars(t, tc.secret))

			rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("deploy: %d %.300s, want 422", rec.Code, rec.Body)
			}
			if raw := rec.Body.String(); !strings.Contains(raw, "stack_invalid") {
				t.Errorf("deploy answer = %s, want stack_invalid", raw)
			}
			words := loaderWordsOf(t, rec)
			if !slices.Contains(words, stacks.SecretFileMessage(tc.key)) {
				t.Errorf("deploy words = %q, want one equal to %q", words, stacks.SecretFileMessage(tc.key))
			}
			stackstest.NoSecretRun(t, tc.secret, words...)
			if w := f.fake.Writes(); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}

// The converter describes a refused value in its own words, and compose-go splits a short syntax before it
// does, so a secret variable can reach the deploy answer in part: a restart policy cut at ":" and a published
// port cut out of a longer spec. The answer for a field that uses a secret variable is the fixed sentence for
// that field, and a plain value keeps the converter's words (07-41, D-02).
func TestADeployNeverQuotesPartOfASecretTheConverterSplits(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		compose, secret string
		// path is the field whose fixed sentence is wanted, empty for a plain value.
		path string
		// want is a piece of the converter's own words a plain value keeps.
		want string
		// parts are the pieces of the secret the answer must not hold.
		parts []string
	}{
		"a restart policy": {head + "    restart: ${PW}\n", "sometimes:3", "services.web.restart", "", []string{"sometimes"}},
		"a published port": {head + "    ports:\n      - \"${PW}\"\n", "127.0.0.1:8000-8001:80", "services.web.ports", "", []string{"8000-8001"}},
		"a plain value keeps the converter's words": {
			head + "    restart: sometimes\n    environment:\n      DB: ${PW}\n", "Password2024!", "", "unknown policy", []string{"Password2024"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "splits")
			v := saveCompose(t, operator, st.ID, tc.compose, partSecretVars(t, tc.secret))

			rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("deploy: %d %.300s, want 422", rec.Code, rec.Body)
			}
			raw := rec.Body.String()
			if tc.path != "" && !strings.Contains(raw, stacks.SecretValueMessage(tc.path)) {
				t.Errorf("deploy answer = %s, want the fixed sentence at %s", raw, tc.path)
			}
			if tc.want != "" && !strings.Contains(raw, tc.want) {
				t.Errorf("deploy answer = %s, want the converter's words (%q)", raw, tc.want)
			}
			for _, part := range tc.parts {
				if strings.Contains(raw, part) {
					t.Errorf("deploy answer = %s, want no part (%q) of the secret", raw, part)
				}
			}
			stackstest.NoSecretRun(t, tc.secret, loaderWordsOf(t, rec)...)
			if w := f.fake.Writes(); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}

// A plain field the deploy refuses gets the same answer whether its value is part of a secret used
// elsewhere in the file or not, so an Operator without secrets.reveal cannot tell a guess at the secret from
// any other value (07-43, 07-44, D-02, T-07-187). The guesses are the Operator's own input, so they are
// replaced before the answers are compared, and the check for a run of the secret is not applied to them.
func TestADeployRefusalOfAPlainFieldIsTheSameForEveryGuessAtASecret(t *testing.T) {
	t.Parallel()
	const needle = "Zq9xkwmpr"
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      DB: ${PW}\n"
	guesses := []string{"xkwm", "kwmp", "xkwn", "aaaa"}
	type shape struct {
		Type, Detail string
		Errors       [][2]string
	}
	cases := map[string]struct {
		field string
		// words is what the answer keeps of the converter's or the loader's words.
		words func(g string) string
	}{
		"dns, refused by the converter": {
			field: "    dns: [\"%s\"]\n",
			words: func(g string) string { return "is not an IP address" },
		},
		"mem_limit, refused by the loader": {
			field: "    mem_limit: %s\n",
			words: func(g string) string { return "invalid size: '" + g + "'" },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "oracle")
			var first shape
			for i, g := range guesses {
				compose := head + strings.Replace(tc.field, "%s", g, 1)
				v := saveCompose(t, operator, st.ID, compose, partSecretVars(t, needle))
				rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("deploy of %q: %d %.300s, want 422", g, rec.Code, rec.Body)
				}
				em := answerOf[huma.ErrorModel](t, rec)
				all := em.Detail
				got := shape{Type: em.Type, Detail: strings.ReplaceAll(em.Detail, g, "GUESS")}
				for _, d := range em.Errors {
					all += " " + d.Message
					got.Errors = append(got.Errors, [2]string{strings.ReplaceAll(d.Location, g, "GUESS"), strings.ReplaceAll(d.Message, g, "GUESS")})
				}
				if !strings.Contains(all, tc.words(g)) {
					t.Errorf("deploy of %q: %s, want the words %q", g, all, tc.words(g))
				}
				if em.Detail == stacks.SecretValueMessage("services.web.dns") {
					t.Errorf("deploy of %q: detail is the secret sentence, want the converter's words", g)
				}
				if i == 0 {
					first = got
				} else if !reflect.DeepEqual(first, got) {
					t.Errorf("deploy of %q = %+v, want the answer of %q (%+v)", g, got, guesses[0], first)
				}
				if w := f.fake.Writes(); len(w) != 0 {
					t.Errorf("writes = %v, want none", w)
				}
			}
		})
	}
}

// Roll back (plan 07-19, STK-07, D-14): one click redeploys exactly what last ran well, with its own
// images, and asks no registry.

const (
	composeWeb127 = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	composeWeb128 = "services:\n  web:\n    image: nginx:1.28-alpine\n"
	composeWeb129 = "services:\n  web:\n    image: nginx:1.29-alpine\n"
)

func rollbackPath(stackID string) string { return "stacks/" + stackID + "/rollback" }

// deployDraft saves compose over the draft and deploys it as c, returning the job.
func (f *envFix) deployDraft(t *testing.T, c *roleCaller, stackID, composeText string) string {
	t.Helper()
	v := saveCompose(t, c, stackID, composeText, `[]`)
	rec := c.deploy(stackID, `{"draftVersion":`+itoaT(v)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	return answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID
}

// deployGood deploys compose and waits for it to end good.
func (f *envFix) deployGood(t *testing.T, c *roleCaller, stackID, composeText string) {
	t.Helper()
	if job := f.awaitDeploy(t, f.deployDraft(t, c, stackID, composeText)); job.State != "done" {
		t.Fatalf("deploy = %+v, want it done", job)
	}
}

// deployThenCancel deploys compose to a service that never settles and cancels it, so the revision ends failed.
func (f *envFix) deployThenCancel(t *testing.T, c *roleCaller, st stackAnswer, composeText string) {
	t.Helper()
	f.fake.SetTaskState(st.Name+"_web", "preparing", "")
	jobID := f.deployDraft(t, c, st.ID, composeText)
	awaitStackJobItem(t, f, jobID, func(it jobs.Item) bool { return it.ID == "service:"+st.Name+"_web" && it.Unit == "tasks" })
	if rec := c.do(http.MethodPost, cancelPath(st.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, jobID); job.State == "done" {
		t.Fatalf("job = %+v, want it stopped", job)
	}
	f.fake.SetTaskState(st.Name+"_web", "", "") // the service settles again from here on
}

type revisionRow struct {
	Seq     int64
	Outcome string
	Pins    string
}

func (f *envFix) revisions(t *testing.T, stackID string) []revisionRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT seq, outcome, pins FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' ORDER BY seq`, stackID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []revisionRow
	for rows.Next() {
		var r revisionRow
		if err := rows.Scan(&r.Seq, &r.Outcome, &r.Pins); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

type rollbackStackAnswer struct {
	State        string `json:"state"`
	DraftSavedAt string `json:"draftSavedAt"`
	Rollback     struct {
		Available  bool   `json:"available"`
		DeployedAt string `json:"deployedAt"`
		DeployedBy string `json:"deployedBy"`
		Revision   int64  `json:"revision"`
	} `json:"rollback"`
	LastOperation *struct {
		Kind, State string
	} `json:"lastOperation"`
}

func TestRollbackRedeploysTheLastGoodRevisionWithoutTheRegistry(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")

	// Revision 1 and 2 are good, 3 is stopped: what last ran well is 2.
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	f.deployThenCancel(t, operator, st, composeWeb129)
	revs := f.revisions(t, st.ID)
	if len(revs) != 3 || revs[0].Outcome != "good" || revs[1].Outcome != "good" || revs[2].Outcome != "failed" {
		t.Fatalf("revisions = %+v, want good, good, failed", revs)
	}
	pin128 := revs[1].Pins
	if !strings.Contains(pin128, "nginx:1.28-alpine@sha256:") {
		t.Fatalf("revision 2 pins = %s, want the 1.28 image pinned to a digest", pin128)
	}

	// The tag has moved on the registry since: a roll back must not follow it.
	f.fake.SetDistributionDigest("docker.io/library/nginx:1.28-alpine", "sha256:"+strings.Repeat("b", 64))
	before := readStack(t, operator, st.ID)
	if !before.Rollback.Available || before.Rollback.DeployedBy != "operator-1" || before.Rollback.DeployedAt == "" || before.State != "deploy_failed" {
		t.Fatalf("stack before = %+v, want a failed deploy with a roll back to operator-1's version", before)
	}
	lookups := len(f.fake.DistributionRefs())
	updates := len(f.fake.ServiceUpdates())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}](t, rec)
	if started.Revision != 4 {
		t.Errorf("revision = %d, want 4: the roll back is a revision of its own", started.Revision)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}

	// No registry was asked, and the service went back to the digest revision 2 ran.
	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a roll back asks no registry", lookups, n)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != updates+1 {
		t.Fatalf("service updates = %d, want one more than %d", len(ups), updates)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(pin128), &pins); err != nil {
		t.Fatal(err)
	}
	if image := ups[len(ups)-1].Spec.TaskTemplate.ContainerSpec.Image; image != pins["rb_web"] {
		t.Errorf("image = %q, want exactly what revision 2 ran, %q", image, pins["rb_web"])
	}

	// The roll back is revision 4, a good copy of 2, and the stack reads running.
	revs = f.revisions(t, st.ID)
	if len(revs) != 4 || revs[3].Outcome != "good" || revs[3].Pins != pin128 {
		t.Errorf("revisions = %+v, want a good revision 4 with revision 2's pins", revs)
	}
	after := readStack(t, operator, st.ID)
	if after.State != "running" || after.LastOperation == nil || after.LastOperation.Kind != "rollback" || after.LastOperation.State != "done" {
		t.Errorf("stack after = %+v %+v, want deployed by a roll back", after, after.LastOperation)
	}
	job := answerOf[struct {
		Mode  string `json:"mode"`
		State string `json:"state"`
	}](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+started.JobID, ""))
	if job.Mode != "rollback" || job.State != "done" {
		t.Errorf("job = %+v, want mode rollback", job)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.deployed" {
			row = &r
		}
	}
	if row == nil || row.Details["mode"] != "rollback" || row.Result != "success" || row.Details["revision"] != "4" || row.TargetID != st.ID {
		t.Errorf("stack.deployed = %+v, want a successful rollback of revision 4", row)
	}
}

// setRevisionPlatforms records on a revision of a stack the platforms its services ran with, as its deploy would have.
func (f *envFix) setRevisionPlatforms(t *testing.T, stackID string, seq int64, platforms map[string][]swarm.Platform) {
	t.Helper()
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	rev, err := repo.Revision(context.Background(), stackID, seq)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetPlatforms(context.Background(), rev.ID, platforms); err != nil {
		t.Fatal(err)
	}
}

// The roll back sends the platforms its revision stored (G-08-4, P7 D-14, T-07-93, T-08-101): they differ from the ones the
// service runs now, and no registry is asked.
func TestRollbackSendsThePlatformsTheRevisionStored(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rbp")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	// The service runs both platforms (the fake registry answers them); revision 1 stored one.
	f.setRevisionPlatforms(t, st.ID, 1, map[string][]swarm.Platform{"rbp_web": {amd}})
	pin127 := f.revisions(t, st.ID)[0].Pins
	lookups, updates := len(f.fake.DistributionRefs()), len(f.fake.ServiceUpdates())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}
	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a roll back asks no registry", lookups, n)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != updates+1 {
		t.Fatalf("service updates = %d, want one more than %d", len(ups), updates)
	}
	last := ups[len(ups)-1].Spec
	var pins map[string]string
	if err := json.Unmarshal([]byte(pin127), &pins); err != nil {
		t.Fatal(err)
	}
	if image := last.TaskTemplate.ContainerSpec.Image; image != pins["rbp_web"] {
		t.Errorf("image = %q, want what revision 1 ran, %q", image, pins["rbp_web"])
	}
	if pl := last.TaskTemplate.Placement; pl == nil || !slices.Equal(pl.Platforms, []swarm.Platform{amd}) {
		t.Errorf("placement = %+v, want the platforms revision 1 stored, %v", pl, []swarm.Platform{amd})
	}
}

// A restore is the same route in mode restore and sends the deployed revision's stored platforms (D-23).
func TestRestoreSendsThePlatformsTheDeployedRevisionStored(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rsp")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}
	f.setRevisionPlatforms(t, st.ID, 2, map[string][]swarm.Platform{"rsp_web": {arm}})
	lookups, updates := len(f.fake.DistributionRefs()), len(f.fake.ServiceUpdates())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"mode":"restore","revision":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Fatalf("restore job = %+v, want it done", job)
	}
	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a restore asks no registry", lookups, n)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != updates+1 {
		t.Fatalf("service updates = %d, want one more than %d", len(ups), updates)
	}
	if pl := ups[len(ups)-1].Spec.TaskTemplate.Placement; pl == nil || !slices.Equal(pl.Platforms, []swarm.Platform{arm}) {
		t.Errorf("placement = %+v, want the platforms the deployed revision stored, %v", pl, []swarm.Platform{arm})
	}
}

// Rolling back a stack that uses env_file restores the file's variables: the revision is judged once, by the
// check pipeline, and the service comes back with what the file gave it (review CR-05).
func TestRollbackOfAStackThatUsesAnEnvFileRestoresTheFilesValues(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rbenv")

	// Revision 1 uses env_file and ran well; revision 2 is stopped, so what last ran well is 1.
	f.deployFilesGood(t, operator, st.ID, envFileFiles(composeEnvFile)...)
	f.deployThenCancel(t, operator, st, composeWeb128)
	updates := len(f.fake.ServiceUpdates())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != updates+1 {
		t.Fatalf("service updates = %d, want one more than %d", len(ups), updates)
	}
	if env := ups[len(ups)-1].Spec.TaskTemplate.ContainerSpec.Env; !hasEnv(env, "FROM_FILE=1") {
		t.Errorf("env = %v, want the env file's FROM_FILE=1 back", env)
	}
}

// A revision the check refuses now is refused by the roll back with the check's own masked problems, once.
func TestRollbackOfARevisionTheCheckRefusesAnswersStackInvalid(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rbbad")
	f.deployFilesGood(t, operator, st.ID, envFileFiles(composeEnvFile)...)
	f.deployThenCancel(t, operator, st, composeWeb128)
	// The stored revision loses its env file after the fact: the check refuses it now.
	if _, err := f.st.Write.Exec(`DELETE FROM stack_snapshot_files WHERE path = 'app.env' AND snapshot_id IN
		(SELECT id FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND seq = 1)`, st.ID); err != nil {
		t.Fatal(err)
	}
	revisions := len(f.revisions(t, st.ID))
	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_invalid" {
		t.Fatalf("roll back: %d %.300s, want 422 stack_invalid", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"location":"compose.yaml`) {
		t.Errorf("answer = %.400s, want the check's problems at their file and line", rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != revisions {
		t.Errorf("revisions went from %d to %d, a refused roll back stores none", revisions, n)
	}
}

func readStack(t *testing.T, c *roleCaller, stackID string) rollbackStackAnswer {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks/"+stackID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get stack: %d %s", rec.Code, rec.Body)
	}
	return answerOf[rollbackStackAnswer](t, rec)
}

func TestRollbackWithNoEarlierVersionIsRefusedAndStoresNothing(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")

	// Never deployed, and then deployed once: neither has a version before the one that runs.
	for step, setup := range []func(){func() {}, func() { f.deployGood(t, operator, st.ID, composeWeb127) }} {
		setup()
		if a := readStack(t, operator, st.ID); a.Rollback.Available || a.Rollback.DeployedAt != "" {
			t.Fatalf("step %d: rollback = %+v, want none", step, a.Rollback)
		}
		revisions, writes := len(f.revisions(t, st.ID)), len(f.fake.Writes())
		rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
		if rec.Code != http.StatusConflict || problemType(t, rec) != "no_previous_version" {
			t.Fatalf("step %d: roll back: %d %s, want 409 no_previous_version", step, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "There's no earlier deployed version to roll back to.") {
			t.Errorf("step %d: body = %s, want the sentence", step, rec.Body)
		}
		if n := len(f.revisions(t, st.ID)); n != revisions {
			t.Errorf("step %d: revisions went from %d to %d", step, revisions, n)
		}
		if n := len(f.fake.Writes()); n != writes {
			t.Errorf("step %d: writes went from %d to %d", step, writes, n)
		}
	}
}

func TestRollbackNeedsOperatorAndStartsOnlyOnARequest(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployThenCancel(t, operator, st, composeWeb128)

	// A failed deploy only offers the way back: nothing rolls the stack back by itself (T-07-89).
	if a := readStack(t, viewer, st.ID); !a.Rollback.Available || a.State != "deploy_failed" {
		t.Fatalf("stack = %+v", a)
	}
	revisions := len(f.revisions(t, st.ID))
	if rec := viewer.do(http.MethodPost, rollbackPath(st.ID), ""); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's roll back: %d %s, want 403", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != revisions {
		t.Errorf("revisions went from %d to %d, a refused roll back stores nothing", revisions, n)
	}
}

func TestRollbackTakesTheOperationLockAndIsRefusedWhileAnotherOperationRuns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha, beta := newStack(t, admin, "alpha"), newStack(t, admin, "beta")
	f.deployGood(t, operator, alpha.ID, composeWeb127)
	f.deployThenCancel(t, operator, alpha, composeWeb128)
	jobID := startStuckDeploy(t, f, operator, beta)

	revisions, writes := len(f.revisions(t, alpha.ID)), len(f.fake.Writes())
	rec := operator.do(http.MethodPost, rollbackPath(alpha.ID), "")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Fatalf("roll back during another operation: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, alpha.ID)); n != revisions {
		t.Errorf("revisions went from %d to %d, a refused operation leaves none behind", revisions, n)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d", writes, n)
	}

	// The roll back holds the lock itself while it runs, and says so.
	if rec := operator.do(http.MethodPost, cancelPath(beta.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, jobID)
	f.fake.SetTaskState(alpha.Name+"_web", "preparing", "")
	rec = operator.do(http.MethodPost, rollbackPath(alpha.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	rbJob := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID
	awaitStackJobItem(t, f, rbJob, func(it jobs.Item) bool { return it.ID == "service:"+alpha.Name+"_web" && it.Unit == "tasks" })
	if lock := lockOf(t, operator, beta.ID); lock == nil || lock.Kind != "rollback" || lock.StackName != "alpha" || lock.JobID != rbJob {
		t.Errorf("lock = %+v, want alpha's roll back %s", lock, rbJob)
	}
	if rec := operator.deploy(beta.ID, `{"draftVersion":1}`); rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Errorf("a deploy during the roll back: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	_ = operator.do(http.MethodPost, cancelPath(alpha.ID, rbJob), "")
	f.awaitDeploy(t, rbJob)
}

func TestRollbackToARevisionOnlyAnAdminMayDeployIsRefusedForAnOperator(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "esc")

	// The Admin deployed host access; the Operator's later deploy was stopped. What last ran well is
	// the Admin's, and the Operator may not put it back (T-07-91).
	f.deployGood(t, admin, st.ID, hostAccessCompose)
	f.deployThenCancel(t, operator, st, composeWeb128)
	revisions := len(f.revisions(t, st.ID))
	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "privileged_compose" {
		t.Fatalf("an operator's roll back: %d %s, want 403 privileged_compose", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != revisions {
		t.Errorf("revisions went from %d to %d, a refused roll back stores nothing", revisions, n)
	}
	rec = admin.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("an admin's roll back: %d %s, want 202", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID); job.State != "done" {
		t.Errorf("job = %+v, want done", job)
	}
}

func TestAFailedRollbackLeavesTheWayBackOpen(t *testing.T) {
	t.Parallel()
	// While armed, the swarm rolls back the update of a service the moment it is written, as it does
	// when the new tasks fail.
	var armed atomic.Bool
	var fake atomic.Pointer[dockertest.Fake]
	f, e := stackEnv(t, func(c *dockertest.FakeConfig) {
		c.OnWrite = func(w string) {
			if fk := fake.Load(); fk != nil && armed.Load() && strings.Contains(w, "/update") {
				fk.SetUpdateState("rb_web", "rollback_completed", "update rolled back: task failed")
			}
		}
	})
	fake.Store(f.fake)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployThenCancel(t, operator, st, composeWeb128)

	// The roll back itself fails: the swarm rolls the update back.
	armed.Store(true)
	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID); job.State == "done" {
		t.Fatalf("job = %+v, want it failed", job)
	}
	revs := f.revisions(t, st.ID)
	if len(revs) != 3 || revs[2].Outcome != "failed" {
		t.Fatalf("revisions = %+v, want the roll back's revision failed", revs)
	}
	a := readStack(t, operator, st.ID)
	if a.State != "deploy_failed" || a.LastOperation == nil || a.LastOperation.Kind != "rollback" || !a.Rollback.Available {
		t.Errorf("stack = %+v %+v, want a failed roll back that can be retried", a, a.LastOperation)
	}
	// Retry: it restores the same good revision (1), not the failed copy.
	armed.Store(false)
	f.fake.SetUpdateState("rb_web", "", "")
	rec = operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID); job.State != "done" {
		t.Errorf("retry job = %+v, want done", job)
	}
}

// Rolling back to a chosen revision (plan 08-03, STK-08, D-02 to D-06, D-51).

type rollbackStarted struct {
	JobID    string `json:"jobId"`
	Revision int64  `json:"revision"`
}

// revisionOrigin is how revision seq of a stack came to be and the revision a roll back copied.
func (f *envFix) revisionOrigin(t *testing.T, stackID string, seq int64) (origin string, restoredFrom int64) {
	t.Helper()
	err := f.st.Read.QueryRow(`SELECT origin, restored_from FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND seq = ?`, stackID, seq).Scan(&origin, &restoredFrom)
	if err != nil {
		t.Fatalf("read revision %d: %v", seq, err)
	}
	return origin, restoredFrom
}

func TestRollbackToAChosenRevision(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	f.deployGood(t, operator, st.ID, composeWeb129)
	revs := f.revisions(t, st.ID)
	if len(revs) != 3 {
		t.Fatalf("revisions = %+v, want three", revs)
	}
	pin127 := revs[0].Pins
	if !strings.Contains(pin127, "nginx:1.27-alpine@sha256:") {
		t.Fatalf("revision 1 pins = %s, want the 1.27 image pinned to a digest", pin127)
	}

	// An edit saved after the last deploy is the draft's, and a roll back leaves it alone (D-03).
	edited := composeWeb129 + "# not deployed yet\n"
	saveCompose(t, operator, st.ID, edited, `[]`)
	draftBefore := answerOf[draftAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", ""))

	// The tag has moved on the registry since: a roll back must not follow it.
	f.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", "sha256:"+strings.Repeat("b", 64))
	lookups, updates := len(f.fake.DistributionRefs()), len(f.fake.ServiceUpdates())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":1,"expectedDeployed":3}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[rollbackStarted](t, rec)
	if started.Revision != 4 {
		t.Errorf("revision = %d, want 4: the roll back is a revision of its own", started.Revision)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}

	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a roll back asks no registry", lookups, n)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != updates+1 {
		t.Fatalf("service updates = %d, want one more than %d", len(ups), updates)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(pin127), &pins); err != nil {
		t.Fatal(err)
	}
	if image := ups[len(ups)-1].Spec.TaskTemplate.ContainerSpec.Image; image != pins["rb_web"] {
		t.Errorf("image = %q, want exactly what revision 1 ran, %q", image, pins["rb_web"])
	}

	revs = f.revisions(t, st.ID)
	if len(revs) != 4 || revs[3].Outcome != "good" || revs[3].Pins != pin127 {
		t.Errorf("revisions = %+v, want a good revision 4 with revision 1's pins", revs)
	}
	if origin, from := f.revisionOrigin(t, st.ID, 4); origin != "rollback" || from != 1 {
		t.Errorf("revision 4 = %q restored from %d, want rollback of 1", origin, from)
	}

	// The draft is as it was: same content, same version (D-03).
	draftAfter := answerOf[draftAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", ""))
	if draftAfter.Version != draftBefore.Version || len(draftAfter.Files) != 1 || draftAfter.Files[0].Content != edited {
		t.Errorf("draft after = %+v, want version %d and the edit untouched", draftAfter, draftBefore.Version)
	}

	job := answerOf[struct {
		Mode             string `json:"mode"`
		State            string `json:"state"`
		Revision         int64  `json:"revision"`
		RecordedRevision int64  `json:"recordedRevision"`
	}](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+started.JobID, ""))
	if job.Mode != "rollback" || job.Revision != 1 || job.RecordedRevision != 4 {
		t.Errorf("job = %+v, want a roll back that restores 1 and was recorded as 4", job)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.deployed" {
			row = &r
		}
	}
	if row == nil || row.Details["mode"] != "rollback" || row.Details["revision"] != "4" || row.Details["restoredFrom"] != "1" || row.Result != "success" {
		t.Errorf("stack.deployed = %+v, want a successful rollback of revision 4 restoring 1", row)
	}
}

func TestRollbackToTheDeployedRevisionIsAllowed(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	// A newer attempt failed: the deployed revision is still 2.
	f.deployThenCancel(t, operator, st, composeWeb129)

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":2,"expectedDeployed":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back to the deployed revision: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[rollbackStarted](t, rec)
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" || started.Revision != 4 {
		t.Fatalf("job = %+v revision %d, want it done as revision 4", job, started.Revision)
	}
	if origin, from := f.revisionOrigin(t, st.ID, 4); origin != "rollback" || from != 2 {
		t.Errorf("revision 4 = %q restored from %d, want rollback of 2", origin, from)
	}
}

func TestRollbackRefusesAFailedRevision(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployThenCancel(t, operator, st, composeWeb128)
	revs := f.revisions(t, st.ID)
	if len(revs) != 2 || revs[1].Outcome != "failed" {
		t.Fatalf("revisions = %+v, want a good one and a failed one", revs)
	}
	writes := len(f.fake.Writes())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":2}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "revision_not_good" ||
		!strings.Contains(rec.Body.String(), "That revision can't be rolled back to. Pick a revision that finished well.") {
		t.Fatalf("roll back to a failed revision: %d %s, want 409 revision_not_good", rec.Code, rec.Body)
	}
	rec = operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":99}`)
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "revision_not_found" {
		t.Fatalf("roll back to revision 99: %d %s, want 404 revision_not_found", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != 2 {
		t.Errorf("revisions = %d, a refused roll back stores none", n)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d, a refused roll back writes nothing", writes, n)
	}
}

func TestRollbackDeployedChanged(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	writes := len(f.fake.Writes())

	// The review was made when revision 1 was the deployed one; revision 2 has been deployed since.
	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":1,"expectedDeployed":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "deployed_changed" ||
		!strings.Contains(rec.Body.String(), "rb was deployed again since you opened this review. Review it again.") {
		t.Fatalf("roll back: %d %s, want 409 deployed_changed", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != 2 {
		t.Errorf("revisions = %d, a refused roll back stores none", n)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d, a refused roll back writes nothing", writes, n)
	}
}

func TestRollbackWithoutABodyKeepsThePreviousVersion(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)

	// The previous version of the deployed revision 2 is revision 1, whether the body is absent or empty.
	for i, body := range []string{"", `{}`} {
		rec := operator.do(http.MethodPost, rollbackPath(st.ID), body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("roll back %d: %d %s", i, rec.Code, rec.Body)
		}
		started := answerOf[rollbackStarted](t, rec)
		if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
			t.Fatalf("roll back %d job = %+v", i, job)
		}
		if origin, _ := f.revisionOrigin(t, st.ID, started.Revision); origin != "rollback" {
			t.Errorf("revision %d origin = %q, want rollback", started.Revision, origin)
		}
		// The first restores revision 1; the second, with revision 3 (a copy of 1) now deployed, restores 2.
		wantFrom := []int64{1, 2}[i]
		if _, from := f.revisionOrigin(t, st.ID, started.Revision); from != wantFrom {
			t.Errorf("roll back %d restored %d, want %d", i, from, wantFrom)
		}
	}
	// Revision 4 is deployed now, so the one a roll back with no body would restore is the good one before it, 3.
	if a := readStack(t, operator, st.ID); !a.Rollback.Available || a.Rollback.Revision != 3 {
		t.Errorf("rollback = %+v, want revision 3 as the one a roll back would restore", a.Rollback)
	}
}

// After a roll back the draft is told the deployed version moved (plan 08-03, D-03, P7 D-09): its next review
// carries the concurrent notice and a deploy of it needs the acknowledgement. Its Draft badge stays.
func TestDeployAfterRollbackNeedsAcknowledgement(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	f.deployGood(t, operator, st.ID, composeWeb129)
	// An edit saved after the last deploy and before the roll back.
	v := saveCompose(t, operator, st.ID, composeWeb129+"# edited\n", `[]`)
	if readStack(t, operator, st.ID).DraftSavedAt == "" {
		t.Fatal("the saved edit does not read as a draft before the roll back")
	}

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":1,"expectedDeployed":3}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}

	if readStack(t, operator, st.ID).DraftSavedAt == "" {
		t.Error("a roll back hid the Draft badge of a draft that holds undeployed changes")
	}
	rv, code, body := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, body)
	}
	if rv.Concurrent == nil || rv.Concurrent.Revision != 4 || rv.Concurrent.DeployedBy != "operator-1" {
		t.Fatalf("concurrent = %+v, want the roll back (revision 4) as a deployment since the draft started", rv.Concurrent)
	}
	rec = operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "concurrent_unacknowledged" {
		t.Fatalf("deploy of the draft: %d %s, want 409 concurrent_unacknowledged", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != 4 {
		t.Errorf("revisions = %d, a refused deploy stores none", n)
	}
	rec = operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`,"acknowledgeConcurrent":true}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("deploy with the acknowledgement: %d %s, want 202", rec.Code, rec.Body)
	}
}

// A standalone stack rolls back to any good revision through the Compose SDK with the images that revision stored
// (plan 08-05, STK-08, D-02, D-03, D-06, T-08-18).
func TestStandaloneRollbackToAChosenRevision(t *testing.T) {
	t.Parallel()
	f, e, up := standaloneEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	f.deployGood(t, operator, st.ID, composeWeb129)
	revs := f.revisions(t, st.ID)
	if len(revs) != 3 {
		t.Fatalf("revisions = %+v, want three", revs)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(revs[0].Pins), &pins); err != nil || !strings.HasPrefix(pins["web"], "nginx:1.27-alpine@sha256:") {
		t.Fatalf("revision 1 pins = %s (%v), want the 1.27 image pinned to a digest", revs[0].Pins, err)
	}

	// An edit saved after the last deploy is the draft's, and a roll back leaves it alone (D-03).
	edited := composeWeb129 + "# not deployed yet\n"
	saveCompose(t, operator, st.ID, edited, `[]`)
	draftBefore := answerOf[draftAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", ""))

	// The tag has moved on the registry since: a roll back must not follow it.
	f.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", "sha256:"+strings.Repeat("b", 64))
	lookups := len(f.fake.DistributionRefs())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"revision":1,"expectedDeployed":3}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[rollbackStarted](t, rec)
	if started.Revision != 4 {
		t.Errorf("revision = %d, want 4: the roll back is a revision of its own", started.Revision)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v, want it done", job)
	}

	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a roll back asks no registry", lookups, n)
	}
	project, calls := up.lastProject()
	if project == nil || calls != 4 || project.Services["web"].Image != pins["web"] {
		t.Fatalf("the Compose engine ran %d times with %+v, want a fourth run with exactly what revision 1 ran, %q", calls, project, pins["web"])
	}
	revs = f.revisions(t, st.ID)
	if len(revs) != 4 || revs[3].Outcome != "good" || revs[3].Pins != revs[0].Pins {
		t.Errorf("revisions = %+v, want a good revision 4 with revision 1's pins", revs)
	}
	if origin, from := f.revisionOrigin(t, st.ID, 4); origin != "rollback" || from != 1 {
		t.Errorf("revision 4 = %q restored from %d, want rollback of 1", origin, from)
	}

	// The draft is as it was: same content, same version (D-03).
	draftAfter := answerOf[draftAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", ""))
	if draftAfter.Version != draftBefore.Version || len(draftAfter.Files) != 1 || draftAfter.Files[0].Content != edited {
		t.Errorf("draft after = %+v, want version %d and the edit untouched", draftAfter, draftBefore.Version)
	}

	job := answerOf[struct {
		Mode             string `json:"mode"`
		State            string `json:"state"`
		Revision         int64  `json:"revision"`
		RecordedRevision int64  `json:"recordedRevision"`
	}](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+started.JobID, ""))
	if job.Mode != "rollback" || job.Revision != 1 || job.RecordedRevision != 4 {
		t.Errorf("job = %+v, want a roll back that restores 1 and was recorded as 4", job)
	}
}

// The Phase 7 one-click roll back: after a deploy that failed on a standalone host, a roll back with no body
// restores the revision that last ran well.
func TestStandaloneOneClickRollback(t *testing.T) {
	t.Parallel()
	f, e, up := standaloneEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rb")
	f.deployGood(t, operator, st.ID, composeWeb127)
	up.failWith(errors.New("Error response from daemon: port is already allocated"))
	if job := f.awaitDeploy(t, f.deployDraft(t, operator, st.ID, composeWeb128)); job.State == "done" {
		t.Fatalf("deploy = %+v, want it failed", job)
	}
	revs := f.revisions(t, st.ID)
	if len(revs) != 2 || revs[1].Outcome != "failed" {
		t.Fatalf("revisions = %+v, want a good one and a failed one", revs)
	}
	up.failWith(nil)

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[rollbackStarted](t, rec)
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" || started.Revision != 3 {
		t.Fatalf("job = %+v revision %d, want it done as revision 3", job, started.Revision)
	}
	if origin, from := f.revisionOrigin(t, st.ID, 3); origin != "rollback" || from != 1 {
		t.Errorf("revision 3 = %q restored from %d, want rollback of 1", origin, from)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(revs[0].Pins), &pins); err != nil {
		t.Fatal(err)
	}
	if project, _ := up.lastProject(); project == nil || project.Services["web"].Image != pins["web"] {
		t.Errorf("the Compose engine was given %+v, want revision 1's image %q", project, pins["web"])
	}
	if a := readStack(t, operator, st.ID); a.State == "deploy_failed" {
		t.Errorf("stack = %+v, want the failed state gone after the roll back", a)
	}
}

// Retention (plan 08-06, STK-08, D-01): a deploy that ends prunes its stack to the instance's limit.

func seqsOfRows(rows []revisionRow) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Seq)
	}
	return out
}

func TestDeployPrunesTheStack(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	operator := f.caller(t, e, "operator-1", "operator")
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "keep")
	f.inst.ApplyStackRevisionsKept(5)

	// Seven good deploys: the sixth takes the first revision out of the window, the seventh the second.
	composes := []string{composeWeb127, composeWeb128, composeWeb129}
	for i := 0; i < 7; i++ {
		f.deployGood(t, operator, st.ID, composes[i%len(composes)])
	}
	if got := seqsOfRows(f.revisions(t, st.ID)); !slices.Equal(got, []int64{3, 4, 5, 6, 7}) {
		t.Fatalf("revisions after seven deploys with 5 kept = %v, want 3 to 7", got)
	}

	// A deploy that ends failed prunes too, and the deployed revision (#7) stays whatever happens to the window.
	f.deployThenCancel(t, operator, st, composeWeb127)
	rows := f.revisions(t, st.ID)
	if got := seqsOfRows(rows); !slices.Equal(got, []int64{4, 5, 6, 7, 8}) || rows[len(rows)-1].Outcome != "failed" {
		t.Fatalf("revisions after a failed deploy = %+v, want 4 to 8 with #8 failed", rows)
	}

	// Raising the limit keeps what is there; nothing comes back and nothing more goes.
	f.inst.ApplyStackRevisionsKept(50)
	f.deployGood(t, operator, st.ID, composeWeb128)
	if got := seqsOfRows(f.revisions(t, st.ID)); !slices.Equal(got, []int64{4, 5, 6, 7, 8, 9}) {
		t.Fatalf("revisions after raising the limit = %v, want 4 to 9", got)
	}
}

func TestDeployedRevisionOutlastsTheWindowAfterFailures(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	operator := f.caller(t, e, "operator-1", "operator")
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "pinned")
	f.inst.ApplyStackRevisionsKept(5)

	f.deployGood(t, operator, st.ID, composeWeb127)
	for i := 0; i < 5; i++ {
		f.deployThenCancel(t, operator, st, composeWeb128+"# "+itoaT(int64(i))+"\n")
	}
	// Revision 1 is the newest good one: six revisions are left, the five newest failures and the deployed one.
	rows := f.revisions(t, st.ID)
	if got := seqsOfRows(rows); !slices.Equal(got, []int64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("revisions = %+v, want 1 to 6: five in the window and the deployed #1", rows)
	}
	f.deployThenCancel(t, operator, st, composeWeb128+"# six\n")
	rows = f.revisions(t, st.ID)
	if got := seqsOfRows(rows); !slices.Equal(got, []int64{1, 3, 4, 5, 6, 7}) || rows[0].Outcome != "good" {
		t.Fatalf("revisions = %+v, want the deployed #1 kept and #2 pruned", rows)
	}
}

// adoptedLegacy adopts the stack legacy that docker stack deploy left on the fake swarm, as an environment admin, and
// answers the stack and the version of its draft.
func adoptedLegacy(t *testing.T, f *envFix, e envBody, admin *roleCaller) (stackAnswer, int64) {
	t.Helper()
	// One service as docker stack deploy leaves it, on the stack's default network, with no secret or config to find.
	replicas := uint64(2)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "legacy_app", Labels: map[string]string{stacks.LabelSwarmNamespace: "legacy", stacks.LabelSwarmImage: "nginx:1.27"}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:      "nginx:1.27@" + dockertest.FakeDigestOf("docker.io/library/nginx:1.27"),
				Labels:     map[string]string{stacks.LabelSwarmNamespace: "legacy"},
				Privileges: &swarm.Privileges{},
			},
			Networks: []swarm.NetworkAttachmentConfig{{Target: "net-legacy-default", Aliases: []string{"app"}}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}
	svc := foreignSwarmService("legacy", "app", 2, 2, nil)
	svc.Spec = &spec
	cfg := swarmManagerEngine(svc)
	cfg.TasksFollowWrites = true
	cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{
		ID: "net-legacy-default", Name: "legacy_default", Driver: "overlay", Scope: "swarm",
		Labels: map[string]string{stacks.LabelSwarmNamespace: "legacy"},
	})
	f.fake.Reconfigure(cfg)
	if err := f.clusters.Refresh(testsupport.Context(t), e.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rec := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fingerprintOf(t, admin, "legacy")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body)
	}
	st := answerOf[stackAnswer](t, rec)
	draft := admin.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	if draft.Code != http.StatusOK {
		t.Fatalf("draft: %d %s", draft.Code, draft.Body)
	}
	return st, answerOf[draftAnswer](t, draft).Version
}

// adoptionPending reads whether the stack's adoption is still pending from the store.
func adoptionPending(t *testing.T, f *envFix, envID, stackID string) bool {
	t.Helper()
	st, err := (stackops.Repo{Store: f.st, Vault: f.v}).Stack(testsupport.Context(t), envID, stackID)
	if err != nil {
		t.Fatal(err)
	}
	return st.Adoption.Pending
}

// TestDeployEndClearsThePendingAdoption: the first deploy of an adopted stack ends the pending window when it ends
// good, and only then; a deploy that stops leaves the adoption pending, since not every service carries Bayplan's
// labels yet (D-16).
func TestDeployEndClearsThePendingAdoption(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st, _ := adoptedLegacy(t, f, e, admin)
	if !adoptionPending(t, f, e.ID, st.ID) {
		t.Fatal("a stack just adopted is not pending")
	}
	detail := func() *StackAdoptionAnswer {
		return answerOf[struct {
			Adoption *StackAdoptionAnswer `json:"adoption"`
		}](t, admin.do(http.MethodGet, "stacks/"+st.ID, "")).Adoption
	}
	if a := detail(); a == nil || !a.Pending || a.AdoptedFrom != "legacy" {
		t.Fatalf("adoption of the stack = %+v, want pending, adopted from legacy", a)
	}

	// The first review names what taking ownership does: one service, ownership labels only, none changed.
	rec := admin.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	rv := answerOf[struct {
		Problems []struct{ Code string } `json:"problems"`
		Summary  struct {
			Services      struct{ Changed int } `json:"services"`
			OwnershipOnly int                   `json:"ownershipOnly"`
		} `json:"summary"`
		Services []struct {
			Short         string `json:"short"`
			OwnershipOnly bool   `json:"ownershipOnly"`
		} `json:"services"`
		Adoption *struct {
			Pending       bool `json:"pending"`
			OwnershipOnly int  `json:"ownershipOnly"`
		} `json:"adoption"`
	}](t, rec)
	if len(rv.Problems) != 0 || rv.Summary.OwnershipOnly != 1 || rv.Summary.Services.Changed != 0 ||
		len(rv.Services) != 1 || !rv.Services[0].OwnershipOnly || rv.Adoption == nil || !rv.Adoption.Pending || rv.Adoption.OwnershipOnly != 1 {
		t.Fatalf("review = %+v, want no problem and the one service ownership-only", rv)
	}

	// A deploy that stops before it has labelled every service ends failed: here at the networks, for an external
	// network that does not exist.
	draft := answerOf[draftAnswer](t, admin.do(http.MethodGet, "stacks/"+st.ID+"/draft", ""))
	rebuilt := draft.Files[0].Content
	broken := rebuilt + "\nnetworks:\n  ghost:\n    external: true\n"
	broken = strings.Replace(broken, "    image: nginx:1.27", "    networks: [default, ghost]\n    image: nginx:1.27", 1)
	jobID := f.deployDraft(t, admin, st.ID, broken)
	if job := f.awaitDeploy(t, jobID); job.State == "done" {
		t.Fatalf("job = %+v, want it stopped", job)
	}
	if !adoptionPending(t, f, e.ID, st.ID) {
		t.Fatal("a deploy that failed ended the pending adoption")
	}

	// The file as it was adopted, deployed, ends good.
	f.deployGood(t, admin, st.ID, rebuilt)
	if adoptionPending(t, f, e.ID, st.ID) {
		t.Fatal("a good first deploy left the adoption pending")
	}
	if a := detail(); a == nil || a.Pending {
		t.Fatalf("adoption of the stack after the deploy = %+v, want it present and not pending", a)
	}
}

// StackAdoptionAnswer is the adoption part of a stack's detail.
type StackAdoptionAnswer struct {
	Pending     bool   `json:"pending"`
	AdoptedFrom string `json:"adoptedFrom"`
}

// The background drift check is asked for a stack when an operation on it ends (plan 08-21, D-22), and by then the
// environment's lock is free, or the worker would find it held and have nothing to do until the next timed look.
func TestStackDeployAsksForADriftRecheckWhenTheOperationEnds(t *testing.T) {
	t.Parallel()
	type ask struct {
		env, stack string
		// locked is what the stack's page said about the operation lock at the moment of the call.
		locked bool
		err    string
	}
	asks := make(chan ask, 8)
	var reader atomic.Pointer[roleCaller]
	f, e := stackEnvTuned(t, func(f *envFix) {
		f.stackDriftRecheck = func(envID, stackID string) {
			a := ask{env: envID, stack: stackID}
			c := reader.Load()
			if c == nil {
				a.err = "no reader"
				asks <- a
				return
			}
			rec := c.do(http.MethodGet, "stacks/"+stackID, "")
			var page struct {
				Lock *lockAnswer `json:"lock"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != http.StatusOK {
				a.err = rec.Body.String()
			}
			a.locked = page.Lock != nil
			asks <- a
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	reader.Store(operator)
	st := newStack(t, admin, "recheck")

	f.deployGood(t, operator, st.ID, composeWeb127)
	got := testsupport.Await(t, "the recheck after the deploy", asks)
	if got.env != e.ID || got.stack != st.ID || got.locked || got.err != "" {
		t.Errorf("after the deploy: %+v, want this stack on this environment with the lock already free", got)
	}

	// A stop is an operation too.
	if _, job := f.lifecycle(t, operator, stopPath(st.ID)); job.State != "done" {
		t.Fatalf("stop = %+v", job)
	}
	got = testsupport.Await(t, "the recheck after the stop", asks)
	if got.stack != st.ID || got.locked || got.err != "" {
		t.Errorf("after the stop: %+v, want this stack with the lock already free", got)
	}
}

// Restoring the deployed revision (plan 08-23, STK-14, D-23): the roll back route in mode restore.

func TestRestoreRedeploysTheDeployedRevision(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rs")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	pin128 := f.revisions(t, st.ID)[1].Pins
	// Somebody outside Bayplan scaled the service, and the tag has moved on the registry since the deploy.
	f.editLiveService(t, e, "rs", "web", scaleTo(4))
	f.fake.SetDistributionDigest("docker.io/library/nginx:1.28-alpine", "sha256:"+strings.Repeat("b", 64))
	lookups, updates := len(f.fake.DistributionRefs()), len(f.fake.ServiceUpdates())

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"mode":"restore","revision":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	started := answerOf[rollbackStarted](t, rec)
	if started.Revision != 3 {
		t.Errorf("revision = %d, want 3: the restore is a revision of its own", started.Revision)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("restore job = %+v, want it done", job)
	}

	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a restore asks no registry", lookups, n)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != updates+1 {
		t.Fatalf("service updates = %d, want one more than %d", len(ups), updates)
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(pin128), &pins); err != nil {
		t.Fatal(err)
	}
	last := ups[len(ups)-1].Spec
	if image := last.TaskTemplate.ContainerSpec.Image; image != pins["rs_web"] {
		t.Errorf("image = %q, want exactly what the deployed revision ran, %q", image, pins["rs_web"])
	}
	if last.Mode.Replicated == nil || last.Mode.Replicated.Replicas == nil || *last.Mode.Replicated.Replicas != 1 {
		t.Errorf("replicas = %+v, want the deployed 1 put back", last.Mode.Replicated)
	}

	revs := f.revisions(t, st.ID)
	if len(revs) != 3 || revs[2].Outcome != "good" || revs[2].Pins != pin128 {
		t.Errorf("revisions = %+v, want a good revision 3 with the deployed revision's pins", revs)
	}
	if origin, from := f.revisionOrigin(t, st.ID, 3); origin != "rollback" || from != 2 {
		t.Errorf("revision 3 = %q restored from %d, want rollback of 2", origin, from)
	}
	job := answerOf[struct {
		Mode             string `json:"mode"`
		Revision         int64  `json:"revision"`
		RecordedRevision int64  `json:"recordedRevision"`
	}](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+started.JobID, ""))
	if job.Mode != "restore" || job.Revision != 2 || job.RecordedRevision != 3 {
		t.Errorf("job = %+v, want a restore of 2 recorded as 3", job)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.deployed" {
			row = &r
		}
	}
	if row == nil || row.Details["mode"] != "restore" || row.Details["revision"] != "3" || row.Details["restoredFrom"] != "2" || row.Result != "success" {
		t.Errorf("stack.deployed = %+v, want a successful restore recorded as revision 3, restoring 2", row)
	}
}

func TestRestoreRefusesAnotherRevision(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "ra")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	revisions, writes := len(f.revisions(t, st.ID)), len(f.fake.Writes())

	// Revision 1 is not the deployed one: a restore puts the deployed revision back and no other.
	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"mode":"restore","revision":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "deployed_changed" {
		t.Errorf("restore of revision 1: %d %s, want 409 deployed_changed", rec.Code, rec.Body)
	}
	rec = operator.do(http.MethodPost, rollbackPath(st.ID), `{"mode":"restore"}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "revision_required" {
		t.Errorf("restore with no revision: %d %s, want 422 revision_required", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != revisions {
		t.Errorf("revisions went from %d to %d, a refused restore stores none", revisions, n)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d, a refused restore writes nothing", writes, n)
	}
}

func TestRestoreOfAStackNeverDeployedIsRefused(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "rz")

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"mode":"restore","revision":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "no_deployed_revision" {
		t.Errorf("restore of a stack never deployed: %d %s, want 409 no_deployed_revision", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, st.ID)); n != 0 {
		t.Errorf("revisions = %d, a refused restore stores none", n)
	}
}

func TestRestoreIsRefusedForHostAccessBelowAdminAndWhileAnotherOperationRuns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	esc, other, busy := newStack(t, admin, "esc"), newStack(t, admin, "alpha"), newStack(t, admin, "beta")
	f.deployGood(t, admin, esc.ID, hostAccessCompose)
	f.deployGood(t, operator, other.ID, composeWeb127)
	revisions := len(f.revisions(t, esc.ID))

	if rec := viewer.do(http.MethodPost, rollbackPath(other.ID), `{"mode":"restore","revision":1}`); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's restore: %d %s, want 403", rec.Code, rec.Body)
	}
	rec := operator.do(http.MethodPost, rollbackPath(esc.ID), `{"mode":"restore","revision":1}`)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "privileged_compose" {
		t.Errorf("an operator's restore of host access: %d %s, want 403 privileged_compose", rec.Code, rec.Body)
	}
	if n := len(f.revisions(t, esc.ID)); n != revisions {
		t.Errorf("revisions went from %d to %d, a refused restore stores none", revisions, n)
	}

	jobID := startStuckDeploy(t, f, operator, busy)
	rec = operator.do(http.MethodPost, rollbackPath(other.ID), `{"mode":"restore","revision":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Errorf("a restore during another operation: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	if rec := operator.do(http.MethodPost, cancelPath(busy.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, jobID)
}
