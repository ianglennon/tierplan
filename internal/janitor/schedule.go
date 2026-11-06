// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ianglennon/tierplan/internal/schedule"
)

// The schedule of an environment's janitor (JAN-01, D-02). The parsing, jitter and next-run maths live
// in internal/schedule, shared with the image update checks; these are the janitor's names for it, with
// the janitor's 15 minute floor.

// MinInterval is the shortest gap a schedule may have between two firings. A janitor reads every node
// of an environment, so one that ran every minute would be a load on the engines, not a clean-up.
const MinInterval = 15 * time.Minute

// MaxJitter is the most a run is moved from its slot to spread the environments out.
const MaxJitter = schedule.MaxJitter

// The schedule presets of the UI. A preset only names the cron expression PresetCron gives it.
const (
	// PresetHourly runs at the start of every hour.
	PresetHourly = schedule.PresetHourly
	// PresetEvery6h runs at 00:00, 06:00, 12:00 and 18:00.
	PresetEvery6h = schedule.PresetEvery6h
	// PresetDaily runs once a day at a time of day.
	PresetDaily = schedule.PresetDaily
	// PresetWeekly runs once a week on a day at a time of day.
	PresetWeekly = schedule.PresetWeekly
	// PresetCustom is a cron expression written by hand.
	PresetCustom = schedule.PresetCustom
)

// ErrTooFrequent means two of the next firings of a schedule are less than MinInterval apart.
var ErrTooFrequent = schedule.ErrTooFrequent

// ErrInvalidSchedule means the text is not a schedule the janitor accepts.
var ErrInvalidSchedule = schedule.ErrInvalidSchedule

// ParseSchedule reads a five-field cron expression with the janitor's MinInterval floor.
func ParseSchedule(spec string) (cron.Schedule, error) {
	return schedule.ParseSchedule(spec, MinInterval)
}

// Jitter is how long after a firing (base) the run of an environment starts; see schedule.Jitter.
func Jitter(envID string, s cron.Schedule, base time.Time) time.Duration {
	return schedule.Jitter(envID, s, base)
}

// NextRun is the next time after now the environment's janitor runs by spec, read in loc.
func NextRun(spec, envID string, now time.Time, loc *time.Location) (time.Time, error) {
	return schedule.NextRun(spec, envID, now, loc, MinInterval)
}

// NextRuns are the next n runs, in order. It stops short when the schedule has no more.
func NextRuns(spec, envID string, now time.Time, loc *time.Location, n int) ([]time.Time, error) {
	return schedule.NextRuns(spec, envID, now, loc, n, MinInterval)
}

// PresetCron is the cron expression of a preset; see schedule.PresetCron.
func PresetCron(preset, hhmm string, weekday int) (string, error) {
	return schedule.PresetCron(preset, hhmm, weekday)
}

// ZoneName is the name of the time zone a schedule is read in; see schedule.ZoneName.
func ZoneName(loc *time.Location, now time.Time) string {
	return schedule.ZoneName(loc, now)
}
