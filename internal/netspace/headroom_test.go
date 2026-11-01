// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

import (
	"slices"
	"testing"
)

// The size recommendation (plan 10-13, D-14): needed = ceil((2 x tasks + services + nodes) x
// 1.25) + 3, checked against one 3-replica service that used 8 addresses (RESEARCH F3).

func TestUsable(t *testing.T) {
	t.Parallel()
	for mask, want := range map[int]int{24: 253, 25: 125, 26: 61, 27: 29, 28: 13, 29: 5, 32: 0} {
		if got := Usable(mask); got != want {
			t.Errorf("Usable(%d) = %d, want %d", mask, got, want)
		}
	}
	if got := Usable(-1); got != 0 {
		t.Errorf("Usable(-1) = %d, want 0", got)
	}
	if !slices.Equal(PresetMasks, []int{24, 25, 26, 27, 28}) {
		t.Errorf("PresetMasks = %v, want 24 to 28", PresetMasks)
	}
}

func TestNeededAndRecommend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		tasks, services, nodes int
		needed                 int
		mask                   int
		ok                     bool
	}{
		// (2*20+1+0)*1.25 = 51.25, ceiled to 52, plus 3.
		{"20 tasks of one service", 20, 1, 0, 55, 26, true},
		// (2*3+1+3)*1.25 = 12.5, ceiled to 13, plus 3: a 3-replica service on 3 nodes.
		{"3 tasks on 3 nodes", 3, 1, 3, 16, 27, true},
		{"one task", 1, 1, 0, 7, 28, true},
		{"just past a /28", 5, 1, 0, 17, 27, true},
		// (2*3+1+1)*1.25 = 10, ceiled to 10, plus 3: exactly the 13 usable addresses of a /28.
		{"exactly the usable addresses of a /28", 3, 1, 1, 13, 28, true},
		{"too many for a /24", 120, 1, 0, 305, 0, false},
		{"just fits a /24", 99, 1, 0, 252, 24, true},
		{"just past a /24", 100, 1, 0, 255, 0, false},
	} {
		got := Needed(tc.tasks, tc.services, tc.nodes)
		if got != tc.needed {
			t.Errorf("%s: Needed = %d, want %d", tc.name, got, tc.needed)
		}
		mask, ok := RecommendMask(got)
		if mask != tc.mask || ok != tc.ok {
			t.Errorf("%s: RecommendMask(%d) = %d, %v, want %d, %v", tc.name, got, mask, ok, tc.mask, tc.ok)
		}
	}
}
