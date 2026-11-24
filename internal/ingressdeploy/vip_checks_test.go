// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"math/rand/v2"
	"net/netip"
	"strconv"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// vipNode is a node of the address checks.
func vipNode(id, host, addr string) ingressdeploy.VIPNode {
	return ingressdeploy.VIPNode{ID: id, Hostname: host, Address: netip.MustParseAddr(addr)}
}

// parseVIP parses an address that must pass.
func parseVIP(t *testing.T, raw string) netip.Prefix {
	t.Helper()
	p, f := ingressdeploy.ParseVIPAddress(raw)
	if f != nil {
		t.Fatalf("ParseVIPAddress(%q) refused with %s", raw, f.Code)
	}
	return p
}

// findingCodes lists the codes of findings in order.
func findingCodes(fs []ingressdeploy.Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Code
	}
	return out
}

// findCode returns the first finding with the code, nil when there is none.
func findCode(fs []ingressdeploy.Finding, code string) *ingressdeploy.Finding {
	for i := range fs {
		if fs[i].Code == code {
			return &fs[i]
		}
	}
	return nil
}

func TestVIPAddressOutsideNodeSubnet(t *testing.T) {
	t.Run("a node outside the network is refused and named", func(t *testing.T) {
		fs := ingressdeploy.CheckVIPAddress(ingressdeploy.VIPAddressInput{
			Prefix: parseVIP(t, "192.168.1.50/24"),
			Nodes: []ingressdeploy.VIPNode{
				vipNode("n1", "node-a", "192.168.1.10"),
				vipNode("n2", "node-b", "192.168.2.7"),
			},
		})
		f := findCode(fs, ingressdeploy.CodeVIPOutsideNodeSubnet)
		if f == nil {
			t.Fatalf("no vip_outside_node_subnet in %v", findingCodes(fs))
		}
		if f.Severity != ingressdeploy.SeverityError {
			t.Errorf("severity = %q, want error", f.Severity)
		}
		if len(f.Items) != 1 || f.Items[0] != "node-b" {
			t.Errorf("items = %v, want [node-b]", f.Items)
		}
		want := "192.168.1.50 isn't inside the network of every node. These nodes are outside it: node-b."
		if f.Message != want {
			t.Errorf("message = %q, want %q", f.Message, want)
		}
	})

	t.Run("every node inside gives no such finding", func(t *testing.T) {
		fs := ingressdeploy.CheckVIPAddress(ingressdeploy.VIPAddressInput{
			Prefix: parseVIP(t, "192.168.1.50/24"),
			Nodes: []ingressdeploy.VIPNode{
				vipNode("n1", "node-a", "192.168.1.10"),
				vipNode("n2", "node-b", "192.168.1.11"),
			},
		})
		if f := findCode(fs, ingressdeploy.CodeVIPOutsideNodeSubnet); f != nil {
			t.Errorf("unexpected %s: %s", f.Code, f.Message)
		}
	})

	t.Run("several nodes are listed by host name then ID", func(t *testing.T) {
		fs := ingressdeploy.CheckVIPAddress(ingressdeploy.VIPAddressInput{
			Prefix: parseVIP(t, "192.168.1.50/24"),
			Nodes: []ingressdeploy.VIPNode{
				vipNode("z", "same", "10.0.0.2"),
				vipNode("a", "same", "10.0.0.3"),
				vipNode("m", "alpha", "10.0.0.4"),
			},
		})
		f := findCode(fs, ingressdeploy.CodeVIPOutsideNodeSubnet)
		if f == nil {
			t.Fatalf("no vip_outside_node_subnet in %v", findingCodes(fs))
		}
		if got, want := len(f.Items), 3; got != want {
			t.Fatalf("items = %v", f.Items)
		}
		if f.Items[0] != "alpha" || f.Items[1] != "same" || f.Items[2] != "same" {
			t.Errorf("items = %v, want alpha, same, same", f.Items)
		}
	})
}

