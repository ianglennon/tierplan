// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// janitorStatusRuns is how many of the newest runs the status looks through for the run that is
// working and the last one that ended.
const janitorStatusRuns = 25

// schedulePreviewRuns is how many upcoming runs the schedule preview answers.
const schedulePreviewRuns = 3

// maxSchedulePreviewBody bounds the body of a schedule preview: one expression of at most 100 characters.
const maxSchedulePreviewBody = 4 << 10

// JanitorActiveBody is the run of the environment that has not ended.
type JanitorActiveBody struct {
	RunID string `json:"runId" doc:"The ID of the run"`
	JobID string `json:"jobId" doc:"The job that does the removing; empty while the run has none yet"`
	State string `json:"state" enum:"waiting,running" doc:"waiting: a scheduled run is waiting for a deploy or another operation to free the environment. running: it holds the environment and is working"`
}

// JanitorRetentionBody is how long the run history is kept.
type JanitorRetentionBody struct {
	Days int `json:"days" minimum:"1" doc:"A run is kept while it started within this many days"`
	Runs int `json:"runs" minimum:"1" doc:"A run is also kept while it is among this many newest runs of the environment, whichever of the two keeps more"`
}

// JanitorStatusBody is the state of an environment's janitor: its schedule, when it runs next and
// what it did last.
type JanitorStatusBody struct {
	Enabled        bool                   `json:"enabled" doc:"Whether the schedule runs the janitor on this environment"`
	Schedule       string                 `json:"schedule" doc:"The cron expression, read in the server's time zone"`
	SchedulePreset string                 `json:"schedulePreset" enum:"hourly,every_6h,daily,weekly,custom" doc:"The preset the expression came from; custom for one written by hand"`
	Timezone       string                 `json:"timezone" doc:"The server's time zone, which the schedule is read in"`
	NextRunAt      *string                `json:"nextRunAt" nullable:"true" format:"date-time" doc:"When the next scheduled run starts, in the server's time zone, including this environment's start offset of up to 10 minutes. Null when the janitor is off or the schedule cannot be used"`
	LastRun        *JanitorRunSummaryBody `json:"lastRun,omitempty" doc:"The newest run that has ended, whatever started it. Absent when there is none"`
	Active         *JanitorActiveBody     `json:"active,omitempty" doc:"The run that is waiting or working. Absent when there is none"`
	Retention      JanitorRetentionBody   `json:"retention"`
}

// JanitorStatusOutput wraps JanitorStatusBody for huma.
type JanitorStatusOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorStatusBody
}

// SchedulePreviewRequest is the body of POST /api/env/{envId}/janitor/schedule-preview.
type SchedulePreviewRequest struct {
	Schedule string `json:"schedule" maxLength:"100" doc:"A five-field cron expression, such as 0 3 * * *"`
}

// SchedulePreviewInput is the request of the schedule preview.
type SchedulePreviewInput struct {
	EnvironmentPathInput
	Body SchedulePreviewRequest
}

// SchedulePreviewBody is what a schedule would do.
type SchedulePreviewBody struct {
	Valid    bool     `json:"valid" doc:"Whether the janitor accepts the expression"`
	Problem  string   `json:"problem" enum:",invalid,too_frequent" doc:"Why it is not accepted: invalid when it is not a five-field cron expression the janitor can use, too_frequent when two of its next ten firings are less than 15 minutes apart. Empty when valid"`
	NextRuns []string `json:"nextRuns" doc:"The next three runs in the server's time zone, with this environment's start offset; empty when the expression is not valid"`
	Timezone string   `json:"timezone" doc:"The server's time zone the expression is read in"`
}

// SchedulePreviewOutput wraps SchedulePreviewBody for huma.
type SchedulePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SchedulePreviewBody
}

// location is the time zone schedules are read in: the scheduler's, else the server's local zone.
func (d JanitorDeps) location() *time.Location {
	if d.Scheduler != nil {
		return d.Scheduler.Location()
	}
	return time.Local
}

// retention is how long the run history is kept: the one the instance defaults store (D-16), and
// DefaultRetention for deps with no policy store.
func (d JanitorDeps) retention(ctx context.Context) (janitor.Retention, error) {
	if d.Policies.St == nil {
		return janitor.DefaultRetention, nil
	}
	return d.Policies.Retention(ctx)
}

