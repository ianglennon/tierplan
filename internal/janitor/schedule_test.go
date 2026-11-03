// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"errors"
	"fmt"
	"testing"
	"time"
	_ "time/tzdata" // the zone database, so these tests do not depend on the machine having one
)

// The schedule math (plan 11-15, JAN-01, D-02): presets, the 15-minute floor, the fixed per-environment
// offset and the next runs across a change of clock in the server's time zone.

func TestPresetCron(t *testing.T) {
	t.Parallel()
	cases := []struct {
		preset, hhmm string
		weekday      int
		want         string
	}{
		{PresetHourly, "", 0, "0 * * * *"},
		{PresetEvery6h, "", 0, "0 */6 * * *"},
		{PresetDaily, "03:00", 0, "0 3 * * *"},
		{PresetDaily, "23:45", 0, "45 23 * * *"},
		{PresetDaily, "00:05", 0, "5 0 * * *"},
		{PresetWeekly, "03:00", 1, "0 3 * * 1"},
		{PresetWeekly, "18:30", 0, "30 18 * * 0"},
		{PresetWeekly, "18:30", 6, "30 18 * * 6"},
	}
	for _, c := range cases {
		got, err := PresetCron(c.preset, c.hhmm, c.weekday)
		if err != nil || got != c.want {
			t.Errorf("PresetCron(%q, %q, %d) = %q, %v, want %q", c.preset, c.hhmm, c.weekday, got, err, c.want)
		}
		// Every preset is itself a schedule the janitor accepts.
		if _, err := ParseSchedule(got); err != nil {
			t.Errorf("the cron of %q, %q, is not accepted: %v", c.preset, got, err)
		}
	}
	for _, bad := range []struct {
		preset, hhmm string
		weekday      int
	}{
		{PresetCustom, "", 0}, {"monthly", "03:00", 0}, {PresetDaily, "25:00", 0}, {PresetDaily, "3am", 0},
		{PresetDaily, "", 0}, {PresetWeekly, "03:00", 7}, {PresetWeekly, "03:00", -1},
	} {
		if got, err := PresetCron(bad.preset, bad.hhmm, bad.weekday); !errors.Is(err, ErrInvalidSchedule) || got != "" {
			t.Errorf("PresetCron(%q, %q, %d) = %q, %v, want ErrInvalidSchedule", bad.preset, bad.hhmm, bad.weekday, got, err)
		}
	}
}

func TestParseScheduleFloor(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{
		"0 3 * * *", "0 * * * *", "0 */6 * * *", "*/15 * * * *", "0,30 * * * *", "0 3 * * 1", "30 2 1 * *", "@daily", "@hourly", "  0 3 * * *  ",
	} {
		if _, err := ParseSchedule(spec); err != nil {
			t.Errorf("ParseSchedule(%q) = %v, want it accepted", spec, err)
		}
	}
	for _, spec := range []string{"* * * * *", "*/10 * * * *", "*/14 * * * *", "0,10 3 * * *", "5,15 * * * *", "0-30 3 * * *"} {
		if _, err := ParseSchedule(spec); !errors.Is(err, ErrTooFrequent) {
			t.Errorf("ParseSchedule(%q) = %v, want ErrTooFrequent", spec, err)
		}
	}
	for _, spec := range []string{
		"bad", "", "   ", "@every 1h", "@every 30m", "0 3 * *", "0 3 * * * *", "60 3 * * *", "0 25 * * *", "0 3 31 2 *",
		"CRON_TZ=Asia/Tokyo 0 3 * * *", "TZ=UTC 0 3 * * *",
	} {
		if _, err := ParseSchedule(spec); !errors.Is(err, ErrInvalidSchedule) {
			t.Errorf("ParseSchedule(%q) = %v, want ErrInvalidSchedule", spec, err)
		}
	}
	if errors.Is(ErrTooFrequent, ErrInvalidSchedule) {
		t.Error("too frequent must be a problem of its own, not an invalid schedule")
	}
}

func TestJitterDeterministicAndBounded(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		spec string
		gap  time.Duration
	}{
		{"0 3 * * *", 24 * time.Hour}, {"0 * * * *", time.Hour}, {"0 */6 * * *", 6 * time.Hour},
		{"*/15 * * * *", 15 * time.Minute}, {"0 3 * * 1", 7 * 24 * time.Hour},
	} {
		s, err := ParseSchedule(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		limit := min(MaxJitter, c.gap/6)
		base := s.Next(time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC))
		seen := map[time.Duration]bool{}
		for i := range 200 {
			id := fmt.Sprintf("env-%d", i)
			j := Jitter(id, s, base)
			if j < 0 || j >= limit || j >= MaxJitter || j >= c.gap/6 {
				t.Fatalf("%s: Jitter(%s) = %v, want 0 <= j < %v", c.spec, id, j, limit)
			}
			if j%time.Second != 0 {
				t.Fatalf("%s: Jitter(%s) = %v, want whole seconds", c.spec, id, j)
			}
			if again := Jitter(id, s, base); again != j {
				t.Fatalf("%s: Jitter(%s) = %v then %v, want the same", c.spec, id, j, again)
			}
			seen[j] = true
		}
		// 200 environments land on many different seconds, so they do not all start together.
		if len(seen) < 50 {
			t.Errorf("%s: 200 environments gave only %d different offsets", c.spec, len(seen))
		}
	}
}

