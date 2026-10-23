// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ianglennon/bayplan/internal/jobs"
)

// The jobs indicator lists the stack operations a person may follow (plan 07-19, UI-SPEC Defaults 4):
// every stack job on an environment where they may read stacks, and nothing else.

type stackJobRow struct {
	ID         string `json:"id"`
	EnvID      string `json:"envId"`
	EnvName    string `json:"envName"`
	StackID    string `json:"stackId"`
	StackName  string `json:"stackName"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	StartedBy  string `json:"startedBy"`
}

func (f *envFix) listStackJobs(t *testing.T, cookie, csrf string) (rows []stackJobRow, rec *httptest.ResponseRecorder) {
	t.Helper()
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/stack-jobs", cookie: cookie, csrf: csrf})
	if rec.Code == http.StatusOK {
		rows = answerOf[struct {
			Jobs []stackJobRow `json:"jobs"`
		}](t, rec).Jobs
	}
	return rows, rec
}

func TestStackJobsListsTheStackOperationsOfAnEnvironmentTheCallerMayRead(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	alpha, beta := newStack(t, admin, "alpha"), newStack(t, admin, "beta")
	f.deployGood(t, operator, alpha.ID, composeWeb127)
	running := startStuckDeploy(t, f, operator, beta)
	t.Cleanup(func() { _ = f.jobHub.Cancel(running) })

	// A viewer of the environment sees both, the running one first, with what the indicator draws.
	rows, rec := f.listStackJobs(t, viewer.cookie, viewer.csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if len(rows) != 2 {
		t.Fatalf("jobs = %+v, want the finished deploy of alpha and the running one of beta", rows)
	}
	first, second := rows[0], rows[1]
	if first.ID != running || first.StackName != "beta" || first.StackID != beta.ID || first.State != "running" || first.Kind != "deploy" ||
		first.EnvID != e.ID || first.EnvName == "" || first.StartedBy != "operator-1" || first.StartedAt == "" || first.FinishedAt != "" {
		t.Errorf("first = %+v, want beta's running deploy started by operator-1", first)
	}
	if second.StackName != "alpha" || second.State != "done" || second.Kind != "deploy" || second.FinishedAt == "" {
		t.Errorf("second = %+v, want alpha's finished deploy", second)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
}

func TestStackJobsLeavesOutAnEnvironmentTheCallerCannotRead(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "alpha")
	f.deployGood(t, operator, st.ID, composeWeb127)

	// Someone with no role on the environment is told nothing about it: the list is empty, not refused.
	f.addUser(t, "outsider", false)
	cookie, csrf := f.signIn(t, "outsider", false)
	rows, rec := f.listStackJobs(t, cookie, csrf)
	if rec.Code != http.StatusOK || len(rows) != 0 {
		t.Fatalf("outsider: %d %+v, want 200 and no jobs", rec.Code, rows)
	}
	if rows, _ := f.listStackJobs(t, operator.cookie, operator.csrf); len(rows) != 1 {
		t.Errorf("operator sees %+v, want the deploy", rows)
	}
	// Without a session there is nothing to list.
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/stack-jobs"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}

func TestStackJobsAreOnlyStackJobsAndSayRollBackForOne(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "alpha")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployThenCancel(t, operator, st, composeWeb128)
	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID)
	// A pull is a job of the hub too, and belongs to the Pulls list, not this one.
	if _, err := f.jobHub.Start(jobs.Spec{Kind: "pull", EnvID: e.ID, Owner: "user:id-operator-1"}, func(ctx context.Context, _ *jobs.Reporter) { <-ctx.Done() }); err != nil {
		t.Fatal(err)
	}

	rows, _ := f.listStackJobs(t, operator.cookie, operator.csrf)
	// Newest first: the roll back, the cancelled deploy, the first deploy.
	if len(rows) != 3 || rows[0].Kind != "rollback" || rows[0].State != "done" || rows[1].Kind != "deploy" || rows[1].State != "cancelled" || rows[2].State != "done" {
		t.Errorf("jobs = %+v, want the roll back, the cancelled deploy and the first deploy, newest first", rows)
	}
}
