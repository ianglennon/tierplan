// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"net/netip"
	"slices"
	"strings"
)

// The codes of the virtual IP input checks (CONTEXT D-05 to D-08, A-11; UI-SPEC S5). The preview, the enable and the
// change jobs all run these checks, so each code means the same thing on every surface.
const (
	// CodeVIPAddressInvalid is the refusal of a value that is not an address with its prefix length.
	CodeVIPAddressInvalid = "vip_address_invalid"
	// CodeVIPIPv6Unsupported is the refusal of an IPv6 address (D-07: not supported yet).
	CodeVIPIPv6Unsupported = "vip_ipv6_unsupported"
	// CodeVIPNotUnicast is the refusal of a loopback, link-local, multicast or unspecified address.
	CodeVIPNotUnicast = "vip_not_unicast"
	// CodeVIPNetworkOrBroadcast is the refusal of the network or broadcast address of the prefix.
	CodeVIPNetworkOrBroadcast = "vip_is_network_or_broadcast"
	// CodeVIPOutsideNodeSubnet is the refusal of an address that is not inside the network of every node.
	CodeVIPOutsideNodeSubnet = "vip_outside_node_subnet"
	// CodeVIPIsNodeAddress is the refusal of an address a node already holds.
	CodeVIPIsNodeAddress = "vip_is_node_address"
	// CodeVIPInUseByEnvironment is the refusal of an address that is another environment's virtual IP.
	CodeVIPInUseByEnvironment = "vip_in_use_by_environment"
	// CodeVIPOverlapsDocker is the refusal of a prefix that overlaps an allocated network's subnet (A-11).
	CodeVIPOverlapsDocker = "vip_overlaps_docker"
	// CodeVIPInsidePool is the warning for a prefix that overlaps a range Docker may hand to networks, an address
	// pool or a host route (A-11). It never refuses, because home networks commonly sit inside Docker's default pools.
	CodeVIPInsidePool = "vip_inside_pool"
	// CodeVIPUnverified is the warning, always present, that the server cannot prove the address is unused. The nodes
	// check when they start.
	CodeVIPUnverified = "vip_unverified"
	// CodeVIPRouterIDInvalid is the refusal of a router ID outside 1 to 255.
	CodeVIPRouterIDInvalid = "vip_router_id_invalid"
	// CodeVIPRouterIDInUse is the refusal of a router ID another environment of this install already uses on an
	// overlapping network (D-06).
	CodeVIPRouterIDInUse = "vip_router_id_in_use"
)

// VIPNode is a swarm node as the address checks see it.
type VIPNode struct {
	ID       string
	Hostname string
	// Address is the node's own address (Status.Addr). It is the zero Addr when the swarm did not report one.
	Address netip.Addr
}

// VIPOther is another environment of this install that has a virtual IP.
type VIPOther struct {
	EnvID   string
	EnvName string
	// Prefix is that environment's virtual IP with its prefix length, the host address kept.
	Prefix   netip.Prefix
	RouterID int
}

// VIPNetwork is an allocated network of the swarm: an overlay, a bridge or the ingress network.
type VIPNetwork struct {
	Name   string
	Subnet netip.Prefix
}

// VIPAddressInput is what CheckVIPAddress reads. Every value is already parsed.
type VIPAddressInput struct {
	// Prefix is the address typed, with its prefix length, the host address kept (ParseVIPAddress).
	Prefix netip.Prefix
	// Nodes are all the swarm's nodes.
	Nodes []VIPNode
	// Others are the other environments' virtual IPs. The environment being configured is not among them.
	Others []VIPOther
	// Allocated are the subnets of the networks that exist on the swarm. Only an overlap with these refuses (A-11).
	Allocated []VIPNetwork
	// Pools are the address pools and host routes. An overlap with these only warns (A-11).
	Pools []netip.Prefix
}

// ParseVIPAddress reads the typed address with its prefix length, for example 192.168.1.50/24 (D-07). The prefix is
// required and only IPv4 unicast is accepted. The returned Prefix keeps the host address. A refusal comes back as a
// Finding and the Prefix is then the zero value.
func ParseVIPAddress(raw string) (netip.Prefix, *Finding) {
	p, err := netip.ParsePrefix(strings.TrimSpace(raw))
	if err != nil {
		return netip.Prefix{}, &Finding{Code: CodeVIPAddressInvalid, Message: vipAddressInvalidText, Severity: SeverityError}
	}
	a := p.Addr()
	if !a.Is4() {
		return netip.Prefix{}, &Finding{Code: CodeVIPIPv6Unsupported, Message: vipIPv6UnsupportedText, Severity: SeverityError}
	}
	return p, nil
}

// CheckVIPAddress runs the address checks of D-08 on a parsed address and returns every finding: the refusals first,
// then the warnings, with the "unverified" warning always last. No finding is built from an engine's text, only from
// the parsed values and the names it was given, cut to 64 characters.
func CheckVIPAddress(in VIPAddressInput) []Finding {
	var out []Finding
	if f := checkOutsideNodeSubnet(in); f != nil {
		out = append(out, *f)
	}
	return out
}

// checkOutsideNodeSubnet refuses an address that some node's address is not inside the prefix of. A node whose
// address the swarm did not report cannot be compared, so it is left to the nodes' own check when they start.
func checkOutsideNodeSubnet(in VIPAddressInput) *Finding {
	var outside []VIPNode
	for _, n := range in.Nodes {
		if !n.Address.IsValid() {
			continue
		}
		if !in.Prefix.Contains(n.Address.Unmap()) {
			outside = append(outside, n)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	slices.SortFunc(outside, func(a, b VIPNode) int {
		if c := strings.Compare(a.Hostname, b.Hostname); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	names := make([]string, len(outside))
	for i, n := range outside {
		names[i] = vipNodeLabel(n)
	}
	return &Finding{
		Code:     CodeVIPOutsideNodeSubnet,
		Message:  vipOutsideNodeSubnetText(in.Prefix.Addr(), names),
		Items:    cutAll(names),
		Severity: SeverityError,
	}
}

// vipNodeLabel is the name a node is quoted by: its host name, or its ID when the swarm gave none.
func vipNodeLabel(n VIPNode) string {
	if n.Hostname != "" {
		return n.Hostname
	}
	return n.ID
}

// cutAll cuts every name to the quoted length.
func cutAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = vipName(n)
	}
	return out
}
