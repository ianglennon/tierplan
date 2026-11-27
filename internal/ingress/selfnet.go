// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"net/netip"
	"slices"
)

// DefaultBridgeGateways are the gateway addresses of Docker's default bridge networks. A connection
// that reaches a container from the node it runs on arrives from one of them, so a forwarded address
// is never believed from them, whatever the self network is (D-06; Phase 24's S2 run saw 172.17.0.1
// and 172.18.0.1 on node-local connections).
var DefaultBridgeGateways = []netip.Addr{
	netip.MustParseAddr("172.17.0.1"),
	netip.MustParseAddr("172.18.0.1"),
}

// SubtractAddr returns p without the address a, as the fewest prefixes that cover every other address
// of p, in address order. An address outside p returns p unchanged. A /24 without one address is the
// eight prefixes /25 through /32 that sit beside the path down to it.
func SubtractAddr(p netip.Prefix, a netip.Addr) []netip.Prefix {
	p = p.Masked()
	a = a.Unmap()
	if !p.IsValid() || !a.IsValid() || !p.Contains(a) {
		return []netip.Prefix{p}
	}
	bits := a.BitLen()
	raw := a.AsSlice()
	var out []netip.Prefix
	for b := p.Bits() + 1; b <= bits; b++ {
		// The prefix of length b that holds a, and its sibling: the same prefix with bit b-1 flipped.
		// The sibling does not hold a, so it is kept whole and the walk goes on inside the other.
		sib := slices.Clone(raw)
		sib[(b-1)/8] ^= 0x80 >> ((b - 1) % 8)
		sibAddr, _ := netip.AddrFromSlice(sib)
		out = append(out, netip.PrefixFrom(sibAddr, b).Masked())
	}
	sortPrefixes(out)
	return out
}

// sortPrefixes orders prefixes by address, then by length.
func sortPrefixes(ps []netip.Prefix) {
	slices.SortFunc(ps, func(x, y netip.Prefix) int {
		if c := x.Addr().Compare(y.Addr()); c != 0 {
			return c
		}
		return x.Bits() - y.Bits()
	})
}

// SelfNetFacts is what the live trusted set is built from: the self network TierPlan reads from
// Docker, and the addresses that must never be trusted.
type SelfNetFacts struct {
	// Subnet is the self network's subnet.
	Subnet netip.Prefix
	// Gateway is the self network's gateway, the address a connection from the node itself carries.
	Gateway netip.Addr
	// IngressSubnets are the subnets of every tierplan-ingress overlay seen. The self network never
	// shares an address with them: a service routed through the proxy must not become a trusted peer.
	IngressSubnets []netip.Prefix
	// BridgeGateways are bridge gateways the caller knows of besides DefaultBridgeGateways.
	BridgeGateways []netip.Addr
}

// SelfTrustedPrefixes builds the live part of the trusted-proxy set (D-06): the self network's
// subnet without its gateway and without any bridge gateway inside it, in address order. It is the
// only builder of that set, so these exclusions hold whatever the live source reads. The result is
// empty when the subnet is invalid or overlaps any of the ingress overlay's subnets.
func SelfTrustedPrefixes(f SelfNetFacts) []netip.Prefix {
	if !f.Subnet.IsValid() {
		return nil
	}
	subnet := f.Subnet.Masked()
	for _, ing := range f.IngressSubnets {
		if ing.IsValid() && ing.Overlaps(subnet) {
			return nil
		}
	}
	out := []netip.Prefix{subnet}
	remove := func(a netip.Addr) {
		if !a.IsValid() {
			return
		}
		var next []netip.Prefix
		for _, p := range out {
			next = append(next, SubtractAddr(p, a)...)
		}
		out = next
	}
	remove(f.Gateway)
	for _, g := range DefaultBridgeGateways {
		remove(g)
	}
	for _, g := range f.BridgeGateways {
		remove(g)
	}
	sortPrefixes(out)
	return out
}
