// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The Let's Encrypt run actions and their stream (phase 14, ACME-04, ACME-06, ACME-07, ACME-08, ACME-10, D-09, D-14).

// acmeStepIDs are the steps of a run, in order.
var acmeStepIDs = []string{"account", "order", "challenge", "validate", "finalize", "stored"}

// unmarshalFrame decodes the data of a stream frame.
func unmarshalFrame[T any](t *testing.T, data string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("decode the frame %q: %v", data, err)
	}
	return v
}

func stepIDsOf(items []api.HTTPSACMEStepBody) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func TestIssueACMEThroughTheAPIStreamsTheSteps(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	// The run waits at finalize until the stream is open, so the stream sees it in progress.
	release := f.fake.HoldFinalize()

	rec := f.send(t, http.MethodPost, "/api/https/acme/issue", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST issue: %d %s, want 202", rec.Code, rec.Body)
	}
	var started api.HTTPSACMERunStartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.JobID == "" || started.Reused || started.ValidUntil != nil {
		t.Fatalf("start = %+v, want a job id and no reuse", started)
	}

	c := openSSE(t, f.authFixture, "/api/https/acme/runs/"+started.JobID+"/stream", f.cookie, "")
	if c.status0() != http.StatusOK || c.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status = %d headers = %v", c.status0(), c.header)
	}
	snap := nextFrame(t, c)
	if snap.event != "snapshot" || snap.id != "" {
		t.Fatalf("first frame = %+v, want a snapshot without an id", snap)
	}
	first := unmarshalFrame[api.HTTPSACMERunFrameBody](t, snap.data)
	if first.JobID != started.JobID || first.Kind != "issue" || first.Trigger != "admin" || first.State != "running" || first.StartedAt.IsZero() {
		t.Errorf("snapshot = %+v, want the running admin issue", first)
	}
	if got := stepIDsOf(first.Items); !slices.Equal(got, acmeStepIDs) {
		t.Fatalf("snapshot steps = %v, want %v", got, acmeStepIDs)
	}
	if first.Challenge != "http-01" {
		t.Errorf("snapshot challenge = %q, want http-01", first.Challenge)
	}

	release()
	var done api.HTTPSACMERunFrameBody
	for {
		fr := nextFrame(t, c)
		if fr.event == "item" {
			it := unmarshalFrame[api.HTTPSACMEStepBody](t, fr.data)
			if !slices.Contains(acmeStepIDs, it.ID) {
				t.Errorf("item frame for an unknown step: %+v", it)
			}
			continue
		}
		if fr.event == "done" {
			done = unmarshalFrame[api.HTTPSACMERunFrameBody](t, fr.data)
			break
		}
		t.Fatalf("unexpected frame %+v", fr)
	}
	c.awaitEnd(t, "the done frame")

	if got := stepIDsOf(done.Items); !slices.Equal(got, acmeStepIDs) {
		t.Fatalf("done steps = %v, want %v", got, acmeStepIDs)
	}
	for _, it := range done.Items {
		want := "done"
		if it.ID == "account" {
			want = "skipped" // registered before the run
		}
		if it.State != want {
			t.Errorf("step %s = %q, want %q", it.ID, it.State, want)
		}
	}
	// The hub ends a job with a skipped step as partial; the run is reported as what it was.
	if done.State != "done" {
		t.Errorf("done state = %q, want done", done.State)
	}

	got := f.status(t).ACME
	if got.LastRun == nil || got.LastRun.Outcome != "succeeded" || got.LastRun.JobID != started.JobID || got.LastRun.Challenge != "http-01" {
		t.Errorf("lastRun = %+v, want the http-01 run that succeeded", got.LastRun)
	}
	if got.Certificate == nil || got.Mode != "active" {
		t.Errorf("mode %q certificate %+v, want a stored certificate and Let's Encrypt in use", got.Mode, got.Certificate)
	}
}

