// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/janitor"
)

// The state of an environment's janitor (plan 11-15, JAN-01, D-02, D-13): its schedule, when it runs
// next and what it did last.

// janitorStatus is the answer of GET /api/env/{envId}/janitor.
type janitorStatus struct {
	Enabled        bool    `json:"enabled"`
	Schedule       string  `json:"schedule"`
	SchedulePreset string  `json:"schedulePreset"`
	Timezone       string  `json:"timezone"`
	NextRunAt      *string `json:"nextRunAt"`
	LastRun        *struct {
		ID      string `json:"id"`
		Trigger string `json:"trigger"`
		State   string `json:"state"`
		Reason  string `json:"reason"`
	} `json:"lastRun"`
	Active *struct {
		RunID string `json:"runId"`
		JobID string `json:"jobId"`
		State string `json:"state"`
	} `json:"active"`
	Retention struct {
		Days int `json:"days"`
		Runs int `json:"runs"`
	} `json:"retention"`
}

func (s *swarmFix) janitorStatus(t *testing.T, envID string) (*envRec, janitorStatus) {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/api/env/"+envID+"/janitor", "")
	var got janitorStatus
	if rec.code == http.StatusOK {
		got = decode[janitorStatus](t, rec)
	}
	return rec, got
}

func TestJanitorStatusNextRun(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(3)), nil)
	now := s.clk.now()

	// Nothing has run: the default policy is on, daily at 03:00 in the server's zone, and the next run
	// is the slot plus this environment's fixed offset.
	rec, got := s.janitorStatus(t, s.env.ID)
	if rec.code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.code, rec.body)
	}
	want, err := janitor.NextRun(janitor.DefaultSchedule, s.env.ID, now, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Schedule != "0 3 * * *" || got.SchedulePreset != "daily" || got.Timezone != janitor.ZoneName(time.Local, now) {
		t.Errorf("status = %+v, want enabled, 0 3 * * *, daily, in the server's zone", got)
	}
	if got.NextRunAt == nil {
		t.Fatal("nextRunAt is null, want the next scheduled run")
	}
	at, err := time.Parse(time.RFC3339, *got.NextRunAt)
	if err != nil || !at.Equal(want) {
		t.Errorf("nextRunAt = %q (%v), want %v", *got.NextRunAt, err, want)
	}
	if got.LastRun != nil || got.Active != nil {
		t.Errorf("lastRun = %+v active = %+v, want neither: no run is on record", got.LastRun, got.Active)
	}
	if got.Retention.Days != 90 || got.Retention.Runs != 100 {
		t.Errorf("retention = %+v, want 90 days or 100 runs", got.Retention)
	}

	// The offset never moves: the same time on a second read.
	if _, again := s.janitorStatus(t, s.env.ID); again.NextRunAt == nil || *again.NextRunAt != *got.NextRunAt {
		t.Errorf("a second read gave %v, want %q again", again.NextRunAt, *got.NextRunAt)
	}

	// The newest run that ended is the last run, and a run that is waiting is the active one.
	store := &janitor.SQLStore{St: s.st}
	done := now.Add(-3 * time.Hour)
	seedRun(t, store, janitor.Run{ID: "run-old", EnvID: s.env.ID, Trigger: janitor.TriggerManual, Actor: "id-admin", State: janitor.StateCompleted, StartedAt: done.Add(-time.Hour), FinishedAt: done.Add(-59 * time.Minute), EstBytes: -1})
	seedRun(t, store, janitor.Run{ID: "run-skipped", EnvID: s.env.ID, Trigger: janitor.TriggerSchedule, Actor: janitor.SystemActor, State: janitor.StateSkipped, Reason: janitor.ReasonDeployInProgress, StartedAt: done, FinishedAt: done.Add(10 * time.Minute), EstBytes: -1})
	seedRun(t, store, janitor.Run{ID: "run-waiting", EnvID: s.env.ID, Trigger: janitor.TriggerSchedule, Actor: janitor.SystemActor, State: janitor.StateWaiting, JobID: "job-1", StartedAt: now.Add(-time.Minute), EstBytes: -1})
	_, got = s.janitorStatus(t, s.env.ID)
	if got.LastRun == nil || got.LastRun.ID != "run-skipped" || got.LastRun.State != "skipped" || got.LastRun.Reason != "deploy_in_progress" || got.LastRun.Trigger != "schedule" {
		t.Errorf("lastRun = %+v, want the newest run that ended: skipped with the reason deploy_in_progress", got.LastRun)
	}
	if got.Active == nil || got.Active.RunID != "run-waiting" || got.Active.JobID != "job-1" || got.Active.State != "waiting" {
		t.Errorf("active = %+v, want the waiting run with its job", got.Active)
	}
}
