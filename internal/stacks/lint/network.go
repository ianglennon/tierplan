// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package lint

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// NetInfo is one network of the environment, as far as the subnet rules need it: its name, the node it was read
// from and its IPv4 subnets. It is plain data; the caller reads it from wherever it likes.
type NetInfo struct {
	Name    string
	Node    string
	Subnets []netip.Prefix
}

// NetEnv is what the environment looks like to the subnet rules: the networks it already has, and the bases of its
// address pools. A pool list that is empty says nothing is known about the pools, so no subnet is outside them.
type NetEnv struct {
	Networks []NetInfo
	Pools    []netip.Prefix
}

// networkSubnetRules warns about an explicit subnet of a stack network that overlaps a network the environment
// already has, or lies outside every address pool of the environment (D-19). Both are warnings: a deploy may still
// do what the person means. Without an environment (Env nil) there is nothing to compare with and nothing is
// reported. An external network, a network without ipam, an IPv6 subnet and a subnet that does not parse are
// left alone; the loader reports the invalid ones.
func networkSubnetRules(in Input) []Finding {
	if in.Env == nil {
		return nil
	}
	names := make([]string, 0, len(in.Project.Networks))
	for name := range in.Project.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Finding
	for _, name := range names {
		nw := in.Project.Networks[name]
		if nw.External {
			continue
		}
		for i, pool := range nw.Ipam.Config {
			if pool == nil {
				continue
			}
			subnet, err := netip.ParsePrefix(strings.TrimSpace(pool.Subnet))
			if err != nil || !subnet.Addr().Is4() {
				continue
			}
			subnet = subnet.Masked()
			base := "networks." + name
			pos, key := in.at(base+".ipam.config."+strconv.Itoa(i)+".subnet", base)
			if other, otherSubnet, ok := in.Env.overlap(subnet); ok {
				out = append(out, Finding{
					Severity: SeverityWarning, Code: CodeNetworkSubnetOverlap, Key: key, Pos: pos,
					Message: fmt.Sprintf(messages[CodeNetworkSubnetOverlap], name, subnet, other, otherSubnet),
				})
			}
			if len(in.Env.Pools) > 0 && !in.Env.inPool(subnet) {
				out = append(out, Finding{
					Severity: SeverityWarning, Code: CodeNetworkSubnetOutsidePool, Key: key, Pos: pos,
					Message: fmt.Sprintf(messages[CodeNetworkSubnetOutsidePool], name, subnet, in.Env.poolList()),
				})
			}
		}
	}
	return out
}

// overlap is the first network of the environment that has a subnet overlapping s, with that subnet.
func (e *NetEnv) overlap(s netip.Prefix) (string, netip.Prefix, bool) {
	for _, n := range e.Networks {
		for _, sub := range n.Subnets {
			if sub.IsValid() && sub.Overlaps(s) {
				return n.Name, sub.Masked(), true
			}
		}
	}
	return "", netip.Prefix{}, false
}

// inPool reports whether the whole of s lies inside one pool base: its first and its last address are both in it.
func (e *NetEnv) inPool(s netip.Prefix) bool {
	for _, p := range e.Pools {
		if p.IsValid() && p.Contains(s.Addr()) && s.Bits() >= p.Bits() {
			return true
		}
	}
	return false
}

// poolList is the pool bases, comma-separated, in the order the environment lists them.
func (e *NetEnv) poolList() string {
	parts := make([]string, 0, len(e.Pools))
	for _, p := range e.Pools {
		parts = append(parts, p.Masked().String())
	}
	return strings.Join(parts, ", ")
}
