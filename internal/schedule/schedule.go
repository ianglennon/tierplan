// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package schedule

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Package schedule is the cron schedule shared by the janitor (JAN-01, D-02) and the image update
// checks (IMG-04, D-01). A schedule is a standard five-field cron expression read in the server's
// time zone. Only the parser of robfig/cron is used: no goroutine of the library runs, the caller
// waits on its own clock. The shortest gap between two firings is a parameter, because a janitor
// (every 15 minutes at most) and a registry check (every hour at most) load the engines differently.

// MaxJitter is the most a run is moved from its slot to spread the environments out.
const MaxJitter = 10 * time.Minute

// floorFirings is how many firings of a schedule are compared against the minimum interval.
const floorFirings = 10

// The schedule presets of the UI. A preset only names the cron expression PresetCron gives it.
const (
	// PresetHourly runs at the start of every hour.
	PresetHourly = "hourly"
	// PresetEvery6h runs at 00:00, 06:00, 12:00 and 18:00.
	PresetEvery6h = "every_6h"
	// PresetDaily runs once a day at a time of day.
	PresetDaily = "daily"
	// PresetWeekly runs once a week on a day at a time of day.
	PresetWeekly = "weekly"
	// PresetCustom is a cron expression written by hand.
	PresetCustom = "custom"
)

// ErrTooFrequent means two of the next firings of a schedule are less than the minimum interval apart.
var ErrTooFrequent = errors.New("schedule: a schedule must not fire more often than its minimum interval")

// ErrInvalidSchedule means the text is not a schedule that is accepted.
var ErrInvalidSchedule = errors.New("schedule: not a valid schedule")

// floorRef is the instant the frequency floor is judged from. It is fixed so that a schedule is
// accepted or refused the same way on every day.
var floorRef = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// ParseSchedule reads a five-field cron expression. It refuses what robfig/cron cannot read
// (ErrInvalidSchedule), a time zone prefix (the schedule is in the server's zone), @every, which has
// no fixed slot, and a schedule that never fires. A schedule that fires twice within minInterval in
// its next ten firings is ErrTooFrequent.
func ParseSchedule(spec string, minInterval time.Duration) (cron.Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "@every") {
		return nil, ErrInvalidSchedule
	}
	s, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSchedule, err)
	}
	t := s.Next(floorRef)
	if t.IsZero() {
		return nil, fmt.Errorf("%w: it never fires", ErrInvalidSchedule)
	}
	for range floorFirings - 1 {
		next := s.Next(t)
		if next.IsZero() {
			break
		}
		if next.Sub(t) < minInterval {
			return nil, ErrTooFrequent
		}
		t = next
	}
	return s, nil
}

// Jitter is how long after a firing (base) the run of an environment starts: a number of whole
// seconds that depends on the environment's ID and nothing else, below MaxJitter and below a sixth of
// the gap to the following firing. The same environment always has the same offset, so its next run
// never moves between reloads, and environments do not all start at the same second.
func Jitter(envID string, s cron.Schedule, base time.Time) time.Duration {
	next := s.Next(base)
	if next.IsZero() {
		return 0
	}
	limit := min(MaxJitter, next.Sub(base)/6)
	secs := int64(limit / time.Second)
	if secs == 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(envID))
	return time.Duration(int64(h.Sum32())%secs) * time.Second
}

// NextRun is the next time after now the environment's job runs by spec, read in loc: the next
// firing plus the environment's Jitter. A run whose slot began a moment ago but whose offset has not
// passed yet is still the next one. It is the very time the Scheduler fires at.
func NextRun(spec, envID string, now time.Time, loc *time.Location, minInterval time.Duration) (time.Time, error) {
	s, err := ParseSchedule(spec, minInterval)
	if err != nil {
		return time.Time{}, err
	}
	return nextRun(s, envID, now, loc), nil
}

// nextRun is NextRun for a schedule that is parsed.
func nextRun(s cron.Schedule, envID string, now time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	// A firing up to MaxJitter ago may still have a run to come, so the search starts that far back.
	probe := now.Add(-MaxJitter)
	for range 4 {
		base := s.Next(probe)
		if base.IsZero() {
			return time.Time{}
		}
		at := base.Add(Jitter(envID, s, base))
		if at.After(now) {
			return at
		}
		probe = base
	}
	return time.Time{}
}

// NextRuns are the next n runs, in order. It stops short when the schedule has no more.
func NextRuns(spec, envID string, now time.Time, loc *time.Location, n int, minInterval time.Duration) ([]time.Time, error) {
	s, err := ParseSchedule(spec, minInterval)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, n)
	at := now
	for range n {
		at = nextRun(s, envID, at, loc)
		if at.IsZero() {
			break
		}
		out = append(out, at)
	}
	return out, nil
}

// PresetCron is the cron expression of a preset. hhmm is the time of day ("03:00") for daily and
// weekly, and weekday the day for weekly, 0 for Sunday to 6 for Saturday. PresetCustom has no
// expression of its own.
func PresetCron(preset, hhmm string, weekday int) (string, error) {
	switch preset {
	case PresetHourly:
		return "0 * * * *", nil
	case PresetEvery6h:
		return "0 */6 * * *", nil
	case PresetDaily, PresetWeekly:
		t, err := time.Parse("15:04", hhmm)
		if err != nil {
			return "", fmt.Errorf("%w: the time of day %q is not HH:MM", ErrInvalidSchedule, hhmm)
		}
		day := "*"
		if preset == PresetWeekly {
			if weekday < 0 || weekday > 6 {
				return "", fmt.Errorf("%w: the day of the week %d is not 0 to 6", ErrInvalidSchedule, weekday)
			}
			day = strconv.Itoa(weekday)
		}
		return strconv.Itoa(t.Minute()) + " " + strconv.Itoa(t.Hour()) + " * * " + day, nil
	}
	return "", fmt.Errorf("%w: %q is not a preset with an expression", ErrInvalidSchedule, preset)
}

// ZoneName is the name of the time zone a schedule is read in, as a person would say it: the zone's
// name, or for the server's local zone the TZ the process was given, else the zone's abbreviation.
func ZoneName(loc *time.Location, now time.Time) string {
	if loc == nil {
		loc = time.Local
	}
	name := loc.String()
	if name != "Local" {
		return name
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" && tz != "Local" {
		return tz
	}
	abbr, _ := now.In(loc).Zone()
	return abbr
}
