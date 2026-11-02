// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import "strings"

// The kinds of a DiffLine, as the first character of a unified diff line.
const (
	DiffContext byte = ' '
	DiffAdded   byte = '+'
	DiffRemoved byte = '-'
)

// The bounds of LineDiff. A document of more than MaxDiffLines lines, or one that needs more than
// MaxDiffEdits added and removed lines to turn into the other, is not compared line by line: the
// answer is one hunk that replaces the whole document. The bounds keep the work and the memory of
// a review fixed, whatever the person typed (T-07-78).
const (
	MaxDiffLines = 20000
	MaxDiffEdits = 4000
)

// DiffLine is one line of a hunk. Old and New are 1-based line numbers: Old is 0 for an added
// line and New is 0 for a removed one.
type DiffLine struct {
	Kind     byte
	Old, New int
	Text     string
}

// Hunk is a run of changed lines with the unchanged lines around them. OldStart and NewStart are
// the number of the first line of the hunk in each document, or, for a hunk with no line on that
// side, the number of the line that would come next, so the unchanged lines between two hunks
// (or before the first) are always next.OldStart - (prev.OldStart + prev.OldLines).
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Lines              []DiffLine
}

// SplitLines is the lines of a text, without their line breaks. A final line break does not make
// another, empty line, and an empty text has no lines.
func SplitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// ChangedLines is how many lines the hunks add and remove.
func ChangedLines(hunks []Hunk) int {
	n := 0
	for _, h := range hunks {
		for _, l := range h.Lines {
			if l.Kind != DiffContext {
				n++
			}
		}
	}
	return n
}

// diffOp is one step of an edit script over line indexes: a line both documents have, one only
// the old has, or one only the new has.
type diffOp struct {
	kind     byte
	old, new int // 0-based indexes; the side a line is not on is -1
}

// LineDiff compares two texts line by line and answers the hunks that turn a into b, each with
// context unchanged lines on either side; hunks that would touch or overlap are one. Identical
// texts have no hunks. It is Myers' O(ND) difference, bounded by MaxDiffLines and MaxDiffEdits:
// beyond either the answer is one hunk that removes every line of a and adds every line of b.
func LineDiff(a, b string, context int) []Hunk {
	if a == b {
		return nil
	}
	if context < 0 {
		context = 0
	}
	x, y := SplitLines(a), SplitLines(b)
	if len(x) == len(y) && equalLines(x, y) {
		return nil // only the line endings differ
	}
	ops, ok := editScript(x, y)
	if !ok {
		return []Hunk{wholeReplacement(x, y)}
	}
	return hunksOf(ops, x, y, context)
}

func equalLines(x, y []string) bool {
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// wholeReplacement is the hunk that removes all of x and adds all of y.
func wholeReplacement(x, y []string) Hunk {
	h := Hunk{OldStart: 1, OldLines: len(x), NewStart: 1, NewLines: len(y), Lines: make([]DiffLine, 0, len(x)+len(y))}
	for i, t := range x {
		h.Lines = append(h.Lines, DiffLine{Kind: DiffRemoved, Old: i + 1, Text: t})
	}
	for i, t := range y {
		h.Lines = append(h.Lines, DiffLine{Kind: DiffAdded, New: i + 1, Text: t})
	}
	return h
}

// editScript is the shortest sequence of equal, removed and added lines that turns x into y, and
// false when x or y is longer than MaxDiffLines or the shortest needs more than MaxDiffEdits.
// The lines both texts start and end with are set aside first, so the search runs over what
// differs only.
func editScript(x, y []string) ([]diffOp, bool) {
	if len(x) > MaxDiffLines || len(y) > MaxDiffLines {
		return nil, false
	}
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	mid, ok := myers(x[pre:len(x)-suf], y[pre:len(y)-suf])
	if !ok {
		return nil, false
	}
	ops := make([]diffOp, 0, len(x)+len(y)-pre-suf)
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{DiffContext, i, i})
	}
	for _, op := range mid {
		if op.old >= 0 {
			op.old += pre
		}
		if op.new >= 0 {
			op.new += pre
		}
		ops = append(ops, op)
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, diffOp{DiffContext, len(x) - suf + i, len(y) - suf + i})
	}
	return ops, true
}

