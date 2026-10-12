// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// Pull jobs (plan 05-01, RES-09, D-25, D-26, D-30, D-31). Docker is a fake daemon behind
// the real monitor, snapshot cache and aggregator; the router, policy, CSRF guard, job hub
// and audit writer are the real ones.

const pullRef = "docker.io/library/busybox:1.36.1"

type pullStarted struct {
	JobID      string          `json:"jobId"`
	Ref        string          `json:"ref"`
	Credential json.RawMessage `json:"credential"`
	Nodes      []struct {
		NodeID string `json:"nodeId"`
		Name   string `json:"name"`
	} `json:"nodes"`
}

type pullItem struct {
	NodeID   string `json:"nodeId"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Current  int64  `json:"current"`
	Total    int64  `json:"total"`
	Done     int    `json:"done"`
	Count    int    `json:"count"`
	Digest   string `json:"digest"`
	UpToDate bool   `json:"upToDate"`
	Layers   []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"layers"`
}

type pullJob struct {
	ID         string     `json:"id"`
	EnvID      string     `json:"envId"`
	Ref        string     `json:"ref"`
	State      string     `json:"state"`
	FinishedAt string     `json:"finishedAt"`
	Items      []pullItem `json:"items"`
}

// startPull posts a pull as the signed-in admin once the monitor has seen the node online.
func (f *envFix) startPull(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/images/pull", body)
}

func (f *envFix) getPull(t *testing.T, id string) *envRec {
	t.Helper()
	return f.req(t, http.MethodGet, "/api/pulls/"+id, "")
}

// pullDone waits for the job to end and returns it.
func (f *envFix) pullDone(t *testing.T, id string) pullJob {
	t.Helper()
	var job pullJob
	waitFor(t, "the pull to end", func() bool {
		rec := f.getPull(t, id)
		if rec.code != http.StatusOK {
			t.Fatalf("GET pull: %d %s", rec.code, rec.body)
		}
		job = decode[pullJob](t, rec)
		return job.State != "running"
	})
	return job
}

