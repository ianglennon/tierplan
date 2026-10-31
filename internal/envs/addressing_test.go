// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
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

// The overlay networks' own fullness (plan 10-03, NET-03, D-04, D-05, RESEARCH F3).

// setUsage gives every manager of the rig the swarm's address count of the network called name,
// as Engine 29 reports it in an inspect's Status.IPAM.
func setUsage(s *swarmRig, name string, usage ...dockertest.FakeSubnetUsage) {
	for _, m := range s.m {
		m.fake.SetNetworkUsage(name, usage...)
	}
}

// overlayLines projects the overlays to what the tests compare.
func overlayLines(v AddressSpace) []string {
	var out []string
	for _, o := range v.Overlays {
		out = append(out, fmt.Sprintf("%s used=%d cap=%d %d%% %s measured=%v", o.Name, o.Used, o.Capacity, o.Percent, o.State, o.Measured))
	}
	return out
}

func overlayNets(nets ...dockertest.FakeNetwork) func(int, *dockertest.FakeConfig) {
	return func(_ int, c *dockertest.FakeConfig) { c.Networks = nets }
}

func TestAddressSpaceOverlayUsage(t *testing.T) {
	s, as := addrSwarm(t, overlayNets(
		dockertest.FakeNetwork{ID: "calm-id", Name: "calm", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
		dockertest.FakeNetwork{ID: "busy-id", Name: "busy", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.2.0/24"}},
		dockertest.FakeNetwork{ID: "ranged-id", Name: "ranged", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.99.0.0/24"}},
		dockertest.FakeNetwork{ID: "bridge-id", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
	))
	setUsage(s, "calm", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 8, Available: 248})
	setUsage(s, "busy", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 200, Available: 56})
	// An --ip-range /25 inside a /24 counts only the range.
	setUsage(s, "ranged", dockertest.FakeSubnetUsage{Subnet: "10.99.0.0/24", InUse: 3, Available: 126})

	v, err := as.Read(testsupport.Context(t), s.env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := "busy used=200 cap=256 78% getting_full measured=true|calm used=8 cap=256 3% normal measured=true|ranged used=3 cap=129 2% normal measured=true"
	if got := strings.Join(overlayLines(v), "|"); got != want {
		t.Fatalf("overlays = %s\nwant      %s", got, want)
	}
	if v.Overlays[0].ID != "busy-id" || v.Ingress != nil {
		t.Errorf("first overlay %q, ingress %v; want busy-id and no ingress", v.Overlays[0].ID, v.Ingress)
	}
}

func TestAddressSpaceIngressInsidePool(t *testing.T) {
	t.Run("pool with the ingress as its first block", func(t *testing.T) {
		s, as := addrSwarm(t, func(_ int, c *dockertest.FakeConfig) {
			c.SwarmDefaultAddrPool, c.SwarmSubnetSize = []string{"10.60.0.0/16"}, 26
			c.Networks = []dockertest.FakeNetwork{
				{ID: "ingress-id", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.60.0.0/26"}},
				{ID: "other-id", Name: "other", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.60.0.64/26"}},
			}
		})
		setUsage(s, "ingress", dockertest.FakeSubnetUsage{Subnet: "10.60.0.0/26", InUse: 5, Available: 59})
		v, err := as.Read(testsupport.Context(t), s.env.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Ingress == nil || v.Ingress.OutsidePool || !v.Ingress.Ingress || !v.Ingress.Measured || v.Ingress.Used != 5 || v.Ingress.Capacity != 64 {
			t.Fatalf("ingress = %+v, want measured 5 of 64 and inside the pool", v.Ingress)
		}
		// The ingress is one of the pool's two blocks, counted once, and is also in the overlays.
		if got := strings.Join(poolLines(v), "|"); got != "10.60.0.0/16/26 used=2 cap=1024 swarm est=false" {
			t.Fatalf("pools = %s", got)
		}
		if len(v.Overlays) != 2 {
			t.Fatalf("overlays = %v, want the ingress among them", overlayLines(v))
		}
	})
	t.Run("default pool takes a wide ingress", func(t *testing.T) {
		s, as := addrSwarm(t, overlayNets(
			dockertest.FakeNetwork{ID: "ingress-id", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.255.0.0/16"}},
		))
		v, err := as.Read(testsupport.Context(t), s.env.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Ingress == nil || v.Ingress.OutsidePool {
			t.Fatalf("ingress = %+v, want inside 10.0.0.0/8", v.Ingress)
		}
		if got := strings.Join(poolLines(v), "|"); got != "10.0.0.0/8/24 used=256 cap=65536 swarm est=false" {
			t.Fatalf("pools = %s, want the /16 ingress as 256 blocks", got)
		}
	})
}

func TestAddressSpaceIngressOutsidePool(t *testing.T) {
	s, as := addrSwarm(t, func(_ int, c *dockertest.FakeConfig) {
		c.SwarmDefaultAddrPool, c.SwarmSubnetSize = []string{"10.60.0.0/16"}, 26
		c.Networks = []dockertest.FakeNetwork{
			{ID: "ingress-id", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.255.0.0/16"}},
			{ID: "other-id", Name: "other", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.60.0.0/26"}},
		}
	})
	setUsage(s, "ingress", dockertest.FakeSubnetUsage{Subnet: "10.255.0.0/16", InUse: 40, Available: 65496})
	v, err := as.Read(testsupport.Context(t), s.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Ingress == nil || !v.Ingress.OutsidePool || !v.Ingress.Measured || v.Ingress.Capacity != 65536 {
		t.Fatalf("ingress = %+v, want measured and outside the pool", v.Ingress)
	}
	if got := strings.Join(poolLines(v), "|"); got != "10.60.0.0/16/26 used=1 cap=1024 swarm est=false" {
		t.Fatalf("pools = %s, want the pool not to count the ingress", got)
	}
	for _, o := range v.Overlays {
		if o.Name != "ingress" && o.OutsidePool {
			t.Errorf("%s has outsidePool; only the ingress can", o.Name)
		}
	}
}

func TestAddressSpaceOverlayNotMeasured(t *testing.T) {
	// An engine that reports no Status (Engine 24 to 28), a manager that cannot ask the leader
	// (G-07-8: a full ID answers the node's own count and the partial ID none), and a healthy
	// overlay beside them.
	s, as := addrSwarm(t, overlayNets(
		dockertest.FakeNetwork{ID: "old-engine-id", Name: "old", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
		dockertest.FakeNetwork{ID: "no-count-id", Name: "nocount", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.2.0/24"}},
		dockertest.FakeNetwork{ID: "fine-id", Name: "fine", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.3.0/24"}},
	))
	for _, m := range s.m {
		m.fake.SetNetworkLocalUsage("nocount", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 4, Available: 252})
		m.fake.SetNetworkNoSwarmStatus("nocount", true)
	}
	setUsage(s, "fine", dockertest.FakeSubnetUsage{Subnet: "10.0.3.0/24", InUse: 10, Available: 246})
	v, err := as.Read(testsupport.Context(t), s.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := "fine used=10 cap=256 3% normal measured=true|nocount used=0 cap=0 0% unknown measured=false|old used=0 cap=0 0% unknown measured=false"
	if got := strings.Join(overlayLines(v), "|"); got != want {
		t.Fatalf("overlays = %s\nwant      %s", got, want)
	}
	// The pools are still computed from the subnets, whatever the engine reports.
	if !v.Measured || len(v.Pools) != 1 || v.Pools[0].Used != 3 {
		t.Fatalf("pools = %v, want the default pool with three blocks used", poolLines(v))
	}
}

func TestAddressSpaceOverlayIPv6Skipped(t *testing.T) {
	s, as := addrSwarm(t, overlayNets(
		dockertest.FakeNetwork{ID: "dual-id", Name: "dual", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24", "fd00:1::/64"}},
		dockertest.FakeNetwork{ID: "six-id", Name: "six", Driver: "overlay", Scope: "swarm", Subnets: []string{"fd00:2::/64"}},
		dockertest.FakeNetwork{ID: "bogus-id", Name: "bogus", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.4.0/24"}},
	))
	const maxU64 = ^uint64(0)
	setUsage(s, "dual",
		dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 8, Available: 248},
		// An IPv6 subnet's counters saturate at 2^64-1.
		dockertest.FakeSubnetUsage{Subnet: "fd00:1::/64", InUse: maxU64, Available: maxU64})
	// A network with only an IPv6 subnet has nothing to meter.
	setUsage(s, "six", dockertest.FakeSubnetUsage{Subnet: "fd00:2::/64", InUse: 3, Available: maxU64})
	// An IPv4 subnet cannot hold more than 2^32 addresses; such a count is not trusted.
	setUsage(s, "bogus", dockertest.FakeSubnetUsage{Subnet: "10.0.4.0/24", InUse: maxU64, Available: 1})
	v, err := as.Read(testsupport.Context(t), s.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := "dual used=8 cap=256 3% normal measured=true|bogus used=0 cap=0 0% unknown measured=false|six used=0 cap=0 0% unknown measured=false"
	if got := strings.Join(overlayLines(v), "|"); got != want {
		t.Fatalf("overlays = %s\nwant      %s", got, want)
	}
	if got := strings.Join(v.Overlays[0].Subnets, ","); got != "10.0.1.0/24" {
		t.Errorf("dual subnets = %s, want the IPv4 subnet only", got)
	}
}

func TestAddressSpaceOverlayCap(t *testing.T) {
	const total = 600
	var nets []dockertest.FakeNetwork
	for i := range total {
		// The distinguishing part comes first: the reader asks again by the ID less its last
		// character, which the fake resolves by prefix.
		nets = append(nets, dockertest.FakeNetwork{
			ID: fmt.Sprintf("%03d-overlay", i), Name: fmt.Sprintf("ovl-%03d", i), Driver: "overlay", Scope: "swarm",
			Subnets: []string{fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)},
		})
	}
	s, as := addrSwarm(t, overlayNets(nets...))
	for i := range total {
		setUsage(s, fmt.Sprintf("ovl-%03d", i), dockertest.FakeSubnetUsage{Subnet: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), InUse: 5, Available: 251})
	}
	var inFlight, peak, started atomic.Int64
	as.overlayReadHook = func() func() {
		started.Add(1)
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		return func() { inFlight.Add(-1) }
	}
	v, err := as.Read(testsupport.Context(t), s.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Overlays) != total {
		t.Fatalf("overlays = %d, want all %d listed", len(v.Overlays), total)
	}
	measured := 0
	for _, o := range v.Overlays {
		if o.Measured {
			measured++
			if o.Name > "ovl-511" {
				t.Errorf("%s is measured; only the first 512 by name are", o.Name)
			}
		} else if o.Name <= "ovl-511" {
			t.Errorf("%s is not measured, but is among the first 512 by name", o.Name)
		}
	}
	if measured != maxMeasuredOverlays || started.Load() != maxMeasuredOverlays {
		t.Errorf("measured %d with %d reads, want %d of each", measured, started.Load(), maxMeasuredOverlays)
	}
	if p := peak.Load(); p < 1 || p > overlayParallel {
		t.Errorf("at most %d reads at once, saw %d", overlayParallel, p)
	}
}

// A refetch within the TTL shares the first read: two browsers polling during a rolling update
// never make two sets of inspects nor see two different numbers.
func TestAddressSpaceOverlayReadIsSharedWithinTheTTL(t *testing.T) {
	s, as := addrSwarm(t, overlayNets(
		dockertest.FakeNetwork{ID: "calm-id", Name: "calm", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
	))
	setUsage(s, "calm", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 8, Available: 248})
	var reads atomic.Int64
	as.overlayReadHook = func() func() { reads.Add(1); return func() {} }
	ctx := testsupport.Context(t)
	first, err := as.Read(ctx, s.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	setUsage(s, "calm", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 100, Available: 156})
	s.clk.Advance(DefaultClusterTTL - time.Second)
	second, err := as.Read(ctx, s.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 || second.Overlays[0].Used != first.Overlays[0].Used {
		t.Fatalf("reads = %d, used %d then %d; want one read and one number", reads.Load(), first.Overlays[0].Used, second.Overlays[0].Used)
	}
	s.clk.Advance(2 * time.Second)
	third, err := as.Read(ctx, s.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 || third.Overlays[0].Used != 100 {
		t.Fatalf("after the TTL: reads = %d, used %d; want a fresh read of 100", reads.Load(), third.Overlays[0].Used)
	}
}
