// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// shape is a hunk's lines as ' a', '-b', '+B' strings, for reading a test.
func shape(h stacks.Hunk) []string {
	out := make([]string, 0, len(h.Lines))
	for _, l := range h.Lines {
		out = append(out, string(l.Kind)+l.Text)
	}
	return out
}

func wantShape(t *testing.T, h stacks.Hunk, want ...string) {
	t.Helper()
	if got := shape(h); !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

func TestLineDiffOneChangedLine(t *testing.T) {
	hunks := stacks.LineDiff("a\nb\nc\n", "a\nB\nc\n", 3)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %+v, want one", hunks)
	}
	h := hunks[0]
	wantShape(t, h, " a", "-b", "+B", " c")
	want := stacks.Hunk{OldStart: 1, OldLines: 3, NewStart: 1, NewLines: 3, Lines: []stacks.DiffLine{
		{Kind: ' ', Old: 1, New: 1, Text: "a"},
		{Kind: '-', Old: 2, New: 0, Text: "b"},
		{Kind: '+', Old: 0, New: 2, Text: "B"},
		{Kind: ' ', Old: 3, New: 3, Text: "c"},
	}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("hunk = %+v, want %+v", h, want)
	}
	if n := stacks.ChangedLines(hunks); n != 2 {
		t.Errorf("changed lines = %d, want 2", n)
	}
}

func TestLineDiffIdenticalTextsHaveNoHunks(t *testing.T) {
	for _, c := range [][2]string{{"a\nb\n", "a\nb\n"}, {"", ""}, {"a\nb", "a\nb\n"}} {
		if hunks := stacks.LineDiff(c[0], c[1], 3); len(hunks) != 0 {
			t.Errorf("LineDiff(%q, %q) = %+v, want none: a final line break is not a change", c[0], c[1], hunks)
		}
	}
}

func TestLineDiffAgainstAnEmptyDocument(t *testing.T) {
	hunks := stacks.LineDiff("", "x: 1\ny: 2\n", 3)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %+v, want one", hunks)
	}
	wantShape(t, hunks[0], "+x: 1", "+y: 2")
	if h := hunks[0]; h.OldStart != 1 || h.OldLines != 0 || h.NewLines != 2 {
		t.Errorf("hunk = %+v", h)
	}

	back := stacks.LineDiff("x: 1\ny: 2\n", "", 3)
	if len(back) != 1 {
		t.Fatalf("hunks = %+v, want one", back)
	}
	wantShape(t, back[0], "-x: 1", "-y: 2")
}

func numbered(n int, changed ...int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if slices.Contains(changed, i) {
			fmt.Fprintf(&b, "changed %d\n", i)
			continue
		}
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestLineDiffKeepsThreeLinesOfContextAndSplitsFarApartChanges(t *testing.T) {
	hunks := stacks.LineDiff(numbered(40), numbered(40, 5, 30), 3)
	if len(hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(hunks))
	}
	if hunks[0].OldStart != 2 || hunks[0].OldLines != 7 {
		t.Errorf("first hunk starts at %d for %d lines, want 2 for 7 (three before, the change, three after)", hunks[0].OldStart, hunks[0].OldLines)
	}
	if hunks[1].OldStart != 27 {
		t.Errorf("second hunk starts at %d, want 27", hunks[1].OldStart)
	}
	// The unchanged lines between the hunks are the gap a page offers to show.
	if gap := hunks[1].OldStart - (hunks[0].OldStart + hunks[0].OldLines); gap != 18 {
		t.Errorf("gap = %d, want 18", gap)
	}
	if n := stacks.ChangedLines(hunks); n != 4 {
		t.Errorf("changed lines = %d, want 4", n)
	}
}

func TestLineDiffJoinsChangesCloserThanTwiceTheContext(t *testing.T) {
	if hunks := stacks.LineDiff(numbered(40), numbered(40, 10, 16), 3); len(hunks) != 1 {
		t.Errorf("five unchanged lines between the changes: their contexts meet, got %d hunks", len(hunks))
	}
	if hunks := stacks.LineDiff(numbered(40), numbered(40, 10, 18), 3); len(hunks) != 2 {
		t.Errorf("seven unchanged lines between the changes: a gap of one, got %d hunks", len(hunks))
	}
}

