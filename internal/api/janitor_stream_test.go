// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// Following a cleanup while it works (plan 11-14, JAN-09, D-14): a janitor run is a job like a
// deploy, and its stream is the shared job stream. The engines are fake daemons behind real agent
// sessions; the route, the jobs hub and the runner are the real ones.

// janitorFrame is the job as the run stream sends it in its snapshot and done frames.
type janitorFrame struct {
	ID         string `json:"id"`
	RunID      string `json:"runId"`
	State      string `json:"state"`
	Trigger    string `json:"trigger"`
	By         string `json:"by"`
	FinishedAt string `json:"finishedAt"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	Items      []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Kind  string `json:"kind"`
		State string `json:"state"`
		Code  string `json:"code"`
	} `json:"items"`
}

func TestJanitorRunStream(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0], preview.Items[1]))
	path := "/api/env/" + s.env.ID + "/janitor/runs/" + started.RunID + "/stream"

	c := openSSE(t, s.authFixture, path, s.cookie, "")
	if c.status0() != http.StatusOK || c.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status = %d headers = %v", c.status0(), c.header)
	}
	first := nextFrame(t, c)
	var snap janitorFrame
	if first.event != "snapshot" || first.id != "" || json.Unmarshal([]byte(first.data), &snap) != nil ||
		snap.ID != started.JobID || snap.RunID != started.RunID || snap.Trigger != "manual" || snap.By != "admin" {
		t.Fatalf("first frame = %+v, want the snapshot of job %s without an event id", first, started.JobID)
	}

	var last uint64
	var items int
	var done janitorFrame
	for {
		fr := nextFrame(t, c)
		if fr.id != "" {
			n, err := strconv.ParseUint(fr.id, 10, 64)
			if err != nil || n <= last {
				t.Fatalf("frame id %q after %d: ids must strictly increase", fr.id, last)
			}
			last = n
		}
		if fr.event == "item" {
			items++
			continue
		}
		if fr.event != "done" {
			t.Fatalf("unexpected frame %+v", fr)
		}
		if err := json.Unmarshal([]byte(fr.data), &done); err != nil {
			t.Fatalf("done frame %q: %v", fr.data, err)
		}
		break
	}
	if items == 0 {
		t.Error("no item frame arrived before the done frame")
	}
	if done.State != jobs.JobDone || done.Total != 2 || done.Done != 2 || len(done.Items) != 2 || done.FinishedAt == "" {
		t.Fatalf("done job = %+v, want two removals, both ended", done)
	}
	for _, it := range done.Items {
		if it.State != jobs.ItemDone || it.Kind != "container" || it.Label == "" {
			t.Errorf("item = %+v, want a done container with its name", it)
		}
	}
	c.awaitEnd(t, "the done frame")
}

// A reader that comes back after the run ended is told how it ended, by the done frame alone.
func TestJanitorRunStreamAfterTheEnd(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	job := s.awaitRun(t, started.JobID)

	c := openSSE(t, s.authFixture, "/api/env/"+s.env.ID+"/janitor/runs/"+started.RunID+"/stream", s.cookie, strconv.Itoa(1<<30))
	if fr := nextFrame(t, c); fr.event != "snapshot" {
		t.Fatalf("first frame = %+v, want the snapshot", fr)
	}
	fr := nextFrame(t, c)
	var done janitorFrame
	if fr.event != "done" || json.Unmarshal([]byte(fr.data), &done) != nil || done.ID != job.ID || done.State != jobs.JobDone {
		t.Fatalf("second frame = %+v, want the done frame", fr)
	}
	c.awaitEnd(t, "the done frame")
}

// T-11-39: only a run of this environment, with a janitor job of this environment, can be followed;
// everything else is a 404 that does not say which, and a person without the permission reads nothing.
func TestJanitorRunStreamNotFound(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	s.awaitRun(t, started.JobID)
	other := s.srv.NewEnvironment(t, "Other")
	store := &janitor.SQLStore{St: s.st}

	// A run whose job the hub no longer holds, one that never had a job, and one that points at a job
	// of another kind or of another environment.
	otherJob, err := s.jobHub.Start(jobs.Spec{Kind: janitor.JobKind, EnvID: other.ID, Owner: "user:x"}, func(ctx context.Context, _ *jobs.Reporter) { <-ctx.Done() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.jobHub.Cancel(otherJob.ID) })
	pullJob, err := s.jobHub.Start(jobs.Spec{Kind: "pull", EnvID: s.env.ID, Owner: "user:x"}, func(ctx context.Context, _ *jobs.Reporter) { <-ctx.Done() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.jobHub.Cancel(pullJob.ID) })
	for id, jobID := range map[string]string{
		"gone-job": "no-such-job", "no-job": "", "foreign-job": otherJob.ID, "other-kind": pullJob.ID,
	} {
		seedRun(t, store, janitor.Run{ID: id, EnvID: s.env.ID, Trigger: janitor.TriggerManual, Actor: "id-admin", State: janitor.StateCompleted, JobID: jobID, StartedAt: s.clk.now(), EstBytes: -1})
		rec := s.call(t, authReq{method: http.MethodGet, path: "/api/env/" + s.env.ID + "/janitor/runs/" + id + "/stream", cookie: s.cookie, csrf: s.csrf})
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "janitor_job_not_found" {
			t.Errorf("%s: %d %s, want 404 janitor_job_not_found", id, rec.Code, rec.Body)
		}
	}
	for name, path := range map[string]string{
		"an unknown run":            "/api/env/" + s.env.ID + "/janitor/runs/no-such-run/stream",
		"another environment's run": "/api/env/" + other.ID + "/janitor/runs/" + started.RunID + "/stream",
	} {
		rec := s.call(t, authReq{method: http.MethodGet, path: path, cookie: s.cookie, csrf: s.csrf})
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "janitor_run_not_found" {
			t.Errorf("%s: %d %s, want 404 janitor_run_not_found", name, rec.Code, rec.Body)
		}
	}

	path := "/api/env/" + s.env.ID + "/janitor/runs/" + started.RunID + "/stream"
	if rec := s.call(t, authReq{method: http.MethodGet, path: path}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
	s.addUser(t, "nell", false)
	cookie, csrf := s.signIn(t, "nell", false)
	if rec := s.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf}); rec.Code == http.StatusOK {
		t.Errorf("a person with no role on the environment followed the run: %d %s", rec.Code, rec.Body)
	}
	s.addUser(t, "vera", false)
	s.addEnvRole(t, "vera", s.env.ID, "viewer")
	cookie, _ = s.signIn(t, "vera", false)
	c := openSSE(t, s.authFixture, path, cookie, "")
	if c.status0() != http.StatusOK {
		t.Errorf("a Viewer following the run: %d, want 200", c.status0())
	}
}

// janitorJobRow is a row of GET /api/janitor-jobs.
type janitorJobRow struct {
	JobID     string `json:"jobId"`
	RunID     string `json:"runId"`
	EnvID     string `json:"envId"`
	State     string `json:"state"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Trigger   string `json:"trigger"`
	By        string `json:"by"`
	StartedAt string `json:"startedAt"`
}

