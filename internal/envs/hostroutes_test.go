// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

// The route check says, for every node of an environment, either its validated host
// routes or exactly why there are none (plan 10-05, NET-05, D-17 as amended). The
// agents are real sessions of the real hub; only what they answer on a host-net
// stream is staged.

func hostNetFacts(name string) *wire.Facts {
	return &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name, ClusterID: "cluster-1", Control: true},
	}
}

// hostNetNode enrols one agent of a swarm environment with the hostNet fact it
// reports and the routes it answers with. Its Docker streams are not served: the
// route check does not use them.
func hostNetNode(t *testing.T, r *tunRig, env tunneltest.Environment, name, hostNet string, routes []wire.HostRoute) *agentConn {
	t.Helper()
	c := r.srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: hostNetFacts(name), HostNet: hostNet, HostRoutes: routes})
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

// fakeHostNet is a HostNetSource that counts its calls per agent and answers from
// what the test staged.
type fakeHostNet struct {
	mu    sync.Mutex
	calls map[string]int
	docs  map[string]wire.HostNetDoc
	errs  map[string]error
	// started, when set, receives the agent ID of each call as it begins, and gate,
	// when set, holds every call until it is closed.
	started chan string
	gate    chan struct{}
}

func newFakeHostNet() *fakeHostNet {
	return &fakeHostNet{calls: map[string]int{}, docs: map[string]wire.HostNetDoc{}, errs: map[string]error{}}
}

func (f *fakeHostNet) HostNet(ctx context.Context, agentID string) (wire.HostNetDoc, error) {
	f.mu.Lock()
	f.calls[agentID]++
	doc, err, started, gate := f.docs[agentID], f.errs[agentID], f.started, f.gate
	f.mu.Unlock()
	if started != nil {
		started <- agentID
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return wire.HostNetDoc{}, ctx.Err()
		}
	}
	return doc, err
}

func (f *fakeHostNet) count(agentID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[agentID]
}

func (f *fakeHostNet) answer(agentID string, doc wire.HostNetDoc) {
	f.mu.Lock()
	f.docs[agentID] = doc
	f.mu.Unlock()
}

func manyRoutes(n int) []wire.HostRoute {
	out := make([]wire.HostRoute, 0, n)
	for i := range n {
		out = append(out, wire.HostRoute{Iface: "eth0", Dst: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)})
	}
	return out
}

