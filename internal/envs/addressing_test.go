// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/proto/testsupport"
	"github.com/ianglennon/coaming/proto/wire"
)

// The address pools of an environment (plan 10-01, NET-02, D-02, D-03, D-05, D-07 as amended).
// The agents are real sessions of the real hub answering from fake daemons, as in
// aggregate_test.go; the pools come from each daemon's /info.

// poolLines projects the rows to what the tests compare: CIDR, mask, used, capacity and source.
func poolLines(v AddressSpace) []string {
	var out []string
	for _, p := range v.Pools {
		out = append(out, fmt.Sprintf("%s/%d used=%d cap=%d %s est=%v", p.Pool.Base, p.Pool.Size, p.Used, p.Capacity, p.Source, p.Estimated))
	}
	return out
}

// addrSwarm is a swarm whose three managers answer the same networks and pool, and the
// reader over it. Each manager reports its cluster, as a real manager does.
func addrSwarm(t *testing.T, tune func(i int, c *dockertest.FakeConfig)) (*swarmRig, *AddressSpaces) {
	t.Helper()
	nodes := managerNodes()
	s := newSwarmRigBare(t, nodes, nil)
	for i := 1; i <= 3; i++ {
		c := managerConfig(i, nodes, nil)
		c.SwarmClusterID, c.SwarmSubnetSize = "cluster-1", 24
		if tune != nil {
			tune(i, &c)
		}
		s.m[i-1] = s.connect(t, s.env, c, managerFacts(i))
	}
	s.whenOnline(t, s.env.ID)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatal(err)
	}
	return s, NewAddressSpaces(s.svc, s.agg, s.c, AddressSpaceOptions{Now: s.clk.Now})
}

