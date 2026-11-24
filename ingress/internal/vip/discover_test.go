// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

func iface(name string, addrs ...string) Interface {
	i := Interface{Name: name}
	for _, a := range addrs {
		i.Addrs = append(i.Addrs, netip.MustParsePrefix(a))
	}
	return i
}

func lister(ifs ...Interface) Lister {
	return func() ([]Interface, error) { return ifs, nil }
}

// noise is every interface name D-09 excludes, each holding an address inside 192.168.1.0/24.
func noise() []Interface {
	var out []Interface
	for i, n := range []string{"lo", "docker0", "docker_gwbridge", "br-1a2b", "veth9", "vxlan0", "vx-001", "ov-x", "cali1", "flannel.1", "cni0", "tun0", "tap0"} {
		out = append(out, iface(n, fmt.Sprintf("192.168.1.%d/24", 100+i)))
	}
	return out
}

func TestDiscover(t *testing.T) {
	vip := netip.MustParsePrefix("192.168.1.50/24")
	tests := []struct {
		name       string
		ifs        []Interface
		want       string
		wantReason string
		wantSeen   []string
	}{
		{name: "excluded names are dropped and the one LAN interface wins",
			ifs: append(noise(), iface("eth0", "192.168.1.10/24")), want: "eth0"},
		{name: "an address outside the prefix is no_interface and the interface is seen",
			ifs: []Interface{iface("eth0", "10.0.0.5/24")}, wantReason: ingressbundle.VIPReasonNoInterface, wantSeen: []string{"eth0"}},
		{name: "two interfaces in the prefix are ambiguous and sorted",
			ifs:        []Interface{iface("eth0", "192.168.1.10/24"), iface("ens18", "192.168.1.11/24")},
			wantReason: ingressbundle.VIPReasonAmbiguousInterface, wantSeen: []string{"ens18", "eth0"}},
		{name: "an IPv6-only interface is skipped",
			ifs: []Interface{iface("eth1", "fd00::1/64"), iface("eth0", "192.168.1.10/24")}, want: "eth0"},
		{name: "an IPv6-only host has nothing to choose and sees nothing",
			ifs: []Interface{iface("eth1", "fd00::1/64")}, wantReason: ingressbundle.VIPReasonNoInterface, wantSeen: nil},
		{name: "an interface with an IPv6 and an IPv4 address counts by its IPv4 one",
			ifs: []Interface{iface("eth0", "fd00::1/64", "192.168.1.10/24")}, want: "eth0"},
		{name: "the VIP already on the interface does not change the choice",
			ifs: []Interface{iface("eth0", "192.168.1.10/24", "192.168.1.50/32")}, want: "eth0"},
		{name: "a host-bits prefix still names its network",
			ifs: []Interface{iface("eth0", "192.168.1.200/24")}, want: "eth0"},
		{name: "an interface name outside the reportable set is never chosen",
			ifs: []Interface{iface("eth0 x", "192.168.1.10/24")}, wantReason: ingressbundle.VIPReasonNoInterface, wantSeen: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Discover(lister(tc.ifs...), vip)
			if tc.want != "" {
				if err != nil || got != tc.want {
					t.Fatalf("Discover = %q, %v; want %q", got, err, tc.want)
				}
				return
			}
			var de *DiscoverError
			if !errors.As(err, &de) {
				t.Fatalf("Discover = %q, %v; want a DiscoverError", got, err)
			}
			if de.Reason != tc.wantReason || !slices.Equal(de.Seen, tc.wantSeen) {
				t.Errorf("DiscoverError = {%s %v}; want {%s %v}", de.Reason, de.Seen, tc.wantReason, tc.wantSeen)
			}
		})
	}

	t.Run("the names seen are capped at eight and sorted", func(t *testing.T) {
		var ifs []Interface
		for _, n := range []string{"eth9", "eth3", "eth7", "eth1", "eth8", "eth2", "eth6", "eth0", "eth5", "eth4"} {
			ifs = append(ifs, iface(n, "10.0.0.5/24"))
		}
		_, err := Discover(lister(ifs...), vip)
		var de *DiscoverError
		if !errors.As(err, &de) {
			t.Fatalf("err = %v", err)
		}
		want := []string{"eth0", "eth1", "eth2", "eth3", "eth4", "eth5", "eth6", "eth7"}
		if !slices.Equal(de.Seen, want) {
			t.Errorf("Seen = %v, want %v", de.Seen, want)
		}
	})

	t.Run("a failing lister is returned and is not a DiscoverError", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := Discover(func() ([]Interface, error) { return nil, boom }, vip)
		var de *DiscoverError
		if !errors.Is(err, boom) || errors.As(err, &de) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestSystemInterfaces(t *testing.T) {
	ifs, err := SystemInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifs {
		if i.Name == "" {
			t.Error("an interface without a name")
		}
		for _, a := range i.Addrs {
			if a.Addr().Is4In6() {
				t.Errorf("%s holds a mapped address %v; they are unmapped", i.Name, a)
			}
		}
	}
}
