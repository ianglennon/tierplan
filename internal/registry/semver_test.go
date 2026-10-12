// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// The order tags are listed in when no dates are known (plan 05-18, IMG-03, D-21): versions
// first, newest first, then everything else by name, descending.

func TestCompareTagsPutsTheNewestVersionFirstAndNamesWithoutAVersionAfterThem(t *testing.T) {
	want := []string{
		"10", "2", "1.30.5", "1.30.5-trixie-perl", "1.30.5-alpine", "v1.10.0", "v1.9.2", "1.9.1", "v1.2.3", "1.2.3",
		"stable", "latest", "edge", "alpine",
	}
	got := slices.Clone(want)
	rand.Shuffle(len(got), func(i, j int) { got[i], got[j] = got[j], got[i] }) //nolint:gosec // G404: a test shuffle, not a secret
	slices.SortFunc(got, CompareTags)
	if !slices.Equal(got, want) {
		t.Errorf("sorted = %v\nwant     %v", got, want)
	}
}

func TestCompareTagsReadsAMissingPartAsZeroAndIgnoresALeadingV(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"1.2", "1.1.9"},    // 1.2.0 is newer than 1.1.9
		{"2", "1.99.99"},    // 2.0.0 is newer than 1.99.99
		{"v1.10", "v1.9"},   // 10 is more than 9, not less
		{"1.10.0", "1.9.0"}, // the same without the v
	} {
		if CompareTags(tc.a, tc.b) >= 0 || CompareTags(tc.b, tc.a) <= 0 {
			t.Errorf("CompareTags(%q, %q) = %d, %d; want %q first", tc.a, tc.b, CompareTags(tc.a, tc.b), CompareTags(tc.b, tc.a), tc.a)
		}
	}
}

func TestCompareTagsPutsAReleaseBeforeItsSuffixedVariantsAndDoesNotOverflow(t *testing.T) {
	if CompareTags("1.30.5", "1.30.5-rc1") >= 0 {
		t.Error("1.30.5 must come before 1.30.5-rc1")
	}
	// A build stamp is not a number a 64-bit integer holds.
	if CompareTags("20240101120000123456789", "20231231235959123456789") >= 0 {
		t.Error("a longer build stamp must come before an earlier one")
	}
}

func TestCompareTagsIsAConsistentTotalOrder(t *testing.T) {
	names := []string{"latest", "1.0", "v1.0", "1.0.0", "1.0.0-x", "", "0", "00", "edge", "v", "1.a", "1..2", "9999999999999999999999.1"}
	for _, a := range names {
		if CompareTags(a, a) != 0 {
			t.Errorf("CompareTags(%q, %q) != 0", a, a)
		}
		for _, b := range names {
			ab, ba := CompareTags(a, b), CompareTags(b, a)
			if (ab < 0) != (ba > 0) || (ab == 0) != (ba == 0) {
				t.Errorf("CompareTags(%q, %q) = %d but the reverse is %d", a, b, ab, ba)
			}
			if ab == 0 && a != b {
				t.Errorf("%q and %q compare equal", a, b)
			}
		}
	}
}
