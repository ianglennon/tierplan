// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package tunnel_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/tunnel"
)

// The compatibility window (D-25, Pitfall 23): protocol 1 only; for a release
// server an agent whose minor is the server's or the one before; a server that is
// not a release accepts any agent of protocol 1.
func TestCompatibleDecidesTheWindowForEveryKindOfBuild(t *testing.T) {
	for _, tc := range []struct {
		name          string
		server, agent string
		proto         int
		ok, tooOld    bool
		detail        string // a fragment the refusal must carry
	}{
		{"the same version", "v0.3.1", "v0.3.1", 1, true, false, ""},
		{"the same minor, an older patch", "v0.3.1", "0.3.0", 1, true, false, ""},
		{"the same minor, a newer patch", "v0.3.1", "v0.3.9", 1, true, false, ""},
		{"the previous minor", "v0.3.1", "0.2.7", 1, true, false, ""},
		{"two minors back is too old", "v0.3.1", "0.1.9", 1, false, true, "too old"},
		{"the next minor is too new", "v0.3.1", "0.4.0", 1, false, false, "too new"},
		{"a prerelease of the same minor", "v0.3.1", "v0.3.0-rc.1", 1, true, false, ""},
		{"a prerelease server", "v0.3.0-rc.2", "v0.2.4", 1, true, false, ""},
		{"a later major is too new", "v0.9.0", "v1.0.0", 1, false, false, "too new"},
		{"an earlier major is too old", "v1.0.0", "v0.9.9", 1, false, true, "too old"},
		{"1.x keeps the same window", "v1.4.0", "v1.3.2", 1, true, false, ""},
		{"1.x refuses two minors back", "v1.4.0", "v1.2.0", 1, false, true, "too old"},
		{"a dev server accepts any agent", "dev (abc123)", "0.1.0", 1, true, false, ""},
		{"a dev server accepts a much newer agent", "dev (abc123)", "v9.9.9", 1, true, false, ""},
		{"an edge server accepts any agent", "edge-20261003-abc1234", "v0.0.1", 1, true, false, ""},
		{"an empty server version is not a release", "", "v0.7.0", 1, true, false, ""},
		{"a release server accepts a dev agent", "v0.3.1", "dev", 1, true, false, ""},
		{"a release server accepts an edge agent", "v0.3.1", "edge", 1, true, false, ""},
		{"protocol 2 is refused by a release server", "v0.3.1", "0.3.1", 2, false, false, "protocol"},
		{"protocol 2 is refused by a dev server", "dev (abc123)", "0.3.1", 2, false, false, "protocol"},
		{"protocol 0 is too old", "v0.3.1", "0.3.1", 0, false, true, "protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, tooOld, detail := tunnel.Compatible(tc.server, tc.agent, tc.proto)
			if ok != tc.ok || (!ok && tooOld != tc.tooOld) {
				t.Fatalf("Compatible(%q, %q, %d) = ok %v, tooOld %v; want ok %v, tooOld %v", tc.server, tc.agent, tc.proto, ok, tooOld, tc.ok, tc.tooOld)
			}
			if ok && detail != "" {
				t.Errorf("an accepted agent got the detail %q", detail)
			}
			if !ok && !strings.Contains(detail, tc.detail) {
				t.Errorf("detail %q does not say %q", detail, tc.detail)
			}
		})
	}
}

func TestCompatibleReadsOnlyPlainReleaseVersions(t *testing.T) {
	// Anything that is not vMAJOR.MINOR.PATCH[-pre] is a non-release build: it is
	// never refused for its version, whichever side it is on.
	for _, v := range []string{"", "dev", "dev (abc123)", "edge", "1.2", "v1", "v1.2.x", "a.b.c", "v-1.2.3", "v1.2.3.4"} {
		if ok, _, _ := tunnel.Compatible("v0.3.1", v, 1); !ok {
			t.Errorf("an agent of %q was refused by a release server", v)
		}
		if ok, _, _ := tunnel.Compatible(v, "v0.1.0", 1); !ok {
			t.Errorf("a server of %q refused an agent", v)
		}
	}
}

// Behind says an agent is older than its server by a minor or a patch (plan 02-12,
// D-32): the "Update" badge of the environments overview. Anything that is not a
// release on both sides, a prerelease suffix and a newer agent never say so.
func TestBehindIsTrueOnlyForAnAgentOlderThanTheServerByAMinorOrAPatch(t *testing.T) {
	for _, tc := range []struct {
		server, agent string
		want          bool
	}{
		{"v0.3.1", "0.3.0", true},
		{"v0.3.0", "v0.2.9", true},
		{"v1.0.0", "0.9.9", true},
		{"v0.3.0", "0.3.0", false},
		{"v0.3.0", "0.3.1", false},
		{"v0.3.0", "0.4.0", false},
		{"v0.3.0-rc.1", "0.3.0", false},
		{"v0.3.0", "0.3.0-rc.1", false},
		{"v0.3.10", "0.3.9", true},
		{"edge", "0.3.0", false},
		{"v0.3.0", "dev (abc123)", false},
		{"", "0.3.0", false},
		{"v0.3.0", "", false},
	} {
		if got := tunnel.Behind(tc.server, tc.agent); got != tc.want {
			t.Errorf("Behind(%q, %q) = %v, want %v", tc.server, tc.agent, got, tc.want)
		}
	}
}