func (s *swarmFix) janitorJobs(t *testing.T, cookie, csrf string) []janitorJobRow {
	t.Helper()
	rec := s.call(t, authReq{method: http.MethodGet, path: "/api/janitor-jobs", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("list janitor jobs: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Jobs []janitorJobRow `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Jobs
}

// D-13, T-11-39: the Jobs indicator lists the cleanups of the environments the caller may read, and
// only those.
func TestJanitorJobsListsTheCleanupsTheCallerMayRead(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0], preview.Items[1]))
	s.awaitRun(t, started.JobID)

	rows := s.janitorJobs(t, s.cookie, s.csrf)
	if len(rows) != 1 {
		t.Fatalf("jobs = %+v, want the one cleanup", rows)
	}
	r := rows[0]
	if r.JobID != started.JobID || r.RunID != started.RunID || r.EnvID != s.env.ID || r.State != jobs.JobDone ||
		r.Done != 2 || r.Total != 2 || r.Trigger != "manual" || r.By != "admin" || r.StartedAt == "" {
		t.Fatalf("job = %+v, want the done manual cleanup of two removals by admin", r)
	}

	// A person with no role on the environment sees none, and one who may read it sees the same row.
	s.addUser(t, "nell", false)
	cookie, csrf := s.signIn(t, "nell", false)
	if rows := s.janitorJobs(t, cookie, csrf); len(rows) != 0 {
		t.Errorf("a person with no role saw %+v, want none", rows)
	}
	s.addUser(t, "vera", false)
	s.addEnvRole(t, "vera", s.env.ID, "viewer")
	cookie, csrf = s.signIn(t, "vera", false)
	if rows := s.janitorJobs(t, cookie, csrf); len(rows) != 1 || rows[0].JobID != started.JobID {
		t.Errorf("a Viewer saw %+v, want the cleanup", rows)
	}
	if rec := s.call(t, authReq{method: http.MethodGet, path: "/api/janitor-jobs"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}
