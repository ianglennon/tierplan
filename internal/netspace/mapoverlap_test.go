// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"
)

// The subnet map's overlap flags and address order (plan 10-06, NET-01, D-01, T-10-15).

func swarmNet(t *testing.T, ss ...string) MapItem {
	return MapItem{Swarm: true, Prefixes: pfx(t, ss...)}
}

func localNet(t *testing.T, node string, ss ...string) MapItem {
	return MapItem{Node: node, Prefixes: pfx(t, ss...)}
}

func TestMapOverlapsScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		items []MapItem
		want  [][]int
	}{
		{"two identical swarm subnets overlap each other", []MapItem{swarmNet(t, "10.0.1.0/24"), swarmNet(t, "10.0.1.0/24")}, [][]int{{1}, {0}}},
		{"a swarm subnet and a local one inside it overlap", []MapItem{swarmNet(t, "10.0.1.0/24"), localNet(t, "node-2", "10.0.1.128/25")}, [][]int{{1}, {0}}},
		{"a local subnet contains a swarm one", []MapItem{localNet(t, "node-1", "10.0.0.0/16"), swarmNet(t, "10.0.9.0/24")}, [][]int{{1}, {0}}},
		{"two different swarm subnets do not", []MapItem{swarmNet(t, "10.0.1.0/24"), swarmNet(t, "10.0.2.0/24")}, [][]int{{}, {}}},
		{"the same local subnet on two nodes is two hosts", []MapItem{localNet(t, "node-1", "172.17.0.0/16"), localNet(t, "node-2", "172.17.0.0/16")}, [][]int{{}, {}}},
		{"nested local subnets on one node overlap", []MapItem{localNet(t, "node-1", "172.20.0.0/16"), localNet(t, "node-1", "172.20.5.0/24")}, [][]int{{1}, {0}}},
		{"a row with only an IPv6 subnet has no overlaps", []MapItem{swarmNet(t, "fd00::/64"), swarmNet(t, "fd00::/64")}, [][]int{{}, {}}},
		{"a row with no subnet has none", []MapItem{localNet(t, "node-1"), swarmNet(t, "10.0.0.0/8")}, [][]int{{}, {}}},
		{
			"either of two IPv4 subnets is enough",
			[]MapItem{swarmNet(t, "10.0.1.0/24", "10.0.5.0/24"), swarmNet(t, "10.0.5.0/25")},
			[][]int{{1}, {0}},
		},
		{
			"every partner is named once, in order",
			[]MapItem{swarmNet(t, "10.0.0.0/16"), swarmNet(t, "10.0.1.0/24", "10.0.2.0/24"), localNet(t, "n", "10.0.1.0/24")},
			[][]int{{1, 2}, {0, 2}, {0, 1}},
		},
		{"a host bit set in a prefix is ignored", []MapItem{swarmNet(t, "10.0.1.7/24"), swarmNet(t, "10.0.1.0/25")}, [][]int{{1}, {0}}},
	} {
		got := MapOverlaps(tc.items)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d answers, want %d", tc.name, len(got), len(tc.want))
			continue
		}
		for i := range tc.want {
			if !slices.Equal(got[i], tc.want[i]) {
				t.Errorf("%s: item %d overlaps %v, want %v", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestMapOverlapsAdjacentNotFlagged(t *testing.T) {
	t.Parallel()
	items := []MapItem{
		swarmNet(t, "10.0.1.0/24"), swarmNet(t, "10.0.2.0/24"), swarmNet(t, "10.0.0.0/24"),
		swarmNet(t, "128.0.0.0/2"), swarmNet(t, "192.0.0.0/2"),
	}
	for i, got := range MapOverlaps(items) {
		if len(got) != 0 {
			t.Errorf("item %d overlaps %v, want none: neighbours that touch do not overlap", i, got)
		}
	}
}

func TestMapOverlapsLarge(t *testing.T) {
	t.Parallel()
	const n = 5000
	items := make([]MapItem, 0, n)
	for i := range n - 2 {
		// 10.a.b.0/24 for n-2 distinct values.
		items = append(items, swarmNet(t, fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)))
	}
	items = append(items, localNet(t, "node-1", "10.0.3.0/25"), localNet(t, "node-1", "10.0.0.0/8"))
	got := MapOverlaps(items)
	if len(got) != n {
		t.Fatalf("%d answers, want %d", len(got), n)
	}
	if want := []int{n - 2, n - 1}; !slices.Equal(got[3], want) {
		t.Errorf("10.0.3.0/24 overlaps %v, want %v: the /25 inside it and the /8 around it", got[3], want)
	}
	// The /8 covers every other network, but names at most MaxMapOverlaps of them.
	if len(got[n-1]) != MaxMapOverlaps {
		t.Errorf("the /8 names %d partners, want the cap %d", len(got[n-1]), MaxMapOverlaps)
	}
}

func TestAddressOrderNumeric(t *testing.T) {
	t.Parallel()
	items := []MapItem{
		swarmNet(t, "10.0.10.0/24"), swarmNet(t, "10.0.9.0/24"), localNet(t, "n"), swarmNet(t, "9.0.0.0/8"),
	}
	pos := AddressOrder(items, []string{"a", "b", "none", "c"})
	if want := []int{2, 1, 3, 0}; !slices.Equal(pos, want) {
		t.Errorf("positions = %v, want %v: 9.0.0.0/8, 10.0.9.0/24, 10.0.10.0/24, then the row with no subnet", pos, want)
	}

	// Equal starts: the shorter mask first, then name, then node.
	items = []MapItem{
		localNet(t, "node-2", "10.0.0.0/24"), localNet(t, "node-1", "10.0.0.0/24"), swarmNet(t, "10.0.0.0/16"), localNet(t, "node-1", "10.0.0.0/24"),
	}
	pos = AddressOrder(items, []string{"b", "b", "z", "a"})
	if want := []int{3, 2, 0, 1}; !slices.Equal(pos, want) {
		t.Errorf("positions = %v, want %v: /16 first, then /24 by name a, then b on node-1, then b on node-2", pos, want)
	}

	// A row with an IPv6 subnet only is ordered with the rows that have no IPv4 subnet, by name.
	items = []MapItem{swarmNet(t, "fd00::/64"), localNet(t, "n"), swarmNet(t, "200.0.0.0/8")}
	pos = AddressOrder(items, []string{"v6", "bare", "hi"})
	if want := []int{2, 1, 0}; !slices.Equal(pos, want) {
		t.Errorf("positions = %v, want %v", pos, want)
	}
	if got := AddressOrder(nil, nil); len(got) != 0 {
		t.Errorf("no items: %v", got)
	}
}

func TestFirstOverlap(t *testing.T) {
	t.Parallel()
	mine := pfx(t, "10.0.1.0/24", "10.0.5.0/24")
	got, ok := FirstOverlap(mine, pfx(t, "fd00::/64", "10.0.9.0/24", "10.0.5.0/25", "10.0.1.0/25"))
	if !ok || got != netip.MustParsePrefix("10.0.5.0/25") {
		t.Errorf("first overlap = %v %v, want 10.0.5.0/25 (the first of theirs that overlaps, in their order)", got, ok)
	}
	if _, ok := FirstOverlap(mine, pfx(t, "10.0.2.0/24")); ok {
		t.Error("a neighbour is not an overlap")
	}
}
