// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

func TestVIPTextIsFixed(t *testing.T) {
	t.Run("the privilege warning is the D-20 text byte for byte", func(t *testing.T) {
		const want = "The virtual IP needs `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` on every manager's agent. " +
			"That setting lets TierPlan run privileged, host-network services on every node of this swarm, " +
			"so a compromised TierPlan could take over every node. " +
			"VRRP authentication only keeps honest neighbours apart; anything on this network segment can still contend for the address."
		if ingressdeploy.VIPPrivilegeWarning != want {
			t.Errorf("VIPPrivilegeWarning = %q", ingressdeploy.VIPPrivilegeWarning)
		}
		if !strings.Contains(ingressdeploy.VIPPrivilegeWarning, "VRRP authentication only keeps honest neighbours apart") {
			t.Error("the VRRP sentence is missing")
		}
	})

	t.Run("a long node name is cut to 64 characters in its sentence", func(t *testing.T) {
		long := strings.Repeat("n", 70)
		fs := ingressdeploy.CheckVIPAddress(ingressdeploy.VIPAddressInput{
			Prefix: parseVIP(t, "192.168.1.50/24"),
			Nodes:  []ingressdeploy.VIPNode{vipNode("n1", long, "10.0.0.2")},
		})
		f := findCode(fs, ingressdeploy.CodeVIPOutsideNodeSubnet)
		if f == nil {
			t.Fatal("no vip_outside_node_subnet")
		}
		cut := strings.Repeat("n", 64)
		want := "192.168.1.50 isn't inside the network of every node. These nodes are outside it: " + cut + "."
		if f.Message != want {
			t.Errorf("message = %q, want the name cut to 64", f.Message)
		}
		if len(f.Items) != 1 || f.Items[0] != cut {
			t.Errorf("items = %v, want the cut name", f.Items)
		}
	})

	t.Run("an environment and a network name are cut too", func(t *testing.T) {
		long := strings.Repeat("e", 70)
		cut := strings.Repeat("e", 64)
		in := ingressdeploy.VIPAddressInput{
			Prefix:    parseVIP(t, "192.168.1.50/24"),
			Others:    []ingressdeploy.VIPOther{{EnvID: "e2", EnvName: long, Prefix: netip.MustParsePrefix("192.168.1.50/24"), RouterID: 1}},
			Allocated: []ingressdeploy.VIPNetwork{{Name: long, Subnet: netip.MustParsePrefix("192.168.1.0/24")}},
		}
		fs := ingressdeploy.CheckVIPAddress(in)
		for _, code := range []string{ingressdeploy.CodeVIPInUseByEnvironment, ingressdeploy.CodeVIPOverlapsDocker} {
			f := findCode(fs, code)
			if f == nil {
				t.Fatalf("no %s", code)
			}
			if !strings.Contains(f.Message, cut) || strings.Contains(f.Message, long) {
				t.Errorf("%s message = %q, want the name cut to 64", code, f.Message)
			}
		}
		f := ingressdeploy.CheckRouterIDCollision("e1", 1, netip.MustParsePrefix("192.168.1.60/24"), in.Others)
		if f == nil || !strings.Contains(f.Message, cut) || strings.Contains(f.Message, long) {
			t.Errorf("collision finding = %+v, want the name cut to 64", f)
		}
	})

	t.Run("control characters in a name never reach a sentence", func(t *testing.T) {
		fs := ingressdeploy.CheckVIPAddress(ingressdeploy.VIPAddressInput{
			Prefix: parseVIP(t, "192.168.1.50/24"),
			Nodes:  []ingressdeploy.VIPNode{vipNode("n1", "bad\nname\x1b[0m", "10.0.0.2")},
		})
		f := findCode(fs, ingressdeploy.CodeVIPOutsideNodeSubnet)
		if f == nil {
			t.Fatal("no vip_outside_node_subnet")
		}
		if strings.ContainsAny(f.Message, "\n\x1b") || strings.ContainsAny(f.Items[0], "\n\x1b") {
			t.Errorf("control characters survived: %q", f.Message)
		}
	})

	t.Run("the local socket manager line is the S5 sentence", func(t *testing.T) {
		got := ingressdeploy.VIPLocalSocketManagerText("node-a")
		want := "node-a is reached through TierPlan's own Docker socket, which already gives TierPlan root there, so it counts as allowed."
		if got != want {
			t.Errorf("text = %q", got)
		}
	})
}