func TestNextRunDeterministicAndStable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	a, err := NextRun("0 3 * * *", "env-1", now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever time it is read at, the same environment gets the same offset into its slot.
	b, _ := NextRun("0 3 * * *", "env-1", now.Add(5*time.Hour), time.UTC)
	if !a.Equal(b) || a.Before(time.Date(2026, 5, 6, 3, 0, 0, 0, time.UTC)) || !a.Before(time.Date(2026, 5, 6, 3, 10, 0, 0, time.UTC)) {
		t.Errorf("NextRun = %v and %v, want the same time between 03:00 and 03:10 tomorrow", a, b)
	}
	// A slot whose offset has not passed yet is still the next run: at 03:00 sharp the run is still to come.
	if c, _ := NextRun("0 3 * * *", "env-1", time.Date(2026, 5, 6, 3, 0, 0, 0, time.UTC), time.UTC); !c.Equal(a) {
		t.Errorf("NextRun at 03:00 = %v, want %v: the offset is still to pass", c, a)
	}
	// At the moment of the run it has run: the next one is a day later.
	if d, _ := NextRun("0 3 * * *", "env-1", a, time.UTC); !d.Equal(a.AddDate(0, 0, 1)) {
		t.Errorf("NextRun at the run = %v, want a day after %v", d, a)
	}
	if _, err := NextRun("* * * * *", "env-1", now, time.UTC); !errors.Is(err, ErrTooFrequent) {
		t.Errorf("NextRun of a too frequent schedule = %v, want ErrTooFrequent", err)
	}
}

// Held out: the next runs follow the server's time zone across a change of clock and skip or repeat an
// hour exactly as the scheduler fires. Europe/London springs forward on 29 March 2026 (01:00 does not
// exist) and falls back on 25 October 2026 (01:00 happens twice).
func TestNextRunDST(t *testing.T) {
	t.Parallel()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSchedule("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// The offset of "env-1" into a daily slot, which is the same on every day: a gap of 23 or 25 hours
	// is far more than the 60 minutes that make the limit 10 minutes.
	off := Jitter("env-1", s, time.Date(2026, 3, 27, 1, 30, 0, 0, london))
	utc := func(mo time.Month, d, h, m int) time.Time {
		return time.Date(2026, mo, d, h, m, 0, 0, time.UTC).Add(off)
	}

	// Spring forward: 01:30 on the 29th is not a time that day, and the day is skipped.
	got, err := NextRuns("30 1 * * *", "env-1", time.Date(2026, 3, 27, 12, 0, 0, 0, london), london, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{utc(time.March, 28, 1, 30), utc(time.March, 30, 0, 30), utc(time.March, 31, 0, 30)}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("spring forward: run %d = %v, want %v", i, got[i], want[i])
		}
	}
	if got[1].In(london).Hour() != 1 || got[1].In(london).Day() != 30 {
		t.Errorf("spring forward: the run after the 28th is %v, want 01:30 on the 30th by London's clock", got[1].In(london))
	}

	// Fall back: 01:30 happens twice on the 25th, once in BST and once in GMT, and both fire.
	got, err = NextRuns("30 1 * * *", "env-1", time.Date(2026, 10, 24, 12, 0, 0, 0, london), london, 3)
	if err != nil {
		t.Fatal(err)
	}
	want = []time.Time{utc(time.October, 25, 0, 30), utc(time.October, 25, 1, 30), utc(time.October, 26, 1, 30)}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("fall back: run %d = %v, want %v", i, got[i], want[i])
		}
	}
	if got[1].Sub(got[0]) != time.Hour {
		t.Errorf("fall back: the repeated hour's two runs are %v apart, want an hour", got[1].Sub(got[0]))
	}

	// A slot after the change that exists on both sides stays at the same wall-clock time.
	got, err = NextRuns("0 3 * * *", "env-1", time.Date(2026, 3, 27, 12, 0, 0, 0, london), london, 4)
	if err != nil {
		t.Fatal(err)
	}
	for i, at := range got {
		in := at.In(london)
		if in.Hour() != 3 || in.Minute() > 10 {
			t.Errorf("daily 03:00: run %d = %v, want 03:00 to 03:10 by London's clock on every day", i, in)
		}
	}
	// The time is read in the zone it is given: the same schedule in UTC gives 03:00 UTC.
	if utcRuns, _ := NextRuns("0 3 * * *", "env-1", time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC), time.UTC, 1); utcRuns[0].UTC().Hour() != 3 {
		t.Errorf("daily 03:00 in UTC = %v", utcRuns[0])
	}
}
