// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"
)

// FindingKind says what a subnet being checked collides with (D-16).
type FindingKind string

// The kinds of finding, in the order FindOverlaps lists them.
const (
	// KindSameEnv is a network of the same environment that overlaps the subnet.
	KindSameEnv FindingKind = "same_env"
	// KindOtherEnv is a network of another environment (a routed or VPN-linked site).
	KindOtherEnv FindingKind = "other_env"
	// KindHostRoute is a route of a host (a LAN or VPN) the subnet would shadow.
	KindHostRoute FindingKind = "host_route"
	// KindOutsidePool is a subnet that lies in none of the address pools.
	KindOutsidePool FindingKind = "outside_pool"
)

// Severity is how much a finding matters (D-18).
type Severity string

const (
	// SeverityBlocking stops a create until a person says to create it anyway.
	SeverityBlocking Severity = "blocking"
	// SeverityWarning is shown and never stops anything.
	SeverityWarning Severity = "warning"
)

// MaxFindingsPerKind is the most findings of one kind FindOverlaps names. A hostile or broken
// host can report the same subnet on thousands of networks or routes; the answer stays bounded
// (T-10-15).
const MaxFindingsPerKind = 50

// Finding is one collision of the subnet being checked. Only the fields its kind names are set;
// the words are the UI's.
type Finding struct {
	Kind     FindingKind
	Severity Severity
	// Network, Subnet and Node name the overlapping network (same_env, other_env): its name,
	// the subnet of it that overlaps, and the node a local network is on.
	Network, Subnet, Node string
	// Swarm is true when the overlapping network (same_env, other_env) is swarm-wide, false for
	// a network local to one node.
	Swarm bool
	// Environment names the other environment (other_env).
	Environment string
	// Route and Iface name the host route (host_route); Node is the node it is on.
	Route, Iface string
	// Pool is the pool (outside_pool); several pools are joined with ", ".
	Pool string
}

// MapItemNamed is a network with its name.
type MapItemNamed struct {
	MapItem
	Name string
}

// OtherEnv is the networks of another environment, by the environment's name.
type OtherEnv struct {
	Name     string
	Networks []MapItemNamed
}

// NodeRoute is one route of a node's host.
type NodeRoute struct {
	Node  string
	Iface string
	Dst   netip.Prefix
}

// OverlapInput is everything a subnet is checked against. A list left empty yields no finding
// of its kind.
type OverlapInput struct {
	// Swarm is true when the network to be made is swarm-wide (an overlay), or when it is not
	// known which node it is made on: it is then compared with every network and every
	// node's routes. False, Node names the node it is local to, and the scope rule of the
	// map applies: that node's local networks and routes, and every swarm network.
	Swarm bool
	Node  string
	// Same are the networks of this environment, every node's.
	Same []MapItemNamed
	// Other are the networks of the other environments the person may read.
	Other []OtherEnv
	// Routes are the host routes of the nodes that were read.
	Routes []NodeRoute
	// Pools are the pools the network is made from; none known means nothing is said about
	// the subnet lying outside them.
	Pools []Pool
}

// FindOverlaps lists what the candidate subnet collides with, in the order same_env, other_env,
// host_route, outside_pool, and within a kind by network name or address. Subnets
// that only touch do not overlap. A candidate that is not IPv4 has no findings. At most
// MaxFindingsPerKind are named per kind.
func FindOverlaps(candidate netip.Prefix, in OverlapInput) []Finding {
	if !isV4(candidate) {
		return nil
	}
	candidate = candidate.Masked()
	cand := []netip.Prefix{candidate}
	me := MapItem{Swarm: in.Swarm, Node: in.Node}

	var same, other, routes []Finding
	for _, n := range in.Same {
		if !sameScope(me, n.MapItem) {
			continue
		}
		if sub, ok := FirstOverlap(cand, n.Prefixes); ok {
			same = append(same, Finding{Kind: KindSameEnv, Severity: SeverityBlocking, Network: n.Name, Subnet: sub.String(), Node: localNode(n), Swarm: n.Swarm})
		}
	}
	for _, e := range in.Other {
		for _, n := range e.Networks {
			if sub, ok := FirstOverlap(cand, n.Prefixes); ok {
				other = append(other, Finding{Kind: KindOtherEnv, Severity: SeverityWarning, Environment: e.Name, Network: n.Name, Subnet: sub.String(), Node: localNode(n), Swarm: n.Swarm})
			}
		}
	}
	for _, r := range in.Routes {
		if !in.Swarm && r.Node != in.Node {
			continue
		}
		if _, ok := FirstOverlap(cand, []netip.Prefix{r.Dst}); ok {
			routes = append(routes, Finding{Kind: KindHostRoute, Severity: SeverityWarning, Node: r.Node, Route: r.Dst.Masked().String(), Iface: r.Iface})
		}
	}

	slices.SortStableFunc(same, func(a, b Finding) int {
		return cmp.Or(strings.Compare(a.Network, b.Network), strings.Compare(a.Node, b.Node), strings.Compare(a.Subnet, b.Subnet))
	})
	slices.SortStableFunc(other, func(a, b Finding) int {
		return cmp.Or(strings.Compare(a.Environment, b.Environment), strings.Compare(a.Network, b.Network), strings.Compare(a.Node, b.Node), strings.Compare(a.Subnet, b.Subnet))
	})
	slices.SortStableFunc(routes, func(a, b Finding) int {
		return cmp.Or(comparePrefixText(a.Route, b.Route), strings.Compare(a.Node, b.Node), strings.Compare(a.Iface, b.Iface))
	})

	var out []Finding
	for _, list := range [][]Finding{same, other, routes} {
		out = append(out, list[:min(len(list), MaxFindingsPerKind)]...)
	}
	if f, ok := outsidePool(candidate, in.Pools); ok {
		out = append(out, f)
	}
	return out
}

// localNode is the node a network is on, empty for a swarm-wide network.
func localNode(n MapItemNamed) string {
	if n.Swarm {
		return ""
	}
	return n.Node
}

// comparePrefixText orders two prefixes given as text by address as a number and then by mask.
func comparePrefixText(a, b string) int {
	pa, errA := netip.ParsePrefix(a)
	pb, errB := netip.ParsePrefix(b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	return cmp.Or(pa.Addr().Compare(pb.Addr()), cmp.Compare(pa.Bits(), pb.Bits()))
}

// outsidePool is the finding for a candidate that lies in no pool. With no pool known there is
// nothing to be outside of.
func outsidePool(candidate netip.Prefix, pools []Pool) (Finding, bool) {
	if len(pools) == 0 {
		return Finding{}, false
	}
	names := make([]string, 0, len(pools))
	for _, p := range pools {
		base := p.Base.Masked()
		if isV4(base) && base.Contains(candidate.Addr()) && candidate.Bits() >= base.Bits() {
			return Finding{}, false
		}
		names = append(names, base.String())
	}
	return Finding{Kind: KindOutsidePool, Severity: SeverityWarning, Pool: strings.Join(names, ", ")}, true
}
