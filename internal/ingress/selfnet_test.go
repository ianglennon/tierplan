// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"net/netip"
	"slices"
	"testing"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func adr(s string) netip.Addr   { return netip.MustParseAddr(s) }

// coverage counts, for each address of base, how many prefixes of ps hold it; an address no prefix
// holds is absent from the map.
func coverage(t *testing.T, ps []netip.Prefix, base netip.Prefix) (held map[netip.Addr]int) {
	t.Helper()
	held = map[netip.Addr]int{}
	for a := base.Addr(); base.Contains(a); a = a.Next() {
		for _, p := range ps {
			if p.Contains(a) {
				held[a]++
			}
		}
	}
	return held
}

func TestSubtractAddr(t *testing.T) {
	t.Parallel()
	base := pfx("10.9.0.0/24")
	gw := adr("10.9.0.1")

	got := SubtractAddr(base, gw)
	if len(got) != 8 {
		t.Fatalf("10.9.0.0/24 minus a host is %d prefixes (%v), want 8", len(got), got)
	}
	held := coverage(t, got, base)
	if _, in := held[gw]; in {
		t.Errorf("the subtracted address %s is still inside %v", gw, got)
	}
	if len(held) != 255 {
		t.Errorf("the result covers %d addresses, want 255", len(held))
	}
	for a, n := range held {
		if n != 1 {
			t.Errorf("%s is held by %d prefixes, want 1", a, n)
		}
	}
	if !slices.IsSortedFunc(got, func(x, y netip.Prefix) int { return x.Addr().Compare(y.Addr()) }) {
		t.Errorf("the result is not in address order: %v", got)
	}

	t.Run("an address outside the prefix leaves it unchanged", func(t *testing.T) {
		t.Parallel()
		if out := SubtractAddr(base, adr("10.9.1.1")); !slices.Equal(out, []netip.Prefix{base}) {
			t.Errorf("got %v, want %v unchanged", out, base)
		}
	})
	t.Run("the first and the last address of the prefix", func(t *testing.T) {
		t.Parallel()
		for _, a := range []netip.Addr{adr("10.9.0.0"), adr("10.9.0.255")} {
			out := SubtractAddr(base, a)
			held := coverage(t, out, base)
			if _, in := held[a]; in || len(held) != 255 {
				t.Errorf("minus %s: %d addresses, subtracted one still held = %v", a, len(held), in)
			}
		}
	})
	t.Run("a prefix that is the address itself leaves nothing", func(t *testing.T) {
		t.Parallel()
		if out := SubtractAddr(pfx("10.9.0.1/32"), gw); len(out) != 0 {
			t.Errorf("got %v, want none", out)
		}
	})
	t.Run("an IPv6 prefix", func(t *testing.T) {
		t.Parallel()
		out := SubtractAddr(pfx("fd00::/120"), adr("fd00::1"))
		if len(out) != 8 {
			t.Fatalf("got %d prefixes (%v), want 8", len(out), out)
		}
		for _, p := range out {
			if p.Contains(adr("fd00::1")) {
				t.Errorf("%v still holds fd00::1", p)
			}
		}
	})
}

func TestSelfTrustedPrefixesNeverList(t *testing.T) {
	t.Parallel()
	t.Run("a subnet without its gateway", func(t *testing.T) {
		t.Parallel()
		got := SelfTrustedPrefixes(SelfNetFacts{Subnet: pfx("10.9.0.0/24"), Gateway: adr("10.9.0.1")})
		if len(got) != 8 {
			t.Fatalf("got %d prefixes (%v), want 8", len(got), got)
		}
		held := coverage(t, got, pfx("10.9.0.0/24"))
		if _, in := held[adr("10.9.0.1")]; in || len(held) != 255 {
			t.Errorf("covers %d addresses, gateway held = %v; want 255 and false", len(held), in)
		}
	})
	t.Run("an overlap with the ingress overlay gives nothing", func(t *testing.T) {
		t.Parallel()
		got := SelfTrustedPrefixes(SelfNetFacts{
			Subnet: pfx("10.9.0.0/24"), Gateway: adr("10.9.0.1"),
			IngressSubnets: []netip.Prefix{pfx("10.9.0.0/16")},
		})
		if len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})
	t.Run("an ingress subnet that does not overlap changes nothing", func(t *testing.T) {
		t.Parallel()
		got := SelfTrustedPrefixes(SelfNetFacts{
			Subnet: pfx("10.9.0.0/24"), Gateway: adr("10.9.0.1"),
			IngressSubnets: []netip.Prefix{pfx("10.0.1.0/24")},
		})
		if len(got) != 8 {
			t.Errorf("got %d prefixes, want 8", len(got))
		}
	})
	t.Run("the default bridge gateway is never trusted", func(t *testing.T) {
		t.Parallel()
		for _, gw := range []string{"172.17.0.1", "172.18.0.1"} {
			got := SelfTrustedPrefixes(SelfNetFacts{Subnet: pfx("172.16.0.0/12"), Gateway: adr(gw)})
			for _, p := range got {
				if p.Contains(adr("172.17.0.1")) || p.Contains(adr("172.18.0.1")) || p.Contains(adr(gw)) {
					t.Errorf("gateway %s: %v holds a bridge gateway (%v)", gw, p, got)
				}
			}
			if len(got) == 0 {
				t.Errorf("gateway %s: nothing is left of the subnet", gw)
			}
		}
		got := SelfTrustedPrefixes(SelfNetFacts{Subnet: pfx("172.17.0.0/16"), Gateway: adr("172.17.0.1")})
		for _, p := range got {
			if p.Contains(adr("172.17.0.1")) {
				t.Errorf("%v holds 172.17.0.1", p)
			}
		}
	})
	t.Run("a bridge gateway the caller names is removed too", func(t *testing.T) {
		t.Parallel()
		got := SelfTrustedPrefixes(SelfNetFacts{
			Subnet: pfx("10.9.0.0/24"), Gateway: adr("10.9.0.1"),
			BridgeGateways: []netip.Addr{adr("10.9.0.200")},
		})
		held := coverage(t, got, pfx("10.9.0.0/24"))
		if _, in := held[adr("10.9.0.200")]; in {
			t.Errorf("10.9.0.200 is still held in %v", got)
		}
		if _, in := held[adr("10.9.0.1")]; in {
			t.Errorf("the gateway is still held in %v", got)
		}
		if len(held) != 254 {
			t.Errorf("covers %d addresses, want 254", len(held))
		}
	})
	t.Run("an invalid subnet gives nothing", func(t *testing.T) {
		t.Parallel()
		if got := SelfTrustedPrefixes(SelfNetFacts{Gateway: adr("10.9.0.1")}); len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})
	t.Run("the result is in address order", func(t *testing.T) {
		t.Parallel()
		got := SelfTrustedPrefixes(SelfNetFacts{Subnet: pfx("10.9.0.0/24"), Gateway: adr("10.9.0.100"), BridgeGateways: []netip.Addr{adr("10.9.0.5")}})
		if !slices.IsSortedFunc(got, func(x, y netip.Prefix) int { return x.Addr().Compare(y.Addr()) }) {
			t.Errorf("not in address order: %v", got)
		}
	})
}
