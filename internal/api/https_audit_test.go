// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// The audit rows of the admin's Let's Encrypt requests (Phase 17, SET-04, D-09, D-11, amended D-27).

// httpsRow is one https.* audit row as the audit page reads it.
type httpsRow struct {
	Seq                              int64
	ActorType, Actor, Action, Result string
	Reason, IP, UserAgent            string
	TargetType, Target               string
	Details                          map[string]string
	hasEnv                           bool
}

// httpsRows reads every https.* row of the audit log, oldest first, through the audit endpoint as the admin.
func (f *acmeFix) httpsRows(t *testing.T) []httpsRow {
	t.Helper()
	rec := f.get(t, "/api/audit?limit=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/audit: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []struct {
			Seq        int64             `json:"seq"`
			ActorType  string            `json:"actorType"`
			Actor      string            `json:"actor"`
			Action     string            `json:"action"`
			TargetType string            `json:"targetType"`
			Target     string            `json:"target"`
			Env        *string           `json:"environment"`
			Result     string            `json:"result"`
			Reason     string            `json:"reason"`
			IP         string            `json:"ip"`
			UserAgent  string            `json:"userAgent"`
			Details    map[string]string `json:"details"`
		} `json:"items"`
		NextCursor *int64 `json:"nextCursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.NextCursor != nil {
		t.Fatalf("the audit log has more than one page of rows; the test reads a single page")
	}
	var rows []httpsRow
	for _, it := range list.Items {
		if !strings.HasPrefix(it.Action, "https.") {
			continue
		}
		rows = append(rows, httpsRow{
			Seq: it.Seq, ActorType: it.ActorType, Actor: it.Actor, Action: it.Action, Result: it.Result,
			Reason: it.Reason, IP: it.IP, UserAgent: it.UserAgent, TargetType: it.TargetType, Target: it.Target,
			Details: it.Details, hasEnv: it.Env != nil,
		})
	}
	slices.SortFunc(rows, func(a, b httpsRow) int { return int(a.Seq - b.Seq) })
	return rows
}

// rowsOf is the rows of one action, oldest first.
func rowsOf(rows []httpsRow, action string) []httpsRow {
	var out []httpsRow
	for _, r := range rows {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// rowsOfJob is the rows of one action whose job detail is id.
func rowsOfJob(rows []httpsRow, action, id string) []httpsRow {
	var out []httpsRow
	for _, r := range rowsOf(rows, action) {
		if r.Details["job"] == id {
			out = append(out, r)
		}
	}
	return out
}

// requireAdminRow checks the row is the signed-in admin's, from the request's address and browser, on the HTTPS target
// and in no environment.
func requireAdminRow(t *testing.T, r httpsRow, action, result string) {
	t.Helper()
	if r.Action != action || r.Result != result {
		t.Errorf("row = %s/%s, want %s/%s", r.Action, r.Result, action, result)
	}
	if r.ActorType != "user" || r.Actor != "admin" {
		t.Errorf("%s actor = %s %q, want the signed-in admin", action, r.ActorType, r.Actor)
	}
	if r.IP != authIP || r.UserAgent != authUA {
		t.Errorf("%s ip %q user-agent %q, want the request's %q and %q", action, r.IP, r.UserAgent, authIP, authUA)
	}
	if r.TargetType != "https" || r.Target != "https" || r.hasEnv {
		t.Errorf("%s target = %q/%q (env %v), want https/https and no environment", action, r.TargetType, r.Target, r.hasEnv)
	}
}

func TestHTTPSAuditRunActions(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")
		id := startRun(t, f.issue(t))
		f.awaitRun(t, id)

		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSIssueRequested)
		if len(rows) != 1 {
			t.Fatalf("%d https.issue_requested rows, want 1: %+v", len(rows), rows)
		}
		requireAdminRow(t, rows[0], audit.ActionHTTPSIssueRequested, "success")
		if rows[0].Details["job"] != id || rows[0].Details["reused"] != "false" {
			t.Errorf("details = %+v, want job %s and reused false", rows[0].Details, id)
		}
	})

	t.Run("request row precedes the outcome row of a run that fails at its first CA call", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")
		f.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 429, Type: "rateLimited", Detail: "too many orders", RetryAfterSeconds: 3600})
		id := startRun(t, f.issue(t))
		f.awaitRun(t, id)

		rows := f.httpsRows(t)
		req, failed := rowsOfJob(rows, audit.ActionHTTPSIssueRequested, id), rowsOfJob(rows, audit.ActionHTTPSRunFailed, id)
		if len(req) != 1 || len(failed) != 1 {
			t.Fatalf("%d request rows and %d run_failed rows for job %s, want 1 and 1: %+v", len(req), len(failed), id, rows)
		}
		if req[0].Seq >= failed[0].Seq {
			t.Errorf("request row seq %d, run_failed seq %d: the request must come first", req[0].Seq, failed[0].Seq)
		}
		if failed[0].ActorType != "system" {
			t.Errorf("the outcome row's actor type = %q, want system", failed[0].ActorType)
		}
	})

	t.Run("reused certificate", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		before := len(rowsOf(f.httpsRows(t), audit.ActionHTTPSIssued))

		rec := f.issue(t)
		if rec.Code != http.StatusOK {
			t.Fatalf("second issue: %d %s, want 200 reused", rec.Code, rec.Body)
		}
		rows := f.httpsRows(t)
		reqs := rowsOf(rows, audit.ActionHTTPSIssueRequested)
		if len(reqs) != 2 {
			t.Fatalf("%d https.issue_requested rows, want 2 (the run's and the reuse)", len(reqs))
		}
		last := reqs[1]
		requireAdminRow(t, last, audit.ActionHTTPSIssueRequested, "success")
		if last.Details["reused"] != "true" || last.Details["valid_until"] == "" || last.Details["job"] != "" {
			t.Errorf("details = %+v, want reused true with valid_until and no job", last.Details)
		}
		if _, err := time.Parse(time.RFC3339, last.Details["valid_until"]); err != nil {
			t.Errorf("valid_until = %q: %v", last.Details["valid_until"], err)
		}
		if after := len(rowsOf(rows, audit.ActionHTTPSIssued)); after != before {
			t.Errorf("%d https.issued rows after a reuse, want %d: nothing ran", after, before)
		}
	})

	t.Run("refused by preflight", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{cfg: config.Config{PublicURL: "https://192.0.2.1"}})
		f.register(t, "")
		acmeProblemOf(t, f.issue(t), http.StatusUnprocessableEntity, "acme_refused")

		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSIssueRequested)
		if len(rows) != 1 {
			t.Fatalf("%d https.issue_requested rows, want 1: %+v", len(rows), rows)
		}
		requireAdminRow(t, rows[0], audit.ActionHTTPSIssueRequested, "failure")
		if rows[0].Reason != "acme_refused" || rows[0].Details["refusals"] != "names_ip" {
			t.Errorf("reason %q details %+v, want acme_refused with refusals names_ip", rows[0].Reason, rows[0].Details)
		}
		if raw, _ := json.Marshal(rows[0]); strings.Contains(string(raw), "192.0.2.1") {
			t.Errorf("the row carries the address the admin typed: %s", raw)
		}
	})

	t.Run("refused with no account", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		acmeProblemOf(t, f.issue(t), http.StatusConflict, "acme_no_account")

		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSIssueRequested)
		if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != "acme_no_account" {
			t.Fatalf("rows = %+v, want one failure row with reason acme_no_account", rows)
		}
	})

	t.Run("blocked by the failure guard", func(t *testing.T) {
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
		rec := f.issue(t)
		acmeProblemOf(t, rec, http.StatusTooManyRequests, "acme_blocked")
		_, errs := acmeProblemErrors(t, rec)
		var until string
		if len(errs) != 1 || json.Unmarshal(errs[0].Value, &until) != nil {
			t.Fatalf("errors = %+v, want one reason with the instant", errs)
		}

		reqs := rowsOf(f.httpsRows(t), audit.ActionHTTPSIssueRequested)
		if len(reqs) != 4 {
			t.Fatalf("%d https.issue_requested rows, want 4 (three runs and the refusal)", len(reqs))
		}
		last := reqs[3]
		requireAdminRow(t, last, audit.ActionHTTPSIssueRequested, "failure")
		if last.Reason != "acme_blocked" || last.Details["until"] != until {
			t.Errorf("reason %q details %+v, want acme_blocked with until %s", last.Reason, last.Details, until)
		}
	})

	t.Run("renew now accepted precedes its outcome row", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		id := startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""))
		f.awaitRun(t, id)

		rows := f.httpsRows(t)
		req, done := rowsOfJob(rows, audit.ActionHTTPSRenewRequested, id), rowsOfJob(rows, audit.ActionHTTPSRenewed, id)
		if len(req) != 1 || len(done) != 1 {
			t.Fatalf("%d request rows and %d renewed rows for job %s, want 1 and 1: %+v", len(req), len(done), id, rows)
		}
		requireAdminRow(t, req[0], audit.ActionHTTPSRenewRequested, "success")
		if req[0].Seq >= done[0].Seq {
			t.Errorf("request row seq %d, renewed seq %d: the request must come first", req[0].Seq, done[0].Seq)
		}
	})

	t.Run("renew now blocked by the CA's wait", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		f.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 429, Type: "rateLimited", Detail: "too many orders", RetryAfterSeconds: 3600})
		f.awaitRun(t, startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", "")))

		rec := f.send(t, http.MethodPost, "/api/https/acme/renew", "")
		acmeProblemOf(t, rec, http.StatusTooManyRequests, "acme_blocked")
		_, errs := acmeProblemErrors(t, rec)
		var until string
		if len(errs) != 1 || json.Unmarshal(errs[0].Value, &until) != nil {
			t.Fatalf("errors = %+v, want one reason with the instant", errs)
		}
		reqs := rowsOf(f.httpsRows(t), audit.ActionHTTPSRenewRequested)
		if len(reqs) != 2 {
			t.Fatalf("%d https.renew_requested rows, want 2 (the run and the refusal)", len(reqs))
		}
		requireAdminRow(t, reqs[1], audit.ActionHTTPSRenewRequested, "failure")
		if reqs[1].Reason != "acme_blocked" || reqs[1].Details["until"] != until {
			t.Errorf("reason %q details %+v, want acme_blocked with until %s", reqs[1].Reason, reqs[1].Details, until)
		}
	})
}

// A run cancelled through the jobs API while it waits at validation, before its certificate was obtained, keeps the
// request row the admin's press wrote and writes no outcome row (amended D-27). The request row is written by the
// run-started hook before the run's body starts, so it exists whatever the cancel does; the test proves the run
// neither adds an outcome row nor removes the request row.
func TestHTTPSAuditCancelledRunKeepsTheRequestRow(t *testing.T) {
	t.Parallel()

	cancelled := func(t *testing.T, f *acmeFix, path, requestAction string) {
		t.Helper()
		release := f.fake.HoldValidation()
		defer release()
		id := startRun(t, f.send(t, http.MethodPost, path, ""))
		f.clock.WaitArmed(t) // the run is parked at validation
		if err := f.hub.Cancel(id); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		f.awaitRun(t, id)
		release()

		// The account and order steps had finished, so the hub reports the job partial; what shows the cancel landed at
		// validation is that step ending cancelled.
		job, ok := f.hub.Get(id)
		if !ok || !job.Finished() {
			t.Fatalf("job %s found %v finished %v, want it ended", id, ok, ok && job.Finished())
		}
		var validate string
		for _, it := range job.Items {
			if it.ID == acme.StepValidate {
				validate = string(it.State)
			}
		}
		if validate != string(jobs.ItemCancelled) {
			t.Fatalf("the validate step ended %q (job %s), want cancelled", validate, job.State)
		}
		rows := f.httpsRows(t)
		if got := rowsOfJob(rows, requestAction, id); len(got) != 1 {
			t.Fatalf("%d %s rows for job %s, want 1: %+v", len(got), requestAction, id, rows)
		} else {
			requireAdminRow(t, got[0], requestAction, "success")
		}
		for _, outcome := range []string{audit.ActionHTTPSIssued, audit.ActionHTTPSRenewed, audit.ActionHTTPSRunFailed} {
			if got := rowsOfJob(rows, outcome, id); len(got) != 0 {
				t.Errorf("%d %s rows for the cancelled job %s, want none: %+v", len(got), outcome, id, got)
			}
		}
	}

	t.Run("issue", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")
		cancelled(t, f, "/api/https/acme/issue", audit.ActionHTTPSIssueRequested)
	})

	t.Run("renew-now", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		cancelled(t, f, "/api/https/acme/renew", audit.ActionHTTPSRenewRequested)
	})
}

func TestHTTPSAuditRegister(t *testing.T) {
	t.Parallel()

	t.Run("registered", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "ops@example.com")

		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSAccountRegistered)
		if len(rows) != 1 {
			t.Fatalf("%d https.account_registered rows, want 1: %+v", len(rows), rows)
		}
		requireAdminRow(t, rows[0], audit.ActionHTTPSAccountRegistered, "success")
		d := rows[0].Details
		if d["contact"] != "ops@example.com" || d["terms_agreed"] != "true" || d["trigger"] != "admin" {
			t.Errorf("details = %+v, want contact ops@example.com, terms_agreed true, trigger admin", d)
		}
	})

	t.Run("registered without an email", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")

		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSAccountRegistered)
		if len(rows) != 1 {
			t.Fatalf("%d rows, want 1", len(rows))
		}
		if _, ok := rows[0].Details["contact"]; ok || rows[0].Details["terms_agreed"] != "true" {
			t.Errorf("details = %+v, want terms_agreed and no contact", rows[0].Details)
		}
	})

	refusals := []struct {
		name, body, reason string
		status             int
	}{
		{name: "terms not agreed", body: `{"agreeTos":false,"email":"not-an-email-4417"}`, reason: "acme_tos_required", status: http.StatusUnprocessableEntity},
		{name: "invalid email", body: `{"agreeTos":true,"email":"not-an-email-4417"}`, reason: "acme_contact_invalid", status: http.StatusUnprocessableEntity},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newACMEFix(t, acmeOpts{})
			acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/account", tc.body), tc.status, tc.reason)

			rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSAccountRegistered)
			if len(rows) != 1 {
				t.Fatalf("%d https.account_registered rows, want 1: %+v", len(rows), rows)
			}
			requireAdminRow(t, rows[0], audit.ActionHTTPSAccountRegistered, "failure")
			if rows[0].Reason != tc.reason || len(rows[0].Details) != 0 {
				t.Errorf("reason %q details %+v, want %s and no details", rows[0].Reason, rows[0].Details, tc.reason)
			}
			if raw, _ := json.Marshal(rows[0]); strings.Contains(string(raw), "not-an-email-4417") {
				t.Errorf("the row carries the typed email: %s", raw)
			}
		})
	}

	t.Run("account exists", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")
		acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/account", `{"agreeTos":true,"email":"other@example.org"}`), http.StatusConflict, "acme_account_exists")

		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSAccountRegistered)
		if len(rows) != 2 || rows[1].Result != "failure" || rows[1].Reason != "acme_account_exists" || len(rows[1].Details) != 0 {
			t.Fatalf("rows = %+v, want the success and a failure row with reason acme_account_exists", rows)
		}
		if raw, _ := json.Marshal(rows[1]); strings.Contains(string(raw), "other@example.org") {
			t.Errorf("the refusal carries the typed email: %s", raw)
		}
	})

	t.Run("the CA is unreachable", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		for range 4 {
			f.fake.FailNext(acmetest.EndpointDirectory, acmetest.Problem{Status: 503, Type: "serverInternal", Detail: "maintenance window secret-4418"})
		}
		acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/account", `{"agreeTos":true}`), http.StatusBadGateway, "acme_ca_error")

		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSAccountRegistered)
		if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != acme.ClassCAUnavailable {
			t.Fatalf("rows = %+v, want one failure row whose reason is the class %s", rows, acme.ClassCAUnavailable)
		}
		if raw, _ := json.Marshal(rows[0]); strings.Contains(string(raw), "secret-4418") {
			t.Errorf("the row carries the CA's text: %s", raw)
		}
	})
}

func TestHTTPSAuditRefusals(t *testing.T) {
	t.Parallel()
	off, on := false, true

	// refused runs one request that the manager refuses and requires its row: a failure of the action with the reason.
	refused := func(t *testing.T, f *acmeFix, path, action string, status int, reason string) httpsRow {
		t.Helper()
		before := len(rowsOf(f.httpsRows(t), action))
		acmeProblemOf(t, f.send(t, http.MethodPost, path, ""), status, reason)
		rows := rowsOf(f.httpsRows(t), action)
		if len(rows) != before+1 {
			t.Fatalf("%d %s rows after the refusal, want %d", len(rows), action, before+1)
		}
		last := rows[len(rows)-1]
		requireAdminRow(t, last, action, "failure")
		if last.Reason != reason {
			t.Errorf("%s reason = %q, want %q", action, last.Reason, reason)
		}
		return last
	}

	t.Run("resume when not paused", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		refused(t, f, "/api/https/acme/resume", audit.ActionHTTPSResumed, http.StatusConflict, "acme_not_paused")
	})
	t.Run("resume after a restore pause", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.pause(t)
		if rec := f.send(t, http.MethodPost, "/api/https/acme/resume", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("resume: %d %s, want 204", rec.Code, rec.Body)
		}
		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSResumed)
		if len(rows) != 1 {
			t.Fatalf("%d https.resumed rows, want 1", len(rows))
		}
		requireAdminRow(t, rows[0], audit.ActionHTTPSResumed, "success")
		if len(rows[0].Details) != 0 {
			t.Errorf("details = %+v, want none", rows[0].Details)
		}
	})
	t.Run("stop while active", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		if rec := f.send(t, http.MethodPost, "/api/https/acme/stop", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("stop: %d %s, want 204", rec.Code, rec.Body)
		}
		rows := rowsOf(f.httpsRows(t), audit.ActionHTTPSStopped)
		if len(rows) != 1 {
			t.Fatalf("%d https.stopped rows, want 1", len(rows))
		}
		requireAdminRow(t, rows[0], audit.ActionHTTPSStopped, "success")
	})
	t.Run("stop while HSTS is on", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{hstsOn: func() bool { return true }})
		refused(t, f, "/api/https/acme/stop", audit.ActionHTTPSStopped, http.StatusConflict, "hsts_on")
	})
	t.Run("stop while TIERPLAN_ACME is set", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{env: config.ACMEEnv{Enabled: &on}})
		f.register(t, "")
		refused(t, f, "/api/https/acme/stop", audit.ActionHTTPSStopped, http.StatusConflict, "acme_locked")
	})
	t.Run("stop when not in use", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		refused(t, f, "/api/https/acme/stop", audit.ActionHTTPSStopped, http.StatusConflict, "acme_not_active")
	})
	t.Run("issue and renew locked off", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{env: config.ACMEEnv{Enabled: &off}})
		f.register(t, "")
		refused(t, f, "/api/https/acme/issue", audit.ActionHTTPSIssueRequested, http.StatusConflict, "acme_locked")
		refused(t, f, "/api/https/acme/renew", audit.ActionHTTPSRenewRequested, http.StatusConflict, "acme_locked")
	})
	t.Run("issue while paused", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")
		f.pause(t)
		refused(t, f, "/api/https/acme/issue", audit.ActionHTTPSIssueRequested, http.StatusConflict, "acme_paused")
	})
	t.Run("issue while certificate files are set", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{certFiles: true})
		refused(t, f, "/api/https/acme/issue", audit.ActionHTTPSIssueRequested, http.StatusConflict, "acme_cert_files")
	})
	t.Run("renew when not in use", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")
		refused(t, f, "/api/https/acme/renew", audit.ActionHTTPSRenewRequested, http.StatusConflict, "acme_not_active")
	})
	t.Run("issue while a run is in progress", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.register(t, "")
		release := f.fake.HoldValidation()
		defer release()
		id := startRun(t, f.issue(t))
		f.clock.WaitArmed(t)
		refused(t, f, "/api/https/acme/issue", audit.ActionHTTPSIssueRequested, http.StatusConflict, "acme_run_active")
		if err := f.hub.Cancel(id); err != nil {
			t.Fatal(err)
		}
		f.awaitRun(t, id)
		release()
	})
	t.Run("stop while a renewal is in progress", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		release := f.fake.HoldValidation()
		defer release()
		id := startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", ""))
		f.clock.WaitArmed(t)
		refused(t, f, "/api/https/acme/stop", audit.ActionHTTPSStopped, http.StatusConflict, "acme_run_active")
		if err := f.hub.Cancel(id); err != nil {
			t.Fatal(err)
		}
		f.awaitRun(t, id)
		release()
	})
}

// A user without settings.write is refused by the permission check before any handler runs: the only row is the
// permission.denied one, and no https.* row is written for the attempt.
func TestHTTPSAuditPermissionDeniedWritesNoHTTPSRow(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	f.addUser(t, "viewer", false) // no role, so no settings.write
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "viewer", false)

	before := len(f.httpsRows(t))
	denied := len(f.audits(t, audit.ActionPermissionDenied))
	paths := []string{"/api/https/acme/issue", "/api/https/acme/renew", "/api/https/acme/resume", "/api/https/acme/stop", "/api/https/acme/account"}
	for _, path := range paths {
		body := ""
		if strings.HasSuffix(path, "/account") {
			body = `{"agreeTos":true,"email":"not-an-email-4417"}`
		}
		rec := f.call(t, authReq{method: http.MethodPost, path: path, body: body, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("POST %s without settings.write: %d %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if after := len(f.httpsRows(t)); after != before {
		t.Errorf("%d https.* rows after the refused requests, want %d", after, before)
	}
	if got := len(f.audits(t, audit.ActionPermissionDenied)); got != denied+len(paths) {
		t.Errorf("%d permission.denied rows, want %d: each refusal keeps its own row", got, denied+len(paths))
	}
}

// A request whose audit row cannot be written is not reported as a clean success: it answers 500 and the failure is
// logged (D-11). The refusals and the account route are used because they start no run.
func TestHTTPSAuditWriteFailureAnswers500(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, action, path, body string
		setup                    func(t *testing.T, f *acmeFix)
	}{
		{name: "resume", action: audit.ActionHTTPSResumed, path: "/api/https/acme/resume", setup: func(t *testing.T, f *acmeFix) { f.pause(t) }},
		{name: "a refused issue", action: audit.ActionHTTPSIssueRequested, path: "/api/https/acme/issue"},
		{name: "register", action: audit.ActionHTTPSAccountRegistered, path: "/api/https/acme/account", body: `{"agreeTos":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newACMEFix(t, acmeOpts{})
			var logs strings.Builder
			f.https.Log = slog.New(slog.NewJSONHandler(&logs, nil))
			f.h = f.build(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			//nolint:gosec // the action is one of the constants in this table, in a throwaway test database
			if _, err := f.st.Write.Exec(`CREATE TRIGGER refuse_https_audit BEFORE INSERT ON audit_log WHEN NEW.action = '` + tc.action + `'
				BEGIN SELECT RAISE(ABORT, 'audit log unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			rec := f.send(t, http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("POST %s with a failing audit log: %d %s, want 500", tc.path, rec.Code, rec.Body)
			}
			if !strings.Contains(logs.String(), "audit the Let's Encrypt request") {
				t.Errorf("the failure is not logged: %s", logs.String())
			}
		})
	}
}
