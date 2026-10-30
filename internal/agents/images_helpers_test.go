// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build integration || swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/server/servertest"
)

// pullItem is one node of a pull job as far as these tests read it.
type pullItem struct {
	NodeID   string `json:"nodeId"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Code     string `json:"code"`
	Digest   string `json:"digest"`
	UpToDate bool   `json:"upToDate"`
}

// pullJob is GET /api/pulls/{jobId} as far as these tests read it.
type pullJob struct {
	ID    string     `json:"id"`
	State string     `json:"state"`
	Items []pullItem `json:"items"`
}

// startPullJob POSTs a pull and returns the job ID it answers 202 with.
func startPullJob(t *testing.T, srv *servertest.Server, envID, ref string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"ref": ref}
	for k, v := range extra {
		body[k] = v
	}
	status, raw := srv.Do(http.MethodPost, "/api/env/"+envID+"/images/pull", body)
	if status != http.StatusAccepted {
		t.Fatalf("POST images/pull %s = %d %s", ref, status, raw)
	}
	var out struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.JobID == "" {
		t.Fatalf("pull answer %s: %v", raw, err)
	}
	return out.JobID
}

// pullAndWait starts a pull and follows the job until it has ended.
func pullAndWait(t *testing.T, srv *servertest.Server, envID, ref string, extra map[string]any) pullJob {
	t.Helper()
	id := startPullJob(t, srv, envID, ref, extra)
	var job pullJob
	srv.WaitFor("the pull of "+ref+" to end", 10*time.Minute, func() (bool, string) {
		status, raw := srv.Do(http.MethodGet, "/api/pulls/"+id, nil)
		if status != http.StatusOK {
			return false, string(raw)
		}
		job = pullJob{}
		if err := json.Unmarshal(raw, &job); err != nil {
			return false, err.Error()
		}
		return job.State != "running", string(raw)
	})
	return job
}
