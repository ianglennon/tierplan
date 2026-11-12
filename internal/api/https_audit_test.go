// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
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
}
