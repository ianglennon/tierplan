// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
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

// holdPulls returns a gate for FakeConfig.PullHold and a function that opens it. The gate
// is opened when the test ends too, so no pull is left held.
func holdPulls(t *testing.T) (gate <-chan struct{}, open func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	open = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(open)
	return ch, open
}

func TestPullJobOnTheLocalHostStreamsToDone(t *testing.T) {
	// The pull is held until the stream is open, so the item frames are always seen.
	gate, release := holdPulls(t)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullHold: gate})
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
	release()

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
	gate, _ := holdPulls(t)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullHold: gate})
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
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
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
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
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
	gate, _ := holdPulls(t)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullHold: gate})
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
	gate, _ := holdPulls(t)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullHold: gate})
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

// Where a pull runs: the nodes of a swarm are listed with their role, architecture and
// reachability, a pull names all of them or the ones the user picks, and a read-only host
// is named and never called (plan 05-03, RES-09, IMG-02, D-25, D-30, P4 D-13).

type pullTargets struct {
	Swarm bool `json:"swarm"`
	Nodes []struct {
		NodeID    string `json:"nodeId"`
		Name      string `json:"name"`
		Role      string `json:"role"`
		Arch      string `json:"arch"`
		ReadOnly  bool   `json:"readOnly"`
		Reachable bool   `json:"reachable"`
	} `json:"nodes"`
}

// pullOnSwarm posts a pull to the swarm fixture's environment once its managers' engines
// have been probed.
func (s *swarmFix) pullOnSwarm(t *testing.T, body string) *envRec {
	t.Helper()
	s.whenProbed(t, envs.AgentTarget(s.m1.id), envs.AgentTarget(s.m2.id), envs.AgentTarget(s.w.id))
	return s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/images/pull", body)
}

func (s *swarmFix) pullsDone(t *testing.T, id string) pullJob {
	t.Helper()
	var job pullJob
	waitFor(t, "the pull to end", func() bool {
		rec := s.getPull(t, id)
		if rec.code != http.StatusOK {
			t.Fatalf("GET pull: %d %s", rec.code, rec.body)
		}
		job = decode[pullJob](t, rec)
		return job.State != "running"
	})
	return job
}

func creates(a *agentConn) int {
	n := 0
	for _, w := range a.fake.Writes() {
		if strings.HasPrefix(w, "POST /images/create") {
			n++
		}
	}
	return n
}

func TestPullTargetsListsEveryNodeWithRoleArchitectureAndReachability(t *testing.T) {
	s := newSwarmFix(t)
	s.whenProbed(t, envs.AgentTarget(s.m1.id), envs.AgentTarget(s.m2.id), envs.AgentTarget(s.w.id))
	rec := s.req(t, http.MethodGet, "/api/env/"+s.env.ID+"/images/pull-targets", "")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[pullTargets](t, rec)
	if !got.Swarm || len(got.Nodes) != 3 {
		t.Fatalf("targets = %s, want a swarm of three nodes", rec.body)
	}
	for i, want := range []struct{ id, name, role string }{
		{s.m1.id, "mgr-1", "manager"}, {s.m2.id, "mgr-2", "manager"}, {s.w.id, "wrk-1", "worker"},
	} {
		n := got.Nodes[i]
		if n.NodeID != want.id || n.Name != want.name || n.Role != want.role || n.Arch != "amd64" || n.ReadOnly || !n.Reachable {
			t.Errorf("node %d = %+v, want %s (%s) amd64, writable and reachable", i, n, want.name, want.role)
		}
	}

	// A worker whose agent is gone is still listed, and unreachable.
	s.srv.Hub.Kick(s.w.id, tunnel.DisconnectClosed)
	waitFor(t, "the worker to be listed unreachable", func() bool {
		got = decode[pullTargets](t, s.req(t, http.MethodGet, "/api/env/"+s.env.ID+"/images/pull-targets", ""))
		return len(got.Nodes) == 3 && !got.Nodes[2].Reachable
	})
	if w := got.Nodes[2]; w.NodeID != s.w.id || w.Name != "wrk-1" || w.Role != "worker" || got.Nodes[0].Reachable != true {
		t.Errorf("targets = %+v, want wrk-1 unreachable by its agent ID and the managers still reachable", got.Nodes)
	}
}

