// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Interface is one network interface of the node with the addresses it holds.
type Interface struct {
	Name  string
	Addrs []netip.Prefix
}

// Lister returns the node's interfaces. It is a function so a test can feed fake ones.
type Lister func() ([]Interface, error)

// SystemInterfaces lists the interfaces of the host the process runs on (the helper runs in the host
// network namespace, D-09).
func SystemInterfaces() ([]Interface, error) {
	nics, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(nics))
	for i := range nics {
		addrs, err := nics[i].Addrs()
		if err != nil {
			return nil, err
		}
		ifc := Interface{Name: nics[i].Name}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ones, _ := ipn.Mask.Size()
			ifc.Addrs = append(ifc.Addrs, netip.PrefixFrom(ip.Unmap(), ones))
		}
		out = append(out, ifc)
	}
	return out, nil
}

// DiscoverError is why no single interface could be chosen. Reason is VIPReasonNoInterface or
// VIPReasonAmbiguousInterface, and Seen names the interfaces that were considered.
type DiscoverError struct {
	Reason string
	Seen   []string
}

func (e *DiscoverError) Error() string { return "no single interface: " + e.Reason }

// excludedNames and excludedPrefixes are the interfaces that are never the node's LAN interface (D-09):
// loopback, the engine's bridges and the overlay, tunnel and CNI devices.
var (
	excludedNames    = []string{"lo", "docker0", "docker_gwbridge"}
	excludedPrefixes = []string{"br-", "veth", "vx-", "vxlan", "ov-", "cali", "flannel", "cni", "tun", "tap"}
)

// reportable is a name the helper can render into keepalived.conf and write into an event line: at most 15
// characters from a set with no space, slash or control character. The kernel allows more, but such a name
// could not be reported, so it is treated as an excluded interface rather than chosen and then refused.
var reportable = regexp.MustCompile(`^[A-Za-z0-9._-]{1,15}$`)

// maxSeen is the most interface names a DiscoverError carries, the event line's bound.
const maxSeen = 8

func excluded(name string) bool {
	if !reportable.MatchString(name) {
		return true
	}
	if slices.Contains(excludedNames, name) {
		return true
	}
	for _, p := range excludedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Discover returns the one interface that has an IPv4 address inside p, after the excluded interfaces are
// dropped (D-09). None is a DiscoverError with VIPReasonNoInterface, more than one VIPReasonAmbiguousInterface.
func Discover(list Lister, p netip.Prefix) (string, error) {
	ifaces, err := list()
	if err != nil {
		return "", err
	}
	net4 := p.Masked()
	var seen, match []string
	for _, ifc := range ifaces {
		if excluded(ifc.Name) {
			continue
		}
		has4, in := false, false
		for _, a := range ifc.Addrs {
			if !a.Addr().Is4() {
				continue
			}
			has4 = true
			if net4.Contains(a.Addr()) {
				in = true
			}
		}
		if !has4 {
			continue
		}
		seen = append(seen, ifc.Name)
		if in {
			match = append(match, ifc.Name)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return "", &DiscoverError{Reason: ingressbundle.VIPReasonNoInterface, Seen: bounded(seen)}
	}
	return "", &DiscoverError{Reason: ingressbundle.VIPReasonAmbiguousInterface, Seen: bounded(match)}
}

// bounded sorts names and keeps the first maxSeen, so the same host always reports the same list.
func bounded(names []string) []string {
	slices.Sort(names)
	return names[:min(len(names), maxSeen)]
}
