// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package redact_test

import (
	"testing"

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