func TestVIPAddressParse(t *testing.T) {
	refused := []struct {
		raw  string
		code string
	}{
		{"192.168.1.50", ingressdeploy.CodeVIPAddressInvalid},
		{"x", ingressdeploy.CodeVIPAddressInvalid},
		{"", ingressdeploy.CodeVIPAddressInvalid},
		{"192.168.1.50/33", ingressdeploy.CodeVIPAddressInvalid},
		{"fd00::5", ingressdeploy.CodeVIPAddressInvalid},
		{"fd00::5/64", ingressdeploy.CodeVIPIPv6Unsupported},
		{"::ffff:192.168.1.50/120", ingressdeploy.CodeVIPIPv6Unsupported},
		{"127.0.0.5/8", ingressdeploy.CodeVIPNotUnicast},
		{"169.254.1.1/16", ingressdeploy.CodeVIPNotUnicast},
		{"224.0.0.18/4", ingressdeploy.CodeVIPNotUnicast},
		{"0.0.0.0/0", ingressdeploy.CodeVIPNotUnicast},
		{"255.255.255.255/32", ingressdeploy.CodeVIPNotUnicast},
		{"192.168.1.0/24", ingressdeploy.CodeVIPNetworkOrBroadcast},
		{"192.168.1.255/24", ingressdeploy.CodeVIPNetworkOrBroadcast},
		{"192.168.1.0/30", ingressdeploy.CodeVIPNetworkOrBroadcast},
		{"192.168.1.3/30", ingressdeploy.CodeVIPNetworkOrBroadcast},
		{"10.0.0.0/8", ingressdeploy.CodeVIPNetworkOrBroadcast},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.raw, func(t *testing.T) {
			p, f := ingressdeploy.ParseVIPAddress(tc.raw)
			if f == nil {
				t.Fatalf("ParseVIPAddress(%q) = %v, want %s", tc.raw, p, tc.code)
			}
			if f.Code != tc.code {
				t.Errorf("code = %s, want %s", f.Code, tc.code)
			}
			if f.Severity != ingressdeploy.SeverityError {
				t.Errorf("severity = %q, want error", f.Severity)
			}
			if f.Message == "" {
				t.Error("message is empty")
			}
			if p.IsValid() {
				t.Errorf("a refused parse returned the prefix %v", p)
			}
		})
	}

	accepted := []struct {
		raw  string
		want string
	}{
		{"192.168.1.50/24", "192.168.1.50/24"},
		{" 192.168.1.50/24 ", "192.168.1.50/24"},
		{"10.0.0.1/31", "10.0.0.1/31"},
		{"10.0.0.0/31", "10.0.0.0/31"},
		{"10.0.0.1/32", "10.0.0.1/32"},
		{"192.168.1.1/30", "192.168.1.1/30"},
		{"192.168.1.2/30", "192.168.1.2/30"},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.raw, func(t *testing.T) {
			p, f := ingressdeploy.ParseVIPAddress(tc.raw)
			if f != nil {
				t.Fatalf("ParseVIPAddress(%q) refused with %s: %s", tc.raw, f.Code, f.Message)
			}
			if p.String() != tc.want {
				t.Errorf("prefix = %s, want %s (the host address is kept)", p, tc.want)
			}
		})
	}

	t.Run("the sentences are the fixed ones", func(t *testing.T) {
		_, f := ingressdeploy.ParseVIPAddress("192.168.1.50")
		if f.Message != "Give the address with its prefix length, for example 192.168.1.50/24." {
			t.Errorf("message = %q", f.Message)
		}
		_, f = ingressdeploy.ParseVIPAddress("fd00::5/64")
		if f.Message != "IPv6 virtual addresses aren't supported yet. Use an IPv4 address." {
			t.Errorf("message = %q", f.Message)
		}
		_, f = ingressdeploy.ParseVIPAddress("127.0.0.5/8")
		if f.Message != "127.0.0.5 can't be a virtual IP. Loopback, link-local, multicast and unspecified addresses aren't allowed." {
			t.Errorf("message = %q", f.Message)
		}
		_, f = ingressdeploy.ParseVIPAddress("192.168.1.255/24")
		if f.Message != "192.168.1.255 is the network or broadcast address of 192.168.1.0/24. Choose another." {
			t.Errorf("message = %q", f.Message)
		}
	})
}

