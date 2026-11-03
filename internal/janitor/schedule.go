// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

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

// The schedule of an environment's janitor (JAN-01, D-02). A schedule is a standard five-field cron
// expression read in the server's time zone. Only the parser of robfig/cron is used: no goroutine of
// the library runs, the Scheduler waits on its own clock.

// MinInterval is the shortest gap a schedule may have between two firings. A janitor reads every node
// of an environment, so one that ran every minute would be a load on the engines, not a clean-up.
const MinInterval = 15 * time.Minute

// MaxJitter is the most a run is moved from its slot to spread the environments out.
const MaxJitter = 10 * time.Minute

// floorFirings is how many firings of a schedule are compared against MinInterval.
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

// ErrTooFrequent means two of the next firings of a schedule are less than MinInterval apart.
var ErrTooFrequent = errors.New("janitor: a schedule must not fire more often than every 15 minutes")

// ErrInvalidSchedule means the text is not a schedule the janitor accepts.
var ErrInvalidSchedule = errors.New("janitor: not a valid schedule")

// floorRef is the instant the frequency floor is judged from. It is fixed so that a schedule is
// accepted or refused the same way on every day.
var floorRef = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// ParseSchedule reads a five-field cron expression. It refuses what robfig/cron cannot read
// (ErrInvalidSchedule), a time zone prefix (the schedule is in the server's zone), @every, which has
// no fixed slot, and a schedule that never fires. A schedule that fires twice within MinInterval in
// its next ten firings is ErrTooFrequent.
func ParseSchedule(spec string) (cron.Schedule, error) {
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
		if next.Sub(t) < MinInterval {
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

// NextRun is the next time after now the environment's janitor runs by spec, read in loc: the next
// firing plus the environment's Jitter. A run whose slot began a moment ago but whose offset has not
// passed yet is still the next one. It is the very time the Scheduler fires at.
func NextRun(spec, envID string, now time.Time, loc *time.Location) (time.Time, error) {
	s, err := ParseSchedule(spec)
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
func NextRuns(spec, envID string, now time.Time, loc *time.Location, n int) ([]time.Time, error) {
	s, err := ParseSchedule(spec)
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
