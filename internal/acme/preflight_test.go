// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"reflect"
	"testing"
)

func TestPreflightRefusals(t *testing.T) {
	ok := PreflightInput{PublicURL: "https://a.example.com", ChallengeType: ChallengeHTTP01}
	tests := []struct {
		name string
		mod  func(*PreflightInput)
		want []Refusal
	}{
		{"nothing wrong", func(*PreflightInput) {}, nil},
		{"a base path under http-01", func(in *PreflightInput) { in.BasePath = "/tierplan" }, []Refusal{{Code: RefuseBasePath, Path: "/tierplan"}}},
		{"a base path under another challenge", func(in *PreflightInput) { in.BasePath = "/tierplan"; in.ChallengeType = ChallengeDNS01 }, nil},
		{"dial hosts not covered", func(in *PreflightInput) {
			in.DialHosts = []string{"old.example.com", "a.example.com", "OLD.example.com"}
		}, []Refusal{{Code: RefuseAgentDialHost, Count: 2, Hosts: []string{"old.example.com"}}}},
		{"no connected agents", func(in *PreflightInput) { in.DialHosts = nil }, nil},
		{"no agent URL leaves the agent_url_ip check out", func(in *PreflightInput) { in.AgentURL = "" }, nil},
		{"a not public name with the directory overridden", func(in *PreflightInput) {
			in.PublicURL = "https://tierplan.test"
			in.DirectoryOverridden = true
		}, nil},
		{"a not public name against production", func(in *PreflightInput) { in.PublicURL = "https://tierplan.test" },
			[]Refusal{{Code: RefuseNamesNotPublic, Host: "tierplan.test"}}},
		{"an empty public URL", func(in *PreflightInput) { in.PublicURL = "" }, []Refusal{{Code: RefuseNamesNotPublic}}},
		{"dial hosts are compared with the A-label names", func(in *PreflightInput) {
			in.PublicURL = "https://bücher.example.com"
			in.DialHosts = []string{"xn--bcher-kva.example.com"}
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := ok
			tc.mod(&in)
			_, got := Preflight(in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("refusals = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPreflightBasePathOnlyForHTTP01 checks D-04 and ACME-10: a public URL with a base path is
// refused for HTTP-01 only, and every other refusal is the same under either challenge.
func TestPreflightBasePathOnlyForHTTP01(t *testing.T) {
	bad := PreflightInput{
		PublicURL: "https://10.0.0.5", AgentURL: "https://203.0.113.9", BasePath: "/tierplan",
		DialHosts: []string{"old.example.com"},
	}
	codes := func(rs []Refusal) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Code)
		}
		return out
	}
	http01, dns01 := bad, bad
	http01.ChallengeType, dns01.ChallengeType = ChallengeHTTP01, ChallengeDNS01
	_, gotHTTP := Preflight(http01)
	_, gotDNS := Preflight(dns01)

	wantHTTP := []string{RefuseNamesIP, RefuseAgentURLIP, RefuseBasePath, RefuseAgentDialHost}
	wantDNS := []string{RefuseNamesIP, RefuseAgentURLIP, RefuseAgentDialHost}
	if !reflect.DeepEqual(codes(gotHTTP), wantHTTP) {
		t.Errorf("http-01 refusals = %v, want %v", codes(gotHTTP), wantHTTP)
	}
	if !reflect.DeepEqual(codes(gotDNS), wantDNS) {
		t.Errorf("dns-01 refusals = %v, want %v", codes(gotDNS), wantDNS)
	}

	clean := PreflightInput{PublicURL: "https://a.example.com", BasePath: "/tierplan", ChallengeType: ChallengeDNS01}
	if _, got := Preflight(clean); got != nil {
		t.Errorf("a clean dns-01 install under a base path is refused: %+v", got)
	}
}

func TestPreflightReturnsEveryRefusalInOrder(t *testing.T) {
	in := PreflightInput{
		PublicURL: "https://192.0.2.1", AgentURL: "https://[2001:db8::1]", BasePath: "/tp",
		ChallengeType: ChallengeHTTP01, DialHosts: []string{"gone.example.com"},
	}
	_, got := Preflight(in)
	want := []string{RefuseNamesIP, RefuseAgentURLIP, RefuseBasePath, RefuseAgentDialHost}
	if !reflect.DeepEqual(codes(got), want) {
		t.Fatalf("codes = %v, want %v", codes(got), want)
	}

	in = PreflightInput{
		PublicURL: "https://*.example.com", AgentURL: "https://nas.lan", BasePath: "/tp",
		ChallengeType: ChallengeHTTP01, DialHosts: []string{"gone.example.com"},
	}
	_, got = Preflight(in)
	want = []string{RefuseNamesWildcard, RefuseNamesNotPublic, RefuseBasePath, RefuseAgentDialHost}
	if !reflect.DeepEqual(codes(got), want) {
		t.Fatalf("codes = %v, want %v", codes(got), want)
	}
	// The same input gives the same list every time.
	_, again := Preflight(in)
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("not stable: %+v then %+v", got, again)
	}
}