func TestPullTargetsOfAStandaloneHostIsOneNodeAndNotASwarm(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	rec := f.get(t, e, "images/pull-targets")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[pullTargets](t, rec)
	if got.Swarm || len(got.Nodes) != 1 || got.Nodes[0].NodeID != e.ID || got.Nodes[0].Name == "" || got.Nodes[0].Arch != "amd64" || !got.Nodes[0].Reachable {
		t.Fatalf("targets = %s, want one reachable amd64 node and swarm false", rec.body)
	}
}

func TestPullTargetsAreForViewersAndNotForAnotherEnvironmentsUsers(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "viewer", false)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/images/pull-targets", cookie: cookie}); rec.Code != http.StatusOK {
		t.Errorf("a Viewer: %d %s, want 200", rec.Code, rec.Body)
	}
	cookie, _ = f.signIn(t, "nobody", false)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/images/pull-targets", cookie: cookie}); rec.Code == http.StatusOK {
		t.Errorf("a user with no role here: %d, want a refusal", rec.Code)
	}
}

func TestPullWithoutNodeIDsRunsOnEveryNodeAndNodeIDsChooseThem(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.pullOnSwarm(t, `{"ref":"busybox:1.36.1"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	started := decode[pullStarted](t, rec)
	var names []string
	for _, n := range started.Nodes {
		names = append(names, n.Name)
	}
	if !slices.Equal(names, []string{"mgr-1", "mgr-2", "wrk-1"}) {
		t.Fatalf("nodes = %v, want every node in name order", names)
	}
	job := s.pullsDone(t, started.JobID)
	if job.State != "done" || len(job.Items) != 3 || job.Items[0].Name != "mgr-1" || job.Items[2].Name != "wrk-1" {
		t.Fatalf("job = %+v, want three done items in node-name order", job)
	}
	if creates(s.m1) != 1 || creates(s.m2) != 1 || creates(s.w) != 1 {
		t.Fatalf("pulls served = %d/%d/%d, want one each", creates(s.m1), creates(s.m2), creates(s.w))
	}

	// Only the worker is chosen.
	rec = s.pullOnSwarm(t, `{"ref":"busybox:1.37","nodeIds":["`+s.w.id+`"]}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	one := s.pullsDone(t, decode[pullStarted](t, rec).JobID)
	if one.State != "done" || len(one.Items) != 1 || one.Items[0].NodeID != s.w.id {
		t.Fatalf("job = %+v, want the one chosen node", one)
	}
	if creates(s.m1) != 1 || creates(s.m2) != 1 || creates(s.w) != 2 {
		t.Fatalf("pulls served = %d/%d/%d, want only the worker to have pulled again", creates(s.m1), creates(s.m2), creates(s.w))
	}
}

func TestPullNodeChoiceIsCheckedAgainstTheEnvironmentsOwnNodes(t *testing.T) {
	s := newSwarmFix(t)
	for name, tc := range map[string]struct{ body, ptyp string }{
		"a node of nowhere":                {`{"ref":"busybox","nodeIds":["not-a-node"]}`, "nodes_unknown"},
		"one good node and one unknown":    {`{"ref":"busybox","nodeIds":["` + s.w.id + `","other"]}`, "nodes_unknown"},
		"a node named instead of its ID":   {`{"ref":"busybox","nodeIds":["wrk-1"]}`, "nodes_unknown"},
		"an empty choice":                  {`{"ref":"busybox","nodeIds":[]}`, "nodes_required"},
		"the same node twice is only once": {`{"ref":"busybox","nodeIds":["` + s.w.id + `","` + s.w.id + `"]}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := s.pullOnSwarm(t, tc.body)
			if tc.ptyp == "" {
				if rec.code != http.StatusAccepted || len(decode[pullStarted](t, rec).Nodes) != 1 {
					t.Fatalf("status = %d: %s, want one item", rec.code, rec.body)
				}
				return
			}
			if rec.code != http.StatusUnprocessableEntity || rec.ptyp != tc.ptyp {
				t.Fatalf("status = %d %q: %s, want 422 %s", rec.code, rec.ptyp, rec.body, tc.ptyp)
			}
		})
	}
	// Only the duplicate case started a pull: nothing else reached an engine.
	waitFor(t, "the one pull to reach the worker", func() bool { return creates(s.w) == 1 })
	if creates(s.m1) != 0 || creates(s.m2) != 0 {
		t.Errorf("managers served %d/%d pulls, want none", creates(s.m1), creates(s.m2))
	}

	t.Run("more than 128 IDs", func(t *testing.T) {
		ids := make([]string, 129)
		for i := range ids {
			ids[i] = "n" + strconv.Itoa(i)
		}
		b, _ := json.Marshal(map[string]any{"ref": "busybox", "nodeIds": ids})
		if rec := s.pullOnSwarm(t, string(b)); rec.code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d: %s, want 422", rec.code, rec.body)
		}
	})
}

// twoNodes is a swarm of two nodes whose engines the caller configures.
func twoNodes(t *testing.T, a, b dockertest.FakeConfig, aFacts, bFacts *wire.Facts) (f *agentFix, env tunneltest.Environment, x, y *agentConn) {
	t.Helper()
	f = newAgentFix(t)
	env = f.srv.NewEnvironment(t, "Pair")
	x = f.connect(t, env, a, aFacts)
	y = f.connect(t, env, b, bFacts)
	f.whenState(t, env.ID, "online")
	f.whenProbed(t, envs.AgentTarget(x.id), envs.AgentTarget(y.id))
	return f, env, x, y
}

func pairFacts(name string, readOnly bool) *wire.Facts {
	return &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: readOnly},
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name, ClusterID: "cluster-1", Control: true},
	}
}

func TestPullOnAReadOnlyNodeIsHostReadOnlyAuditedAndNeverCallsItsEngine(t *testing.T) {
	f, env, ro, rw := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", true), pairFacts("bravo", false))
	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/images/pull", `{"ref":"busybox:1.36.1"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	job := f.pullDone(t, decode[pullStarted](t, rec).JobID)
	if job.State != "partial" || len(job.Items) != 2 {
		t.Fatalf("job = %+v, want partial with two items", job)
	}
	if a := job.Items[0]; a.Name != "alpha" || a.State != "failed" || a.Code != "host_read_only" {
		t.Errorf("alpha = %+v, want failed with host_read_only", a)
	}
	if b := job.Items[1]; b.Name != "bravo" || b.State != "done" {
		t.Errorf("bravo = %+v, want done", b)
	}
	if w := ro.fake.Writes(); len(w) != 0 {
		t.Errorf("the read-only host's engine received %v", w)
	}
	if creates(rw) != 1 {
		t.Errorf("the writable host served %d pulls, want 1", creates(rw))
	}
	rows := f.pullAudits(t)
	if len(rows) != 2 || rows[0].Reason != "host_read_only" || rows[0].Result != string(audit.ResultFailure) {
		t.Fatalf("audit rows = %+v, want a host_read_only failure and a success", rows)
	}
}

func TestOneNodeFailingLeavesTheOthersDoneAndEveryAuditRowCarriesTheSameBulkID(t *testing.T) {
	f, env, _, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true, PullStatus: dockertest.FakePullStatus{Code: http.StatusInternalServerError, Message: "boom"}},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/images/pull", `{"ref":"busybox:1.36.1"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	started := decode[pullStarted](t, rec)
	job := f.pullDone(t, started.JobID)
	if job.State != "partial" || len(job.Items) != 2 || job.Items[0].State != "failed" || job.Items[0].Code == "" || job.Items[1].State != "done" {
		t.Fatalf("job = %+v, want alpha failed with a code and bravo done, so partial", job)
	}
	rows := f.pullAudits(t)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want one per node", rows)
	}
	for _, r := range rows {
		if r.Details["bulk"] != started.JobID {
			t.Errorf("audit row %+v does not carry the job ID %s as its bulk", r, started.JobID)
		}
	}
	if rows[0].Result == rows[1].Result {
		t.Errorf("audit results = %s and %s, want one failure and one success", rows[0].Result, rows[1].Result)
	}
}

func TestPullOnAnUnreachableNodeFailsItAloneWithNodeUnreachable(t *testing.T) {
	s := newSwarmFix(t)
	s.whenProbed(t, envs.AgentTarget(s.m1.id), envs.AgentTarget(s.m2.id), envs.AgentTarget(s.w.id))
	s.srv.Hub.Kick(s.w.id, tunnel.DisconnectClosed)
	waitFor(t, "the worker to be listed unreachable", func() bool {
		got := decode[pullTargets](t, s.req(t, http.MethodGet, "/api/env/"+s.env.ID+"/images/pull-targets", ""))
		return len(got.Nodes) == 3 && !got.Nodes[2].Reachable
	})
	rec := s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/images/pull", `{"ref":"busybox:1.36.1"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	job := s.pullsDone(t, decode[pullStarted](t, rec).JobID)
	if job.State != "partial" || len(job.Items) != 3 {
		t.Fatalf("job = %+v, want partial with the unreachable node as an item too", job)
	}
	if w := job.Items[2]; w.Name != "wrk-1" || w.State != "failed" || w.Code != "node_unreachable" {
		t.Errorf("worker = %+v, want failed with node_unreachable", w)
	}
	if job.Items[0].State != "done" || job.Items[1].State != "done" {
		t.Errorf("managers = %+v %+v, want both done", job.Items[0], job.Items[1])
	}
}

// The list of pulls: a user sees their own, an instance admin and an Env Admin also see
// those other people started on the environments they administer, and nobody else does
// (plan 05-04, D-26, T-05-16).

type pullListed struct {
	ID         string `json:"id"`
	EnvID      string `json:"envId"`
	EnvName    string `json:"envName"`
	Ref        string `json:"ref"`
	State      string `json:"state"`
	Done       int    `json:"done"`
	Failed     int    `json:"failed"`
	Total      int    `json:"total"`
	CreatedAt  string `json:"createdAt"`
	FinishedAt string `json:"finishedAt"`
	OwnerName  string `json:"ownerName"`
}

func (f *authFixture) listedPulls(t *testing.T, cookie string) []pullListed {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/pulls", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/pulls: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Pulls []pullListed `json:"pulls"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Pulls
}

func TestListPullsShowsTheStartersAndTheAdministratorsOfTheEnvironmentOnly(t *testing.T) {
	gate, _ := holdPulls(t)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullHold: gate})
	for _, u := range []struct{ name, role string }{{"op1", "operator"}, {"op2", "operator"}, {"envadmin", "admin"}, {"viewer", "viewer"}} {
		f.addUser(t, u.name, false)
		f.addEnvRole(t, u.name, e.ID, u.role)
	}
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	op1, x1 := f.signIn(t, "op1", false)
	op2, _ := f.signIn(t, "op2", false)
	envAdmin, _ := f.signIn(t, "envadmin", false)
	viewer, _ := f.signIn(t, "viewer", false)

	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/images/pull", body: `{"ref":"busybox:1"}`, cookie: op1, csrf: x1})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var started pullStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	own := f.listedPulls(t, op1)
	if len(own) != 1 {
		t.Fatalf("the starter's list = %+v, want the one pull", own)
	}
	row := own[0]
	if row.ID != started.JobID || row.EnvID != e.ID || row.EnvName != e.Name || row.Ref != "docker.io/library/busybox:1" ||
		row.State != "running" || row.Total != 1 || row.Done != 0 || row.Failed != 0 || row.CreatedAt == "" || row.FinishedAt != "" {
		t.Errorf("the starter's row = %+v", row)
	}
	if row.OwnerName != "" {
		t.Errorf("the starter's own row names an owner: %q", row.OwnerName)
	}

	for name, cookie := range map[string]string{"an Env Admin": envAdmin, "an instance admin": f.cookie} {
		rows := f.listedPulls(t, cookie)
		if len(rows) != 1 || rows[0].ID != started.JobID || rows[0].OwnerName != "op1" {
			t.Errorf("%s sees %+v, want the pull with ownerName op1", name, rows)
		}
		if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/pulls/" + started.JobID, cookie: cookie}); rec.Code != http.StatusOK {
			t.Errorf("%s reading the job: %d %s", name, rec.Code, rec.Body)
		}
	}
	for name, cookie := range map[string]string{"another Operator": op2, "a Viewer": viewer} {
		if rows := f.listedPulls(t, cookie); len(rows) != 0 {
			t.Errorf("%s sees %+v, want nothing", name, rows)
		}
		rec := f.call(t, authReq{method: http.MethodGet, path: "/api/pulls/" + started.JobID, cookie: cookie})
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "pull_not_found" {
			t.Errorf("%s reading the job: %d %s, want 404 pull_not_found", name, rec.Code, rec.Body)
		}
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/pulls"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}

// Cancelling a pull (plan 05-04, D-26, T-05-17, T-05-18).

func (f *authFixture) cancelPull(t *testing.T, id, cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPost, path: "/api/pulls/" + id + "/cancel", cookie: cookie, csrf: csrf})
}

