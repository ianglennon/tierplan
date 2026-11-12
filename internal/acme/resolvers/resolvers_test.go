// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package resolvers

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestResolverParsing(t *testing.T) {
	valid := []struct {
		name string
		in   []string
		want []string
	}{
		{"an IPv4 address gets port 53", []string{"1.1.1.1"}, []string{"1.1.1.1:53"}},
		{"a host name is lowercased and keeps its port", []string{"DNS.Example.com:5353"}, []string{"dns.example.com:5353"}},
		{"a trailing dot on a name is dropped", []string{"dns.example.com."}, []string{"dns.example.com:53"}},
		{"a bare IPv6 address is bracketed", []string{"::1"}, []string{"[::1]:53"}},
		{"a bracketed IPv6 address keeps its port", []string{"[::1]:5353"}, []string{"[::1]:5353"}},
		{"a bracketed IPv6 address without a port gets 53", []string{"[2001:DB8::1]"}, []string{"[2001:db8::1]:53"}},
		{"an IPv4 address keeps its port", []string{"10.0.0.53:5300"}, []string{"10.0.0.53:5300"}},
		{"the highest port", []string{"x.example:65535"}, []string{"x.example:65535"}},
		{"nothing", nil, nil},
		{"only spaces and commas", []string{" , ,"}, nil},
		{"an empty entry", []string{"", "  "}, nil},
		{"commas inside one entry split it", []string{"1.1.1.1, 8.8.8.8"}, []string{"1.1.1.1:53", "8.8.8.8:53"}},
		{"duplicates after normalisation keep the first and the admin's order", []string{"8.8.8.8", "DNS.example.com", "8.8.8.8:53", "dns.example.com:53", "1.1.1.1"}, []string{"8.8.8.8:53", "dns.example.com:53", "1.1.1.1:53"}},
		{"five entries", []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4", "9.9.9.9"}, []string{"1.1.1.1:53", "1.0.0.1:53", "8.8.8.8:53", "8.8.4.4:53", "9.9.9.9:53"}},
		{"a sixth entry that repeats one of five is not a sixth", []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4", "9.9.9.9", "1.1.1.1:53"}, []string{"1.1.1.1:53", "1.0.0.1:53", "8.8.8.8:53", "8.8.4.4:53", "9.9.9.9:53"}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseResolvers(tc.in)
			if err != nil {
				t.Fatalf("ParseResolvers(%q) = %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseResolvers(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	invalid := []struct {
		name   string
		in     []string
		entry  string
		reason string
	}{
		{"a scheme", []string{"https://x"}, "https://x", "invalid"},
		{"a path", []string{"x/y"}, "x/y", "invalid"},
		{"port zero", []string{"host:0"}, "host:0", "invalid"},
		{"a port above 65535", []string{"host:70000"}, "host:70000", "invalid"},
		{"an empty port", []string{"host:"}, "host:", "invalid"},
		{"a port that is not a number", []string{"host:dns"}, "host:dns", "invalid"},
		{"a signed port", []string{"host:+53"}, "host:+53", "invalid"},
		{"a non-ASCII name", []string{"bücher.example"}, "bücher.example", "invalid"},
		{"a name with a space inside", []string{"a b.example"}, "a b.example", "invalid"},
		{"an empty label", []string{"a..example"}, "a..example", "invalid"},
		{"a label starting with a hyphen", []string{"-a.example"}, "-a.example", "invalid"},
		{"a label longer than 63", []string{strings.Repeat("a", 64) + ".example"}, strings.Repeat("a", 64) + ".example", "invalid"},
		{"an IPv4 address with a number out of range", []string{"300.1.1.1"}, "300.1.1.1", "invalid"},
		{"a bracketed IPv4 address", []string{"[1.1.1.1]:53"}, "[1.1.1.1]:53", "invalid"},
		{"an unclosed bracket", []string{"[::1"}, "[::1", "invalid"},
		{"an IPv6 address with a zone", []string{"fe80::1%eth0"}, "fe80::1%eth0", "invalid"},
		{"an IPv6 address with an unbracketed port", []string{"::1:5353:5353:5353:5353:5353:5353:1:2:3"}, "::1:5353:5353:5353:5353:5353:5353:1:2:3", "invalid"},
		{"a query string", []string{"dns.example.com?x=1"}, "dns.example.com?x=1", "invalid"},
		{"a user part", []string{"me@dns.example.com"}, "me@dns.example.com", "invalid"},
		{"a bad entry among good ones names itself", []string{"1.1.1.1", "nope://", "8.8.8.8"}, "nope://", "invalid"},
		{"a sixth entry", []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4", "9.9.9.9", "9.9.9.10"}, "9.9.9.10", "too_many"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseResolvers(tc.in)
			var re *ResolverError
			if !errors.As(err, &re) {
				t.Fatalf("ParseResolvers(%q) = %q, %v; want a *ResolverError", tc.in, got, err)
			}
			if re.Entry != tc.entry || re.Reason != tc.reason {
				t.Errorf("ParseResolvers(%q) error = {Entry: %q, Reason: %q}, want {%q, %q}", tc.in, re.Entry, re.Reason, tc.entry, tc.reason)
			}
			if !strings.Contains(re.Error(), tc.entry) && tc.reason == "invalid" {
				t.Errorf("the error text %q does not name %q", re.Error(), tc.entry)
			}
		})
	}
}
