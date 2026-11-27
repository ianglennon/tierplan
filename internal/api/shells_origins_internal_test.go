// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"slices"
	"testing"
)

type fixedOrigin string

func (o fixedOrigin) PublicOrigin() string { return string(o) }

// The shell WebSocket reads the same origin set as the mutation guard (D-07): the public
// origin and, while TierPlan's own route is on, the route host's origin, each as an escaped
// literal pattern. The option that switches the origin check off is never used.
func TestShellOriginPatternsIncludeExtraOrigins(t *testing.T) {
	t.Parallel()
	const public = "https://tierplan.example.org:7480"
	route := func() []string { return []string{"https://tp.apps.example.org"} }
	none := func() []string { return nil }

	cases := []struct {
		name  string
		in    PublicOriginSource
		extra func() []string
		want  []string
	}{
		{"public and route", fixedOrigin(public), route, []string{public, "https://tp.apps.example.org"}},
		{"no route: a nil source", fixedOrigin(public), nil, []string{public}},
		{"no route: an empty list", fixedOrigin(public), none, []string{public}},
		{"canonical form, repeats and junk dropped", fixedOrigin(public), func() []string {
			return []string{"HTTPS://TP.Apps.Example.Org:443", "https://tp.apps.example.org", "", "not a url", "https://tierplan.example.org:7480"}
		}, []string{public, "https://tp.apps.example.org"}},
		{"glob characters in a host are escaped", fixedOrigin(public), func() []string { return []string{"https://t*p.example"} },
			[]string{public, `https://t\*p.example`}},
		{"no public URL: the route only", fixedOrigin(""), route, []string{"https://tp.apps.example.org"}},
		{"no instance and no route", nil, nil, nil},
		{"no instance, a route", nil, route, []string{"https://tp.apps.example.org"}},
	}
	for _, tc := range cases {
		got := originPatterns(tc.in, tc.extra)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: patterns = %q, want %q", tc.name, got, tc.want)
		}
		if slices.Contains(got, "*") {
			t.Errorf("%s: a pattern switches the origin check off", tc.name)
		}
	}
}