func TestCancelPullByTheStarterStopsTheNodeAuditsItAndASecondCancelIs409(t *testing.T) {
	gate, _ := holdPulls(t)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullHold: gate})
	f.addUser(t, "op1", false)
	f.addEnvRole(t, "op1", e.ID, "operator")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	op1, x1 := f.signIn(t, "op1", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/images/pull", body: `{"ref":"busybox:1"}`, cookie: op1, csrf: x1})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var started pullStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	rec = f.cancelPull(t, started.JobID, op1, x1)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	var job pullJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.ID != started.JobID || job.State != "cancelled" || len(job.Items) != 1 || job.Items[0].State != "cancelled" || job.Items[0].Code != "cancelled" || job.FinishedAt == "" {
		t.Fatalf("the cancelled job = %+v, want cancelled with its node cancelled", job)
	}

	rows := f.pullAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "cancelled" || rows[0].Details["code"] != "cancelled" || rows[0].Details["bulk"] != started.JobID {
		t.Fatalf("audit rows = %+v, want one failure with code cancelled", rows)
	}

	again := f.cancelPull(t, started.JobID, op1, x1)
	if again.Code != http.StatusConflict || problemType(t, again) != "pull_finished" {
		t.Fatalf("second cancel: %d %s, want 409 pull_finished", again.Code, again.Body)
	}
	if got := f.listedPulls(t, op1); len(got) != 1 || got[0].State != "cancelled" {
		t.Errorf("the list after a cancel = %+v, want the pull cancelled and still listed", got)
	}
}

