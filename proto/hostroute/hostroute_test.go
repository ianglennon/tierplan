// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package hostroute

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
)

const header = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

// hexOf is the text the kernel prints for an address on this host: the four
// bytes read as a number in the host's byte order.
func hexOf(a netip.Addr) string {
	b := a.As4()
	return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(b[:]))
}

func maskOf(bits int) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], ^uint32(0)<<(32-bits))
	return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(b[:]))
}

func line(iface string, dst netip.Addr, bits int) string {
	return fmt.Sprintf("%s\t%s\t00000000\t0001\t0\t0\t0\t%s\t0\t0\t0\n", iface, hexOf(dst), maskOf(bits))
}

func TestParseTheFixture(t *testing.T) {
	f, err := os.Open("testdata/route-host.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	routes, truncated, err := Parse(f)
	if err != nil || truncated {
		t.Fatalf("err %v truncated %v", err, truncated)
	}
	var got []string
	for _, r := range routes {
		got = append(got, r.Iface+" "+r.Dst.String())
	}
	want := "eth0 192.168.2.0/24,docker0 172.17.0.0/16,wg0 10.8.0.0/24"
	if strings.Join(got, ",") != want {
		t.Fatalf("routes = %v, want %s", got, want)
	}
}

func TestParseKeepsTheGatewayOfAGatewayedRoute(t *testing.T) {
	gw := netip.MustParseAddr("192.168.2.1")
	in := header + fmt.Sprintf("eth0\t%s\t%s\t0003\t0\t0\t0\t%s\t0\t0\t0\n", hexOf(netip.MustParseAddr("10.9.0.0")), hexOf(gw), maskOf(16))
	routes, _, err := Parse(strings.NewReader(in))
	if err != nil || len(routes) != 1 || routes[0].Gateway != gw || routes[0].Dst != netip.MustParsePrefix("10.9.0.0/16") {
		t.Fatalf("routes = %+v err %v", routes, err)
	}
}

func TestParseMasksTheDestination(t *testing.T) {
	// A destination with host bits set under a /24 mask is the network, not the host.
	in := header + line("eth0", netip.MustParseAddr("10.1.2.3"), 24)
	routes, _, err := Parse(strings.NewReader(in))
	if err != nil || len(routes) != 1 || routes[0].Dst != netip.MustParsePrefix("10.1.2.0/24") {
		t.Fatalf("routes = %+v err %v", routes, err)
	}
}

func TestParseCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString(header)
	for i := 0; i < 600; i++ {
		b.WriteString(line("eth0", netip.AddrFrom4([4]byte{10, byte(i >> 8 & 0xff), byte(i & 0xff), 0}), 24))
	}
	routes, truncated, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != MaxRoutes || !truncated {
		t.Fatalf("%d routes, truncated %v; want %d and true", len(routes), truncated, MaxRoutes)
	}
	// Exactly the limit is not truncated.
	b.Reset()
	b.WriteString(header)
	for i := 0; i < MaxRoutes; i++ {
		b.WriteString(line("eth0", netip.AddrFrom4([4]byte{10, byte(i >> 8 & 0xff), byte(i & 0xff), 0}), 24))
	}
	routes, truncated, _ = Parse(strings.NewReader(b.String()))
	if len(routes) != MaxRoutes || truncated {
		t.Fatalf("%d routes, truncated %v at exactly the limit", len(routes), truncated)
	}
}

func TestParseDedupesTheSameInterfaceAndNetwork(t *testing.T) {
	l := line("eth0", netip.MustParseAddr("10.0.0.0"), 8)
	routes, truncated, err := Parse(strings.NewReader(header + l + l + line("eth1", netip.MustParseAddr("10.0.0.0"), 8)))
	if err != nil || truncated || len(routes) != 2 {
		t.Fatalf("routes = %+v truncated %v err %v", routes, truncated, err)
	}
}