func TestAddressSpaceSwarmDefaultPool(t *testing.T) {
	s, as := addrSwarm(t, func(_ int, c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{
			{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.0.0.0/24"}},
			{ID: "o1", Name: "o1", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
			{ID: "o2", Name: "o2", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.2.0/24"}},
			{ID: "o3", Name: "o3", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.3.0/24"}},
			{ID: "br", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
		}
	})
	v, err := as.Read(testsupport.Context(t), s.env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !v.Swarm || !v.Measured || v.NotMeasured != "" {
		t.Fatalf("space = %+v, want a measured swarm", v)
	}
	if got := strings.Join(poolLines(v), "|"); got != "10.0.0.0/8/24 used=4 cap=65536 swarm est=false" {
		t.Fatalf("pools = %s, want the default 10.0.0.0/8 with ingress and three overlays used", got)
	}
	p := v.Pools[0]
	if p.Percent != 0 || p.State != netspace.StateNormal {
		t.Errorf("percent/state = %d/%s, want 0/normal", p.Percent, p.State)
	}
	if p.LargestFree != netip.MustParsePrefix("10.128.0.0/9") || p.LargestFreeBlocks != 32768 {
		t.Errorf("largest free = %s with %d subnets, want 10.128.0.0/9 with 32768", p.LargestFree, p.LargestFreeBlocks)
	}
	if len(v.NotReporting) != 0 {
		t.Errorf("not reporting = %v", v.NotReporting)
	}
}

func TestAddressSpaceSwarmReportedPoolsKeepTheirOrderAndCountIngressOnceAndLocalNetworksThatOverlap(t *testing.T) {
	s, as := addrSwarm(t, func(i int, c *dockertest.FakeConfig) {
		c.SwarmDefaultAddrPool, c.SwarmSubnetSize = []string{"10.60.0.0/16", "10.20.0.0/16"}, 26
		c.Networks = []dockertest.FakeNetwork{
			{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.60.0.0/26"}},
			{ID: "o1", Name: "o1", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.60.0.64/26", "fd00::/64"}},
			// A local network of a node that lies inside the second pool consumes it from that node's view.
			{ID: fmt.Sprintf("br%d", i), Name: "br1", Driver: "bridge", Scope: "local", Subnets: []string{"10.20.0.0/24"}},
		}
	})
	v, err := as.Read(testsupport.Context(t), s.env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// The bridge is on three nodes with the same subnet, and counts once: 4 blocks of /26 in a /24.
	want := "10.60.0.0/16/26 used=2 cap=1024 swarm est=false|10.20.0.0/16/26 used=4 cap=1024 swarm est=false"
	if got := strings.Join(poolLines(v), "|"); got != want {
		t.Fatalf("pools = %s\nwant    %s", got, want)
	}
}

func TestAddressSpaceUnknownEnvironmentIsAnError(t *testing.T) {
	r := newTunRig(t, nil)
	as := NewAddressSpaces(r.svc, r.agg, NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now}), AddressSpaceOptions{Now: r.clk.Now})
	if v, err := as.Read(testsupport.Context(t), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read = %+v, %v; want ErrNotFound", v, err)
	}
}

func TestAddressSpaceTTL(t *testing.T) {
	s, as := addrSwarm(t, func(_ int, c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "o1", Name: "o1", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}}}
	})
	infoRequests := func() int {
		n := 0
		for _, m := range s.m {
			for _, r := range m.fake.Requests() {
				if strings.HasSuffix(r, "/info") {
					n++
				}
			}
		}
		return n
	}
	ctx := testsupport.Context(t)
	if _, err := as.Read(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	first := infoRequests()
	s.clk.Advance(DefaultClusterTTL - time.Second)
	if _, err := as.Read(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if got := infoRequests(); got != first {
		t.Fatalf("a read inside the TTL made %d engine requests, want 0", got-first)
	}
	s.clk.Advance(2 * time.Second)
	if _, err := as.Read(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if got := infoRequests(); got <= first {
		t.Fatalf("a read after the TTL made no engine request")
	}
}

func TestAddressSpaceStandaloneDaemonPools(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Edge")
	r.connect(t, env, dockertest.FakeConfig{
		Hostname:            "edge-1",
		DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "10.50.0.0/16", Size: 26}},
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "app", Name: "app", Driver: "bridge", Scope: "local", Subnets: []string{"10.50.0.0/26", "10.50.0.64/26"}},
			// An overlay is the swarm's, and never takes a pool of a standalone host.
			{ID: "ovl", Name: "ovl", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.50.0.128/26"}},
		},
	}, nil)
	r.whenOnline(t, env.ID)
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
	as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})

	v, err := as.Read(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Swarm || v.Measured || v.NotMeasured != "" {
		t.Fatalf("space = %+v, want a standalone host that is estimated and not a failed measurement", v)
	}
	if got := strings.Join(poolLines(v), "|"); got != "10.50.0.0/16/26 used=2 cap=1024 daemon est=true" {
		t.Fatalf("pools = %s, want the daemon's pool with the two bridge blocks and not the overlay", got)
	}
}

func TestAddressSpaceStandaloneDefaults(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Edge")
	r.connect(t, env, dockertest.FakeConfig{
		Hostname: "edge-1",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "lan", Name: "lan", Driver: "bridge", Scope: "local", Subnets: []string{"192.168.16.0/20"}},
		},
	}, nil)
	r.whenOnline(t, env.ID)
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
	as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})

	v, err := as.Read(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []string{
		"172.17.0.0/16/16 used=1 cap=1 defaults est=true",
		"172.18.0.0/16/16 used=0 cap=1 defaults est=true",
		"172.19.0.0/16/16 used=0 cap=1 defaults est=true",
		"172.20.0.0/14/16 used=0 cap=4 defaults est=true",
		"172.24.0.0/14/16 used=0 cap=4 defaults est=true",
		"172.28.0.0/14/16 used=0 cap=4 defaults est=true",
		"192.168.0.0/16/20 used=1 cap=16 defaults est=true",
	}
	if got := poolLines(v); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("pools = %v\nwant    %v", got, want)
	}
	if v.Pools[1].State != netspace.StateNormal || v.Pools[0].State != netspace.StateAlmostFull {
		t.Errorf("states = %s / %s, want the full /16 almost_full and the empty one normal", v.Pools[0].State, v.Pools[1].State)
	}
}

