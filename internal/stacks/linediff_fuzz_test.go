// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/coaming/internal/stacks"
)

// FuzzLineDiff: whatever two texts a person sends, LineDiff neither panics nor answers hunks
// that fail to turn the first into the second, and every hunk's counts and line numbers agree
// with its lines.
func FuzzLineDiff(f *testing.F) {
	for _, seed := range [][2]string{
		{"", ""}, {"a\nb\nc\n", "a\nB\nc\n"}, {"", "x\n"}, {"x\n", ""}, {"a\n\n\nb", "b\n\n\na"},
		{"a\na\na\n", "a\n"}, {"\n\n", "\n"}, {"a\r\nb\r\n", "a\nb\n"},
	} {
		f.Add(seed[0], seed[1], 3)
	}
	f.Fuzz(func(t *testing.T, a, b string, context int) {
		context %= 8
		hunks := stacks.LineDiff(a, b, context)
		want := stacks.SplitLines(b)
		if got := applyHunks(a, hunks); !slices.Equal(got, want) {
			t.Fatalf("applying the hunks to %q gave %q, want %q", a, got, want)
		}
		for _, h := range hunks {
			var oldN, newN int
			for _, l := range h.Lines {
				if l.Kind != stacks.DiffAdded {
					oldN++
				}
				if l.Kind != stacks.DiffRemoved {
					newN++
				}
			}
			if oldN != h.OldLines || newN != h.NewLines {
				t.Fatalf("hunk counts %d/%d disagree with its lines %d/%d", h.OldLines, h.NewLines, oldN, newN)
			}
		}
	})
}
