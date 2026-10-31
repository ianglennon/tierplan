// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package lint_test

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/lint"
)

// The two explicit-subnet hints (plan 10-08, D-19): a subnet that overlaps a network the environment has, and one
// outside the environment's pools. Both are warnings, and both need the environment the editor's check hands in.

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

// checkWithEnv is checkOf with the environment's networks and pools.
func checkWithEnv(t *testing.T, compose string, env *lint.NetEnv) stacks.CheckResult {
	t.Helper()
	return stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm, Env: env,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}},
	})
}

// netFile is a compose file whose one network (or several) has the given ipam config entries, one subnet each.
func netFile(subnets ...string) string {
	s := "services:\n  web:\n    image: nginx:1.27\n    networks: [backend]\nnetworks:\n  backend:\n    ipam:\n      config:\n"
	for _, sub := range subnets {
		s += "        - subnet: " + sub + "\n"
	}
	return s
}

func netProblems(res stacks.CheckResult) stacks.Problems {
	var out stacks.Problems
	for _, p := range res.Problems {
		if p.Code == stacks.CodeNetworkSubnetOverlap || p.Code == stacks.CodeNetworkSubnetOutsidePool {
			out = append(out, p)
		}
	}
	return out
}

func TestNetworkSubnetOverlap(t *testing.T) {
	t.Parallel()
	env := &lint.NetEnv{
		Networks: []lint.NetInfo{
			{Name: "far", Node: "n1", Subnets: prefixes(t, "10.9.0.0/24")},
			{Name: "wide", Node: "n1", Subnets: prefixes(t, "10.0.0.0/16")},
		},
		Pools: prefixes(t, "10.0.0.0/8"),
	}
	tests := []struct {
		name    string
		subnets []string
		want    []string // messages, in order
		lines   []int
	}{
		{"inside a wider live subnet", []string{"10.0.5.0/24"}, []string{
			"network “backend” uses subnet 10.0.5.0/24, which overlaps “wide” (10.0.0.0/16) in this environment."}, []int{9}},
		{"wider than a live subnet", []string{"10.9.0.0/16"}, []string{
			"network “backend” uses subnet 10.9.0.0/16, which overlaps “far” (10.9.0.0/24) in this environment."}, []int{9}},
		{"clear of every live network", []string{"10.1.0.0/24"}, nil, nil},
		{"host bits are ignored", []string{"10.9.0.7/24"}, []string{
			"network “backend” uses subnet 10.9.0.0/24, which overlaps “far” (10.9.0.0/24) in this environment."}, []int{9}},
		{"only the overlapping entry is reported, at its index", []string{"10.1.0.0/24", "10.0.7.0/24"}, []string{
			"network “backend” uses subnet 10.0.7.0/24, which overlaps “wide” (10.0.0.0/16) in this environment."}, []int{10}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := checkWithEnv(t, netFile(tc.subnets...), env)
			got := netProblems(res)
			if len(got) != len(tc.want) {
				t.Fatalf("problems = %+v, want messages %q", got, tc.want)
			}
			for i, p := range got {
				if p.Severity != stacks.SeverityWarning || p.Code != stacks.CodeNetworkSubnetOverlap || p.Message != tc.want[i] || p.Line != tc.lines[i] {
					t.Errorf("problem %d = %+v, want a warning %q at line %d", i, p, tc.want[i], tc.lines[i])
				}
			}
			if res.Problems.Errors() != 0 || res.Loaded == nil {
				t.Errorf("a warning must not block: errors %d, loaded %v", res.Problems.Errors(), res.Loaded)
			}
		})
	}
}