func TestParseHostileInput(t *testing.T) {
	cases := map[string]string{
		"eth0;rm -rf":                  "eth0rm-rf",
		"eth0\x00\x1b[31m":             "eth031m",
		"a\xffb\xc3":                   "ab",
		"0123456789abcdefXYZ":          "0123456789abcde",
		"wg0@if3":                      "wg0@if3",
		"br-1a2b3c4d5e6f.7":            "br-1a2b3c4d5e6f",
		"éth0":                         "th0",
		"<script>alert(1)</script>":    "scriptalert1scr",
		"":                             "",
		"..":                           "..",
		"x" + strings.Repeat("y", 100): "xyyyyyyyyyyyyyy",
	}
	for in, want := range cases {
		if got := SanitizeIface(in); got != want {
			t.Errorf("SanitizeIface(%q) = %q, want %q", in, got, want)
		}
	}
	// A hostile name inside a line comes out clean and bounded.
	dst := netip.MustParseAddr("10.0.0.0")
	in := header + fmt.Sprintf("eth0;rm\t%s\t00000000\t0001\t0\t0\t0\t%s\t0\t0\t0\n", hexOf(dst), maskOf(8))
	routes, _, err := Parse(strings.NewReader(in))
	if err != nil || len(routes) != 1 || routes[0].Iface != "eth0rm" {
		t.Fatalf("routes = %+v err %v", routes, err)
	}
}

func TestParseSkipsMalformed(t *testing.T) {
	d := hexOf(netip.MustParseAddr("10.0.0.0"))
	good := line("ok0", netip.MustParseAddr("10.7.0.0"), 16)
	bad := []string{
		"eth0\tXYZ\t00000000\t0001\t0\t0\t0\t" + maskOf(8) + "\n",           // not hex
		"eth0\t" + d[:6] + "\t00000000\t0001\t0\t0\t0\t" + maskOf(8) + "\n", // 6 digits
		"eth0\t" + d + "0\t00000000\t0001\t0\t0\t0\t" + maskOf(8) + "\n",    // 9 digits
		"eth0\t" + d + "\tzzzzzzzz\t0001\t0\t0\t0\t" + maskOf(8) + "\n",     // gateway not hex
		"eth0\t" + d + "\t00000000\tQ\t0\t0\t0\t" + maskOf(8) + "\n",        // flags not hex
		"eth0\t" + d + "\t00000000\t0001\t0\t0\t0\tFFFFFFZZ\n",              // mask not hex
		"eth0\t" + d + "\t00000000\t0001\t0\t0\t0\t00FF00FF\n",              // non-contiguous mask
		"eth0\t" + d + "\t00000000\t0001\t0\t0\t0\t" + maskOf(32) + "\n",    // host route
		"eth0\t" + d + "\t00000000\t0001\t0\t0\t0\t00000000\n",              // default
		"eth0\t" + d + "\t00000000\t0000\t0\t0\t0\t" + maskOf(8) + "\n",     // down
		"eth0\t" + d + "\t00000000\n",                                       // 3 fields
		"\n",                                                                // empty
		"   \t  \n",                                                         // blanks
	}
	routes, _, err := Parse(strings.NewReader(header + strings.Join(bad, "") + good))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Iface != "ok0" {
		t.Fatalf("routes = %+v, want only ok0", routes)
	}
}

func TestParseAcceptsALastLineWithoutALineBreakAndCRLF(t *testing.T) {
	l := strings.TrimSuffix(line("eth0", netip.MustParseAddr("10.5.0.0"), 16), "\n")
	routes, _, err := Parse(strings.NewReader(strings.ReplaceAll(header, "\n", "\r\n") + l))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes = %+v err %v", routes, err)
	}
}

func TestParseTooLarge(t *testing.T) {
	long := header + line("eth0", netip.MustParseAddr("10.0.0.0"), 8) + "eth0 " + strings.Repeat("0", 600) + "\n"
	if routes, _, err := Parse(strings.NewReader(long)); !errors.Is(err, ErrTooLarge) || routes != nil {
		t.Fatalf("a 600 byte line: routes %v err %v", routes, err)
	}
	huge := header + "eth0 " + strings.Repeat("0", 5000) // no line break within the buffer
	if _, _, err := Parse(strings.NewReader(huge)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a 5000 byte line: err %v", err)
	}
	edge := header + strings.Repeat("x", MaxLine) + "\n"
	if _, _, err := Parse(strings.NewReader(edge)); err != nil {
		t.Fatalf("a line of exactly MaxLine bytes: %v", err)
	}
	over := header + strings.Repeat("x", MaxLine+1) + "\n"
	if _, _, err := Parse(strings.NewReader(over)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a line of MaxLine+1 bytes: %v", err)
	}
	var b strings.Builder
	b.WriteString(header)
	for b.Len() < 300<<10 {
		b.WriteString(line("eth0", netip.MustParseAddr("10.0.0.0"), 8)) // short, valid lines
	}
	if routes, _, err := Parse(strings.NewReader(b.String())); !errors.Is(err, ErrTooLarge) || routes != nil {
		t.Fatalf("a 300 KiB file: routes %d err %v", len(routes), err)
	}
}
