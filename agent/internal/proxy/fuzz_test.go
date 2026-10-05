// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

// oracle is the allowlist written out a second time, on purpose: it is what
// "allowed" is allowed to mean. If the production expression ever grows a path
// this one does not name, the fuzz target finds it and fails, so widening the
// allowlist is a decision made in two places and not an accident of a regexp.
var oracle = regexp.MustCompile(
	`^/(v[0-9]+\.[0-9]+/)?(_ping|version|info|containers/json|images/json|volumes|networks|events|nodes|services|tasks)$`)

// FuzzAllowed drives the allowlist decision with arbitrary input. Whatever it is
// given, an allowed request has a method that reads, a path that is exactly one
// of the allowlisted shapes and already clean, no control character anywhere, a
// query within the cap, no body and no upgrade; and a read-only agent allows
// nothing that changes anything.
func FuzzAllowed(f *testing.F) {
	for _, s := range []struct {
		method, path, query string
		body, upgrade, ro   bool
	}{
		{"GET", "/info", "", false, false, false},
		{"HEAD", "/_ping", "", false, false, true},
		{"GET", "/v1.47/containers/json", "all=1", false, false, false},
		{"POST", "/containers/create", "", true, false, false},
		{"GET", "//info", "", false, false, false},
		{"GET", "/./info", "", false, false, false},
		{"GET", "/%2e%2e/info", "", false, false, false},
		{"GET", "/v1.47//info", "", false, false, false},
		{"GET", "/info%00", "", false, false, false},
		{"GET", "/info\x00", "", false, false, false},
		{"GET", "/info", "a=\n", false, false, false},
		{"GET", "/containers/json", strings.Repeat("a", 5<<10), false, false, false},
		{"GET", "/info", "", false, true, false},
		{"POST", "/info", "", false, false, true},
	} {
		f.Add(s.method, s.path, s.query, s.body, s.upgrade, s.ro)
	}
	f.Fuzz(func(t *testing.T, method, rawPath, rawQuery string, hasBody, hasUpgrade, readOnly bool) {
		ok, reason := Allowed(method, rawPath, rawQuery, hasBody, hasUpgrade, Policy{ReadOnly: readOnly})
		if !ok {
			if reason != "allowlist" && reason != "read_only_flag" {
				t.Fatalf("a refusal with the reason %q", reason)
			}
			return
		}
		if reason != "" {
			t.Fatalf("an allowed request came with the reason %q", reason)
		}
		if method != "GET" && method != "HEAD" {
			t.Fatalf("method %q was allowed", method)
		}
		if !oracle.MatchString(rawPath) {
			t.Fatalf("path %q was allowed but is not one of the allowlisted shapes", rawPath)
		}
		if rawPath != path.Clean(rawPath) {
			t.Fatalf("path %q was allowed but is not clean", rawPath)
		}
		if hasControlBytes(rawPath) || hasControlBytes(rawQuery) {
			t.Fatalf("%q?%q was allowed with a control character", rawPath, rawQuery)
		}
		if len(rawQuery) > maxQuery {
			t.Fatalf("a query of %d bytes was allowed", len(rawQuery))
		}
		if hasBody || hasUpgrade {
			t.Fatalf("a request with a body (%v) or an upgrade (%v) was allowed", hasBody, hasUpgrade)
		}
	})
}

func hasControlBytes(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
