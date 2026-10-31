// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

import "net/netip"

// MapItem is one network on the subnet map: where it lives and the prefixes it holds.
type MapItem struct {
	// Swarm is true for a swarm-wide network, which every other network is compared with.
	Swarm bool
	// Node is the node a local network is on. It is ignored for a swarm-wide network.
	Node     string
	Prefixes []netip.Prefix
}

// MaxMapOverlaps is the most partners MapOverlaps names for one item.
const MaxMapOverlaps = 50

// MapOverlaps is not implemented yet.
func MapOverlaps(items []MapItem) [][]int { return nil }

// FirstOverlap is not implemented yet.
func FirstOverlap(mine, theirs []netip.Prefix) (netip.Prefix, bool) { return netip.Prefix{}, false }

// AddressOrder is not implemented yet.
func AddressOrder(items []MapItem, names []string) []int { return nil }
