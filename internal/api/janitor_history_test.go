// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/janitor"
)

// The run history (plan 11-14, JAN-10, D-16): every environment lists its runs, newest first, without
// their items.

// seedRun puts a run on record as the janitor itself would have left it.
func seedRun(t *testing.T, store *janitor.SQLStore, run janitor.Run) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if !run.FinishedAt.IsZero() {
		if err := store.FinishRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
}

// janitorRunsList is the answer of GET /api/env/{envId}/janitor/runs.
type janitorRunsList struct {
	Runs []struct {
		ID              string  `json:"id"`
		Trigger         string  `json:"trigger"`
		Actor           string  `json:"actor"`
		ActorName       string  `json:"actorName"`
		State           string  `json:"state"`
		Reason          string  `json:"reason"`
		StartedAt       string  `json:"startedAt"`
		FinishedAt      *string `json:"finishedAt"`
		DurationSeconds *int64  `json:"durationSeconds"`
		EstBytes        *int64  `json:"estBytes"`
		JobID           string  `json:"jobId"`
		Counts          struct {
			RemovedContainers int `json:"removedContainers"`
			Failed            int `json:"failed"`
		} `json:"counts"`
	} `json:"runs"`
	Total int `json:"total"`
}

func (s *swarmFix) listRuns(t *testing.T, envID, query string) (*envRec, janitorRunsList) {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/api/env/"+envID+"/janitor/runs"+query, "")
	var got janitorRunsList
	if rec.code == http.StatusOK {
		got = decode[janitorRunsList](t, rec)
	}
	return rec, got
}

func TestJanitorRunsListNewestFirstWithoutItems(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	store := &janitor.SQLStore{St: s.st}
	other := s.srv.NewEnvironment(t, "Other")
	base := time.Date(2026, 2, 1, 8, 0, 0, 0, time.UTC)
	// 30 runs an hour apart, and three that began at the very same instant: those read in ID order,
	// the newest ID first. One run of another environment is not listed.
	for i := range 30 {
		started := base.Add(time.Duration(i) * time.Hour)
		seedRun(t, store, janitor.Run{
			ID: fmt.Sprintf("run-%02d", i), EnvID: s.env.ID, Trigger: janitor.TriggerSchedule, Actor: janitor.SystemActor,
			State: janitor.StateCompleted, StartedAt: started, FinishedAt: started.Add(90 * time.Second), EstBytes: 2048,
			Counts: janitor.Counts{RemovedContainers: i},
		})
	}
	tie := base.Add(100 * time.Hour)
	for _, id := range []string{"tie-a", "tie-c", "tie-b"} {
		seedRun(t, store, janitor.Run{ID: id, EnvID: s.env.ID, Trigger: janitor.TriggerManual, Actor: "id-admin", State: janitor.StateRunning, StartedAt: tie, EstBytes: -1})
	}
	seedRun(t, store, janitor.Run{ID: "elsewhere", EnvID: other.ID, Trigger: janitor.TriggerManual, Actor: "id-admin", State: janitor.StateCompleted, StartedAt: tie.Add(time.Hour), FinishedAt: tie.Add(2 * time.Hour), EstBytes: -1})

	rec, got := s.listRuns(t, s.env.ID, "")
	if rec.code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.code, rec.body)
	}
	if len(got.Runs) != 25 || got.Total != 33 {
		t.Fatalf("listed %d runs of %d, want the default 25 of the 33 on record", len(got.Runs), got.Total)
	}
	var ids []string
	for _, r := range got.Runs[:5] {
		ids = append(ids, r.ID)
	}
	if want := []string{"tie-c", "tie-b", "tie-a", "run-29", "run-28"}; !slices.Equal(ids, want) {
		t.Fatalf("first runs = %v, want %v: newest start first, the same start by ID descending", ids, want)
	}
	if strings.Contains(string(rec.body), `"items"`) {
		t.Errorf("the list carries items: %s", rec.body)
	}

	running, done := got.Runs[0], got.Runs[3]
	if running.State != "running" || running.FinishedAt != nil || running.DurationSeconds != nil || running.EstBytes != nil ||
		running.Trigger != "manual" || running.ActorName != "admin" {
		t.Errorf("running run = %+v, want no finish time, duration or size, started by admin", running)
	}
	if done.FinishedAt == nil || done.DurationSeconds == nil || *done.DurationSeconds != 90 || done.EstBytes == nil || *done.EstBytes != 2048 ||
		done.Trigger != "schedule" || done.ActorName != "TierPlan" || done.Counts.RemovedContainers != 29 {
		t.Errorf("finished run = %+v, want 90 s, 2048 bytes, TierPlan's, 29 removed", done)
	}

	// limit: 100 lists them all, and 25 to 100 is the range.
	if _, all := s.listRuns(t, s.env.ID, "?limit=100"); len(all.Runs) != 33 {
		t.Errorf("limit=100 listed %d runs, want all 33", len(all.Runs))
	}
	for _, q := range []string{"?limit=24", "?limit=101", "?limit=0", "?limit=-5", "?limit=lots"} {
		if rec, _ := s.listRuns(t, s.env.ID, q); rec.code != http.StatusUnprocessableEntity && rec.code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want a refusal", q, rec.code, rec.body)
		}
	}
	if _, theirs := s.listRuns(t, other.ID, ""); len(theirs.Runs) != 1 || theirs.Runs[0].ID != "elsewhere" || theirs.Total != 1 {
		t.Errorf("the other environment lists %+v, want its own run only", theirs)
	}
}

// A run that was just made by Clean now is in the history while it works and after it ends.
func TestJanitorRunsListShowsCleanNow(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	s.awaitRun(t, started.JobID)

	rec, got := s.listRuns(t, s.env.ID, "")
	if rec.code != http.StatusOK || len(got.Runs) != 1 || got.Total != 1 {
		t.Fatalf("list: %d %s, want the one run", rec.code, rec.body)
	}
	r := got.Runs[0]
	if r.ID != started.RunID || r.JobID != started.JobID || r.State != "completed" || r.Trigger != "manual" || r.Actor != "id-admin" ||
		r.ActorName != "admin" || r.FinishedAt == nil || r.DurationSeconds == nil || r.Counts.RemovedContainers != 1 {
		t.Fatalf("run = %+v, want the completed manual run with one removal", r)
	}

	// The list is the permission of the run: a Viewer reads it, a person with no role does not.
	s.addUser(t, "vera", false)
	s.addEnvRole(t, "vera", s.env.ID, "viewer")
	cookie, csrf := s.signIn(t, "vera", false)
	path := "/api/env/" + s.env.ID + "/janitor/runs"
	rec2 := s.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf})
	var viewed janitorRunsList
	if rec2.Code != http.StatusOK || json.Unmarshal(rec2.Body.Bytes(), &viewed) != nil || len(viewed.Runs) != 1 {
		t.Fatalf("a Viewer lists the runs: %d %s, want 200 with the run", rec2.Code, rec2.Body)
	}
	if rec := s.call(t, authReq{method: http.MethodGet, path: path}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
	s.addUser(t, "nell", false)
	cookie, csrf = s.signIn(t, "nell", false)
	if rec := s.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf}); rec.Code == http.StatusOK {
		t.Errorf("a person with no role listed the runs: %d %s", rec.Code, rec.Body)
	}
}
