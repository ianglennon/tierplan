// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package redact_test

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/redact"
)

// MaskLogLine hides secrets in a crashed container's log tail before it is stored (plan 11-13, D-06,
// T-11-35). Masking is best effort; these are the forms it is held to.

func TestMaskLogLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, line string
		secrets    []string
		want       string
		masked     bool
	}{
		{name: "a secret-named pair", line: "password=hunter2", want: "password=<hidden>", masked: true},
		{name: "a pair among words", line: "boot DB_PASSWORD=hunter2 started", want: "boot DB_PASSWORD=<hidden> started", masked: true},
		{name: "a plain pair is left alone", line: "listening PORT=8080", want: "listening PORT=8080"},
		{name: "a line with nothing to hide", line: "2026-01-01T00:00:00Z started", want: "2026-01-01T00:00:00Z started"},
		{name: "url user-info", line: "connecting to postgres://app:s3cret@db/x", want: "connecting to postgres://<hidden>@db/x", masked: true},
		{name: "an authorization header", line: "Authorization: Bearer abc.def", want: "Authorization: Bearer <hidden>", masked: true},
		{name: "a lower case authorization pair", line: "authorization=xyz", want: "authorization=<hidden>", masked: true},
		{name: "a basic credential", line: "sent authorization: Basic dXNlcjpwdw== now", want: "sent authorization: Basic <hidden> now", masked: true},
		{name: "a bearer token alone", line: "retry with Bearer eyJhbGciOi.payload.sig", want: "retry with Bearer <hidden>", masked: true},
		{name: "prose about a bearer is left alone", line: "the bearer of bad news", want: "the bearer of bad news"},
		{name: "a secret name with a colon", line: "api_key: 12345abc went wrong", want: "api_key: <hidden> went wrong", masked: true},
		{name: "a json secret", line: `{"password": "hunter2", "user": "bob"}`, want: `{"password": "<hidden>", "user": "bob"}`, masked: true},
		{name: "a plain name with a colon", line: "user: bob", want: "user: bob"},
		{name: "a clock time is not a pair", line: "at 12:30:45 the token expired", want: "at 12:30:45 the token expired"},
		{name: "a quoted pair", line: `starting secret="two words" now`, want: "starting secret=<hidden> now", masked: true},
		{name: "a literal secret anywhere", line: "dial tcp: hunter2xyz refused for x-hunter2xyz-y", secrets: []string{"hunter2xyz"}, want: "dial tcp: <hidden> refused for x-<hidden>-y", masked: true},
		{name: "the longest secret goes first", line: "token abcdefgh1234 and abcdefgh", secrets: []string{"abcdefgh", "abcdefgh1234"}, want: "token <hidden> and <hidden>", masked: true},
		{name: "a short secret is not worth hiding", line: "ab ab abc", secrets: []string{"ab", ""}, want: "ab ab abc"},
		{name: "colour codes are stripped", line: "\x1b[31mred\x1b[0m text\x1b]0;title\x07 end", want: "red text end"},
		{name: "other control characters are stripped", line: "a\x00b\x07c\x7fd\rt\tx", want: "abcdt\tx"},
		{name: "a control character cannot split a secret", line: "pass hun\x1b[1mter2xyz", secrets: []string{"hunter2xyz"}, want: "pass <hidden>", masked: true},
		{name: "bytes that are not text are dropped", line: "a\xffb", want: "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, masked := redact.MaskLogLine(tc.line, tc.secrets)
			if got != tc.want || masked != tc.masked {
				t.Fatalf("MaskLogLine(%q) = %q, %v; want %q, %v", tc.line, got, masked, tc.want, tc.masked)
			}
		})
	}
}

func TestMaskLogLineCutsLongLines(t *testing.T) {
	t.Parallel()
	got, masked := redact.MaskLogLine(strings.Repeat("a", 10000), nil)
	if n := utf8.RuneCountInString(got); n != 2048 || masked {
		t.Fatalf("a 10,000 character line came out as %d characters, masked %v; want 2048 and not masked", n, masked)
	}
	// A secret across the cut is hidden whole, never cut in two with a half left behind.
	secret := "SECRET-" + strings.Repeat("z", 30)
	line := strings.Repeat("a", 2040) + secret + "tail"
	got, masked = redact.MaskLogLine(line, []string{secret})
	if strings.Contains(got, "SECRET") || strings.Contains(got, "zzz") || !masked {
		t.Fatalf("the cut left part of a secret: %q (masked %v)", got[len(got)-30:], masked)
	}
}

// A multi-line secret (a PEM key, a JSON credential) is never one line of a log, so each of its lines
// is a value of its own as well as the whole (WR-04), and an app that prints the key has every body
// line of it hidden.
func TestSecretEnvValuesMultiLine(t *testing.T) {
	t.Parallel()
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\r\n  bKcwggSjAgEAAoIBAQC7  \nab\n\n-----END PRIVATE KEY-----\n"
	got := redact.SecretEnvValues([]string{"TLS_KEY=" + key, "PORT=8080"})
	want := []string{key, "-----BEGIN PRIVATE KEY-----", "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC", "bKcwggSjAgEAAoIBAQC7", "-----END PRIVATE KEY-----"}
	if !slices.Equal(got, want) {
		t.Fatalf("SecretEnvValues = %q, want %q: the whole value and each line of 4 characters or more", got, want)
	}
	for _, line := range strings.Split(key, "\n") {
		masked, _ := redact.MaskLogLine("key line: "+line, got)
		if body := strings.TrimSpace(line); len(body) >= 4 && strings.Contains(masked, body) {
			t.Errorf("MaskLogLine(%q) = %q, want the key's line hidden", line, masked)
		}
	}
}

func TestSecretEnvValues(t *testing.T) {
	t.Parallel()
	got := redact.SecretEnvValues([]string{"DB_PASSWORD=hunter2", "PORT=8080", "API_TOKEN=a=b", "EMPTY_SECRET=", "SHORT_KEY=abc", "NOEQUALS", "SESSION_KEY=abcd"})
	want := []string{"hunter2", "abcd"}
	if !slices.Equal(got, want) {
		t.Fatalf("SecretEnvValues = %q, want %q: secret-looking names only, values of 4 characters or more", got, want)
	}
}