func TestVIPAddressChecks(t *testing.T) {
	nodes := []ingressdeploy.VIPNode{
		vipNode("n1", "node-a", "192.168.1.10"),
		vipNode("n2", "node-b", "192.168.1.11"),
	}
	base := func() ingressdeploy.VIPAddressInput {
		return ingressdeploy.VIPAddressInput{Prefix: parseVIP(t, "192.168.1.50/24"), Nodes: nodes}
	}
	pfx := netip.MustParsePrefix

	t.Run("a clean address has only the unverified warning", func(t *testing.T) {
		fs := ingressdeploy.CheckVIPAddress(base())
		if len(fs) != 1 || fs[0].Code != ingressdeploy.CodeVIPUnverified {
			t.Fatalf("findings = %v, want only vip_unverified", findingCodes(fs))
		}
		if fs[0].Severity != ingressdeploy.SeverityWarning {
			t.Errorf("severity = %q, want warning", fs[0].Severity)
		}
		want := "TierPlan can't check from here that 192.168.1.50 is unused. Each node checks when it starts and reports it if another machine answers."
		if fs[0].Message != want {
			t.Errorf("message = %q, want %q", fs[0].Message, want)
		}
	})

	t.Run("an address equal to a node's is refused naming it", func(t *testing.T) {
		in := base()
		in.Prefix = parseVIP(t, "192.168.1.11/24")
		f := findCode(ingressdeploy.CheckVIPAddress(in), ingressdeploy.CodeVIPIsNodeAddress)
		if f == nil {
			t.Fatal("no vip_is_node_address")
		}
		if f.Severity != ingressdeploy.SeverityError || len(f.Items) != 1 || f.Items[0] != "node-b" {
			t.Errorf("finding = %+v", f)
		}
		want := "192.168.1.11 is already the address of node-b. Choose an address no machine uses."
		if f.Message != want {
			t.Errorf("message = %q, want %q", f.Message, want)
		}
	})

	t.Run("another environment's virtual IP is refused naming it", func(t *testing.T) {
		in := base()
		in.Others = []ingressdeploy.VIPOther{
			{EnvID: "e2", EnvName: "staging", Prefix: pfx("192.168.1.51/24"), RouterID: 9},
			{EnvID: "e3", EnvName: "lab", Prefix: pfx("192.168.1.50/24"), RouterID: 10},
		}
		f := findCode(ingressdeploy.CheckVIPAddress(in), ingressdeploy.CodeVIPInUseByEnvironment)
		if f == nil {
			t.Fatal("no vip_in_use_by_environment")
		}
		if len(f.Items) != 1 || f.Items[0] != "lab" || f.Severity != ingressdeploy.SeverityError {
			t.Errorf("finding = %+v", f)
		}
		if want := "192.168.1.50 is already the virtual IP of lab."; f.Message != want {
			t.Errorf("message = %q, want %q", f.Message, want)
		}
	})

	t.Run("an allocated network's subnet is refused naming the network", func(t *testing.T) {
		in := base()
		in.Allocated = []ingressdeploy.VIPNetwork{
			{Name: "far", Subnet: pfx("10.0.0.0/24")},
			{Name: "app_default", Subnet: pfx("192.168.1.0/25")},
		}
		f := findCode(ingressdeploy.CheckVIPAddress(in), ingressdeploy.CodeVIPOverlapsDocker)
		if f == nil {
			t.Fatal("no vip_overlaps_docker")
		}
		if len(f.Items) != 1 || f.Items[0] != "app_default" || f.Severity != ingressdeploy.SeverityError {
			t.Errorf("finding = %+v", f)
		}
		want := "192.168.1.0/24 overlaps the network app_default on this swarm. Choose an address outside Docker's networks."
		if f.Message != want {
			t.Errorf("message = %q, want %q", f.Message, want)
		}
	})

	t.Run("a pool only warns, with the pool sentence", func(t *testing.T) {
		in := base()
		in.Pools = []netip.Prefix{pfx("10.0.0.0/8"), pfx("192.168.0.0/16")}
		fs := ingressdeploy.CheckVIPAddress(in)
		for _, f := range fs {
			if f.Severity == ingressdeploy.SeverityError {
				t.Fatalf("a pool refused: %v", findingCodes(fs))
			}
		}
		f := findCode(fs, ingressdeploy.CodeVIPInsidePool)
		if f == nil {
			t.Fatalf("no vip_inside_pool in %v", findingCodes(fs))
		}
		if f.Severity != ingressdeploy.SeverityWarning {
			t.Errorf("severity = %q, want warning", f.Severity)
		}
		want := "192.168.1.0/24 sits inside a range Docker may hand to networks (192.168.0.0/16). A network created later could take this address range."
		if f.Message != want {
			t.Errorf("message = %q, want %q", f.Message, want)
		}
	})

	t.Run("refusals come first and the unverified warning is last", func(t *testing.T) {
		in := base()
		in.Prefix = parseVIP(t, "192.168.1.10/24") // a node's address
		in.Others = []ingressdeploy.VIPOther{{EnvID: "e2", EnvName: "staging", Prefix: pfx("192.168.1.10/24"), RouterID: 9}}
		in.Allocated = []ingressdeploy.VIPNetwork{{Name: "net", Subnet: pfx("192.168.1.0/24")}}
		in.Pools = []netip.Prefix{pfx("192.168.0.0/16")}
		in.Nodes = append(in.Nodes, vipNode("n3", "node-c", "10.1.1.1"))
		fs := ingressdeploy.CheckVIPAddress(in)
		want := []string{
			ingressdeploy.CodeVIPOutsideNodeSubnet,
			ingressdeploy.CodeVIPIsNodeAddress,
			ingressdeploy.CodeVIPInUseByEnvironment,
			ingressdeploy.CodeVIPOverlapsDocker,
			ingressdeploy.CodeVIPInsidePool,
			ingressdeploy.CodeVIPUnverified,
		}
		got := findingCodes(fs)
		if len(got) != len(want) {
			t.Fatalf("codes = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("codes = %v, want %v", got, want)
			}
		}
	})

	t.Run("a node with no reported address is not compared", func(t *testing.T) {
		in := base()
		in.Nodes = append(in.Nodes, ingressdeploy.VIPNode{ID: "n9", Hostname: "unknown"})
		if f := findCode(ingressdeploy.CheckVIPAddress(in), ingressdeploy.CodeVIPOutsideNodeSubnet); f != nil {
			t.Errorf("unexpected %s", f.Code)
		}
	})
}

