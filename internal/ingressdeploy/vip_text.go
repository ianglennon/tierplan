// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// VIPPrivilegeWarning is the fixed text the virtual IP preview always carries, not only on a warning (CONTEXT D-20,
// UI-SPEC S5). The same words go into the docs. Nothing in it is built from a value.
const VIPPrivilegeWarning = "The virtual IP needs `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` on every manager's agent. " +
	"That setting lets TierPlan run privileged, host-network services on every node of this swarm, " +
	"so a compromised TierPlan could take over every node. " +
	"VRRP authentication only keeps honest neighbours apart; anything on this network segment can still contend for the address."

// vipNameLimit is the longest a node, environment or network name is quoted back (UI-SPEC S5).
const vipNameLimit = 64

// The sentences of UI-SPEC S5 that carry no value.
const (
	vipAddressInvalidText   = "Give the address with its prefix length, for example 192.168.1.50/24."
	vipPrefixRangeText      = "The prefix length must be from 8 to 32, for example 192.168.1.50/24."
	vipIPv6UnsupportedText  = "IPv6 virtual addresses aren't supported yet. Use an IPv4 address."
	vipRouterIDInvalidText  = "The router ID must be a whole number from 1 to 255."
	vipNotUnicastTextSuffix = " can't be a virtual IP. Loopback, link-local, multicast and unspecified addresses aren't allowed."
)

// vipName makes a name safe to quote in a sentence: control characters are dropped and it is cut to 64 characters.
// A name comes from an engine or a database row, so it is never trusted to be short or plain.
func vipName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	runes := []rune(s)
	if len(runes) > vipNameLimit {
		runes = runes[:vipNameLimit]
	}
	return string(runes)
}

// vipNames cuts every name and joins them for a sentence.
func vipNames(names []string) string {
	cut := make([]string, len(names))
	for i, n := range names {
		cut[i] = vipName(n)
	}
	return strings.Join(cut, ", ")
}

// vipOutsideNodeSubnetText is the vip_outside_node_subnet sentence.
func vipOutsideNodeSubnetText(addr netip.Addr, nodes []string) string {
	return addr.String() + " isn't inside the network of every node. These nodes are outside it: " + vipNames(nodes) + "."
}

// vipIsNodeAddressText is the vip_is_node_address sentence.
func vipIsNodeAddressText(addr netip.Addr, nodes []string) string {
	return addr.String() + " is already the address of " + vipNames(nodes) + ". Choose an address no machine uses."
}

// vipIsNetworkOrBroadcastText is the vip_is_network_or_broadcast sentence. network is the masked prefix.
func vipIsNetworkOrBroadcastText(addr netip.Addr, network netip.Prefix) string {
	return addr.String() + " is the network or broadcast address of " + network.String() + ". Choose another."
}

// vipNotUnicastText is the vip_not_unicast sentence.
func vipNotUnicastText(addr netip.Addr) string {
	return addr.String() + vipNotUnicastTextSuffix
}

// vipInUseByEnvironmentText is the vip_in_use_by_environment sentence.
func vipInUseByEnvironmentText(addr netip.Addr, env string) string {
	return addr.String() + " is already the virtual IP of " + vipName(env) + "."
}

// vipOverlapsDockerText is the vip_overlaps_docker sentence. network is the masked prefix.
func vipOverlapsDockerText(network netip.Prefix, name string) string {
	return network.String() + " overlaps the network " + vipName(name) + " on this swarm. Choose an address outside Docker's networks."
}

// vipRouterIDInUseText is the vip_router_id_in_use sentence.
func vipRouterIDInUseText(env string, id int) string {
	return vipName(env) + " already uses router ID " + strconv.Itoa(id) + " on an overlapping network. Set a different router ID."
}

// vipInsidePoolText is the sentence of the warning for an address range Docker may hand to networks (CONTEXT A-11).
// network is the masked prefix, pool the range it overlaps.
func vipInsidePoolText(network, pool netip.Prefix) string {
	return network.String() + " sits inside a range Docker may hand to networks (" + pool.String() +
		"). A network created later could take this address range."
}

// vipUnverifiedText is the line that says the server cannot tell whether the address is unused (CONTEXT D-08). It is a
// warning and never a refusal.
func vipUnverifiedText(addr netip.Addr) string {
	return "TierPlan can't check from here that " + addr.String() +
		" is unused. Each node checks when it starts and reports it if another machine answers."
}

// VIPLocalSocketManagerText is the preview line for a manager reached through TierPlan's own Docker socket, which
// counts as allowing privileged services (CONTEXT D-19, UI-SPEC S5). The name is cut to 64 characters.
func VIPLocalSocketManagerText(node string) string {
	return vipName(node) + " is reached through TierPlan's own Docker socket, which already gives TierPlan root there, so it counts as allowed."
}