// myers finds the shortest edit script of two line lists. Each round d keeps the furthest x that
// every diagonal k of the right parity reaches with d edits; the rounds are kept to walk the
// script back from the end. That costs about d*d/2 numbers, which MaxDiffEdits bounds.
func myers(a, b []string) ([]diffOp, bool) {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil, true
	}
	limit := min(n+m, MaxDiffEdits)
	// v[k+offset] is the furthest x reached on diagonal k = x - y.
	offset := limit + 1
	v := make([]int32, 2*limit+3)
	trace := make([][]int32, 0, 64)
	found := -1
	for d := 0; d <= limit && found < 0; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = int(v[offset+k+1])
			} else {
				x = int(v[offset+k-1]) + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = int32(x) //nolint:gosec // x is at most MaxDiffLines
			if x >= n && y >= m {
				found = d
				break
			}
		}
		// Keep this round's furthest x per diagonal: d+1 numbers, k = -d, -d+2, ..., d.
		keep := make([]int32, d+1)
		for i := range keep {
			keep[i] = v[offset-d+2*i]
		}
		trace = append(trace, keep)
	}
	if found < 0 {
		return nil, false
	}

	// Walk the rounds back from (n, m): in round d the path left diagonal prevK with one edit
	// (down is an added line of b, right a removed line of a) and then followed a run of lines
	// both have.
	rev := make([]diffOp, 0, n+m)
	x, y := n, m
	for d := found; d > 0; d-- {
		prev := trace[d-1]
		at := func(k int) int { return int(prev[(k+d-1)/2]) }
		k := x - y
		down := k == -d || (k != d && at(k-1) < at(k+1))
		prevK := k - 1
		if down {
			prevK = k + 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		afterX, afterY := prevX+1, prevY
		if down {
			afterX, afterY = prevX, prevY+1
		}
		for x > afterX && y > afterY {
			x--
			y--
			rev = append(rev, diffOp{DiffContext, x, y})
		}
		if down {
			rev = append(rev, diffOp{DiffAdded, -1, prevY})
		} else {
			rev = append(rev, diffOp{DiffRemoved, prevX, -1})
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, diffOp{DiffContext, x, y})
	}
	out := make([]diffOp, len(rev))
	for i, op := range rev {
		out[len(rev)-1-i] = op
	}
	return out, true
}

// hunksOf groups an edit script into hunks with context unchanged lines around the changes.
func hunksOf(ops []diffOp, x, y []string, context int) []Hunk {
	// before[i] counts the lines of each document that come before ops[i].
	type seen struct{ old, new int }
	before := make([]seen, len(ops)+1)
	for i, op := range ops {
		before[i+1] = before[i]
		if op.old >= 0 {
			before[i+1].old++
		}
		if op.new >= 0 {
			before[i+1].new++
		}
	}
	var hunks []Hunk
	i := 0
	for i < len(ops) {
		for i < len(ops) && ops[i].kind == DiffContext {
			i++
		}
		if i == len(ops) {
			break
		}
		start := max(i-context, 0)
		// Extend over the changes, and over unchanged runs short enough that the next change's
		// context would meet this one's.
		end := i
		for end < len(ops) {
			if ops[end].kind != DiffContext {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == DiffContext {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				break
			}
			end = run
		}
		stop := min(end+context, len(ops))
		hunks = append(hunks, buildHunk(ops[start:stop], x, y, before[start].old+1, before[start].new+1))
		i = stop
	}
	return hunks
}

// buildHunk is the hunk of ops, which begin after oldStart-1 lines of the old document and
// newStart-1 of the new.
func buildHunk(ops []diffOp, x, y []string, oldStart, newStart int) Hunk {
	h := Hunk{OldStart: oldStart, NewStart: newStart, Lines: make([]DiffLine, 0, len(ops))}
	for _, op := range ops {
		line := DiffLine{Kind: op.kind}
		if op.old >= 0 {
			line.Old, line.Text = op.old+1, x[op.old]
			h.OldLines++
		}
		if op.new >= 0 {
			line.New, line.Text = op.new+1, y[op.new]
			h.NewLines++
		}
		h.Lines = append(h.Lines, line)
	}
	return h
}
