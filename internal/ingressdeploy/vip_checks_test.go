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