func TestHostRoutesValidation(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Cluster")
	a := hostNetNode(t, r, env, "n-1", wire.HostNetOK, nil)

	// A hostile agent answers anything it likes. One bad entry makes the node failed
	// with no routes: a partial list would look measured and be wrong.
	bad := map[string]wire.HostNetDoc{
		"an unknown version":                {V: 2, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "10.8.0.0/24"}}},
		"no version":                        {Routes: []wire.HostRoute{{Iface: "eth0", Dst: "10.8.0.0/24"}}},
		"513 routes":                        {V: 1, Routes: manyRoutes(513)},
		"host bits set":                     {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "10.8.0.0/24"}, {Iface: "eth0", Dst: "10.0.0.1/24"}}},
		"an IPv6 prefix":                    {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "fd00::/64"}}},
		"an IPv4-mapped IPv6 prefix":        {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "::ffff:10.0.0.0/104"}}},
		"the default route":                 {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "0.0.0.0/0"}}},
		"a single host":                     {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "10.0.0.5/32"}}},
		"a destination that is not a route": {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "not-a-prefix"}}},
		"a gateway that is not an address":  {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "10.8.0.0/24", Gateway: "not-an-ip"}}},
		"an IPv6 gateway":                   {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "10.8.0.0/24", Gateway: "fe80::1"}}},
		"an interface name over 15 bytes":   {V: 1, Routes: []wire.HostRoute{{Iface: "abcdefghijklmnop", Dst: "10.8.0.0/24"}}},
		"an interface name with markup":     {V: 1, Routes: []wire.HostRoute{{Iface: "<b>x</b>", Dst: "10.8.0.0/24"}}},
		"an interface name with a newline":  {V: 1, Routes: []wire.HostRoute{{Iface: "eth0\n", Dst: "10.8.0.0/24"}}},
		"an error the agent reports":        {V: 1, Routes: []wire.HostRoute{}, Error: wire.HostNetUnreadable},
		"an error with routes beside it":    {V: 1, Routes: []wire.HostRoute{{Iface: "eth0", Dst: "10.8.0.0/24"}}, Error: wire.HostNetTooLarge},
	}
	for name, doc := range bad {
		t.Run("refused/"+name, func(t *testing.T) {
			src := newFakeHostNet()
			src.answer(a.id, doc)
			got, err := NewHostRoutes(r.svc, src, HostRoutesOptions{Now: newStepClock().now}).Get(testsupport.Context(t), env.ID)
			if err != nil {
				t.Fatal(err)
			}
			if n := byNode(t, got, "n-1"); n.State != RouteCheckFailed || len(n.Routes) != 0 || n.Truncated {
				t.Fatalf("node = %+v, want failed with no routes", n)
			}
		})
	}

	t.Run("accepted/two routes, masked and sorted by address", func(t *testing.T) {
		src := newFakeHostNet()
		src.answer(a.id, wire.HostNetDoc{V: 1, Truncated: true, Routes: []wire.HostRoute{
			{Iface: "eth0", Dst: "192.168.2.0/24", Gateway: "192.168.1.1"}, {Iface: "wg0", Dst: "10.8.0.0/24"},
		}})
		got, err := NewHostRoutes(r.svc, src, HostRoutesOptions{Now: newStepClock().now}).Get(testsupport.Context(t), env.ID)
		if err != nil {
			t.Fatal(err)
		}
		n := byNode(t, got, "n-1")
		if n.State != RouteCheckOK || !n.Truncated || len(n.Routes) != 2 ||
			n.Routes[0].Dst != netip.MustParsePrefix("10.8.0.0/24") || n.Routes[0].Iface != "wg0" || n.Routes[0].Gateway.IsValid() ||
			n.Routes[1].Dst != netip.MustParsePrefix("192.168.2.0/24") || n.Routes[1].Gateway != netip.MustParseAddr("192.168.1.1") {
			t.Fatalf("node = %+v, want ok with 10.8.0.0/24 then 192.168.2.0/24", n)
		}
	})
	t.Run("accepted/512 routes, and none at all", func(t *testing.T) {
		for _, routes := range [][]wire.HostRoute{manyRoutes(512), {}, nil} {
			src := newFakeHostNet()
			src.answer(a.id, wire.HostNetDoc{V: 1, Routes: routes})
			got, err := NewHostRoutes(r.svc, src, HostRoutesOptions{Now: newStepClock().now}).Get(testsupport.Context(t), env.ID)
			if err != nil {
				t.Fatal(err)
			}
			if n := byNode(t, got, "n-1"); n.State != RouteCheckOK || len(n.Routes) != len(routes) {
				t.Fatalf("%d routes: node = %+v, want ok with all of them", len(routes), n)
			}
		}
	})
}

