// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"crypto/sha256"
	"encoding/binary"
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

// VIPMinPrefixLen is the shortest prefix length a virtual IP's network may have (the widest private range is a /8).
const VIPMinPrefixLen = 8

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
	// IsGlobalUnicast is false for loopback, link-local, multicast and unspecified addresses, and for the limited
	// broadcast address, which no node could hold either.
	if !a.IsGlobalUnicast() {
		return netip.Prefix{}, &Finding{Code: CodeVIPNotUnicast, Message: vipNotUnicastText(a), Severity: SeverityError}
	}
	if isNetworkOrBroadcast(p) {
		return netip.Prefix{}, &Finding{
			Code: CodeVIPNetworkOrBroadcast, Message: vipIsNetworkOrBroadcastText(a, p.Masked()), Severity: SeverityError,
		}
	}
	// No LAN is wider than a /8, and the table of virtual IPs holds only 1 to 32: a prefix outside that is refused here, so every
	// refusal comes before anything is created and a job never fails at the row for it.
	if p.Bits() < VIPMinPrefixLen {
		return netip.Prefix{}, &Finding{Code: CodeVIPAddressInvalid, Message: vipPrefixRangeText, Severity: SeverityError}
	}
	return p, nil
}

// isNetworkOrBroadcast reports whether the address of an IPv4 prefix is its network or its broadcast address. A /31
// and a /32 have neither (RFC 3021), so every address of them is a host.
func isNetworkOrBroadcast(p netip.Prefix) bool {
	if p.Bits() >= 31 {
		return false
	}
	a := p.Addr().As4()
	hostMask := ^uint32(0) >> uint(p.Bits())
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	host := v & hostMask
	return host == 0 || host == hostMask
}

// CheckVIPAddress runs the address checks of D-08 on a parsed address and returns every finding: the refusals first,
// then the warnings, with the "unverified" warning always last. No finding is built from an engine's text, only from
// the parsed values and the names it was given, cut to 64 characters.
func CheckVIPAddress(in VIPAddressInput) []Finding {
	var out []Finding
	if f := checkOutsideNodeSubnet(in); f != nil {
		out = append(out, *f)
	}
	if f := checkIsNodeAddress(in); f != nil {
		out = append(out, *f)
	}
	out = append(out, checkInUseByEnvironment(in)...)
	out = append(out, checkOverlapsDocker(in)...)
	out = append(out, checkInsidePool(in)...)
	return append(out, Finding{
		Code: CodeVIPUnverified, Message: vipUnverifiedText(in.Prefix.Addr()), Severity: SeverityWarning,
	})
}

