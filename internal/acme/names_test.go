// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"reflect"
	"testing"
)

func codes(rs []Refusal) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Code)
	}
	return out
}

func TestWantedNames(t *testing.T) {
	tests := []struct {
		name         string
		public, agnt string
		names        []string
		refusals     []Refusal
	}{
		{"one host, upper case and a port", "https://TierPlan.Example.com:7480", "", []string{"tierplan.example.com"}, nil},
		{"two hosts, public first", "https://a.example.com", "https://b.example.com:8443", []string{"a.example.com", "b.example.com"}, nil},
		{"agent host listed first in the settings still comes second", "https://z.example.com", "https://a.example.com", []string{"z.example.com", "a.example.com"}, nil},
		{"the same host twice is one name", "https://a.example.com", "https://A.example.com:8443", []string{"a.example.com"}, nil},
		{"a Unicode host is its A-label", "https://bücher.example.com", "", []string{"xn--bcher-kva.example.com"}, nil},
		{"an IPv4 public host", "https://192.0.2.1:7480", "", []string{"192.0.2.1"}, []Refusal{{Code: RefuseNamesIP, Host: "192.0.2.1"}}},
		{"an IPv6 agent host", "https://a.example.com", "https://[2001:db8::1]", []string{"a.example.com", "2001:db8::1"}, []Refusal{{Code: RefuseAgentURLIP, Host: "2001:db8::1"}}},
		{"a wildcard", "https://*.example.com", "", []string{"*.example.com"}, []Refusal{{Code: RefuseNamesWildcard, Host: "*.example.com"}}},
		{"an empty public URL has no names", "", "", nil, []Refusal{{Code: RefuseNamesNotPublic, Host: ""}}},
		{"an empty agent URL does not apply", "https://a.example.com", "", []string{"a.example.com"}, nil},
		{"never localhost, loopback or the machine name", "https://a.example.com", "https://a.example.com", []string{"a.example.com"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WantedNames(tc.public, tc.agnt, true)
			if !reflect.DeepEqual(got.Names, tc.names) {
				t.Errorf("names = %v, want %v", got.Names, tc.names)
			}
			if !reflect.DeepEqual(got.Refusals, tc.refusals) {
				t.Errorf("refusals = %+v, want %+v", got.Refusals, tc.refusals)
			}
		})
	}
}

func TestWantedNamesPublicRule(t *testing.T) {
	notPublic := []string{
		"https://tierplan", "https://nas.lan", "https://x.home.arpa", "https://x.local",
		"https://x.internal", "https://localhost", "https://x.localhost", "https://x.home",
		"https://x.test", "https://x.example", "https://x.invalid", "https://x.onion", "https://x.alt",
		"https://x.corp", "https://co.uk",
	}
	for _, u := range notPublic {
		t.Run(u, func(t *testing.T) {
			got := WantedNames(u, "", true)
			if !reflect.DeepEqual(codes(got.Refusals), []string{RefuseNamesNotPublic}) {
				t.Errorf("refusals = %+v, want names_not_public", got.Refusals)
			}
			off := WantedNames(u, "", false)
			if len(off.Refusals) != 0 || len(off.Names) != 1 {
				t.Errorf("with checkPublic false: names %v refusals %+v, want one name and no refusal", off.Names, off.Refusals)
			}
		})
	}
	// A private-section suffix (a dynamic DNS provider) sits under an ICANN TLD, and Let's
	// Encrypt issues for it.
	for _, u := range []string{"https://me.duckdns.org", "https://me.github.io", "https://a.b.example.com", "https://xn--bcher-kva.de"} {
		t.Run(u, func(t *testing.T) {
			if got := WantedNames(u, "", true); len(got.Refusals) != 0 {
				t.Errorf("refusals = %+v, want none", got.Refusals)
			}
		})
	}
	t.Run("the agent host is held to the same rule", func(t *testing.T) {
		got := WantedNames("https://a.example.com", "https://nas.lan", true)
		if !reflect.DeepEqual(got.Refusals, []Refusal{{Code: RefuseNamesNotPublic, Host: "nas.lan"}}) {
			t.Errorf("refusals = %+v", got.Refusals)
		}
	})
}

func TestSameNames(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{[]string{"b", "a"}, []string{"a", "b"}, true},
		{[]string{"a"}, []string{"a", "b"}, false},
		{[]string{"a", "b"}, []string{"a", "c"}, false},
		{nil, []string{}, true},
	}
	for _, tc := range tests {
		if got := SameNames(tc.a, tc.b); got != tc.want {
			t.Errorf("SameNames(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	a := []string{"b", "a"}
	SameNames(a, []string{"a", "b"})
	if a[0] != "b" {
		t.Errorf("SameNames reordered its argument: %v", a)
	}
}