func TestHostRoutesCache(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Cluster")
	a := hostNetNode(t, r, env, "n-1", wire.HostNetOK, nil)
	src := newFakeHostNet()
	src.answer(a.id, wire.HostNetDoc{V: 1, Routes: []wire.HostRoute{{Iface: "wg0", Dst: "10.8.0.0/24"}}})
	clk := newStepClock()
	h := NewHostRoutes(r.svc, src, HostRoutesOptions{Now: clk.now})
	get := func() NodeRoutes {
		t.Helper()
		got, err := h.Get(testsupport.Context(t), env.ID)
		if err != nil {
			t.Fatal(err)
		}
		return byNode(t, got, "n-1")
	}

	// A picker that checks every 400 ms reads the agent once, not every time.
	for range 5 {
		if n := get(); n.State != RouteCheckOK || len(n.Routes) != 1 {
			t.Fatalf("node = %+v, want ok with its route", n)
		}
		clk.advance(400 * time.Millisecond)
	}
	if c := src.count(a.id); c != 1 {
		t.Fatalf("%d reads inside the TTL, want 1", c)
	}
	clk.advance(5*time.Minute - 2*time.Second - time.Millisecond) // 1 ms short of 5 minutes since the read
	get()
	if c := src.count(a.id); c != 1 {
		t.Fatalf("%d reads 1 ms before the TTL ended, want 1", c)
	}
	clk.advance(time.Millisecond)
	get()
	if c := src.count(a.id); c != 2 {
		t.Fatalf("%d reads once the TTL ended, want 2", c)
	}

	// A session that ended drops the cache entry, and the node reads offline.
	_ = a.conn.WS.CloseNow()
	awaitGone(t, r, a.id)
	if n := get(); n.State != RouteCheckOffline || len(n.Routes) != 0 {
		t.Fatalf("node = %+v, want offline with no routes", n)
	}
	h.mu.Lock()
	kept := len(h.cache)
	h.mu.Unlock()
	if kept != 0 {
		t.Fatalf("%d cache entries after the session ended, want none", kept)
	}
	if c := src.count(a.id); c != 2 {
		t.Fatalf("%d reads for an agent that is offline, want the 2 before", c)
	}

	// A new session is a new agent process, possibly on a changed host: it is asked
	// again at once, however recently the old one was.
	r.clk.Advance(time.Minute) // no session is open, so the watchdog has nothing to close
	id := a.conn.Identity
	c := r.srv.Dial(t, tunneltest.Agent{Identity: &id, Facts: hostNetFacts("n-1"), HostNet: wire.HostNetOK})
	if c.Welcome == nil {
		t.Fatalf("hello was refused: %+v (%v)", c.Refuse, c.Err)
	}
	c.AnswerHostNet(t)
	r.awaitSessions(t, a.id)
	if n := get(); n.State != RouteCheckOK {
		t.Fatalf("node = %+v after the reconnect, want ok", n)
	}
	if c := src.count(a.id); c != 3 {
		t.Fatalf("%d reads after the reconnect, want 3", c)
	}
}