// issue is an admin POST of the issue action.
func (f *acmeFix) issue(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, http.MethodPost, "/api/https/acme/issue", "")
}

// startRun requires the answer to be 202 with a job id, and returns the id.
func startRun(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start a run: %d %s, want 202", rec.Code, rec.Body)
	}
	var b api.HTTPSACMERunStartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.JobID == "" || b.Reused {
		t.Fatalf("start = %+v, want a job id and no reuse", b)
	}
	return b.JobID
}

// awaitRun waits for a run's job to end. The job ends after the run's outcome is stored.
func (f *acmeFix) awaitRun(t *testing.T, jobID string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := f.hub.Wait(context.Background(), jobID); err != nil {
			t.Errorf("Wait: %v", err)
		}
	}()
	testsupport.Await(t, "the certificate run to end", done)
}

// issued registers an account, issues and waits for the run: a stored certificate.
func (f *acmeFix) issued(t *testing.T) string {
	t.Helper()
	f.register(t, "")
	id := startRun(t, f.issue(t))
	f.awaitRun(t, id)
	return id
}

// ordersSeen is how many orders the CA has accepted.
func (f *acmeFix) ordersSeen() int { return f.fake.Count(acmetest.EndpointNewOrder) }

// acmeProblemErrors decodes the errors of a problem body with their values.
func acmeProblemErrors(t *testing.T, rec *httptest.ResponseRecorder) (detail string, errs []struct {
	Location string          `json:"location"`
	Message  string          `json:"message"`
	Value    json.RawMessage `json:"value"`
}) {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
		Errors []struct {
			Location string          `json:"location"`
			Message  string          `json:"message"`
			Value    json.RawMessage `json:"value"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %v: %s", err, rec.Body)
	}
	return p.Detail, p.Errors
}

func TestIssueACMERefusalsAre422WithEveryCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		opts  acmeOpts
		setup func(f *acmeFix)
		codes []string
		check func(t *testing.T, values map[string]api.HTTPSACMERefusalBody)
	}{
		{
			name: "an IP address under a base path", opts: acmeOpts{cfg: config.Config{PublicURL: "https://192.0.2.1/tierplan"}},
			codes: []string{"names_ip", "base_path"},
			check: func(t *testing.T, v map[string]api.HTTPSACMERefusalBody) {
				if v["names_ip"].Host != "192.0.2.1" || v["base_path"].Path != "/tierplan" {
					t.Errorf("values = %+v, want the host and the path", v)
				}
			},
		},
		{
			name: "an agent address that is an IP address", setup: func(f *acmeFix) { f.inst.ApplyAgentURL("https://192.0.2.9") },
			codes: []string{"agent_url_ip"},
			check: func(t *testing.T, v map[string]api.HTTPSACMERefusalBody) {
				if v["agent_url_ip"].Host != "192.0.2.9" {
					t.Errorf("values = %+v", v)
				}
			},
		},
		{
			name: "agents dial hosts the certificate would not cover", setup: func(f *acmeFix) { f.setDialHosts("old.example.com", "old.example.com") },
			codes: []string{"agent_dial_host"},
			check: func(t *testing.T, v map[string]api.HTTPSACMERefusalBody) {
				if d := v["agent_dial_host"]; d.Count != 2 || !slices.Equal(d.Hosts, []string{"old.example.com"}) {
					t.Errorf("values = %+v, want two agents on old.example.com", v)
				}
			},
		},
		{
			name: "a name Let's Encrypt can never issue for", opts: acmeOpts{cfg: config.Config{PublicURL: "https://tierplan.internal"}, realDirectory: true},
			codes: []string{"names_not_public"},
			check: func(t *testing.T, v map[string]api.HTTPSACMERefusalBody) {
				if v["names_not_public"].Host != "tierplan.internal" {
					t.Errorf("values = %+v", v)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newACMEFix(t, tc.opts)
			f.register(t, "")
			if tc.setup != nil {
				tc.setup(f)
			}
			rec := f.issue(t)
			acmeProblemOf(t, rec, http.StatusUnprocessableEntity, "acme_refused")
			detail, errs := acmeProblemErrors(t, rec)
			if detail == "" || strings.ContainsAny(detail, "{}") {
				t.Errorf("detail = %q, want one English sentence", detail)
			}
			values := map[string]api.HTTPSACMERefusalBody{}
			var codes []string
			for _, e := range errs {
				if e.Location != "refusals" {
					t.Errorf("error location = %q, want refusals", e.Location)
				}
				var v api.HTTPSACMERefusalBody
				if err := json.Unmarshal(e.Value, &v); err != nil {
					t.Fatalf("value %s: %v", e.Value, err)
				}
				if v.Code != e.Message {
					t.Errorf("value code %q differs from the message %q", v.Code, e.Message)
				}
				values[e.Message] = v
				codes = append(codes, e.Message)
			}
			for _, want := range tc.codes {
				if !slices.Contains(codes, want) {
					t.Errorf("refusals = %v, want %q among them", codes, want)
				}
			}
			tc.check(t, values)
			if n := f.ordersSeen(); n != 0 {
				t.Errorf("the CA saw %d orders from a refused issue, want none", n)
			}
			if got := f.status(t).ACME; got.ActiveRun != nil || got.LastRun != nil {
				t.Errorf("a refused issue left a run: active %+v last %+v", got.ActiveRun, got.LastRun)
			}
		})
	}

	t.Run("no account yet", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		acmeProblemOf(t, f.issue(t), http.StatusConflict, "acme_no_account")
		acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""), http.StatusConflict, "acme_not_active")
		if n := f.ordersSeen(); n != 0 {
			t.Errorf("the CA saw %d orders, want none", n)
		}
	})
}

func TestIssueACMEReusedIs200(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.issued(t)
	cert := f.status(t).ACME.Certificate
	if cert == nil {
		t.Fatal("no certificate after the issue")
	}
	orders := f.ordersSeen()

	rec := f.issue(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("second issue: %d %s, want 200", rec.Code, rec.Body)
	}
	var b api.HTTPSACMERunStartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if !b.Reused || b.JobID != "" || b.ValidUntil == nil || !b.ValidUntil.Equal(cert.NotAfter) {
		t.Errorf("answer = %+v, want reused with validUntil %v and no job", b, cert.NotAfter)
	}
	if n := f.ordersSeen(); n != orders {
		t.Errorf("the CA saw %d orders after a reused issue, want %d", n, orders)
	}
}

func TestACMERunActiveIs409(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	release := f.fake.HoldFinalize()
	id := startRun(t, f.issue(t))

	for _, path := range []string{"/api/https/acme/issue", "/api/https/acme/renew", "/api/https/acme/stop"} {
		acmeProblemOf(t, f.send(t, http.MethodPost, path, ""), http.StatusConflict, "acme_run_active")
	}
	release()
	f.awaitRun(t, id)
	if n := f.ordersSeen(); n != 1 {
		t.Errorf("the CA saw %d orders, want only the first run's", n)
	}
	if got := f.status(t).ACME; got.LastRun == nil || got.LastRun.Outcome != "succeeded" || got.ActiveRun != nil {
		t.Errorf("after the run: active %+v last %+v", got.ActiveRun, got.LastRun)
	}
}

func TestRenewNowStartsARun(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	first := f.issued(t)
	f.clock.Advance(time.Minute)

	id := startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""))
	if id == first {
		t.Fatalf("the renewal reused the issue's job %s", id)
	}
	f.awaitRun(t, id)
	if n := f.ordersSeen(); n != 2 {
		t.Errorf("the CA saw %d orders, want the issue and the renewal", n)
	}
	got := f.status(t).ACME
	if got.LastRun == nil || got.LastRun.JobID != id || got.LastRun.Kind != "renew" || got.LastRun.Outcome != "succeeded" || got.LastRun.Trigger != "admin" {
		t.Errorf("lastRun = %+v, want the admin renewal that succeeded", got.LastRun)
	}
	if got.Certificate == nil || !got.Certificate.Renewed {
		t.Errorf("certificate = %+v, want it marked renewed", got.Certificate)
	}
}

func TestRenewNowBlockedByRetryAfterIs429(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.issued(t)
	f.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 429, Type: "rateLimited", Detail: "too many orders", RetryAfterSeconds: 3600})
	id := startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""))
	f.awaitRun(t, id)

	// The failed run is reported failed on the stream, with the class and without the CA's words.
	c := openSSE(t, f.authFixture, "/api/https/acme/runs/"+id+"/stream", f.cookie, "")
	nextFrame(t, c) // the snapshot
	fr := nextFrame(t, c)
	if fr.event != "done" {
		t.Fatalf("frame = %+v, want done", fr)
	}
	done := unmarshalFrame[api.HTTPSACMERunFrameBody](t, fr.data)
	if done.State != "failed" || done.Kind != "renew" {
		t.Errorf("done = %+v, want a failed renewal", done)
	}
	if strings.Contains(c.bodyString(), "too many orders") {
		t.Errorf("the stream carries the CA's text: %s", c.bodyString())
	}

	orders := f.ordersSeen()
	for _, path := range []string{"/api/https/acme/renew", "/api/https/acme/issue"} {
		rec := f.send(t, http.MethodPost, path, "")
		acmeProblemOf(t, rec, http.StatusTooManyRequests, "acme_blocked")
		secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if err != nil || secs < 3599 || secs > 3601 {
			t.Errorf("%s Retry-After = %q, want 3600 give or take a second", path, rec.Header().Get("Retry-After"))
		}
		_, errs := acmeProblemErrors(t, rec)
		var until string
		if len(errs) != 1 || errs[0].Location != "reason" || errs[0].Message != "told_to_wait" || json.Unmarshal(errs[0].Value, &until) != nil {
			t.Fatalf("%s errors = %+v, want one told_to_wait reason with the instant", path, errs)
		}
		if want := acmeNow.Add(time.Hour).Format(time.RFC3339); until != want {
			t.Errorf("%s reopens at %q, want %q", path, until, want)
		}
	}
	if n := f.ordersSeen(); n != orders {
		t.Errorf("the CA saw %d orders while blocked, want %d", n, orders)
	}
	if b := f.status(t).ACME.Blocked; b == nil || b.Reason != "told_to_wait" {
		t.Errorf("status blocked = %+v, want told_to_wait", b)
	}

	// When the wait is over the same request starts a run.
	f.clock.Advance(time.Hour)
	startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""))
}

func TestIssueBlockedByTheFailureGuardIs429(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	f.fake.FailValidation("tierplan.example.com", acmetest.Problem{Type: "connection", Detail: "refused"})
	for i := range 3 {
		f.awaitRun(t, startRun(t, f.issue(t)))
		if i < 2 {
			f.clock.Advance(10 * time.Minute)
		}
	}
	orders := f.ordersSeen()

	rec := f.issue(t)
	acmeProblemOf(t, rec, http.StatusTooManyRequests, "acme_blocked")
	_, errs := acmeProblemErrors(t, rec)
	var until string
	if len(errs) != 1 || errs[0].Message != "failure_guard" || json.Unmarshal(errs[0].Value, &until) != nil {
		t.Fatalf("errors = %+v, want one failure_guard reason with the instant", errs)
	}
	reopens, err := time.Parse(time.RFC3339, until)
	if err != nil || !reopens.Equal(acmeNow.Add(time.Hour)) {
		t.Errorf("reopens at %q (%v), want an hour after the first failed run", until, err)
	}
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs != int(reopens.Sub(f.clock.Now()).Seconds()) {
		t.Errorf("Retry-After = %q, want %d", rec.Header().Get("Retry-After"), int(reopens.Sub(f.clock.Now()).Seconds()))
	}
	if n := f.ordersSeen(); n != orders {
		t.Errorf("the CA saw %d orders while blocked, want %d", n, orders)
	}
}

func TestResumeACMEEndpoint(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/resume", ""), http.StatusConflict, "acme_not_paused")

	f.pause(t)
	acmeProblemOf(t, f.issue(t), http.StatusConflict, "acme_paused")
	if n := f.ordersSeen(); n != 0 {
		t.Errorf("the CA saw %d orders while paused, want none", n)
	}
	if rec := f.send(t, http.MethodPost, "/api/https/acme/resume", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("resume: %d %s, want 204", rec.Code, rec.Body)
	}
	if got := f.status(t).ACME.PausedReason; got != "" {
		t.Errorf("pausedReason = %q after the resume", got)
	}
	acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/resume", ""), http.StatusConflict, "acme_not_paused")
	startRun(t, f.issue(t)) // and an issue is allowed again
}

func TestStopACMEEndpoint(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/stop", ""), http.StatusConflict, "acme_not_active")

	id := startRun(t, f.issue(t))
	f.awaitRun(t, id)
	if got := f.status(t).ACME.Mode; got != "active" {
		t.Fatalf("mode = %q after the issue, want active", got)
	}
	orders := f.ordersSeen()

	if rec := f.send(t, http.MethodPost, "/api/https/acme/stop", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("stop: %d %s, want 204", rec.Code, rec.Body)
	}
	got := f.status(t)
	if got.ACME.Mode != "off" || got.Certificate.Source != "self_signed" {
		t.Errorf("after the stop: mode %q source %q, want off and self_signed", got.ACME.Mode, got.Certificate.Source)
	}
	if got.ACME.Account == nil || got.ACME.Certificate == nil {
		t.Errorf("the stop dropped the account or the stored certificate: %+v", got.ACME)
	}
	acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/stop", ""), http.StatusConflict, "acme_not_active")
	acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""), http.StatusConflict, "acme_not_active")

	// Issuing again for the same names serves the stored certificate: no order.
	if rec := f.issue(t); rec.Code != http.StatusOK {
		t.Fatalf("issue after the stop: %d %s, want 200 reused", rec.Code, rec.Body)
	}
	if n := f.ordersSeen(); n != orders {
		t.Errorf("the CA saw %d orders after the stop and the reissue, want %d", n, orders)
	}
	if got := f.status(t).ACME.Mode; got != "active" {
		t.Errorf("mode = %q after the reissue, want active", got)
	}
}

func TestHTTPSRunRoutesNeedSettingsWrite(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	id := f.issued(t)
	f.addUser(t, "viewer", false) // no role at all
	viewer, viewerCSRF := f.signIn(t, "viewer", false)
	orders, mode := f.ordersSeen(), f.status(t).ACME.Mode

	writes := []string{"/api/https/acme/issue", "/api/https/acme/renew", "/api/https/acme/resume", "/api/https/acme/stop"}
	for _, path := range writes {
		if rec := f.call(t, authReq{method: http.MethodPost, path: path}); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous POST %s: %d, want 401", path, rec.Code)
		}
		if rec := f.call(t, authReq{method: http.MethodPost, path: path, cookie: viewer, csrf: viewerCSRF}); rec.Code != http.StatusForbidden {
			t.Errorf("a user without settings.write on POST %s: %d, want 403", path, rec.Code)
		}
		if rec := f.call(t, authReq{method: http.MethodPost, path: path, cookie: f.cookie}); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without the CSRF token: %d, want 403", path, rec.Code)
		}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: f.cookie}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s from a foreign origin: %d %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if n := f.ordersSeen(); n != orders {
		t.Errorf("refused requests reached the CA: %d orders, want %d", n, orders)
	}
	if got := f.status(t).ACME.Mode; got != mode {
		t.Errorf("a refused request changed the mode from %q to %q", mode, got)
	}

	// The stream needs settings.read: refused to a user without it, open to the admin.
	stream := "/api/https/acme/runs/" + id + "/stream"
	if rec := f.call(t, authReq{method: http.MethodGet, path: stream}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous stream: %d, want 401", rec.Code)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: stream, cookie: viewer}); rec.Code != http.StatusForbidden {
		t.Errorf("a user without settings.read on the stream: %d, want 403", rec.Code)
	}
	c := openSSE(t, f.authFixture, stream, f.cookie, "")
	if c.status0() != http.StatusOK {
		t.Errorf("admin stream: %d, want 200", c.status0())
	}
}

func TestACMEStreamRefusesOtherJobsAndCarriesNoSecrets(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})

	// A job that is not a certificate run, and one that does not exist, are both just not there.
	other, err := f.hub.Start(jobs.Spec{Kind: "pull", Items: []jobs.ItemSpec{{ID: "n1", Label: "node"}}}, func(_ context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.awaitRun(t, other.ID)
	for _, id := range []string{other.ID, "no-such-job"} {
		rec := f.get(t, "/api/https/acme/runs/"+id+"/stream")
		acmeProblemOf(t, rec, http.StatusNotFound, "acme_run_not_found")
		if strings.Contains(rec.Body.String(), "pull") || strings.Contains(rec.Body.String(), "node") {
			t.Errorf("the refusal describes the other job: %s", rec.Body)
		}
	}

	// A run that succeeds, and one that fails with words of the CA's own, show neither key material
	// nor those words.
	issueID := f.issued(t)
	f.fake.FailValidation("tierplan.example.com", acmetest.Problem{Type: "connection", Detail: "ca-private-detail-7731"})
	f.clock.Advance(time.Minute)
	renewID := startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""))
	f.awaitRun(t, renewID)

	ctx := context.Background()
	_, acctKey, found, err := f.store.Account(ctx, f.dir.URL)
	if err != nil || !found {
		t.Fatalf("Account = found %v, %v", found, err)
	}
	_, certKey, found, err := f.store.Certificate(ctx)
	if err != nil || !found {
		t.Fatalf("Certificate = found %v, %v", found, err)
	}
	var secrets []string
	for _, k := range []*ecdsa.PrivateKey{acctKey, certKey} {
		d, err := k.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, hex.EncodeToString(d), base64.StdEncoding.EncodeToString(d), base64.RawURLEncoding.EncodeToString(d))
	}
	for _, k := range []any{acctKey, certKey} {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, base64.StdEncoding.EncodeToString(der))
	}
	for name, id := range map[string]string{"issue": issueID, "renewal": renewID} {
		c := openSSE(t, f.authFixture, "/api/https/acme/runs/"+id+"/stream", f.cookie, "")
		snap := nextFrame(t, c)
		fr := nextFrame(t, c)
		if snap.event != "snapshot" || fr.event != "done" {
			t.Fatalf("%s: frames %q then %q, want snapshot then done", name, snap.event, fr.event)
		}
		body := c.bodyString()
		for _, bad := range []string{"PRIVATE KEY", "BEGIN ", "keyAuthorization", "key_authorization", "ca-private-detail-7731"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s stream holds %q: %s", name, bad, body)
			}
		}
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("%s stream holds key material %q", name, secret)
			}
		}
		done := unmarshalFrame[api.HTTPSACMERunFrameBody](t, fr.data)
		want := map[string]string{"issue": "done", "renewal": "failed"}[name]
		if done.State != want {
			t.Errorf("%s done state = %q, want %q", name, done.State, want)
		}
		if name == "renewal" {
			var failedCode string
			for _, it := range done.Items {
				if it.State == "failed" {
					failedCode = it.Code
				}
			}
			if failedCode == "" {
				t.Errorf("no failed step carries a class code: %+v", done.Items)
			}
		}
	}
}
