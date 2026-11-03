// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"errors"
	"testing"
	"time"
)

// Inheritance and validation of the policy (plan 11-17, D-04): a field follows the level above until
// a level sets it, and a value that is saved is a value the janitor can run with.

func ptr[T any](v T) *T { return &v }

func TestEffectiveInheritance(t *testing.T) {
	t.Parallel()
	def := DefaultPolicy()

	if got := Effective(def, Overrides{}); got != def {
		t.Errorf("no overrides = %+v, want every default %+v", got, def)
	}

	got := Effective(def, Overrides{KeepN: ptr(5)})
	want := def
	want.KeepN = 5
	if got != want {
		t.Errorf("keep 5 = %+v, want only keep-N changed: %+v", got, want)
	}

	all := Effective(def, Overrides{
		Enabled: ptr(false), Schedule: ptr("0 4 * * 1"), SchedulePreset: ptr(PresetWeekly), KeepN: ptr(0), GraceHours: ptr(48),
		RemoveUnusedNetworks: ptr(true), RemoveStoppedContainers: ptr(true),
	})
	if all.Enabled || all.Schedule != "0 4 * * 1" || all.SchedulePreset != PresetWeekly || all.KeepN != 0 || all.Grace != 48*time.Hour || !all.RemoveUnusedNetworks || !all.RemoveStoppedContainers {
		t.Errorf("every field overridden = %+v", all)
	}

	// A schedule with no preset is a custom one; a preset with no schedule is ignored.
	if got := Effective(def, Overrides{Schedule: ptr("5 * * * *")}); got.SchedulePreset != PresetCustom || got.Schedule != "5 * * * *" {
		t.Errorf("schedule alone = %q %q, want the schedule as custom", got.Schedule, got.SchedulePreset)
	}
	if got := Effective(def, Overrides{SchedulePreset: ptr(PresetHourly)}); got.Schedule != def.Schedule || got.SchedulePreset != def.SchedulePreset {
		t.Errorf("preset alone = %q %q, want the default schedule", got.Schedule, got.SchedulePreset)
	}
}

func TestEffectiveForNode(t *testing.T) {
	t.Parallel()
	env := DefaultPolicy()
	env.KeepN = 3
	env.Grace = 36 * time.Hour

	if got := EffectiveForNode(env, NodeOverrides{}); got != env {
		t.Errorf("no node overrides = %+v, want the environment's %+v", got, env)
	}
	got := EffectiveForNode(env, NodeOverrides{Enabled: ptr(false), KeepN: ptr(4)})
	if got.Enabled || got.KeepN != 4 || got.Grace != env.Grace || got.Schedule != env.Schedule {
		t.Errorf("node overrides = %+v, want enabled false and keep 4 over the environment", got)
	}
	if got := EffectiveForNode(env, NodeOverrides{KeepN: ptr(0)}); got.KeepN != 0 || !got.Enabled {
		t.Errorf("keep 0 only = %+v, want keep 0 and the environment's enabled", got)
	}
}

func TestValidatePolicy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		o    Overrides
		want error
	}{
		"nothing set":              {Overrides{}, nil},
		"keep 0":                   {Overrides{KeepN: ptr(0)}, nil},
		"keep 20":                  {Overrides{KeepN: ptr(20)}, nil},
		"keep 21":                  {Overrides{KeepN: ptr(21)}, ErrOutOfRange},
		"keep -1":                  {Overrides{KeepN: ptr(-1)}, ErrOutOfRange},
		"grace 1":                  {Overrides{GraceHours: ptr(1)}, nil},
		"grace 720":                {Overrides{GraceHours: ptr(720)}, nil},
		"grace 0":                  {Overrides{GraceHours: ptr(0)}, ErrOutOfRange},
		"grace 721":                {Overrides{GraceHours: ptr(721)}, ErrOutOfRange},
		"daily":                    {Overrides{Schedule: ptr("0 3 * * *"), SchedulePreset: ptr(PresetDaily)}, nil},
		"every 15 minutes":         {Overrides{Schedule: ptr("*/15 * * * *")}, nil},
		"every minute":             {Overrides{Schedule: ptr("* * * * *")}, ErrScheduleTooFrequent},
		"every 5 minutes":          {Overrides{Schedule: ptr("*/5 * * * *")}, ErrScheduleTooFrequent},
		"nonsense":                 {Overrides{Schedule: ptr("nonsense")}, ErrInvalidSchedule},
		"empty schedule":           {Overrides{Schedule: ptr("")}, ErrInvalidSchedule},
		"a preset that is no name": {Overrides{Schedule: ptr("0 3 * * *"), SchedulePreset: ptr("monthly")}, ErrInvalidSchedule},
		"a preset with no cron":    {Overrides{SchedulePreset: ptr(PresetDaily)}, ErrInvalidSchedule},
		"the opt-ins":              {Overrides{RemoveUnusedNetworks: ptr(true), RemoveStoppedContainers: ptr(false)}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.o)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
	// The error the schedule check answers is the one the saves name.
	if !errors.Is(ErrScheduleTooFrequent, ErrTooFrequent) {
		t.Error("ErrScheduleTooFrequent is not ErrTooFrequent")
	}
}