// registerJanitorStatus registers the status of an environment's janitor and the schedule preview.
func registerJanitorStatus(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getJanitorStatus", Method: http.MethodGet, Path: "/api/env/{envId}/janitor",
		Summary: "Read the state of an environment's janitor",
		Description: "Answers whether the janitor runs by itself on this environment, its schedule and the time zone it is read in, when the next run starts, " +
			"the newest run that ended, the run that is waiting or working now, and how long the run history is kept. " +
			"The schedule runs every environment at its slot, daily at 03:00 server time by default, moved later by a start offset of up to 10 minutes that depends on the environment and never changes. " +
			"A run missed while the server was stopped is not made up. Needs environments.read on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, _ *EnvironmentPathInput) (*JanitorStatusOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Runs == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := janitorStatus(ctx, d, env.ID)
			if err != nil {
				d.log().Error("could not read the state of the janitor", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &JanitorStatusOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "previewJanitorSchedule", Method: http.MethodPost, Path: "/api/env/{envId}/janitor/schedule-preview",
		Summary: "Preview a janitor schedule",
		Description: "Checks a cron expression and answers the next three times it would run in the server's time zone, with this environment's start offset, " +
			"so the page shows what a schedule does before it is saved. An expression is valid when the janitor can read it as five fields and no two of its next ten firings are less than 15 minutes apart. " +
			"An expression that is not valid answers 200 with valid false and the problem. It writes nothing. Needs environments.read on the environment.",
		Tags: []string{"environments"}, MaxBodyBytes: maxSchedulePreviewBody,
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SchedulePreviewInput) (*SchedulePreviewOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			now := d.now()
			loc := d.location()
			body := SchedulePreviewBody{NextRuns: []string{}, Timezone: janitor.ZoneName(loc, now)}
			runs, err := janitor.NextRuns(in.Body.Schedule, env.ID, now, loc, schedulePreviewRuns)
			switch {
			case errors.Is(err, janitor.ErrTooFrequent):
				body.Problem = "too_frequent"
			case err != nil:
				body.Problem = "invalid"
			default:
				body.Valid = true
				for _, at := range runs {
					body.NextRuns = append(body.NextRuns, at.In(loc).Format(time.RFC3339))
				}
			}
			return &SchedulePreviewOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// janitorStatus reads the policy, the scheduler and the run record into the status of an environment.
func janitorStatus(ctx context.Context, d JanitorDeps, envID string) (JanitorStatusBody, error) {
	pol := janitor.DefaultPolicy()
	if d.Runner != nil && d.Runner.Policy != nil {
		p, err := d.Runner.Policy(ctx, envID)
		if err != nil {
			return JanitorStatusBody{}, err
		}
		pol = p
	}
	keep, err := d.retention(ctx)
	if err != nil {
		return JanitorStatusBody{}, err
	}
	now := d.now()
	loc := d.location()
	spec := pol.Schedule
	if spec == "" {
		spec = janitor.DefaultSchedule
	}
	preset := pol.SchedulePreset
	switch preset {
	case janitor.PresetHourly, janitor.PresetEvery6h, janitor.PresetDaily, janitor.PresetWeekly:
	default:
		preset = janitor.PresetCustom
	}
	b := JanitorStatusBody{
		Enabled: pol.Enabled, Schedule: spec, SchedulePreset: preset, Timezone: janitor.ZoneName(loc, now),
		Retention: JanitorRetentionBody{Days: keep.Days, Runs: keep.Runs},
	}
	if pol.Enabled {
		// The scheduler's own slot is the truth when it has one: it is the very time it fires at. Before
		// it has looked at the environment, the same function gives the same time.
		var next time.Time
		var ok bool
		if d.Scheduler != nil {
			next, ok = d.Scheduler.Next(envID)
		}
		if !ok {
			if at, err := janitor.NextRun(spec, envID, now, loc); err == nil && !at.IsZero() {
				next, ok = at, true
			}
		}
		if ok {
			at := next.In(loc).Format(time.RFC3339)
			b.NextRunAt = &at
		}
	}
	runs, err := d.Runs.ListRuns(ctx, envID, janitorStatusRuns)
	if err != nil {
		return JanitorStatusBody{}, err
	}
	names := map[string]string{}
	for _, run := range runs {
		switch {
		case b.Active == nil && (run.State == janitor.StateWaiting || run.State == janitor.StateRunning):
			b.Active = &JanitorActiveBody{RunID: run.ID, JobID: run.JobID, State: run.State}
		case b.LastRun == nil && run.State != janitor.StateWaiting && run.State != janitor.StateRunning:
			sum := janitorRunSummary(ctx, d, names, run)
			b.LastRun = &sum
		}
	}
	return b, nil
}