// checkIsNodeAddress refuses an address a node already holds.
func checkIsNodeAddress(in VIPAddressInput) *Finding {
	addr := in.Prefix.Addr()
	var names []string
	for _, n := range sortedNodes(in.Nodes) {
		if n.Address.IsValid() && n.Address.Unmap() == addr {
			names = append(names, vipNodeLabel(n))
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &Finding{
		Code: CodeVIPIsNodeAddress, Message: vipIsNodeAddressText(addr, names), Items: cutAll(names), Severity: SeverityError,
	}
}

// checkInUseByEnvironment refuses an address that is another environment's virtual IP, one finding per environment.
func checkInUseByEnvironment(in VIPAddressInput) []Finding {
	addr := in.Prefix.Addr()
	others := slices.Clone(in.Others)
	slices.SortFunc(others, func(a, b VIPOther) int {
		if c := strings.Compare(a.EnvName, b.EnvName); c != 0 {
			return c
		}
		return strings.Compare(a.EnvID, b.EnvID)
	})
	var out []Finding
	for _, o := range others {
		if !o.Prefix.IsValid() || o.Prefix.Addr().Unmap() != addr {
			continue
		}
		out = append(out, Finding{
			Code: CodeVIPInUseByEnvironment, Message: vipInUseByEnvironmentText(addr, o.EnvName),
			Items: []string{vipName(o.EnvName)}, Severity: SeverityError,
		})
	}
	return out
}

// checkOverlapsDocker refuses a prefix that overlaps the subnet of an allocated network, one finding per network
// (A-11). An address pool or a host route does not refuse (checkInsidePool).
func checkOverlapsDocker(in VIPAddressInput) []Finding {
	network := in.Prefix.Masked()
	nets := slices.Clone(in.Allocated)
	slices.SortFunc(nets, func(a, b VIPNetwork) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return a.Subnet.Addr().Compare(b.Subnet.Addr())
	})
	var out []Finding
	for _, n := range nets {
		if !n.Subnet.IsValid() || !network.Overlaps(n.Subnet.Masked()) {
			continue
		}
		out = append(out, Finding{
			Code: CodeVIPOverlapsDocker, Message: vipOverlapsDockerText(network, n.Name),
			Items: []string{vipName(n.Name)}, Severity: SeverityError,
		})
	}
	return out
}

// checkInsidePool warns about each address pool or host route the prefix overlaps (A-11).
func checkInsidePool(in VIPAddressInput) []Finding {
	network := in.Prefix.Masked()
	pools := slices.Clone(in.Pools)
	slices.SortFunc(pools, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	var out []Finding
	for _, pool := range pools {
		if !pool.IsValid() || !network.Overlaps(pool.Masked()) {
			continue
		}
		out = append(out, Finding{
			Code: CodeVIPInsidePool, Message: vipInsidePoolText(network, pool.Masked()),
			Items: []string{pool.Masked().String()}, Severity: SeverityWarning,
		})
	}
	return out
}

// sortedNodes returns the nodes ordered by host name then ID, so findings read the same on every run.
func sortedNodes(nodes []VIPNode) []VIPNode {
	out := slices.Clone(nodes)
	slices.SortFunc(out, func(a, b VIPNode) int {
		if c := strings.Compare(a.Hostname, b.Hostname); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// checkOutsideNodeSubnet refuses an address that some node's address is not inside the prefix of. A node whose
// address the swarm did not report cannot be compared, so it is left to the nodes' own check when they start.
func checkOutsideNodeSubnet(in VIPAddressInput) *Finding {
	var outside []VIPNode
	for _, n := range sortedNodes(in.Nodes) {
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

// The bounds of a virtual router ID (VRRP carries it in one byte and 0 is reserved).
const (
	vipRouterIDMin = 1
	vipRouterIDMax = 255
)

// DerivedRouterID is the virtual router ID a swarm gets when the Admin gives none (D-05): 1 plus the first four
// bytes of the SHA-256 of the swarm cluster ID, read big-endian as an unsigned integer, modulo 255, so always 1 to 255.
//
// Costly to change: every swarm that runs without an override uses this value, so a different formula would move
// their router IDs on upgrade. TestDerivedRouterID pins the byte order with literal vectors.
func DerivedRouterID(clusterID string) int {
	sum := sha256.Sum256([]byte(clusterID))
	return vipRouterIDMin + int(binary.BigEndian.Uint32(sum[:4])%255)
}

// ResolveRouterID picks the router ID to use: the override when there is one and it is 1 to 255, else the value derived
// from the cluster ID (derived is then true). An override outside 1 to 255 is refused with vip_router_id_invalid.
func ResolveRouterID(override *int, clusterID string) (id int, derived bool, f *Finding) {
	if override == nil {
		return DerivedRouterID(clusterID), true, nil
	}
	if *override < vipRouterIDMin || *override > vipRouterIDMax {
		return 0, false, &Finding{Code: CodeVIPRouterIDInvalid, Message: vipRouterIDInvalidText, Severity: SeverityError}
	}
	return *override, false, nil
}

// CheckRouterIDCollision refuses a router ID that another environment of this install already uses on an overlapping
// network (D-06). self is the environment being configured, which never collides with itself. A collision on the wire
// with a router TierPlan does not manage cannot be refused in advance; the helper reports it later (A-04).
func CheckRouterIDCollision(self string, id int, prefix netip.Prefix, others []VIPOther) *Finding {
	if !prefix.IsValid() {
		return nil
	}
	network := prefix.Masked()
	sorted := slices.Clone(others)
	slices.SortFunc(sorted, func(a, b VIPOther) int {
		if c := strings.Compare(a.EnvName, b.EnvName); c != 0 {
			return c
		}
		return strings.Compare(a.EnvID, b.EnvID)
	})
	for _, o := range sorted {
		if o.EnvID == self || o.RouterID != id || !o.Prefix.IsValid() {
			continue
		}
		if !network.Overlaps(o.Prefix.Masked()) {
			continue
		}
		return &Finding{
			Code: CodeVIPRouterIDInUse, Message: vipRouterIDInUseText(o.EnvName, id),
			Items: []string{vipName(o.EnvName)}, Severity: SeverityError,
		}
	}
	return nil
}