func TestHostRoutesSingleFlight(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Cluster")
	a := hostNetNode(t, r, env, "n-1", wire.HostNetOK, nil)
	src := newFakeHostNet()
	src.answer(a.id, wire.HostNetDoc{V: 1, Routes: []wire.HostRoute{{Iface: "wg0", Dst: "10.8.0.0/24"}}})
	src.started, src.gate = make(chan string, 4), make(chan struct{})
	joined := make(chan struct{}, 4)
	h := NewHostRoutes(r.svc, src, HostRoutesOptions{Now: newStepClock().now})
	h.onJoin = func() { joined <- struct{}{} }

	type result struct {
		got []NodeRoutes
		err error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	ctx := testsupport.Context(t)
	go func() { g, err := h.Get(ctx, env.ID); first <- result{g, err} }()
	<-src.started // the first Get has a read under way and holds it
	go func() { g, err := h.Get(ctx, env.ID); second <- result{g, err} }()
	<-joined // the second Get found that read and waits for it
	close(src.gate)

	for _, ch := range []chan result{first, second} {
		res := <-ch
		if res.err != nil {
			t.Fatal(res.err)
		}
		if n := byNode(t, res.got, "n-1"); n.State != RouteCheckOK || len(n.Routes) != 1 {
			t.Fatalf("node = %+v, want ok with its route", n)
		}
	}
	if c := src.count(a.id); c != 1 {
		t.Fatalf("%d reads for two concurrent Gets, want 1", c)
	}
}

func TestHostRoutesOneNodeFails(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Cluster")
	bad := hostNetNode(t, r, env, "n-bad", wire.HostNetOK, nil)
	good := hostNetNode(t, r, env, "n-good", wire.HostNetOK, nil)
	src := newFakeHostNet()
	src.errs[bad.id] = errors.New("the agent did not answer")
	src.answer(good.id, wire.HostNetDoc{V: 1, Routes: []wire.HostRoute{{Iface: "wg0", Dst: "10.8.0.0/24"}}})

	got, err := NewHostRoutes(r.svc, src, HostRoutesOptions{Now: newStepClock().now}).Get(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatalf("one node failing failed the whole answer: %v", err)
	}
	if n := byNode(t, got, "n-bad"); n.State != RouteCheckFailed || len(n.Routes) != 0 {
		t.Errorf("n-bad = %+v, want failed with no routes", n)
	}
	if n := byNode(t, got, "n-good"); n.State != RouteCheckOK || len(n.Routes) != 1 {
		t.Errorf("n-good = %+v, want ok with its route", n)
	}
}

// TierPlan's own host has no agent: its routes come from the route file mounted into
// TierPlan's own container, and the answer says so (Local) so the fix is worded for it.
func TestHostRoutesLocalSocket(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the fixture is a little-endian host's route table")
	}
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host"})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitProbed(t, r.mon, e.ID)
	fixture := filepath.Join("..", "..", "proto", "hostroute", "testdata", "route-host.txt")
	src := newFakeHostNet()

	cases := map[string]struct {
		file  string
		state string
	}{
		"a route file that reads":     {fixture, RouteCheckOK},
		"a route file that is absent": {filepath.Join(t.TempDir(), "no-such-route"), RouteCheckUnmounted},
		"a directory":                 {t.TempDir(), RouteCheckUnmounted},
		"no route file configured":    {"", RouteCheckUnmounted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NewHostRoutes(r.svc, src, HostRoutesOptions{Now: r.clk.Now, LocalRouteFile: tc.file}).Get(testsupport.Context(t), e.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].State != tc.state || !got[0].Local || got[0].Node != "this-host" {
				t.Fatalf("got %+v, want one local node this-host in state %q", got, tc.state)
			}
			if tc.state != RouteCheckOK {
				if len(got[0].Routes) != 0 {
					t.Errorf("routes %+v for a file that did not read", got[0].Routes)
				}
				return
			}
			want := []string{"10.8.0.0/24", "172.17.0.0/16", "192.168.2.0/24"}
			if len(got[0].Routes) != len(want) {
				t.Fatalf("routes = %+v, want %v", got[0].Routes, want)
			}
			for i, w := range want {
				if got[0].Routes[i].Dst != netip.MustParsePrefix(w) {
					t.Errorf("route %d = %v, want %s (sorted by address, default and host routes dropped)", i, got[0].Routes[i].Dst, w)
				}
			}
		})
	}
	// The local host is read from a file, never through the tunnel.
	if n := len(src.calls); n != 0 {
		t.Errorf("the agent source was asked %d times for the local host", n)
	}
}

func TestHostRoutesLocalFileOverTheLimitsIsUnmounted(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host"})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitProbed(t, r.mon, e.ID)
	big := filepath.Join(t.TempDir(), "route")
	// One line over the 512-byte limit: the table is not a route table, so nothing is used.
	if err := os.WriteFile(big, []byte("Iface\tDestination\n"+string(make([]byte, 600))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewHostRoutes(r.svc, newFakeHostNet(), HostRoutesOptions{Now: r.clk.Now, LocalRouteFile: big}).Get(testsupport.Context(t), e.ID)
	if err != nil || len(got) != 1 || got[0].State != RouteCheckUnmounted || len(got[0].Routes) != 0 {
		t.Fatalf("got %+v, %v; want unmounted with no routes", got, err)
	}
}
