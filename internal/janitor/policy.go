// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
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
