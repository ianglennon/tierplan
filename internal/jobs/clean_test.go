// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package jobs_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ianglennon/coaming/internal/jobs"
)

func TestCleanRemovesControlCharactersExceptATab(t *testing.T) {
	t.Parallel()
	got := jobs.Clean("a\x00b\x1b[31mc\r\nd\te\x7ff\u0085g\u009bh", nil)
	if want := "ab[31mcd\tefgh"; got != want {
		t.Errorf("Clean = %q, want %q", got, want)
	}
}

func TestCleanDropsBytesThatAreNotUTF8(t *testing.T) {
	t.Parallel()
	got := jobs.Clean("ok\xff\xfe-ü", nil)
	if want := "ok-ü"; got != want || !utf8.ValidString(got) {
		t.Errorf("Clean = %q, want %q", got, want)
	}
}

func TestCleanLeavesATextOfExactlyTheLimitAlone(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("é", jobs.MaxDetailRunes)
	if got := jobs.Clean(text, nil); got != text {
		t.Errorf("a text of %d characters changed to %d characters", jobs.MaxDetailRunes, utf8.RuneCountInString(got))
	}
}

func TestCleanClipsALongerTextToTheLimitEndingInAnEllipsis(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("é", jobs.MaxDetailRunes+500)
	got := jobs.Clean(text, nil)
	if n := utf8.RuneCountInString(got); n != jobs.MaxDetailRunes {
		t.Fatalf("clipped to %d characters, want %d", n, jobs.MaxDetailRunes)
	}
	if !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "éé") || !utf8.ValidString(got) {
		t.Errorf("clipped text = %q, want the start of the text and an ellipsis on a character boundary", head(got, 20))
	}
}

func TestCleanMasksBeforeItClipsSoASecretAtTheEdgeIsNeverHalfShown(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-hunter2-hunter2"
	// The secret starts 10 characters before the limit: clipped first it would leave its first
	// ten characters in the text.
	text := strings.Repeat("x", jobs.MaxDetailRunes-10) + secret + strings.Repeat("y", 100)
	mask := func(s string) string { return strings.ReplaceAll(s, secret, "<hidden>") }
	got := jobs.Clean(text, mask)
	if strings.Contains(got, "hunter") {
		t.Errorf("part of the secret is in the text: %q", tail(got, 40))
	}
	if !strings.Contains(got, "<hidden>") {
		t.Errorf("the secret was not replaced: %q", tail(got, 40))
	}
}

func TestCleanCleansWhatTheMaskReturned(t *testing.T) {
	t.Parallel()
	mask := func(s string) string { return strings.ReplaceAll(s, "secret", "\x1b[0m<hidden>\n") }
	if got, want := jobs.Clean("a secret b", mask), "a [0m<hidden> b"; got != want {
		t.Errorf("Clean = %q, want %q", got, want)
	}
}

func TestCleanOfAnEmptyTextIsEmpty(t *testing.T) {
	t.Parallel()
	if got := jobs.Clean("", func(s string) string { return s }); got != "" {
		t.Errorf("Clean = %q", got)
	}
}

// head and tail are the start and end of a text, for a failure message that must not panic on a
// text shorter than it expects.
func head(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