func TestDerivedRouterID(t *testing.T) {
	// The vectors were computed with an independent SHA-256 (1 + big-endian uint32 of the first four bytes, mod 255)
	// and are fixed: a change here moves the router ID of every swarm that runs without an override (D-05).
	vectors := []struct {
		clusterID string
		want      int
	}{
		{"swarm-a", 240},
		{"abcdefghijklmnopqrstuvwxy", 248},
		{"w3k1z0q8c7m2n5p4r6t9y1u3x", 19},
		{"cluster-1", 70},
		{"0", 159},
		{"l8s9q2m7n4b1v6c3x5z0a2d4f", 155},
	}
	for _, v := range vectors {
		if got := ingressdeploy.DerivedRouterID(v.clusterID); got != v.want {
			t.Errorf("DerivedRouterID(%q) = %d, want %d", v.clusterID, got, v.want)
		}
	}

	t.Run("always in 1 to 255", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(26, 6)) //nolint:gosec // G404: a seeded generator for test vectors, not a secret
		const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
		seen := map[int]bool{}
		for range 1000 {
			b := make([]byte, 25)
			for i := range b {
				b[i] = alphabet[rng.IntN(len(alphabet))]
			}
			id := ingressdeploy.DerivedRouterID(string(b))
			if id < 1 || id > 255 {
				t.Fatalf("DerivedRouterID(%q) = %d, outside 1..255", b, id)
			}
			seen[id] = true
		}
		if len(seen) < 200 {
			t.Errorf("only %d distinct IDs over 1000 cluster IDs, the spread is wrong", len(seen))
		}
	})

	t.Run("the same cluster gives the same ID", func(t *testing.T) {
		first := ingressdeploy.DerivedRouterID("swarm-a")
		if again := ingressdeploy.DerivedRouterID("swarm-a"); again != first {
			t.Error("not deterministic")
		}
	})
}

