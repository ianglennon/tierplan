// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The crash evidence's log read (WR-05): the newest lines are kept, and a line the read's limit cut
// is dropped rather than stored half.
func TestTailLines(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	stamp := func(text string) string { return t1 + " " + text + "\n" }
	texts := func(lines []string) []string {
		out := make([]string, 0, len(lines))
		for _, l := range lines {
			out = append(out, strings.TrimPrefix(l, t1+" "))
		}
		return out
	}

	t.Run("the newest keep lines", func(t *testing.T) {
		t.Parallel()
		var b bytes.Buffer
		for i := range 10 {
			b.WriteString(stamp("l" + strconv.Itoa(i)))
		}
		got, err := tailLines(ctx, &b, true, 1<<20, 3)
		if err != nil || !slices.Equal(texts(got), []string{"l7", "l8", "l9"}) {
			t.Fatalf("tailLines = %q, %v; want the last three", got, err)
		}
	})

	t.Run("a log's own unfinished last line is kept", func(t *testing.T) {
		t.Parallel()
		got, err := tailLines(ctx, strings.NewReader(stamp("a")+t1+" end"), true, 1<<20, 10)
		if err != nil || !slices.Equal(texts(got), []string{"a", "end"}) {
			t.Fatalf("tailLines = %q, %v; want both lines", got, err)
		}
	})

	t.Run("a line cut by the limit is dropped", func(t *testing.T) {
		t.Parallel()
		log := stamp("a") + stamp("b") + stamp("password-is-hunter2")
		limit := int64(len(stamp("a")+stamp("b")) + len(t1) + len(" password-is-hun"))
		got, err := tailLines(ctx, strings.NewReader(log), true, limit, 10)
		if err != nil || !slices.Equal(texts(got), []string{"a", "b"}) {
			t.Fatalf("tailLines = %q, %v; want a and b, and not the half line", got, err)
		}
	})

	t.Run("a multiplexed line cut by the limit is dropped", func(t *testing.T) {
		t.Parallel()
		stream := append(dockerFrame(1, stamp("a")), dockerFrame(1, t1+" password-is-")...)
		stream = append(stream, dockerFrame(1, "hunter2\n")...)
		limit := int64(len(stream) - 3)
		got, err := tailLines(ctx, bytes.NewReader(stream), false, limit, 10)
		if !slices.Equal(texts(got), []string{"a"}) {
			t.Fatalf("tailLines = %q, %v; want a, and not the half line", got, err)
		}
	})
}
