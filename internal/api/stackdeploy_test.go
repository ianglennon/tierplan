// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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
	const secret = "tok-9f8e7d6c5b4a"
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) {
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
	for _, body := range []string{read, snapshot} {
		if strings.Contains(body, secret) {
			t.Fatalf("a secret reached the console: %s", body)
		}
		if !strings.Contains(body, "TOKEN=<hidden> is not allowed") {
			t.Errorf("body = %s, want Docker's words with the secret hidden, in a detail and in the output", body)
		}
	}
	var job struct {
		Items []struct {
			ID, Detail, Message, Code string
		} `json:"items"`
		Output []struct {
			Seq  uint64 `json:"seq"`
			Text string `json:"text"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(read), &job); err != nil {
		t.Fatal(err)
	}
	var failed bool
	for _, it := range job.Items {
		if it.ID == "service:shop_web" && it.Code == "service_rejected" && strings.Contains(it.Detail, "TOKEN=<hidden>") {
			failed = true
		}
		if strings.Contains(it.Message, "TOKEN") {
			t.Errorf("item %s message %q holds Docker's words", it.ID, it.Message)
		}
	}
	if !failed {
		t.Errorf("items = %+v, want the service row failed with Docker's words in its detail", job.Items)
	}
	if len(job.Output) == 0 || job.Output[0].Seq == 0 {
		t.Errorf("output = %+v, want the lines the deploy wrote, each with its sequence", job.Output)
	}
}

func TestStackJobStreamCarriesTheOutputLogAndResumesIt(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
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