func (f *envFix) pullAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action = 'image.pulled' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPullJobOnTheLocalHostStreamsToDone(t *testing.T) {
	// The write delay holds the pull back long enough for the stream to open first.
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", WriteDelay: 300 * time.Millisecond})
	rec := f.startPull(t, e, `{"ref":"busybox:1.36.1"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	started := decode[pullStarted](t, rec)
	if started.JobID == "" || started.Ref != pullRef || string(started.Credential) != "null" ||
		len(started.Nodes) != 1 || started.Nodes[0].NodeID != e.ID || started.Nodes[0].Name == "" {
		t.Fatalf("answer = %s", rec.body)
	}

	c := openSSE(t, f.authFixture, "/api/pulls/"+started.JobID+"/stream", f.cookie, "")
	if c.status0() != http.StatusOK || c.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status = %d headers = %v", c.status0(), c.header)
	}
	first := nextFrame(t, c)
	var snap pullJob
	if first.event != "snapshot" || first.id != "" || json.Unmarshal([]byte(first.data), &snap) != nil || snap.ID != started.JobID || len(snap.Items) != 1 {
		t.Fatalf("first frame = %+v, want the snapshot without an id", first)
	}

	var last uint64
	var items []pullItem
	var done pullJob
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
			var it pullItem
			if err := json.Unmarshal([]byte(fr.data), &it); err != nil {
				t.Fatalf("item frame %q: %v", fr.data, err)
			}
			items = append(items, it)
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
	if len(items) == 0 {
		t.Error("no item frame arrived before the done frame")
	}
	if done.State != "done" || len(done.Items) != 1 || done.Items[0].State != "done" || done.Items[0].Digest != dockertest.FakeDigestOf(pullRef) ||
		done.Items[0].UpToDate || done.Items[0].Count != 2 || done.Items[0].Done != 2 || done.FinishedAt == "" {
		t.Fatalf("done job = %+v", done)
	}
	c.waitEnded(t, 5*time.Second, "the done frame")

	if w := f.fake.Writes(); len(w) != 1 || !strings.HasPrefix(w[0], "POST /images/create?") || !strings.Contains(w[0], "fromImage=docker.io%2Flibrary%2Fbusybox") || !strings.Contains(w[0], "tag=1.36.1") {
		t.Fatalf("the engine saw %v, want one pull", w)
	}
	rows := f.pullAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Result != string(audit.ResultSuccess) || r.TargetType != "image" || r.TargetID != pullRef || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"ref": pullRef, "digest": dockertest.FakeDigestOf(pullRef), "node": e.ID, "credential": "anonymous", "bulk": started.JobID} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}
}

func TestPullOfAnImageTheNodeHasIsUpToDateAndStillAudited(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	for range 2 {
		rec := f.startPull(t, e, `{"ref":"busybox:1.36.1"}`)
		if rec.code != http.StatusAccepted {
			t.Fatalf("status = %d: %s", rec.code, rec.body)
		}
		f.pullDone(t, decode[pullStarted](t, rec).JobID)
	}
	rec := f.startPull(t, e, `{"ref":"busybox:1.36.1"}`)
	job := f.pullDone(t, decode[pullStarted](t, rec).JobID)
	if job.State != "done" || !job.Items[0].UpToDate || job.Items[0].Digest != dockertest.FakeDigestOf(pullRef) {
		t.Fatalf("third pull = %+v, want done and up to date", job)
	}
	rows := f.pullAudits(t)
	if len(rows) != 3 || rows[0].Details["code"] != "pulled" || rows[2].Details["code"] != "up_to_date" || rows[2].Result != string(audit.ResultSuccess) {
		t.Fatalf("audit rows = %+v, want three, the first pulled and the last up to date", rows)
	}
}

func TestPullRefusals(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", WriteDelay: 1500 * time.Millisecond})
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })

	t.Run("a Viewer is refused", func(t *testing.T) {
		cookie, csrf := f.signIn(t, "viewer", false)
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/images/pull", body: `{"ref":"busybox"}`, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a reference that does not parse starts nothing", func(t *testing.T) {
		for _, ref := range []string{"Bad Ref!", "GHCR.io/Acme/app", "   "} {
			body, _ := json.Marshal(map[string]string{"ref": ref})
			rec := f.startPull(t, e, string(body))
			if rec.code != http.StatusUnprocessableEntity {
				t.Fatalf("ref %q: status = %d: %s", ref, rec.code, rec.body)
			}
			if ref != "   " && rec.ptyp != "ref_invalid" {
				t.Errorf("ref %q: type = %q, want ref_invalid", ref, rec.ptyp)
			}
		}
		if w := f.fake.Writes(); len(w) != 0 {
			t.Fatalf("the engine saw %v, want nothing", w)
		}
		if got := f.jobHub.List(nil); len(got) != 0 {
			t.Fatalf("%d jobs started, want none", len(got))
		}
	})

	t.Run("a sixth running pull of one user is refused", func(t *testing.T) {
		for i := range 5 {
			rec := f.startPull(t, e, `{"ref":"busybox:1.`+strconv.Itoa(i)+`"}`)
			if rec.code != http.StatusAccepted {
				t.Fatalf("pull %d: %d %s", i+1, rec.code, rec.body)
			}
		}
		rec := f.startPull(t, e, `{"ref":"busybox:1.9"}`)
		if rec.code != http.StatusTooManyRequests || rec.ptyp != "too_many_pulls" {
			t.Fatalf("sixth pull: %d %q %s, want 429 too_many_pulls", rec.code, rec.ptyp, rec.body)
		}
	})
}

func TestPullJobIsOnlyVisibleToItsOwner(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", WriteDelay: 300 * time.Millisecond})
	for _, name := range []string{"op1", "op2"} {
		f.addUser(t, name, false)
		f.addEnvRole(t, name, e.ID, "operator")
	}
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	c1, x1 := f.signIn(t, "op1", false)
	c2, x2 := f.signIn(t, "op2", false)

	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/images/pull", body: `{"ref":"busybox:1"}`, cookie: c1, csrf: x1})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("op1 start: %d %s", rec.Code, rec.Body)
	}
	var started pullStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	path := "/api/pulls/" + started.JobID

	if rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: c1}); rec.Code != http.StatusOK {
		t.Errorf("the owner: %d %s", rec.Code, rec.Body)
	}
	rec = f.call(t, authReq{method: http.MethodGet, path: path, cookie: c2, csrf: x2})
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "pull_not_found" {
		t.Errorf("another user: %d %s, want 404 pull_not_found", rec.Code, rec.Body)
	}
	rec = f.call(t, authReq{method: http.MethodGet, path: path + "/stream", cookie: c2})
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "pull_not_found" {
		t.Errorf("another user's stream: %d %s, want 404 pull_not_found", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: f.cookie}); rec.Code != http.StatusOK {
		t.Errorf("an instance admin: %d %s", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/pulls/nope", cookie: c1}); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown job: %d", rec.Code)
	}
}

func TestPullStateHoldsNoRegistryHeader(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", WriteDelay: 200 * time.Millisecond})
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	secret := "c2VjcmV0LXJlZ2lzdHJ5LXRva2Vu" //nolint:gosec // a made-up value the test looks for, not a credential
	rec := f.call(t, authReq{
		method: http.MethodPost, path: "/api/env/" + e.ID + "/images/pull", body: `{"ref":"busybox:1.36.1"}`,
		cookie: f.cookie, csrf: f.csrf, headers: map[string]string{"X-Registry-Auth": secret},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var started pullStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	c := openSSE(t, f.authFixture, "/api/pulls/"+started.JobID+"/stream", f.cookie, "")
	var seen []string
	for {
		fr := nextFrame(t, c)
		seen = append(seen, fr.data)
		if fr.event == "done" {
			break
		}
	}
	seen = append(seen, rec.Body.String(), string(f.getPull(t, started.JobID).body))
	for _, s := range seen {
		if strings.Contains(s, secret) || strings.Contains(strings.ToLower(s), "x-registry-auth") {
			t.Errorf("job state or a frame carries the registry header: %s", s)
		}
	}
	if got := f.fake.PullAuth(); !slices.Equal(got, []string{""}) {
		t.Errorf("the engine received the headers %q, want none: a caller's header is not a credential", got)
	}
}

func TestAFailedPullIsMappedToACodeAuditedAndNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  dockertest.FakeConfig
		code string
	}{
		{"denied", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 403, Message: "denied: secret-registry-body-A"}}, "pull_denied"},
		{"not found", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 404, Message: "manifest unknown secret-registry-body-B"}}, "image_not_found"},
		{"in the stream", dockertest.FakeConfig{PullStreamError: "layer exploded secret-registry-body-C"}, "pull_failed"},
		{"policy", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 403, Message: "agent_policy_denied: POST /v1.52/images/create"}}, "agent_policy_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Hostname = "node-1"
			f, e := dashEnv(t, tc.cfg)
			rec := f.startPull(t, e, `{"ref":"busybox:1.36.1"}`)
			if rec.code != http.StatusAccepted {
				t.Fatalf("status = %d: %s", rec.code, rec.body)
			}
			started := decode[pullStarted](t, rec)
			job := f.pullDone(t, started.JobID)
			it := job.Items[0]
			if job.State != "failed" || it.State != "failed" || it.Code != tc.code || it.Message == "" {
				t.Fatalf("job = %+v", job)
			}
			if raw := string(f.getPull(t, started.JobID).body); strings.Contains(raw, "secret-registry-body") {
				t.Errorf("the job carries the engine's answer: %s", raw)
			}
			// The pull is tried once: Bayplan never retries a failed pull.
			if w := f.fake.Writes(); len(w) != 1 {
				t.Errorf("the engine saw %d writes, want exactly one: %v", len(w), w)
			}
			rows := f.pullAudits(t)
			if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != tc.code || rows[0].Details["code"] != tc.code || rows[0].Details["bulk"] != started.JobID {
				t.Fatalf("audit rows = %+v", rows)
			}
		})
	}
}

func TestPullStreamResumesFromLastEventIDAndPingsAndEndsWhenSignedOut(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	f.pullsHeartbeat = 20 * time.Millisecond
	f.h = f.build(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	rec := f.startPull(t, e, `{"ref":"busybox:1.36.1"}`)
	started := decode[pullStarted](t, rec)
	f.pullDone(t, started.JobID)
	path := "/api/pulls/" + started.JobID + "/stream"

	// A stream opened after the end: the snapshot, then the done frame, then it closes.
	c := openSSE(t, f.authFixture, path, f.cookie, "")
	if fr := nextFrame(t, c); fr.event != "snapshot" {
		t.Fatalf("first = %+v", fr)
	}
	if fr := nextFrame(t, c); fr.event != "done" {
		t.Fatalf("second = %+v, want done", fr)
	}
	c.waitEnded(t, 5*time.Second, "the done frame")

	// A reconnect that last saw event 1 is sent a snapshot and what came after 1.
	c = openSSE(t, f.authFixture, path, f.cookie, "1")
	if fr := nextFrame(t, c); fr.event != "snapshot" || fr.id != "" {
		t.Fatalf("first after resume = %+v", fr)
	}
	var ids []int
	for {
		fr := nextFrame(t, c)
		if n, err := strconv.Atoi(fr.id); err == nil {
			ids = append(ids, n)
		}
		if fr.event == "done" {
			break
		}
	}
	if len(ids) == 0 || ids[0] <= 1 || !slices.IsSorted(ids) {
		t.Errorf("resumed ids = %v, want only events newer than 1, in order", ids)
	}

	// The SPA cannot set the header when it reopens a stream, so the query says it too.
	c = openSSE(t, f.authFixture, path+"?lastEventId=1", f.cookie, "")
	nextFrame(t, c) // the snapshot
	if fr := nextFrame(t, c); fr.id == "" || fr.id == "1" {
		t.Errorf("first frame after ?lastEventId=1 = %+v, want an event newer than 1", fr)
	}
}

func TestPullStreamHeartbeatReauthorises(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", WriteDelay: 2 * time.Second})
	f.pullsHeartbeat = 20 * time.Millisecond
	f.h = f.build(t)
	f.cookie, f.csrf = f.signIn(t, "admin", false)
	rec := f.startPull(t, e, `{"ref":"busybox:1.36.1"}`)
	started := decode[pullStarted](t, rec)
	c := openSSE(t, f.authFixture, "/api/pulls/"+started.JobID+"/stream", f.cookie, "")
	if fr := nextFrame(t, c); fr.event != "snapshot" {
		t.Fatalf("first = %+v", fr)
	}
	waitPings(t, c, 2)
	if _, err := f.st.Write.Exec(`DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	c.waitEnded(t, time.Second, "the session was deleted")
	// The pull itself goes on without the stream.
	if job, ok := f.jobHub.Get(started.JobID); !ok || job.Finished() {
		t.Errorf("closing the stream stopped the pull: %+v", job)
	}
}

func TestPullStreamsPerUserAreCapped(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", WriteDelay: 1500 * time.Millisecond})
	f.pullsMaxStreams = 2
	f.h = f.build(t)
	f.cookie, f.csrf = f.signIn(t, "admin", false)
	rec := f.startPull(t, e, `{"ref":"busybox:1.36.1"}`)
	started := decode[pullStarted](t, rec)
	path := "/api/pulls/" + started.JobID + "/stream"
	openSSE(t, f.authFixture, path, f.cookie, "")
	openSSE(t, f.authFixture, path, f.cookie, "")
	third := f.call(t, authReq{method: http.MethodGet, path: path, cookie: f.cookie})
	if third.Code != http.StatusTooManyRequests || problemType(t, third) != "too_many_streams" {
		t.Fatalf("third stream: %d %s, want 429 too_many_streams", third.Code, third.Body)
	}
}