func TestCancelPullKeepsTheNodesThatFinishedDoneAndTheJobIsPartial(t *testing.T) {
	gate, _ := holdPulls(t)
	f, env, _, held := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true, PullHold: gate},
		pairFacts("alpha", false), pairFacts("bravo", false))
	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/images/pull", `{"ref":"busybox:1.36.1"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.code, rec.body)
	}
	started := decode[pullStarted](t, rec)
	// A node's item turns running when its pull is dispatched, before the request reaches
	// the node's engine, so "running" alone does not mean the node was asked. Wait for the
	// held node to have recorded its request too; only then is it known to be held on the gate.
	waitFor(t, "alpha to finish and bravo to be asked and held", func() bool {
		job := decode[pullJob](t, f.getPull(t, started.JobID))
		return len(job.Items) == 2 && job.Items[0].State == "done" && job.Items[1].State == "running" && creates(held) >= 1
	})
	if creates(held) != 1 {
		t.Fatalf("the held node was asked %d times", creates(held))
	}

	cancelled := f.cancelPull(t, started.JobID, f.cookie, f.csrf)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", cancelled.Code, cancelled.Body)
	}
	var job pullJob
	if err := json.Unmarshal(cancelled.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.State != "partial" || job.Items[0].State != "done" || job.Items[0].Digest == "" || job.Items[1].State != "cancelled" || job.Items[1].Code != "cancelled" {
		t.Fatalf("job = %+v, want alpha done with its digest, bravo cancelled, partial", job)
	}
	rows := f.pullAudits(t)
	if len(rows) != 2 || rows[0].Result != string(audit.ResultSuccess) || rows[1].Result != string(audit.ResultFailure) || rows[1].Reason != "cancelled" {
		t.Fatalf("audit rows = %+v, want a success and a cancelled failure", rows)
	}
}