func TestNetworkSubnetOutsidePool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		subnet string
		pools  []string
		want   string
	}{
		{"inside the pool", "10.5.0.0/24", []string{"10.0.0.0/8"}, ""},
		{"the whole pool", "10.0.0.0/8", []string{"10.0.0.0/8"}, ""},
		{"outside the pool", "172.30.0.0/24", []string{"10.0.0.0/8"},
			"network “backend” uses subnet 172.30.0.0/24, which is outside the address pool 10.0.0.0/8."},
		{"wider than the pool starting inside it", "10.0.0.0/7", []string{"10.0.0.0/8"},
			"network “backend” uses subnet 10.0.0.0/7, which is outside the address pool 10.0.0.0/8."},
		{"several pools are listed", "172.30.0.0/24", []string{"10.0.0.0/8", "10.200.0.0/16"},
			"network “backend” uses subnet 172.30.0.0/24, which is outside the address pool 10.0.0.0/8, 10.200.0.0/16."},
		{"inside the second of two pools", "10.200.4.0/24", []string{"192.168.0.0/16", "10.200.0.0/16"}, ""},
		{"no pool is known", "172.30.0.0/24", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := checkWithEnv(t, netFile(tc.subnet), &lint.NetEnv{Pools: prefixes(t, tc.pools...)})
			got := netProblems(res)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("problems = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("problems = %+v, want one", got)
			}
			if p := got[0]; p.Severity != stacks.SeverityWarning || p.Code != stacks.CodeNetworkSubnetOutsidePool || p.Message != tc.want || p.Line != 9 {
				t.Errorf("problem = %+v, want a warning %q at line 9", p, tc.want)
			}
		})
	}
}

func TestNetworkSubnetIgnored(t *testing.T) {
	t.Parallel()
	env := &lint.NetEnv{
		Networks: []lint.NetInfo{{Name: "live", Subnets: prefixes(t, "10.0.1.0/24")}},
		Pools:    prefixes(t, "10.0.0.0/8"),
	}
	files := map[string]string{
		"no ipam block":   "services:\n  web:\n    image: nginx:1.27\n    networks: [backend]\nnetworks:\n  backend:\n    driver: overlay\n",
		"external":        "services:\n  web:\n    image: nginx:1.27\n    networks: [backend]\nnetworks:\n  backend:\n    external: true\n",
		"an IPv6 subnet":  netFile("fd00:1::/64"),
		"not a CIDR":      netFile("not-a-cidr"),
		"no subnet given": "services:\n  web:\n    image: nginx:1.27\n    networks: [backend]\nnetworks:\n  backend:\n    ipam:\n      config:\n        - gateway: 172.30.0.1\n",
	}
	for name, compose := range files {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := netProblems(checkWithEnv(t, compose, env)); len(got) != 0 {
				t.Errorf("problems = %+v, want none", got)
			}
		})
	}
	// An external network that also carries an ipam block (the loader may or may not accept it) is never
	// compared: only the rule is asserted here, through the lint input directly.
	t.Run("external with ipam", func(t *testing.T) {
		t.Parallel()
		compose := "services:\n  web:\n    image: nginx:1.27\n    networks: [backend]\nnetworks:\n  backend:\n    external: true\n    ipam:\n      config:\n        - subnet: 10.0.1.0/24\n"
		if got := netProblems(checkWithEnv(t, compose, env)); len(got) != 0 {
			t.Errorf("problems = %+v, want none for an external network", got)
		}
	})
}

func TestNetworkSubnetNoEnv(t *testing.T) {
	t.Parallel()
	for _, compose := range []string{netFile("10.0.1.0/24"), netFile("172.30.0.0/24"), netFile("10.0.0.0/7", "not-a-cidr")} {
		if got := netProblems(checkWithEnv(t, compose, nil)); len(got) != 0 {
			t.Errorf("problems = %+v, want none without an environment", got)
		}
	}
}

func TestNetworkSubnetFindingsAreInAStableOrder(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    networks: [b, a]
networks:
  b:
    ipam:
      config:
        - subnet: 10.0.1.0/24
        - subnet: 10.0.2.0/24
  a:
    ipam:
      config:
        - subnet: 10.0.3.0/24
`
	env := &lint.NetEnv{Networks: []lint.NetInfo{{Name: "live", Subnets: prefixes(t, "10.0.0.0/8")}}}
	// The check orders what it reports by position, so the same file always answers the same list: b's entries
	// by index, then a's.
	want := []string{
		"network “b” uses subnet 10.0.1.0/24, which overlaps “live” (10.0.0.0/8) in this environment.",
		"network “b” uses subnet 10.0.2.0/24, which overlaps “live” (10.0.0.0/8) in this environment.",
		"network “a” uses subnet 10.0.3.0/24, which overlaps “live” (10.0.0.0/8) in this environment.",
	}
	for i := 0; i < 5; i++ {
		var msgs []string
		for _, p := range netProblems(checkWithEnv(t, compose, env)) {
			msgs = append(msgs, p.Message)
		}
		if !slices.Equal(msgs, want) {
			t.Fatalf("run %d: messages = %q, want %q", i, msgs, want)
		}
	}
}