func TestLineDiffInsertionAndDeletion(t *testing.T) {
	hunks := stacks.LineDiff("a\nb\nc\nd\n", "a\nb\nX\nc\nd\n", 1)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %+v", hunks)
	}
	wantShape(t, hunks[0], " b", "+X", " c")
	if hunks[0].OldStart != 2 || hunks[0].NewStart != 2 {
		t.Errorf("starts = %d and %d, want 2 and 2", hunks[0].OldStart, hunks[0].NewStart)
	}

	hunks = stacks.LineDiff("a\nb\nc\nd\n", "a\nd\n", 1)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %+v", hunks)
	}
	wantShape(t, hunks[0], " a", "-b", "-c", " d")
}

func TestLineDiffOfVeryDifferentDocumentsIsTheShortestScript(t *testing.T) {
	hunks := stacks.LineDiff("a\nb\nc\na\nb\nb\na\n", "c\nb\na\nb\na\nc\n", 0)
	if n := stacks.ChangedLines(hunks); n != 5 {
		t.Errorf("changed lines = %d, want 5: the textbook Myers example takes 5 edits", n)
	}
}

func TestLineDiffMoreLinesThanTheBoundIsOneReplacingHunk(t *testing.T) {
	hunks := stacks.LineDiff(numbered(25000), numbered(25000, 12000), 3)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %d, want one", len(hunks))
	}
	h := hunks[0]
	if h.OldStart != 1 || h.OldLines != 25000 || h.NewLines != 25000 || len(h.Lines) != 50000 {
		t.Errorf("hunk = %d from %d for %d/%d lines (%d), want the whole of both", h.OldStart, h.OldStart, h.OldLines, h.NewLines, len(h.Lines))
	}
	if h.Lines[0].Kind != stacks.DiffRemoved || h.Lines[25000].Kind != stacks.DiffAdded {
		t.Errorf("the removed lines come first and the added after: %c then %c", h.Lines[0].Kind, h.Lines[25000].Kind)
	}
}

func TestLineDiffMoreEditsThanTheBoundIsOneReplacingHunk(t *testing.T) {
	var a, b strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&a, "old %d\n", i)
		fmt.Fprintf(&b, "new %d\n", i)
	}
	hunks := stacks.LineDiff(a.String(), b.String(), 3)
	if len(hunks) != 1 || hunks[0].OldLines != 3000 || hunks[0].NewLines != 3000 {
		t.Fatalf("hunks = %d, want one that replaces all 3000 lines: 6,000 edits are over the bound of 4,000", len(hunks))
	}
}

func TestLineDiffUnderTheBoundsStillFindsTheEdits(t *testing.T) {
	// 10,000 lines with 30 changed: far under both bounds, so every change is its own small hunk.
	var changed []int
	for i := 1; i <= 30; i++ {
		changed = append(changed, i*300)
	}
	hunks := stacks.LineDiff(numbered(10000), numbered(10000, changed...), 3)
	if len(hunks) != 30 || stacks.ChangedLines(hunks) != 60 {
		t.Errorf("hunks = %d, changed lines = %d, want 30 and 60", len(hunks), stacks.ChangedLines(hunks))
	}
}

// applyHunks rebuilds the new document from the old one and the hunks, the way a reader would.
func applyHunks(a string, hunks []stacks.Hunk) []string {
	old := stacks.SplitLines(a)
	var out []string
	pos := 0 // index in old
	for _, h := range hunks {
		for pos < h.OldStart-1 {
			out = append(out, old[pos])
			pos++
		}
		for _, l := range h.Lines {
			switch l.Kind {
			case stacks.DiffContext:
				out = append(out, l.Text)
				pos++
			case stacks.DiffAdded:
				out = append(out, l.Text)
			case stacks.DiffRemoved:
				pos++
			}
		}
	}
	return append(out, old[pos:]...)
}

func TestLineDiffHunksRebuildTheNewDocument(t *testing.T) {
	cases := [][2]string{
		{"", "a\n"}, {"a\n", ""}, {"a\nb\nc\n", "c\nb\na\n"}, {"x\n", "y\n"},
		{numbered(60), numbered(60, 1, 2, 30, 60)}, {"a\na\na\na\n", "a\na\n"},
	}
	for _, c := range cases {
		for _, ctx := range []int{0, 1, 3} {
			hunks := stacks.LineDiff(c[0], c[1], ctx)
			want, got := stacks.SplitLines(c[1]), applyHunks(c[0], hunks)
			if !slices.Equal(want, got) {
				t.Errorf("context %d, %q -> %q: rebuilt %q", ctx, c[0], c[1], got)
			}
		}
	}
}
