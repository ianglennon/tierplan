// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"net/netip"
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
