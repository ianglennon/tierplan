// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The framing of a container's log stream (plan 04-19, OBS-01, D-15, Pitfall 12).

// dockerFrame is one frame of the engine's multiplexed stream.
func dockerFrame(kind byte, text string) []byte {
	h := make([]byte, 8)
	h[0] = kind
	binary.BigEndian.PutUint32(h[4:], uint32(len(text))) //nolint:gosec // a test string
	return append(h, text...)
}

func pumpAll(t *testing.T, stream []byte, tty bool) []LogLine {
	t.Helper()
	var got []LogLine
	if err := pumpLogs(context.Background(), bytes.NewReader(stream), tty, func(l LogLine) { got = append(got, l) }); err != nil {
		t.Fatalf("pumpLogs: %v", err)
	}
	return got
}

const (
	t1 = "2026-10-09T12:58:00.123456789Z"
	t2 = "2026-10-09T12:58:00.223456789Z"
	t3 = "2026-10-09T12:58:00.323456789Z"
)

func TestLogFrameSplitsEachStreamIntoLinesAndStripsTheStampIntoT(t *testing.T) {
	t.Parallel()
	var s []byte
	s = append(s, dockerFrame(1, t1+" first\n"+t2+" second\n")...)
	s = append(s, dockerFrame(2, t3+" oops\n")...)
	got := pumpAll(t, s, false)
	want := []LogLine{{T: t1, S: "o", M: "first"}, {T: t2, S: "o", M: "second"}, {T: t3, S: "e", M: "oops"}}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
}

func TestLogFrameKeepsALineThatCrossesTwoDockerFramesWhole(t *testing.T) {
	t.Parallel()
	var s []byte
	s = append(s, dockerFrame(1, t1+" hel")...)
	// The other stream's frame in the middle must not cut the line of the first.
	s = append(s, dockerFrame(2, t2+" err\n")...)
	s = append(s, dockerFrame(1, "lo wor")...)
	s = append(s, dockerFrame(1, "ld\n"+t3+" next\n")...)
	got := pumpAll(t, s, false)
	want := []LogLine{{T: t2, S: "e", M: "err"}, {T: t1, S: "o", M: "hello world"}, {T: t3, S: "o", M: "next"}}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
}

func TestLogFrameKeepsA20KBLineWhole(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 20<<10)
	var s []byte
	// The engine's logger cuts long lines into 16 KB frames.
	s = append(s, dockerFrame(1, t1+" "+long[:16<<10])...)
	s = append(s, dockerFrame(1, long[16<<10:]+"\n")...)
	got := pumpAll(t, s, false)
	if len(got) != 1 || got[0].M != long || got[0].T != t1 {
		t.Fatalf("got %d lines, first of %d bytes; want one of %d", len(got), len(got[0].M), len(long))
	}
}

func TestLogFrameCutsALineThatNeverEndsAtTheLimitAndSendsEveryByte(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("y", maxLogLine*2+10)
	s := dockerFrame(1, t1+" "+long+"\n"+t2+" after\n")
	got := pumpAll(t, s, false)
	if len(got) != 4 {
		t.Fatalf("got %d lines, want the line in 3 pieces and the next one", len(got))
	}
	var joined strings.Builder
	for _, l := range got[:3] {
		if len(l.M) > maxLogLine {
			t.Errorf("a piece of %d bytes is over the limit", len(l.M))
		}
		if l.T != t1 {
			t.Errorf("a piece has the stamp %q, want the line's %q", l.T, t1)
		}
		joined.WriteString(l.M)
	}
	if joined.String() != long {
		t.Errorf("the pieces do not add up to the line")
	}
	if got[3] != (LogLine{T: t2, S: "o", M: "after"}) {
		t.Errorf("the line after = %+v", got[3])
	}
}

func TestLogFrameSendsTheLastLineEvenWithoutALineEnding(t *testing.T) {
	t.Parallel()
	got := pumpAll(t, dockerFrame(1, t1+" a\n"+t2+" no newline"), false)
	if len(got) != 2 || got[1].M != "no newline" {
		t.Fatalf("lines = %+v", got)
	}
}

func TestLogFrameKeepsEmptyLinesAndLinesWithoutAStamp(t *testing.T) {
	t.Parallel()
	got := pumpAll(t, dockerFrame(1, t1+" \n"+"plain text\n"), false)
	want := []LogLine{{T: t1, S: "o", M: ""}, {T: t1, S: "o", M: "plain text"}}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
}

func TestLogFrameATTYStreamIsRawAndEveryLineIsStdout(t *testing.T) {
	t.Parallel()
	raw := []byte(t1 + " prompt$ \r\n" + t2 + " second\r\n" + t3 + " par")
	got := pumpAll(t, raw, true)
	want := []LogLine{{T: t1, S: "o", M: "prompt$ "}, {T: t2, S: "o", M: "second"}, {T: t3, S: "o", M: "par"}}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
}

func TestLogFrameANonTTYStreamShowsALineExactlyAsTheProgramWroteIt(t *testing.T) {
	t.Parallel()
	got := pumpAll(t, dockerFrame(1, t1+" with cr\r\n"), false)
	if len(got) != 1 || got[0].M != "with cr\r" {
		t.Fatalf("lines = %q", got)
	}
}

func TestLogFrameBatcherIsFullAtHundredLinesOrThirtyTwoKilobytes(t *testing.T) {
	t.Parallel()
	var b batcher
	for i := range batchLines - 1 {
		if b.add(LogLine{T: t1, S: "o", M: "x"}) {
			t.Fatalf("full after %d lines", i+1)
		}
	}
	if !b.add(LogLine{T: t1, S: "o", M: "x"}) {
		t.Fatal("not full at 100 lines")
	}
	if got := len(b.take()); got != batchLines || !b.empty() {
		t.Fatalf("take returned %d lines and left the batch empty = %v", got, b.empty())
	}

	big := LogLine{T: t1, S: "o", M: strings.Repeat("b", 12<<10)}
	for i := range 2 {
		if b.add(big) {
			t.Fatalf("full after %d lines of 12 KB, before 32 KB", i+1)
		}
	}
	if !b.add(big) {
		t.Fatal("not full at 36 KB of text")
	}
}

func TestLogFramePumpStopsWhenTheReaderIsClosed(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- pumpLogs(ctx, pr, false, func(LogLine) {}) }()
	_, _ = pw.Write(dockerFrame(1, t1+" a\n"))
	cancel()
	_ = pr.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a read that was cut short returned no error")
		}
	case <-testsupport.Context(t).Done():
		t.Fatal("pumpLogs did not return after its reader was closed")
	}
}
