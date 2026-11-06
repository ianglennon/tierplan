// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/schedule"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The policy of the update checks (plan 12-09, D-01): a default every environment follows, and an
// environment's own overrides, where NULL follows.

func ptr[T any](v T) *T { return &v }

func TestPolicyDefaultsAreEverySixHours(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	p := &updates.PolicyStore{Q: r.st.Q, W: r.st.Write}
	def, version, err := p.Defaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (updates.Policy{Enabled: true, Schedule: "0 */6 * * *", Preset: schedule.PresetEvery6h}); def != want || version != 1 {
		t.Errorf("defaults = %+v at version %d, want %+v at 1", def, version, want)
	}
	if got, err := p.Effective(ctx, "env-a"); err != nil || got != def {
		t.Errorf("an environment with no row follows %+v, got %+v, %v", def, got, err)
	}
	if n, err := p.Followers(ctx); err != nil || n != 1 {
		t.Errorf("followers = %d, %v, want 1", n, err)
	}
}

// TestPolicyInheritance: an environment follows the default until it sets a field itself, takes the
// default's changes while it follows and keeps its own when it does not.
func TestPolicyInheritance(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	r.addEnvironment(t, "env-b")
	p := &updates.PolicyStore{Q: r.st.Q, W: r.st.Write}
	now := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

	// env-a sets its own schedule: 03:00 daily, which brings its preset.
	if changed, err := p.SetEnv(ctx, r.st.Q, "env-a", updates.Overrides{Schedule: ptr("0 3 * * *"), Preset: ptr(schedule.PresetDaily)}, 0, "ann", now); err != nil || !changed {
		t.Fatalf("SetEnv = %v, %v, want a change", changed, err)
	}
	a, err := p.Effective(ctx, "env-a")
	if err != nil || a != (updates.Policy{Enabled: true, Schedule: "0 3 * * *", Preset: schedule.PresetDaily}) {
		t.Fatalf("env-a = %+v, %v, want its own schedule and the default's enabled", a, err)
	}
	if b, err := p.Effective(ctx, "env-b"); err != nil || b != updates.DefaultPolicy() {
		t.Fatalf("env-b = %+v, %v, want the default", b, err)
	}

	// The default changes to hourly: env-b follows, env-a keeps its own schedule.
	def, version, err := p.Defaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	def.Schedule, def.Preset = "0 * * * *", schedule.PresetHourly
	if changed, err := p.SetDefaults(ctx, r.st.Q, def, version, "ann", now); err != nil || !changed {
		t.Fatalf("SetDefaults = %v, %v, want a change", changed, err)
	}
	if b, _ := p.Effective(ctx, "env-b"); b.Schedule != "0 * * * *" {
		t.Errorf("env-b = %+v, want the changed default", b)
	}
	if a, _ := p.Effective(ctx, "env-a"); a.Schedule != "0 3 * * *" {
		t.Errorf("env-a = %+v, want its own schedule", a)
	}

	// Reset to NULL: env-a follows the default again.
	_, v, err := p.Env(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := p.SetEnv(ctx, r.st.Q, "env-a", updates.Overrides{}, v, "ann", now); err != nil || !changed {
		t.Fatalf("reset = %v, %v, want a change", changed, err)
	}
	if a, _ := p.Effective(ctx, "env-a"); a.Schedule != "0 * * * *" || a.Preset != schedule.PresetHourly {
		t.Errorf("env-a after the reset = %+v, want the default again", a)
	}
}

func TestPolicySaveRules(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	p := &updates.PolicyStore{Q: r.st.Q, W: r.st.Write}
	now := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

	for name, o := range map[string]updates.Overrides{
		"under the one hour floor":  {Schedule: ptr("*/30 * * * *")},
		"not a schedule":            {Schedule: ptr("sometimes")},
		"not a preset":              {Schedule: ptr("0 3 * * *"), Preset: ptr("fortnightly")},
		"a preset with no schedule": {Preset: ptr(schedule.PresetDaily)},
	} {
		if _, err := p.SetEnv(ctx, r.st.Q, "env-a", o, 0, "ann", now); !errors.Is(err, updates.ErrInvalidSchedule) && !errors.Is(err, updates.ErrTooFrequent) {
			t.Errorf("%s: SetEnv = %v, want a schedule error", name, err)
		}
	}
	if _, err := p.SetEnv(ctx, r.st.Q, "env-a", updates.Overrides{Schedule: ptr("*/30 * * * *")}, 0, "ann", now); !errors.Is(err, updates.ErrTooFrequent) {
		t.Errorf("a half hourly schedule = %v, want ErrTooFrequent", err)
	}

	off := false
	if changed, err := p.SetEnv(ctx, r.st.Q, "env-a", updates.Overrides{Enabled: &off}, 0, "ann", now); err != nil || !changed {
		t.Fatalf("switch off = %v, %v", changed, err)
	}
	if changed, err := p.SetEnv(ctx, r.st.Q, "env-a", updates.Overrides{Enabled: &off}, 1, "ann", now); err != nil || changed {
		t.Errorf("saving what is stored = %v, %v, want no change", changed, err)
	}
	if _, err := p.SetEnv(ctx, r.st.Q, "env-a", updates.Overrides{}, 0, "bob", now); !errors.Is(err, updates.ErrVersionChanged) {
		t.Errorf("a save from a stale read = %v, want ErrVersionChanged", err)
	}
	if _, err := p.SetDefaults(ctx, r.st.Q, updates.DefaultPolicy(), 7, "bob", now); !errors.Is(err, updates.ErrVersionChanged) {
		t.Errorf("defaults saved from a stale read = %v, want ErrVersionChanged", err)
	}
}
