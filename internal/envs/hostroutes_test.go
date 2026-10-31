// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"net/netip"
	"testing"

	"github.com/ianglennon/coaming/internal/tunnel"
	"github.com/ianglennon/coaming/internal/tunnel/tunneltest"
	"github.com/ianglennon/coaming/proto/testsupport"
	"github.com/ianglennon/coaming/proto/wire"
)

// The route check says, for every node of an environment, either its validated host
// routes or exactly why there are none (plan 10-05, NET-05, D-17 as amended). The
// agents are real sessions of the real hub; only what they answer on a host-net
// stream is staged.

// hostNetNode enrols one agent of a swarm environment with the hostNet fact it
// reports and the routes it answers with. Its Docker streams are not served: the
// route check does not use them.
func hostNetNode(t *testing.T, r *tunRig, env tunneltest.Environment, name, hostNet string, routes []wire.HostRoute) *agentConn {
	t.Helper()
	facts := &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name, ClusterID: "cluster-1", Control: true},
	}
	c := r.srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: facts, HostNet: hostNet, HostRoutes: routes})
	if c.Welcome == nil {
		t.Fatalf("enrolment of %s refused: %+v", name, c.Refuse)
	}
	c.AnswerHostNet(t)
	r.awaitSessions(t, c.Identity.AgentID)
	return &agentConn{conn: c, id: c.Identity.AgentID}
}

// awaitGone waits until the hub holds no session for the agent.
func awaitGone(t *testing.T, r *tunRig, id string) {
	t.Helper()
	if _, err := r.srv.Hub.AwaitSession(testsupport.Context(t), id, func(s *tunnel.Session) bool { return s == nil }); err != nil {
		t.Fatalf("agent %s kept its session: %v", id, err)
	}
}

func byNode(t *testing.T, got []NodeRoutes, node string) NodeRoutes {
	t.Helper()
	for _, n := range got {
		if n.Node == node {
			return n
		}
	}
	t.Fatalf("no node %q in %+v", node, got)
	return NodeRoutes{}
}

func TestHostRoutesStates(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Cluster")
	good := []wire.HostRoute{{Iface: "wg0", Dst: "10.8.0.0/24"}, {Iface: "eth0", Dst: "192.168.2.0/24", Gateway: "192.168.1.1"}}
	nodes := map[string]*agentConn{
		"n-ok":        hostNetNode(t, r, env, "n-ok", wire.HostNetOK, good),
		"n-old":       hostNetNode(t, r, env, "n-old", "", good),
		"n-unmounted": hostNetNode(t, r, env, "n-unmounted", wire.HostNetUnmounted, nil),
		"n-offline":   hostNetNode(t, r, env, "n-offline", wire.HostNetOK, good),
	}
	_ = nodes["n-offline"].conn.WS.CloseNow()
	awaitGone(t, r, nodes["n-offline"].id)

	h := NewHostRoutes(r.svc, r.srv.Hub, HostRoutesOptions{Now: r.clk.Now})
	got, err := h.Get(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d nodes, want 4: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Node > got[i].Node {
			t.Errorf("nodes are not sorted by name: %q before %q", got[i-1].Node, got[i].Node)
		}
	}
	for node, want := range map[string]string{
		"n-ok": RouteCheckOK, "n-old": RouteCheckOldAgent, "n-unmounted": RouteCheckUnmounted, "n-offline": RouteCheckOffline,
	} {
		n := byNode(t, got, node)
		if n.State != want {
			t.Errorf("%s: state %q, want %q", node, n.State, want)
		}
		if n.State != RouteCheckOK && len(n.Routes) != 0 {
			t.Errorf("%s: routes %+v for a node that is not ok", node, n.Routes)
		}
		if n.Local {
			t.Errorf("%s: marked local, but it is an agent", node)
		}
		if n.NodeID != "node-"+node {
			t.Errorf("%s: node ID %q, want node-%s", node, n.NodeID, node)
		}
	}
	ok := byNode(t, got, "n-ok")
	if len(ok.Routes) != 2 || ok.Routes[0].Dst != netip.MustParsePrefix("10.8.0.0/24") || ok.Routes[0].Iface != "wg0" ||
		ok.Routes[1].Dst != netip.MustParsePrefix("192.168.2.0/24") || ok.Routes[1].Gateway != netip.MustParseAddr("192.168.1.1") {
		t.Errorf("ok routes = %+v, want the two the agent answered", ok.Routes)
	}
	// Only the node that said it could answer, and was connected, was asked (the
	// monitor's own Docker streams to the agents are not counted).
	for node, want := range map[string]int32{"n-ok": 1, "n-old": 0, "n-unmounted": 0, "n-offline": 0} {
		if n := nodes[node].conn.HostNetStreams.Load(); n != want {
			t.Errorf("%s was opened %d host-net streams, want %d", node, n, want)
		}
	}
	if _, err := h.Get(testsupport.Context(t), "no-such-env"); err == nil {
		t.Error("an unknown environment was answered")
	}
}
