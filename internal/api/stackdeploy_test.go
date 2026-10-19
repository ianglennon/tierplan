// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The deploy console follows a deploy over server-sent events (plan 07-17, STK-07, D-13). The hub is
// real; the job under test is driven by the test, so every frame is awaited in order and nothing
// depends on a clock.

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
		<-addRow
		r.Add(jobs.ItemSpec{ID: "service:web_api", Label: "api", Parent: "services", Kind: "service"})
		<-finishRow
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
