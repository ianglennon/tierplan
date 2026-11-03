// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/netspace"
)

// The defaults of a policy (D-01, D-03) and the limits of one plan.
const (
	// DefaultKeepN is how many of the newest exited task containers of a service slot are kept.
	DefaultKeepN = 2
	// DefaultGrace is how long an object must have been stopped or idle before it can be removed.
	// It is the grace period of the orphan network check, so the two judge alike.
	DefaultGrace = netspace.GracePeriod
	// DefaultSchedule is the cron expression of the daily run at 03:00.
	DefaultSchedule = "0 3 * * *"
	// DefaultSchedulePreset names DefaultSchedule in the UI.
	DefaultSchedulePreset = "daily"
	// MaxInspectsPerRun caps how many containers one plan inspects for their end time, exit code
	// and size. A candidate beyond the cap is cant_tell, never removable (RESEARCH F6, Q7).
	MaxInspectsPerRun = 1000
	// ClockSkewTolerance is how far in the future a container's finish time may lie before the
	// planner calls the host's clock wrong and leaves the container alone.
	ClockSkewTolerance = 5 * time.Minute
)

// Policy is what an environment's janitor does. The zero value removes nothing useful: use
// DefaultPolicy.
type Policy struct {
	// Enabled says whether the schedule runs the janitor.
	Enabled bool
	// Schedule is a cron expression; SchedulePreset names it for the UI ("daily", "weekly",
	// "custom").
	Schedule, SchedulePreset string
	// KeepN is how many of the newest exited task containers of each service slot (of each node,
	// for a global service) are kept. Zero keeps none; a negative number counts as zero.
	KeepN int
	// Grace is how long an object must have been stopped or idle before it is removable.
	Grace time.Duration
	// RemoveUnusedNetworks and RemoveStoppedContainers are the two opt-ins for what no stack or
	// project ever owned. Both are off by default (D-01).
	RemoveUnusedNetworks, RemoveStoppedContainers bool
}

// DefaultPolicy is the policy of an environment nobody has configured: enabled, daily at 03:00,
// the two newest exited task containers of each slot kept, a day of grace, both opt-ins off.
func DefaultPolicy() Policy {
	return Policy{
		Enabled: true, Schedule: DefaultSchedule, SchedulePreset: DefaultSchedulePreset,
		KeepN: DefaultKeepN, Grace: DefaultGrace,
	}
}

// keepN is the number of newest containers of a group to keep, never negative.
func (p Policy) keepN() int { return max(p.KeepN, 0) }

// grace is the grace period, DefaultGrace when the policy leaves it zero.
func (p Policy) grace() time.Duration {
	if p.Grace <= 0 {
		return DefaultGrace
	}
	return p.Grace
}

// The limits the settings accept (UI-SPEC), the same ones the database CHECKs.
const (
	// MinKeepN and MaxKeepN bound how many exited containers of a service slot are kept.
	MinKeepN, MaxKeepN = 0, 20
	// MinGraceHours and MaxGraceHours bound the grace period, in hours.
	MinGraceHours, MaxGraceHours = 1, 720
)

// ErrOutOfRange means a number is outside the range the policy accepts.
var ErrOutOfRange = errors.New("janitor: a policy value is out of range")

// ErrScheduleTooFrequent means a schedule fires more often than every 15 minutes. It is the error
// ParseSchedule answers (ErrTooFrequent), under the name the policy saves use.
var ErrScheduleTooFrequent = ErrTooFrequent

// Overrides are the fields an environment sets itself. A nil field follows the instance default: it
// is never a copy of it, so a changed default reaches every environment that follows it (D-04).
type Overrides struct {
	Enabled                  *bool
	Schedule, SchedulePreset *string
	KeepN, GraceHours        *int
	RemoveUnusedNetworks     *bool
	RemoveStoppedContainers  *bool
}

// NodeOverrides are the fields a swarm node sets itself; a nil field follows its environment.
type NodeOverrides struct {
	Enabled *bool
	KeepN   *int
}

// Effective is the policy of an environment: each field of its overrides, or the default where the
// override is nil. A schedule override brings its preset with it ("custom" when it names none); a
// preset without a schedule is ignored, so the two always describe one schedule.
func Effective(defaults Policy, env Overrides) Policy {
	p := defaults
	if env.Enabled != nil {
		p.Enabled = *env.Enabled
	}
	if env.Schedule != nil {
		p.Schedule = *env.Schedule
		p.SchedulePreset = PresetCustom
		if env.SchedulePreset != nil {
			p.SchedulePreset = *env.SchedulePreset
		}
	}
	if env.KeepN != nil {
		p.KeepN = *env.KeepN
	}
	if env.GraceHours != nil {
		p.Grace = time.Duration(*env.GraceHours) * time.Hour
	}
	if env.RemoveUnusedNetworks != nil {
		p.RemoveUnusedNetworks = *env.RemoveUnusedNetworks
	}
	if env.RemoveStoppedContainers != nil {
		p.RemoveStoppedContainers = *env.RemoveStoppedContainers
	}
	return p
}

// EffectiveForNode is the policy of one swarm node: whether it is cleaned and how many containers of
// a slot it keeps from its own overrides, everything else from its environment.
func EffectiveForNode(env Policy, n NodeOverrides) Policy {
	p := env
	if n.Enabled != nil {
		p.Enabled = *n.Enabled
	}
	if n.KeepN != nil {
		p.KeepN = *n.KeepN
	}
	return p
}

// Validate checks the values an environment would save: keep-N in MinKeepN..MaxKeepN and grace in
// MinGraceHours..MaxGraceHours (ErrOutOfRange), the schedule through ParseSchedule, which includes
// the 15-minute floor (ErrInvalidSchedule, ErrScheduleTooFrequent), and a preset that names a
// schedule. A nil field is not checked: it follows the default.
func Validate(o Overrides) error {
	if o.KeepN != nil && (*o.KeepN < MinKeepN || *o.KeepN > MaxKeepN) {
		return fmt.Errorf("%w: keep-N is %d, it must be %d to %d", ErrOutOfRange, *o.KeepN, MinKeepN, MaxKeepN)
	}
	if o.GraceHours != nil && (*o.GraceHours < MinGraceHours || *o.GraceHours > MaxGraceHours) {
		return fmt.Errorf("%w: grace is %d hours, it must be %d to %d", ErrOutOfRange, *o.GraceHours, MinGraceHours, MaxGraceHours)
	}
	if o.Schedule != nil {
		if _, err := ParseSchedule(*o.Schedule); err != nil {
			return err
		}
	}
	if o.SchedulePreset != nil {
		switch *o.SchedulePreset {
		case PresetHourly, PresetEvery6h, PresetDaily, PresetWeekly, PresetCustom:
		default:
			return fmt.Errorf("%w: %q is not a schedule preset", ErrInvalidSchedule, *o.SchedulePreset)
		}
		if o.Schedule == nil {
			return fmt.Errorf("%w: a preset needs the schedule it names", ErrInvalidSchedule)
		}
	}
	return nil
}
