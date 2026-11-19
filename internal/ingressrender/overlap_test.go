// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"slices"
	"strings"
	"testing"
)

// accepts is the reference segment matcher for a route with path prefix p (the same meaning as the
// rule Rule renders): p is "/", or the request path q equals p, or q starts with p followed by "/".
// It is the model of what Traefik is given, not a second implementation of Traefik; 21-11 confirms the
// same table against a real Traefik.
func accepts(p, q string) bool {
	return p == "/" || q == p || strings.HasPrefix(q, p+"/")
}

// winner is the prefix a request path resolves to: among the prefixes that accept q, the one with the
// highest Priority. It returns "" when none accepts (Traefik answers 404 for a known host).
func winner(prefixes []string, q string) string {
	best, bestPriority := "", 0
	for _, p := range prefixes {
		if !accepts(p, q) {
			continue
		}
		if pr := Priority(p); pr > bestPriority {
			best, bestPriority = p, pr
		}
	}
	return best
}

// TestOverlapTable is D-16 (RTE-03) at matcher level: the longest segment prefix wins, a prefix that
// only shares letters (/apiary against /api, /api/v2x against /api/v2) does not, and a known host with
// no matching route matches nothing.
func TestOverlapTable(t *testing.T) {
	cases := []struct {
		name     string
		prefixes []string
		path     string
		want     string
	}{
		{"root takes /", []string{"/", "/api", "/api/v2"}, "/", "/"},
		{"root takes an unrelated path", []string{"/", "/api", "/api/v2"}, "/x", "/"},
		{"/apiary is not under /api", []string{"/", "/api", "/api/v2"}, "/apiary", "/"},
		{"/api exact", []string{"/", "/api", "/api/v2"}, "/api", "/api"},
		{"/api with a trailing slash", []string{"/", "/api", "/api/v2"}, "/api/", "/api"},
		{"below /api", []string{"/", "/api", "/api/v2"}, "/api/x", "/api"},
		{"/api/v2 exact", []string{"/", "/api", "/api/v2"}, "/api/v2", "/api/v2"},
		{"below /api/v2", []string{"/", "/api", "/api/v2"}, "/api/v2/z", "/api/v2"},
		{"/api/v2x is not under /api/v2", []string{"/", "/api", "/api/v2"}, "/api/v2x", "/api"},

		{"no root, unrelated path matches nothing", []string{"/api", "/api/v2"}, "/x", ""},
		{"no root, /apiary matches nothing", []string{"/api", "/api/v2"}, "/apiary", ""},
		{"no root, /ap matches nothing", []string{"/api", "/api/v2"}, "/ap", ""},
		{"no root, /api/v2x falls to /api", []string{"/api", "/api/v2"}, "/api/v2x", "/api"},
		{"no root, below /api/v2", []string{"/api", "/api/v2"}, "/api/v2/z", "/api/v2"},

		{"disjoint /ab", []string{"/ab", "/cd"}, "/ab", "/ab"},
		{"disjoint below /ab", []string{"/ab", "/cd"}, "/ab/x", "/ab"},
		{"disjoint /cd", []string{"/ab", "/cd"}, "/cd", "/cd"},
		{"disjoint below /cd", []string{"/ab", "/cd"}, "/cd/y", "/cd"},
		{"disjoint /abcd", []string{"/ab", "/cd"}, "/abcd", ""},
		{"disjoint root path", []string{"/ab", "/cd"}, "/", ""},

		{"the order of the routes does not matter", []string{"/api/v2", "/", "/api"}, "/api/v2/z", "/api/v2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := winner(tc.prefixes, tc.path); got != tc.want {
				t.Errorf("%v: %s resolves to %q, want %q", tc.prefixes, tc.path, got, tc.want)
			}
		})
	}

	t.Run("with a root route every path matches something", func(t *testing.T) {
		prefixes := []string{"/", "/api", "/api/v2"}
		for _, q := range []string{"/", "/x", "/api", "/apiary", "/api/v2x", "/a/b/c", "/%2e"} {
			if winner(prefixes, q) == "" {
				t.Errorf("%s matches no route although / is routed", q)
			}
		}
	})

	t.Run("two disjoint prefixes share a priority and never meet", func(t *testing.T) {
		if Priority("/ab") != 1003 || Priority("/cd") != 1003 {
			t.Fatalf("Priority(/ab) = %d, Priority(/cd) = %d, want 1003 for both", Priority("/ab"), Priority("/cd"))
		}
		for _, q := range []string{"/ab", "/ab/x", "/cd", "/cd/y", "/abcd", "/"} {
			if accepts("/ab", q) && accepts("/cd", q) {
				t.Errorf("%s is accepted by both /ab and /cd, so their equal priority would decide", q)
			}
		}
	})
}

// longPrefixes is 30 distinct valid prefixes (a leading slash, no trailing slash, no empty segment,
// at most 255 bytes) that cover nesting, adjacency and the length cap.
func longPrefixes() []string {
	a252 := strings.Repeat("a", 252)
	return []string{
		"/", "/a", "/a/b", "/ab", "/a/bc", "/a/b/c", "/abc", "/a/b/cd",
		"/api", "/api/v2", "/apiary", "/api/v2x", "/api/v2/z",
		"/cd", "/c/d", "/x", "/x/y", "/x/y/z", "/x/yz", "/xy", "/xy/z",
		"/static", "/static/css", "/static/css/app", "/v1", "/v1/users", "/v1/users/42",
		"/" + a252,                     // 253 bytes, one segment
		"/" + a252 + "/b",              // 255 bytes, nested under the one above
		"/" + strings.Repeat("a", 254), // 255 bytes, one segment: the cap
	}
}

// TestLongerSegmentPrefixAlwaysWins is the property behind D-16: over every pair of the 30 prefixes,
// when one is a segment prefix of the other the longer has the strictly higher Priority, and when two
// share a Priority no request path (from probes built on every prefix) is accepted by both.
func TestLongerSegmentPrefixAlwaysWins(t *testing.T) {
	prefixes := longPrefixes()
	if len(prefixes) != 30 {
		t.Fatalf("%d prefixes, want 30", len(prefixes))
	}
	sorted := slices.Clone(prefixes)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != 30 {
		t.Fatalf("the prefixes are not distinct")
	}
	for _, p := range prefixes {
		if len(p) > 255 {
			t.Fatalf("prefix of %d bytes is over the 255 cap", len(p))
		}
	}

	var probes []string
	for _, p := range prefixes {
		probes = append(probes, p, p+"/x", p+"x")
	}

	for _, short := range prefixes {
		for _, long := range prefixes {
			if short == long {
				continue
			}
			if accepts(short, long) && !accepts(long, short) {
				if Priority(long) <= Priority(short) {
					t.Errorf("%q is under %q but Priority %d <= %d", long, short, Priority(long), Priority(short))
				}
			}
			if Priority(short) == Priority(long) {
				for _, q := range probes {
					if accepts(short, q) && accepts(long, q) {
						t.Errorf("%q and %q share Priority %d and both accept %q", short, long, Priority(short), q)
					}
				}
			}
		}
	}
}