func TestCancelPullIsAllowedToAnEnvAdminAndAnInstanceAdminAndNeverToAnyoneWhoCannotSeeIt(t *testing.T) {
	gate, _ := holdPulls(t)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullHold: gate})
	for _, u := range []struct{ name, role string }{{"op1", "operator"}, {"op2", "operator"}, {"envadmin", "admin"}, {"viewer", "viewer"}} {
		f.addUser(t, u.name, false)
		f.addEnvRole(t, u.name, e.ID, u.role)
	}
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	op1, x1 := f.signIn(t, "op1", false)
	op2, x2 := f.signIn(t, "op2", false)
	envAdmin, xa := f.signIn(t, "envadmin", false)
	viewer, xv := f.signIn(t, "viewer", false)
	start := func() string {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/images/pull", body: `{"ref":"busybox:1"}`, cookie: op1, csrf: x1})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("start: %d %s", rec.Code, rec.Body)
		}
		var s pullStarted
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s.JobID
	}
	id := start()

	for name, who := range map[string][2]string{"another Operator": {op2, x2}, "a Viewer": {viewer, xv}} {
		rec := f.cancelPull(t, id, who[0], who[1])
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "pull_not_found" {
			t.Errorf("%s: %d %s, want 404 pull_not_found", name, rec.Code, rec.Body)
		}
	}
	if rec := f.cancelPull(t, "nope", op1, x1); rec.Code != http.StatusNotFound || problemType(t, rec) != "pull_not_found" {
		t.Errorf("an unknown job: %d %s, want 404 pull_not_found", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/pulls/" + id + "/cancel"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
	if job, _ := f.jobHub.Get(id); job.Finished() {
		t.Fatalf("a refused cancel stopped the pull: %+v", job)
	}

	if rec := f.cancelPull(t, id, envAdmin, xa); rec.Code != http.StatusOK {
		t.Errorf("an Env Admin: %d %s, want 200", rec.Code, rec.Body)
	}
	cancelledID := id
	id = start()
	if rec := f.cancelPull(t, id, f.cookie, f.csrf); rec.Code != http.StatusOK {
		t.Errorf("an instance admin: %d %s, want 200", rec.Code, rec.Body)
	}
	// Each cancel by someone other than the starter is a row of its own, with the canceller as
	// the actor and the starter named (WR-B04); the starter's own cancel writes none.
	rows, err := f.st.Read.Query(`SELECT actor_name, env_id, target_id, details FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionImagePullCancelled)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var actors, bulks []string
	for rows.Next() {
		var actor, env, target, details string
		if err := rows.Scan(&actor, &env, &target, &details); err != nil {
			t.Fatal(err)
		}
		d := map[string]string{}
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			t.Fatal(err)
		}
		if env != e.ID || target != "docker.io/library/busybox:1" || d["ref"] != target || !strings.HasPrefix(d["started_by"], "user:") {
			t.Errorf("cancel row = env %q target %q details %v", env, target, d)
		}
		actors, bulks = append(actors, actor), append(bulks, d["bulk"])
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actors) != 2 || actors[0] != "envadmin" || bulks[0] != cancelledID || bulks[1] != id {
		t.Errorf("cancel rows by %v for jobs %v, want the Env Admin's for %s and the instance admin's for %s", actors, bulks, cancelledID, id)
	}
}

// Pulls with a stored credential (plan 05-09, IMG-02, D-07, D-14, D-25).

const (
	pullCredSecret = "pat_super-secret-token-0123456789"
	privateRef     = "localhost:5000/private/busybox:1"
)

// addPullCredential stores a credential for localhost:5000/private and returns it.
func (f *authFixture) addPullCredential(t *testing.T, name, envID string) registry.Credential {
	t.Helper()
	c, err := registry.NewStore(f.st, f.v, f.clk.now).Create(context.Background(), registry.CreateInput{
		EnvID: envID, Name: name, Type: registry.TypeGeneric, Host: "localhost:5000", Prefix: "private",
		Username: "bot", Secret: pullCredSecret,
	}, "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func lastFailure(t *testing.T, f *authFixture, id string) (code string, set bool) {
	t.Helper()
	var at sql.NullInt64
	if err := f.st.Read.QueryRow(`SELECT last_failure_at, last_failure_code FROM registry_credentials WHERE id = ?`, id).Scan(&at, &code); err != nil {
		t.Fatal(err)
	}
	return code, at.Valid
}

func decodedPullAuth(t *testing.T, header string) (user, password, server string) {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("header %q: %v", header, err)
	}
	var a struct{ Username, Password, ServerAddress string }
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	return a.Username, a.Password, a.ServerAddress
}

func TestAPullWithAMatchingCredentialSendsItOnEveryNodeAndNamesItEverywhereButTheHeader(t *testing.T) {
	f, env, x, y := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	cred := f.addPullCredential(t, "local-bot", "")

	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/images/pull", `{"ref":"`+privateRef+`"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	started := decode[pullStarted](t, rec)
	wantCred := `{"id":"` + cred.ID + `","name":"local-bot","layer":"instance"}`
	if string(started.Credential) != wantCred {
		t.Errorf("answer credential = %s, want %s", started.Credential, wantCred)
	}
	job := f.pullDone(t, started.JobID)
	if job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	var got struct{ Credential json.RawMessage }
	if err := json.Unmarshal(f.getPull(t, started.JobID).body, &got); err != nil || string(got.Credential) != wantCred {
		t.Errorf("job credential = %s (%v), want %s", got.Credential, err, wantCred)
	}

	var headers []string
	for _, n := range []*agentConn{x, y} {
		h := n.fake.PullAuth()
		if len(h) != 1 || h[0] == "" {
			t.Fatalf("a node received %q, want exactly one header", h)
		}
		user, pw, server := decodedPullAuth(t, h[0])
		if user != "bot" || pw != pullCredSecret || server != "localhost:5000" {
			t.Errorf("decoded header = %q %q %q", user, pw, server)
		}
		headers = append(headers, h[0])
	}
	if headers[0] != headers[1] {
		t.Errorf("the nodes got different headers: the credential is resolved once per job")
	}

	rows := f.pullAudits(t)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Details["credential"] != "local-bot" {
			t.Errorf("audit row %+v does not name the credential", r)
		}
	}

	// Nothing the browser, the audit log or the server log holds has the header or the secret.
	seen := []string{string(rec.body), string(f.getPull(t, started.JobID).body), f.logs.String()}
	for _, r := range rows {
		b, _ := json.Marshal(r.Details)
		seen = append(seen, string(b))
	}
	for _, s := range seen {
		if strings.Contains(s, pullCredSecret) || strings.Contains(s, headers[0]) {
			t.Errorf("the header or the secret leaked: %s", s)
		}
	}
	if code, set := lastFailure(t, f.authFixture, cred.ID); set || code != "" {
		t.Errorf("a pull that worked left the failure %q", code)
	}

	// An image of another registry is pulled anonymously.
	rec = f.req(t, http.MethodPost, "/api/env/"+env.ID+"/images/pull", `{"ref":"busybox:1.36.1"}`)
	started = decode[pullStarted](t, rec)
	f.pullDone(t, started.JobID)
	if string(started.Credential) != "null" {
		t.Errorf("credential = %s, want null", started.Credential)
	}
	if h := x.fake.PullAuth(); len(h) != 2 || h[1] != "" {
		t.Errorf("headers = %q, want the second pull anonymous", h)
	}
}

func TestARefusedPullRecordsTheCredentialsFailureOnceAndALaterSuccessClearsItWithNoRetryAndNoFallback(t *testing.T) {
	f, env, x, y := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true, PullStatus: dockertest.FakePullStatus{Code: http.StatusUnauthorized, Message: "unauthorized: bad token"}},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true, PullStatus: dockertest.FakePullStatus{Code: http.StatusUnauthorized, Message: "unauthorized: bad token"}},
		pairFacts("alpha", false), pairFacts("bravo", false))
	cred := f.addPullCredential(t, "local-bot", "")
	// A second, shorter credential for the same registry: it must not be tried after a failure.
	if _, err := registry.NewStore(f.st, f.v, f.clk.now).Create(context.Background(), registry.CreateInput{
		Name: "local-all", Type: registry.TypeGeneric, Host: "localhost:5000", Username: "other", Secret: "other-secret-value-xyz",
	}, "admin", nil); err != nil {
		t.Fatal(err)
	}

	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/images/pull", `{"ref":"`+privateRef+`"}`)
	job := f.pullDone(t, decode[pullStarted](t, rec).JobID)
	if job.State != "failed" || job.Items[0].Code != "pull_unauthorized" || job.Items[1].Code != "pull_unauthorized" {
		t.Fatalf("job = %+v", job)
	}
	if code, set := lastFailure(t, f.authFixture, cred.ID); !set || code != "pull_unauthorized" {
		t.Errorf("failure = %q set=%v, want pull_unauthorized", code, set)
	}
	for _, n := range []*agentConn{x, y} {
		if h := n.fake.PullAuth(); len(h) != 1 {
			t.Errorf("a node saw %d pulls, want exactly one: no retry, no second credential", len(h))
		} else if user, _, _ := decodedPullAuth(t, h[0]); user != "bot" {
			t.Errorf("the pull was made as %q, want the best match", user)
		}
	}
	for _, r := range f.pullAudits(t) {
		if r.Details["credential"] != "local-bot" {
			t.Errorf("audit row %+v does not name the credential", r)
		}
	}
	if strings.Contains(f.logs.String(), pullCredSecret) {
		t.Errorf("the server log holds the secret")
	}
}

