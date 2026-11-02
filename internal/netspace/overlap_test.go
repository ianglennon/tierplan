// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"
)

// The findings of a subnet check (plan 10-16, NET-05, D-16, D-18, T-10-15).

func named(name string, it MapItem) MapItemNamed { return MapItemNamed{MapItem: it, Name: name} }

// kinds is the kind of each finding, in order, with the severity.
func kinds(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, string(f.Kind)+"/"+string(f.Severity))
	}
	return out
}

func TestFindOverlapsKinds(t *testing.T) {
	t.Parallel()
	cand := netip.MustParsePrefix("192.168.5.0/24")

	// Two networks of another environment.
	other := OtherEnv{Name: "site-b", Networks: []MapItemNamed{
		named("lan", swarmNet(t, "192.168.5.0/25")),
		named("dmz", localNet(t, "n2", "192.168.5.128/25")),
		named("far", swarmNet(t, "10.9.0.0/24")),
	}}
	got := FindOverlaps(cand, OverlapInput{Swarm: true, Other: []OtherEnv{other}})
	if len(got) != 2 || got[0].Kind != KindOtherEnv || got[0].Environment != "site-b" || got[0].Network != "dmz" || got[0].Node != "n2" || got[1].Network != "lan" || got[1].Node != "" {
		t.Errorf("other environment = %+v, want dmz on n2 then lan, both naming site-b", got)
	}

	// A route on a node.
	routes := []NodeRoute{{Node: "n1", Iface: "eth0", Dst: netip.MustParsePrefix("192.168.5.0/24")}, {Node: "n1", Iface: "tun0", Dst: netip.MustParsePrefix("10.8.0.0/24")}}
	got = FindOverlaps(netip.MustParsePrefix("192.168.5.0/25"), OverlapInput{Swarm: true, Routes: routes})
	if len(got) != 1 || got[0].Kind != KindHostRoute || got[0].Node != "n1" || got[0].Route != "192.168.5.0/24" || got[0].Iface != "eth0" {
		t.Errorf("a route = %+v, want one host_route on eth0", got)
	}

	// Outside the pools.
	pools := []Pool{{Base: netip.MustParsePrefix("10.0.0.0/16"), Size: 24}, {Base: netip.MustParsePrefix("10.2.0.0/16"), Size: 24}}
	got = FindOverlaps(netip.MustParsePrefix("10.1.0.0/24"), OverlapInput{Swarm: true, Pools: pools})
	if len(got) != 1 || got[0].Kind != KindOutsidePool || got[0].Pool != "10.0.0.0/16, 10.2.0.0/16" {
		t.Errorf("outside the pools = %+v, want one outside_pool naming both", got)
	}
	// Wider than the pool is outside it too; inside it is not; no pool known says nothing.
	if got := FindOverlaps(netip.MustParsePrefix("10.0.0.0/15"), OverlapInput{Swarm: true, Pools: pools[:1]}); len(got) != 1 {
		t.Errorf("a subnet wider than the pool = %+v, want outside_pool", got)
	}
	if got := FindOverlaps(netip.MustParsePrefix("10.2.7.0/24"), OverlapInput{Swarm: true, Pools: pools}); len(got) != 0 {
		t.Errorf("inside a pool = %+v, want none", got)
	}
	if got := FindOverlaps(cand, OverlapInput{Swarm: true}); len(got) != 0 {
		t.Errorf("no pool known = %+v, want none", got)
	}

	// Same environment is blocking, and follows the map's scope rule.
	same := []MapItemNamed{
		named("ovl", swarmNet(t, "10.0.1.0/24")),
		named("br-a", localNet(t, "n1", "172.30.0.0/24")),
		named("br-b", localNet(t, "n2", "172.30.0.0/24")),
	}
	got = FindOverlaps(netip.MustParsePrefix("172.30.0.0/25"), OverlapInput{Node: "n1", Same: same})
	if len(got) != 1 || got[0].Network != "br-a" || got[0].Severity != SeverityBlocking || got[0].Node != "n1" {
		t.Errorf("local on n1 = %+v, want br-a only", got)
	}
	got = FindOverlaps(netip.MustParsePrefix("172.30.0.0/25"), OverlapInput{Swarm: true, Same: same})
	if len(got) != 2 {
		t.Errorf("an overlay = %+v, want both bridges", got)
	}
	got = FindOverlaps(netip.MustParsePrefix("10.0.1.128/25"), OverlapInput{Node: "n2", Same: same})
	if len(got) != 1 || got[0].Network != "ovl" || got[0].Node != "" {
		t.Errorf("local on n2 against a swarm network = %+v, want ovl", got)
	}
	// A route of another node does not concern a local network of this one.
	if got := FindOverlaps(cand, OverlapInput{Node: "n2", Routes: []NodeRoute{{Node: "n1", Dst: netip.MustParsePrefix("192.168.0.0/16")}}}); len(got) != 0 {
		t.Errorf("a route of another node = %+v, want none", got)
	}
	// IPv6 is not checked.
	if got := FindOverlaps(netip.MustParsePrefix("fd00::/64"), OverlapInput{Swarm: true, Pools: pools}); got != nil {
		t.Errorf("IPv6 = %+v, want none", got)
	}
}