func TestResolveRouterID(t *testing.T) {
	intp := func(v int) *int { return &v }
	derived := ingressdeploy.DerivedRouterID("swarm-a")

	t.Run("no override uses the derived value", func(t *testing.T) {
		id, isDerived, f := ingressdeploy.ResolveRouterID(nil, "swarm-a")
		if f != nil || !isDerived || id != derived || id != 240 {
			t.Errorf("got id=%d derived=%v finding=%v, want 240 true nil", id, isDerived, f)
		}
	})

	for _, v := range []int{1, 77, 255} {
		t.Run("accepts "+strconv.Itoa(v), func(t *testing.T) {
			id, isDerived, f := ingressdeploy.ResolveRouterID(intp(v), "swarm-a")
			if f != nil || isDerived || id != v {
				t.Errorf("got id=%d derived=%v finding=%v, want %d false nil", id, isDerived, f, v)
			}
		})
	}

	for _, v := range []int{0, 256, -1, 1000000} {
		t.Run("refuses "+strconv.Itoa(v), func(t *testing.T) {
			_, _, f := ingressdeploy.ResolveRouterID(intp(v), "swarm-a")
			if f == nil || f.Code != ingressdeploy.CodeVIPRouterIDInvalid || f.Severity != ingressdeploy.SeverityError {
				t.Fatalf("finding = %+v, want an error with vip_router_id_invalid", f)
			}
			if f.Message != "The router ID must be a whole number from 1 to 255." {
				t.Errorf("message = %q", f.Message)
			}
		})
	}
}

func TestRouterIDCollision(t *testing.T) {
	pfx := netip.MustParsePrefix
	others := []ingressdeploy.VIPOther{
		{EnvID: "e2", EnvName: "staging", Prefix: pfx("192.168.1.50/24"), RouterID: 51},
		{EnvID: "e3", EnvName: "lab", Prefix: pfx("10.9.0.5/24"), RouterID: 51},
		{EnvID: "e4", EnvName: "other-id", Prefix: pfx("192.168.1.70/24"), RouterID: 52},
	}

	t.Run("the same ID on an overlapping prefix is refused naming the environment", func(t *testing.T) {
		f := ingressdeploy.CheckRouterIDCollision("e1", 51, pfx("192.168.1.60/24"), others)
		if f == nil {
			t.Fatal("no finding")
		}
		if f.Code != ingressdeploy.CodeVIPRouterIDInUse || f.Severity != ingressdeploy.SeverityError {
			t.Errorf("finding = %+v", f)
		}
		if len(f.Items) != 1 || f.Items[0] != "staging" {
			t.Errorf("items = %v, want [staging]", f.Items)
		}
		want := "staging already uses router ID 51 on an overlapping network. Set a different router ID."
		if f.Message != want {
			t.Errorf("message = %q, want %q", f.Message, want)
		}
	})

	t.Run("the same ID on a prefix that does not overlap passes", func(t *testing.T) {
		if f := ingressdeploy.CheckRouterIDCollision("e1", 51, pfx("172.16.0.5/24"), others); f != nil {
			t.Errorf("unexpected %s", f.Code)
		}
	})

	t.Run("a different ID on an overlapping prefix passes", func(t *testing.T) {
		if f := ingressdeploy.CheckRouterIDCollision("e1", 53, pfx("192.168.1.60/24"), others); f != nil {
			t.Errorf("unexpected %s", f.Code)
		}
	})

	t.Run("the environment itself is never a collision", func(t *testing.T) {
		if f := ingressdeploy.CheckRouterIDCollision("e2", 51, pfx("192.168.1.50/24"), others); f != nil {
			t.Errorf("unexpected %s", f.Code)
		}
	})

	t.Run("the overlap is between the masked networks", func(t *testing.T) {
		// 192.168.1.60/16 contains the whole /24 of the other environment.
		if f := ingressdeploy.CheckRouterIDCollision("e1", 51, pfx("192.168.9.60/16"), others); f == nil {
			t.Error("a /16 over the other's /24 was not a collision")
		}
	})
}
