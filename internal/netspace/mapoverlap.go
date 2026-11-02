// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import (
	"cmp"
	"net/netip"
	"slices"
	"sort"
)

// MapItem is one network on the subnet map: where it lives and the prefixes it holds.
type MapItem struct {
	// Swarm is true for a swarm-wide network, which every other network is compared with.
	Swarm bool
	// Node is the node a local network is on. It is ignored for a swarm-wide network.
	Node     string
	Prefixes []netip.Prefix
}

// MaxMapOverlaps is the most partners MapOverlaps names for one item. A hostile or broken host
// can report the same subnet on thousands of networks; the answer stays bounded (T-10-15).
const MaxMapOverlaps = 50

// sameScope reports whether two networks are compared at all: a swarm network is compared with
// every network, a local network with the local networks of its own node.
func sameScope(a, b MapItem) bool {
	return a.Swarm || b.Swarm || a.Node == b.Node
}

// MapOverlaps gives, for each item, the sorted indexes of the other items whose IPv4 subnets
// overlap one of its own, by the scope rule of sameScope. Subnets that only touch do not
// overlap. A prefix that is not IPv4 is skipped, so an item with none has no overlaps. At most
// MaxMapOverlaps indexes are named per item.
//
// It sorts the intervals by start and sweeps with the set of intervals still open, so the work
// is the sorting plus the overlapping pairs and not every pair of items.
func MapOverlaps(items []MapItem) [][]int {
	type seg struct {
		lo, hi uint64
		item   int
	}
	var spans []seg
	for i, it := range items {
		for _, p := range it.Prefixes {
			if isV4(p) {
				lo, hi := span(p)
				spans = append(spans, seg{lo, hi, i})
			}
		}
	}
	slices.SortFunc(spans, func(a, b seg) int {
		return cmp.Or(cmp.Compare(a.lo, b.lo), cmp.Compare(a.hi, b.hi), cmp.Compare(a.item, b.item))
	})

	found := make([]map[int]struct{}, len(items))
	note := func(a, b int) {
		if found[a] == nil {
			found[a] = map[int]struct{}{}
		}
		found[a][b] = struct{}{}
	}
	var open []seg
	for _, s := range spans {
		// An interval that ended at or before this start touches at most; drop it for good,
		// since every later interval starts no earlier.
		open = slices.DeleteFunc(open, func(o seg) bool { return o.hi <= s.lo })
		for _, o := range open {
			if o.item != s.item && sameScope(items[o.item], items[s.item]) {
				note(o.item, s.item)
				note(s.item, o.item)
			}
		}
		open = append(open, s)
	}

	out := make([][]int, len(items))
	for i := range items {
		out[i] = make([]int, 0, len(found[i]))
		for j := range found[i] {
			out[i] = append(out[i], j)
		}
		sort.Ints(out[i])
		if len(out[i]) > MaxMapOverlaps {
			out[i] = out[i][:MaxMapOverlaps]
		}
	}
	return out
}

// FirstOverlap is the first prefix of theirs, in their order, that overlaps one of mine. Only
// IPv4 prefixes are compared.
func FirstOverlap(mine, theirs []netip.Prefix) (netip.Prefix, bool) {
	for _, t := range theirs {
		if !isV4(t) {
			continue
		}
		tlo, thi := span(t)
		for _, m := range mine {
			if !isV4(m) {
				continue
			}
			if mlo, mhi := span(m); mlo < thi && tlo < mhi {
				return t, true
			}
		}
	}
	return netip.Prefix{}, false
}

// AddressOrder gives each item its position in the address order: by the start address of its
// first IPv4 subnet as a number (10.0.9.0/24 before 10.0.10.0/24), then the mask (the shorter
// first), then the name, then the node. An item with no IPv4 subnet comes last, by name and
// node. names holds the name of each item.
func AddressOrder(items []MapItem, names []string) []int {
	type key struct {
		lo   uint64
		bits int
		has  bool
		name string
		node string
		idx  int
	}
	keys := make([]key, len(items))
	for i, it := range items {
		k := key{node: it.Node, idx: i}
		if i < len(names) {
			k.name = names[i]
		}
		for _, p := range it.Prefixes {
			if isV4(p) {
				k.lo, _ = span(p)
				k.bits, k.has = p.Bits(), true
				break
			}
		}
		keys[i] = k
	}
	order := slices.Clone(keys)
	slices.SortFunc(order, func(a, b key) int {
		if a.has != b.has {
			if a.has {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.lo, b.lo), cmp.Compare(a.bits, b.bits), cmp.Compare(a.name, b.name), cmp.Compare(a.node, b.node), cmp.Compare(a.idx, b.idx))
	})
	pos := make([]int, len(items))
	for at, k := range order {
		pos[k.idx] = at
	}
	return pos
}
