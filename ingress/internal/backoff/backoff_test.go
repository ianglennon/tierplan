// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backoff

import (
	"testing"
	"time"
)

func fixed(v float64) func() float64 { return func() float64 { return v } }

func TestDelayFirstFailureSitsInTheUpperHalfOfTheMinimum(t *testing.T) {
	if got := Delay(1, time.Second, time.Minute, fixed(0)); got != 500*time.Millisecond {
		t.Fatalf("rnd 0: Delay = %s, want 500ms", got)
	}
	got := Delay(1, time.Second, time.Minute, fixed(0.999))
	if got >= time.Second || got < 990*time.Millisecond {
		t.Fatalf("rnd 0.999: Delay = %s, want just under 1s", got)
	}
}

func TestDelayDoublesThenCapsAtTheMaximum(t *testing.T) {
	tests := []struct {
		failures int
		lo, hi   time.Duration // the expected range for rnd 0 and rnd just under 1
	}{
		{2, time.Second, 2 * time.Second},
		{3, 2 * time.Second, 4 * time.Second},
		{6, 16 * time.Second, 32 * time.Second},
		// 2^6 s is above 60 s, so the step is capped at 60 s: [30 s, 60 s].
		{7, 30 * time.Second, 60 * time.Second},
		{50, 30 * time.Second, 60 * time.Second},
	}
	for _, tc := range tests {
		low := Delay(tc.failures, time.Second, time.Minute, fixed(0))
		high := Delay(tc.failures, time.Second, time.Minute, fixed(0.999999))
		if low != tc.lo {
			t.Errorf("failures %d rnd 0: Delay = %s, want %s", tc.failures, low, tc.lo)
		}
		if high < tc.lo || high > tc.hi {
			t.Errorf("failures %d rnd ~1: Delay = %s, want within [%s, %s]", tc.failures, high, tc.lo, tc.hi)
		}
	}
}

func TestDelayNeverExceedsTheTraefikRestartMaximum(t *testing.T) {
	for failures := 1; failures <= 100; failures++ {
		for _, r := range []float64{0, 0.5, 0.999999} {
			if got := Delay(failures, time.Second, 30*time.Second, fixed(r)); got > 30*time.Second {
				t.Fatalf("failures %d rnd %v: Delay = %s, above the 30s maximum", failures, r, got)
			}
		}
	}
}

func TestDelayGuardsItsBounds(t *testing.T) {
	// A lo of zero or less is 1 s.
	if got := Delay(1, 0, time.Minute, fixed(0)); got != 500*time.Millisecond {
		t.Errorf("lo 0: Delay = %s, want 500ms", got)
	}
	if got := Delay(1, -time.Second, time.Minute, fixed(0)); got != 500*time.Millisecond {
		t.Errorf("lo negative: Delay = %s, want 500ms", got)
	}
	// A hi below lo is lo, so the delay stays in [lo/2, lo].
	if got := Delay(5, 4*time.Second, time.Second, fixed(0.999999)); got > 4*time.Second || got < 2*time.Second {
		t.Errorf("hi below lo: Delay = %s, want within [2s, 4s]", got)
	}
	// Zero and negative failures behave like one.
	want := Delay(1, time.Second, time.Minute, fixed(0.25))
	for _, f := range []int{0, -3} {
		if got := Delay(f, time.Second, time.Minute, fixed(0.25)); got != want {
			t.Errorf("failures %d: Delay = %s, want %s (the same as one failure)", f, got, want)
		}
	}
}

func TestDelayWithoutARandomSourceStaysInRange(t *testing.T) {
	for range 50 {
		got := Delay(3, time.Second, time.Minute, nil)
		if got < 2*time.Second || got >= 4*time.Second {
			t.Fatalf("Delay = %s, want within [2s, 4s)", got)
		}
	}
}