// A same_env finding says whether the network it overlaps is swarm-wide, which is what decides
// whether creating the subnet anyway can work (G-10-8).
func TestFindOverlapsMarksSwarmNetworks(t *testing.T) {
	t.Parallel()
	same := []MapItemNamed{
		named("ovl", swarmNet(t, "10.0.1.0/24")),
		named("br-a", localNet(t, "n1", "10.0.1.0/24")),
	}
	got := FindOverlaps(netip.MustParsePrefix("10.0.1.0/25"), OverlapInput{Swarm: true, Same: same})
	if len(got) != 2 || got[0].Network != "br-a" || got[0].Swarm || got[1].Network != "ovl" || !got[1].Swarm {
		t.Errorf("findings = %+v, want br-a not swarm, then ovl swarm", got)
	}
	other := OtherEnv{Name: "site-b", Networks: []MapItemNamed{named("lan", swarmNet(t, "10.0.1.0/25")), named("dmz", localNet(t, "n2", "10.0.1.128/25"))}}
	got = FindOverlaps(netip.MustParsePrefix("10.0.1.0/24"), OverlapInput{Swarm: true, Other: []OtherEnv{other}})
	if len(got) != 2 || got[0].Network != "dmz" || got[0].Swarm || got[1].Network != "lan" || !got[1].Swarm {
		t.Errorf("other environment = %+v, want dmz not swarm, then lan swarm", got)
	}
}

func TestFindOverlapsAdjacent(t *testing.T) {
	t.Parallel()
	in := OverlapInput{
		Swarm: true,
		Same:  []MapItemNamed{named("a", swarmNet(t, "10.0.1.0/24"))},
		Other: []OtherEnv{{Name: "e", Networks: []MapItemNamed{named("b", swarmNet(t, "10.0.3.0/24"))}}},
		Routes: []NodeRoute{
			{Node: "n", Iface: "eth0", Dst: netip.MustParsePrefix("10.0.0.0/24")},
		},
		Pools: []Pool{{Base: netip.MustParsePrefix("10.0.0.0/8"), Size: 24}},
	}
	if got := FindOverlaps(netip.MustParsePrefix("10.0.2.0/24"), in); len(got) != 0 {
		t.Errorf("a subnet between the others = %+v, want no finding of any kind", got)
	}
}

func TestFindOverlapsOrder(t *testing.T) {
	t.Parallel()
	in := OverlapInput{
		Swarm: true,
		Same: []MapItemNamed{
			named("zeta", swarmNet(t, "10.0.0.0/16")),
			named("alpha", swarmNet(t, "10.0.4.0/24")),
		},
		Other: []OtherEnv{
			{Name: "b-site", Networks: []MapItemNamed{named("x", swarmNet(t, "10.0.0.0/20"))}},
			{Name: "a-site", Networks: []MapItemNamed{named("y", swarmNet(t, "10.0.0.0/22"))}},
		},
		Routes: []NodeRoute{
			{Node: "n2", Iface: "eth0", Dst: netip.MustParsePrefix("10.0.9.0/24")},
			{Node: "n1", Iface: "eth1", Dst: netip.MustParsePrefix("10.0.8.0/24")},
			{Node: "n1", Iface: "eth0", Dst: netip.MustParsePrefix("10.0.8.0/24")},
			{Node: "n1", Iface: "tun0", Dst: netip.MustParsePrefix("10.0.10.0/24")},
		},
		Pools: []Pool{{Base: netip.MustParsePrefix("192.168.0.0/16"), Size: 24}},
	}
	got := FindOverlaps(netip.MustParsePrefix("10.0.0.0/12"), in)
	var order []string
	for _, f := range got {
		switch f.Kind {
		case KindSameEnv, KindOtherEnv:
			order = append(order, string(f.Kind)+":"+f.Environment+f.Network)
		case KindHostRoute:
			order = append(order, string(f.Kind)+":"+f.Route+f.Node+f.Iface)
		default:
			order = append(order, string(f.Kind))
		}
	}
	want := []string{
		"same_env:alpha", "same_env:zeta",
		"other_env:a-sitey", "other_env:b-sitex",
		"host_route:10.0.8.0/24n1eth0", "host_route:10.0.8.0/24n1eth1", "host_route:10.0.9.0/24n2eth0", "host_route:10.0.10.0/24n1tun0",
		"outside_pool",
	}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v\nwant    %v", order, want)
	}
	if got := kinds(got)[:3]; !slices.Equal(got, []string{"same_env/blocking", "same_env/blocking", "other_env/warning"}) {
		t.Errorf("kinds = %v", got)
	}
}

func TestFindOverlapsBounded(t *testing.T) {
	t.Parallel()
	var same []MapItemNamed
	for i := range 200 {
		same = append(same, named(fmt.Sprintf("n%03d", i), swarmNet(t, "10.0.0.0/24")))
	}
	got := FindOverlaps(netip.MustParsePrefix("10.0.0.0/24"), OverlapInput{Swarm: true, Same: same})
	if len(got) != MaxFindingsPerKind || got[0].Network != "n000" {
		t.Errorf("200 overlapping networks gave %d findings, want %d, the first by name", len(got), MaxFindingsPerKind)
	}
}

// Private (RFC 1918) ranges are not a finding of their own: a real LAN or VPN clash shows as a
// host route.
func TestFindOverlapsPrivateRangesAreNotFindings(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		subnet string
		pool   Pool
	}{
		{"192.168.5.0/24", Pool{Base: netip.MustParsePrefix("192.168.0.0/16"), Size: 24}},
		{"172.17.3.0/24", Pool{Base: netip.MustParsePrefix("172.17.0.0/16"), Size: 24}},
		{"10.0.5.0/24", Pool{Base: DefaultSwarmPool, Size: 24}},
	} {
		got := FindOverlaps(netip.MustParsePrefix(c.subnet), OverlapInput{Swarm: true, Pools: []Pool{c.pool}})
		if len(got) != 0 {
			t.Errorf("%s = %+v, want no findings", c.subnet, got)
		}
	}
}
