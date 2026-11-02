// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package hostroute

import (
	"os"
	"strings"
	"testing"
)

// FuzzParse holds the parser to its promise for any bytes at all: no panic, at
// most MaxRoutes routes, each a masked IPv4 prefix of 1 to 31 bits, each
// interface name at most MaxIfaceLen bytes of the allowed set.
func FuzzParse(f *testing.F) {
	fixture, err := os.ReadFile("testdata/route-host.txt")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(fixture)
	for _, s := range []string{
		"",
		"Iface\n",
		"Iface\neth0;rm -rf\t0000080A\t00000000\t0001\t0\t0\t0\t00FFFFFF\n",
		"Iface\neth0\t0000080A\t00000000\t0001\t0\t0\t0\t00FF00FF\n",
		"Iface\neth0\tFFFFFFFF\tFFFFFFFF\tFFFF\t0\t0\t0\tFFFFFFFE\n",
		"Iface\n\x00\xff\xfe\n",
		"Iface\n" + strings.Repeat("a", 600) + "\n",
		"Iface\r\neth0\t0000080A\t00000000\t0001\t0\t0\t0\t00FFFFFF",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		routes, _, err := Parse(strings.NewReader(string(data)))
		if err != nil {
			if routes != nil {
				t.Fatalf("routes returned with an error: %v", err)
			}
			return
		}
		if len(routes) > MaxRoutes {
			t.Fatalf("%d routes", len(routes))
		}
		for _, r := range routes {
			if !r.Dst.IsValid() || !r.Dst.Addr().Is4() || r.Dst.Bits() < 1 || r.Dst.Bits() > 31 || r.Dst != r.Dst.Masked() {
				t.Fatalf("route %+v is not a masked IPv4 prefix of 1 to 31 bits", r)
			}
			if r.Gateway.IsValid() && !r.Gateway.Is4() {
				t.Fatalf("gateway %v is not IPv4", r.Gateway)
			}
			if len(r.Iface) > MaxIfaceLen || SanitizeIface(r.Iface) != r.Iface {
				t.Fatalf("interface %q is not safe", r.Iface)
			}
		}
	})
}
