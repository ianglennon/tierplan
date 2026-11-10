// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, time.November, 10, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func TestFallbackRenewAt(t *testing.T) {
	tests := []struct {
		name     string
		lifetime time.Duration
		want     time.Duration
		basis    RenewBasis
	}{
		{"90 days renews at two thirds", 90 * day, 60 * day, BasisTwoThirds},
		{"6 days renews at half", 6 * day, 3 * day, BasisHalf},
		{"exactly 10 days is two thirds", 10 * day, 10 * day * 2 / 3, BasisTwoThirds},
		{"one minute under 10 days is half", 10*day - time.Minute, (10*day - time.Minute) / 2, BasisHalf},
		{"9 days 23 h 59 m is half", 9*day + 23*time.Hour + 59*time.Minute, (9*day + 23*time.Hour + 59*time.Minute) / 2, BasisHalf},
		{"a 100 day lifetime is still two thirds, not a fixed day count", 100 * day, 100 * day * 2 / 3, BasisTwoThirds},
		{"a zero lifetime is due at once", 0, 0, BasisHalf},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, basis := FallbackRenewAt(t0, t0.Add(tc.lifetime))
			if want := t0.Add(tc.want); !got.Equal(want) {
				t.Errorf("renew at = %v, want %v", got, want)
			}
			if basis != tc.basis {
				t.Errorf("basis = %q, want %q", basis, tc.basis)
			}
		})
	}
	t.Run("the basis strings are stable", func(t *testing.T) {
		if BasisARI != "ari" || BasisTwoThirds != "two_thirds" || BasisHalf != "half" {
			t.Errorf("basis strings changed: %q %q %q", BasisARI, BasisTwoThirds, BasisHalf)
		}
	})
}

func TestPickInWindow(t *testing.T) {
	start := t0.Add(10 * time.Hour)
	end := t0.Add(20 * time.Hour)
	mid := t0.Add(15 * time.Hour)
	fixed := func(v int64) func(int64) int64 { return func(int64) int64 { return v } }
	var asked int64
	record := func(n int64) int64 { asked = n; return 0 }

	tests := []struct {
		name       string
		start, end time.Time
		kept       time.Time
		rnd        func(int64) int64
		want       time.Time
	}{
		{"a kept instant inside the window is kept", start, end, mid, fixed(1), mid},
		{"a kept instant at the start is kept", start, end, start, fixed(1), start},
		{"a kept instant at the end is kept", start, end, end, fixed(1), end},
		{"a kept instant before the window is re-picked", start, end, start.Add(-time.Second), fixed(int64(time.Hour)), start.Add(time.Hour)},
		{"a kept instant after the window is re-picked", start, end, end.Add(time.Second), fixed(int64(2 * time.Hour)), start.Add(2 * time.Hour)},
		{"no kept instant is picked from rnd", start, end, time.Time{}, fixed(int64(3 * time.Hour)), start.Add(3 * time.Hour)},
		{"rnd returning zero is the start", start, end, time.Time{}, fixed(0), start},
		{"rnd outside its range is brought back inside the window", start, end, time.Time{}, fixed(int64(100 * time.Hour)), end.Add(-1)},
		{"a negative rnd is the start", start, end, time.Time{}, fixed(-5), start},
		{"an empty window is its start", start, start, time.Time{}, fixed(7), start},
		{"an inverted window is its start", end, start, time.Time{}, fixed(7), end},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickInWindow(tc.start, tc.end, tc.kept, tc.rnd); !got.Equal(tc.want) {
				t.Errorf("picked %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("rnd is asked for the window width", func(t *testing.T) {
		PickInWindow(start, end, time.Time{}, record)
		if asked != int64(10*time.Hour) {
			t.Errorf("rnd asked for %d, want %d", asked, int64(10*time.Hour))
		}
	})
}

func TestNextARICheck(t *testing.T) {
	tests := []struct {
		name string
		ra   time.Duration
		want time.Duration
	}{
		{"none given is 6 h", 0, 6 * time.Hour},
		{"a negative value is 6 h", -time.Minute, 6 * time.Hour},
		{"30 m is raised to 1 h", 30 * time.Minute, time.Hour},
		{"exactly 1 h", time.Hour, time.Hour},
		{"6 h", 6 * time.Hour, 6 * time.Hour},
		{"exactly 12 h", 12 * time.Hour, 12 * time.Hour},
		{"48 h is cut to 12 h", 48 * time.Hour, 12 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextARICheck(t0, tc.ra); !got.Equal(t0.Add(tc.want)) {
				t.Errorf("next check = %v, want %v", got, t0.Add(tc.want))
			}
		})
	}
}