func TestAddressSpaceStandaloneLocalSocket(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{
		Hostname:            "this-host",
		DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "10.70.0.0/16", Size: 24}},
		Networks:            []dockertest.FakeNetwork{{ID: "app", Name: "app", Driver: "bridge", Scope: "local", Subnets: []string{"10.70.3.0/24"}}},
	})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// What the local engine is is not known before its first probe.
	awaitProbed(t, r.mon, e.ID)
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
	as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})
	v, err := as.Read(testsupport.Context(t), e.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := strings.Join(poolLines(v), "|"); got != "10.70.0.0/16/24 used=1 cap=256 daemon est=true" {
		t.Fatalf("pools = %s, want the local engine's pool read from its one node", got)
	}
}

func TestAddressSpaceNoManager(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Workers")
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs})
	r.svc.WithClusters(c)
	for i := 1; i <= 2; i++ {
		r.connect(t, env, dockertest.FakeConfig{
			Hostname: fmt.Sprintf("w-%d", i), SwarmState: "active",
			Networks: []dockertest.FakeNetwork{{ID: fmt.Sprintf("br%d", i), Name: "bridge", Driver: "bridge", Scope: "local"}},
		}, &wire.Facts{
			EngineID: fmt.Sprintf("ENG:w%d", i), Hostname: fmt.Sprintf("w-%d", i), EngineReachable: true,
			Swarm: &wire.SwarmFacts{State: "active", NodeID: fmt.Sprintf("node-w%d", i), Control: false},
		})
	}
	r.awaitHealth(t, env.ID, "the workers to be probed", func(e Environment) bool { return e.Status.State == StateOnline })
	as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})

	v, err := as.Read(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !v.Swarm || v.Measured || v.NotMeasured != NotMeasuredNoManager || len(v.Pools) != 0 {
		t.Fatalf("space = %+v, want a swarm that says no_manager and has no pool to show", v)
	}
}

func TestAddressSpaceNoManagerAfterAGoodReadKeepsThePoolsWithUnknownState(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Swarm")
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs})
	r.svc.WithClusters(c)
	nodes := managerNodes()[:1]
	mc := managerConfig(1, nodes, nil)
	mc.SwarmClusterID, mc.SwarmSubnetSize = "cluster-1", 24
	mc.Networks = []dockertest.FakeNetwork{{ID: "o1", Name: "o1", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}}}
	mgr := r.connect(t, env, mc, managerFacts(1))
	r.connect(t, env, dockertest.FakeConfig{Hostname: "wrk-1", SwarmState: "active"}, workerFacts("wrk-1"))
	r.whenOnline(t, env.ID)
	if err := c.Refresh(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})
	ctx := testsupport.Context(t)

	good, err := as.Read(ctx, env.ID)
	if err != nil || !good.Measured || len(good.Pools) != 1 || good.Pools[0].Used != 1 {
		t.Fatalf("first read = %+v, %v; want the pool measured with one block used", good, err)
	}

	r.kick(t, mgr)
	r.clk.Advance(DefaultClusterTTL + time.Second)
	v, err := as.Read(ctx, env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if v.Measured || v.NotMeasured != NotMeasuredNoManager || len(v.Pools) != 1 {
		t.Fatalf("space = %+v, want no_manager with the remembered pool", v)
	}
	p := v.Pools[0]
	if p.State != netspace.StateUnknown || p.Used != 0 || p.Percent != 0 || p.LargestFree.IsValid() || p.Capacity != 65536 || p.Source != PoolSourceSwarm {
		t.Errorf("remembered pool = %+v, want the same pool with state unknown and nothing counted", p)
	}
	if len(v.NotReporting) != 1 || v.NotReporting[0] != "mgr-1" {
		t.Errorf("not reporting = %v, want the manager that stopped answering", v.NotReporting)
	}
}