// "denied" is what a registry says of a repository that does not exist or that the account may not
// read, so it is not the credential failing and must not alert the people who manage it (WR-B03).
func TestADeniedPullDoesNotFlagTheCredentialItWasMadeWith(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", PullStatus: dockertest.FakePullStatus{Code: http.StatusForbidden, Message: "denied: requested access to the resource is denied"}})
	cred := f.addPullCredential(t, "local-bot", "")
	rec := f.startPull(t, e, `{"ref":"`+privateRef+`"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	job := f.pullDone(t, decode[pullStarted](t, rec).JobID)
	if job.Items[0].Code != "pull_denied" {
		t.Fatalf("job = %+v, want pull_denied", job)
	}
	if code, set := lastFailure(t, f.authFixture, cred.ID); set || code != "" {
		t.Errorf("failure = %q set=%v, want none for a denied pull", code, set)
	}
}

func TestAPullThatWorksAfterAFailureClearsIt(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	cred := f.addPullCredential(t, "local-bot", "")
	if err := registry.NewStore(f.st, f.v, f.clk.now).RecordFailure(context.Background(), cred.ID, "pull_denied"); err != nil {
		t.Fatal(err)
	}
	rec := f.startPull(t, e, `{"ref":"`+privateRef+`"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	f.pullDone(t, decode[pullStarted](t, rec).JobID)
	if code, set := lastFailure(t, f.authFixture, cred.ID); set || code != "" {
		t.Errorf("failure = %q set=%v, want cleared", code, set)
	}
}
