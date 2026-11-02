// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
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

// setEnteredPools stores the pools a person entered for an environment, as the settings route does.
func setEnteredPools(t *testing.T, r *tunRig, envID string, pools ...netspace.Pool) {
	t.Helper()
	if _, err := r.svc.st.Write.ExecContext(testsupport.Context(t), `UPDATE environments SET address_pools = ? WHERE id = ?`, EncodeEnteredPools(pools), envID); err != nil {
		t.Fatal(err)
	}
}

func pool(base string, size int) netspace.Pool {
	return netspace.Pool{Base: netip.MustParsePrefix(base), Size: size}
}

// Pools a person entered replace what the daemon reports, are counted like any other and read source
// entered; clearing them, and dropping the reader's last answer, goes back to the daemon's pools.
func TestAddressSpaceEnteredPools(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Edge")
	r.connect(t, env, dockertest.FakeConfig{
		Hostname:            "edge-1",
		DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "10.50.0.0/16", Size: 26}},
		Networks: []dockertest.FakeNetwork{
			{ID: "app", Name: "app", Driver: "bridge", Scope: "local", Subnets: []string{"10.20.0.0/24", "10.20.1.0/24"}},
		},
	}, nil)
	r.whenOnline(t, env.ID)
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
	as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})

	setEnteredPools(t, r, env.ID, pool("10.20.0.0/16", 24), pool("10.99.0.0/24", 28))
	v, err := as.Read(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := "10.20.0.0/16/24 used=2 cap=256 entered est=true|10.99.0.0/24/28 used=0 cap=16 entered est=true"
	if got := strings.Join(poolLines(v), "|"); got != want {
		t.Fatalf("pools = %s\nwant    %s", got, want)
	}
	if v.Swarm || v.Measured {
		t.Errorf("space = %+v, want a standalone host that is not measured", v)
	}

	// The cached read is kept until it is dropped.
	setEnteredPools(t, r, env.ID)
	if again, _ := as.Read(testsupport.Context(t), env.ID); len(again.Pools) != 2 {
		t.Fatalf("pools = %v, want the cached read within the TTL", poolLines(again))
	}
	as.Invalidate(env.ID)
	v, err = as.Read(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatalf("Read after clearing: %v", err)
	}
	if got := strings.Join(poolLines(v), "|"); got != "10.50.0.0/16/26 used=0 cap=1024 daemon est=true" {
		t.Fatalf("pools = %s, want the daemon's pool back", got)
	}
}

// Every pool row of a standalone host says where it came from and is an estimate, whatever the
// source (D-07 as amended, the assumption-delta invariant).
func TestStandalonePoolRowsCarrySourceAndEstimate(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	cases := map[string]struct {
		cfg     dockertest.FakeConfig
		entered []netspace.Pool
		source  string
	}{
		"daemon":   {dockertest.FakeConfig{Hostname: "a", DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "10.50.0.0/16", Size: 26}}}, nil, PoolSourceDaemon},
		"defaults": {dockertest.FakeConfig{Hostname: "b"}, nil, PoolSourceDefaults},
		"entered":  {dockertest.FakeConfig{Hostname: "c"}, []netspace.Pool{pool("10.20.0.0/16", 24)}, PoolSourceEntered},
	}
	for name, tc := range cases {
		env := r.srv.NewEnvironment(t, name)
		r.connect(t, env, tc.cfg, nil)
		r.whenOnline(t, env.ID)
		setEnteredPools(t, r, env.ID, tc.entered...)
		c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
		as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})
		v, err := as.Read(testsupport.Context(t), env.ID)
		if err != nil || len(v.Pools) == 0 {
			t.Fatalf("%s: Read = %v, %v", name, v.Pools, err)
		}
		for _, p := range v.Pools {
			if p.Source != tc.source || !p.Estimated {
				t.Errorf("%s: pool %s has source %q estimated %v, want %q and true", name, p.Pool.Base, p.Source, p.Estimated, tc.source)
			}
		}
	}
}

// routeFile writes a route table as /proc/net/route has it, in the byte order of the host running the
// test, with an interface-less UP route for each destination.
func routeFile(t *testing.T, dsts ...string) string {
	t.Helper()
	hex := func(a netip.Addr) string {
		b := a.As4()
		return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(b[:]))
	}
	var b strings.Builder
	b.WriteString("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n")
	for _, d := range dsts {
		p := netip.MustParsePrefix(d)
		mask := netip.MustParseAddr("255.255.255.255").As4()
		for i := range 4 {
			bitsHere := min(8, max(0, p.Bits()-8*i))
			mask[i] = byte(0xFF << (8 - bitsHere))
		}
		fmt.Fprintf(&b, "eth0\t%s\t00000000\t0001\t0\t0\t0\t%s\t0\t0\t0\n", hex(p.Addr()), hex(netip.AddrFrom4(mask)))
	}
	path := filepath.Join(t.TempDir(), "route")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The daemon skips a pool slot that overlaps a route of its host (RESEARCH F1), so a standalone host's
// pool counts those blocks as used and says how many only a route takes. A route equal to a network's
// subnet touches the blocks that network already holds and adds none.
func TestAddressSpaceStandaloneSkipsHostRouteSlots(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the fixture is a little-endian host's route table")
	}
	local := dockertest.NewFake(t, dockertest.FakeConfig{
		Hostname: "this-host",
		Networks: []dockertest.FakeNetwork{{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}}},
	})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitProbed(t, r.mon, e.ID)
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
	last := func(v AddressSpace) PoolUsage { return v.Pools[len(v.Pools)-1] }

	// Without the routes the last default pool has nothing in it.
	plain := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})
	v, err := plain.Read(testsupport.Context(t), e.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if p := last(v); p.Pool.Base.String() != "192.168.0.0/16" || p.Used != 0 || p.HostRouteBlocks != 0 {
		t.Fatalf("last pool = %+v, want 192.168.0.0/16 with nothing used", p)
	}

	routes := NewHostRoutes(r.svc, newFakeHostNet(), HostRoutesOptions{Now: r.clk.Now, LocalRouteFile: routeFile(t, "172.17.0.0/16", "192.168.0.0/20")})
	as := NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now}).WithHostRoutes(routes)
	v, err = as.Read(testsupport.Context(t), e.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if p := last(v); p.Used != 1 || p.HostRouteBlocks != 1 || p.Capacity != 16 || !p.Estimated || p.Source != PoolSourceDefaults {
		t.Errorf("last pool = %+v, want the /20 block counted, hostRouteBlocks 1, still an estimate", p)
	}
	if p := v.Pools[0]; p.Pool.Base.String() != "172.17.0.0/16" || p.Used != 1 || p.HostRouteBlocks != 0 {
		t.Errorf("first pool = %+v, want its block used by the bridge once and no route-only block", p)
	}

	// A host whose routes cannot be read adds nothing and is no worse than before.
	unread := NewHostRoutes(r.svc, newFakeHostNet(), HostRoutesOptions{Now: r.clk.Now, LocalRouteFile: filepath.Join(t.TempDir(), "no-such-route")})
	as = NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now}).WithHostRoutes(unread)
	if v, err = as.Read(testsupport.Context(t), e.ID); err != nil || last(v).Used != 0 || last(v).HostRouteBlocks != 0 {
		t.Errorf("unreadable routes: %+v, %v, want nothing counted", last(v), err)
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

// The cached networks and the allocation lock (plan 10-16, NET-05, T-10-39, T-10-40).

func TestCachedNetworks(t *testing.T) {
	s, as := addrSwarm(t, func(_ int, c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{
			{ID: "o1", Name: "o1", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
			{ID: "br", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "none", Name: "none", Driver: "null", Scope: "local"},
		}
	})
	if nets, _, ok := as.CachedNetworks(s.env.ID); ok || nets != nil || len(as.Environments()) != 0 {
		t.Fatalf("before any read: %v %v, want nothing cached", nets, ok)
	}
	if _, ok := func() (int, bool) { _, _, ok := as.CachedNetworks("nope"); return 0, ok }(); ok {
		t.Error("an unknown environment has cached networks")
	}
	if _, err := as.Read(testsupport.Context(t), s.env.ID); err != nil {
		t.Fatal(err)
	}
	nets, at, ok := as.CachedNetworks(s.env.ID)
	if !ok || at.IsZero() {
		t.Fatalf("after a read: ok=%v at=%v, want cached networks", ok, at)
	}
	var got []string
	for _, n := range nets {
		got = append(got, fmt.Sprintf("%s swarm=%v %v", n.Name, n.Swarm, n.Prefixes))
	}
	// The overlay once however many managers listed it, a bridge per node, no network with no subnet.
	if len(nets) != 4 || !slices.Contains(got, "o1 swarm=true [10.0.1.0/24]") {
		t.Errorf("networks = %v, want o1 once and the three nodes' bridges", got)
	}
	if ids := as.Environments(); len(ids) != 1 || ids[0] != s.env.ID {
		t.Errorf("Environments = %v, want the one read", ids)
	}
	// What is handed out is a copy.
	first := nets[0].Name
	nets[0].Name = "changed"
	if again, _, _ := as.CachedNetworks(s.env.ID); again[0].Name != first {
		t.Error("CachedNetworks hands out its own slice")
	}
}

func TestLockAllocationIsPerEnvironment(t *testing.T) {
	as := NewAddressSpaces(nil, nil, nil, AddressSpaceOptions{Log: slog.New(slog.DiscardHandler)})
	unlock := as.LockAllocation("env-a")
	// The lock is held: it cannot be taken again, while another environment's can.
	if as.entry("env-a").alloc.TryLock() {
		t.Fatal("the lock of an environment was granted twice")
	}
	if !as.entry("env-b").alloc.TryLock() {
		t.Fatal("the lock of another environment was held up")
	}
	unlock()
	if !as.entry("env-a").alloc.TryLock() {
		t.Fatal("the lock was not given back by the unlock")
	}
}

// The summary, its hysteresis and the background reader (plan 10-17, D-06, D-07, D-09, user decision 3).

// overlaySpace is a measured swarm read with one overlay holding used of 100 addresses.
func overlaySpace(name string, used uint64) AddressSpace {
	return AddressSpace{
		Swarm: true, Measured: true,
		Overlays: []OverlayUsage{{
			ID: name + "-id", Name: name, Used: used, Capacity: 100, Percent: netspace.PercentOf(used, 100),
			State: netspace.StateOf(used, 100), Measured: true,
		}},
	}
}

// fullAfter feeds the reads in turn to summarise and answers whether each left the swarm full.
func fullAfter(reads ...AddressSpace) []bool {
	var sum AddressSummary
	var out []bool
	for i, v := range reads {
		sum = summarise(sum, v, t0.Add(time.Duration(i)*time.Minute))
		out = append(out, sum.Full)
	}
	return out
}

func TestAddressSummaryHysteresis(t *testing.T) {
	got := fullAfter(overlaySpace("net", 89), overlaySpace("net", 90), overlaySpace("net", 86), overlaySpace("net", 84), overlaySpace("net", 86), overlaySpace("net", 90))
	want := []bool{false, true, true, false, false, true}
	if !slices.Equal(got, want) {
		t.Fatalf("full after each read = %v, want %v", got, want)
	}

	// The subject, the percent and the time it turned full follow the reads.
	first := summarise(AddressSummary{}, overlaySpace("net", 91), t0)
	again := summarise(first, overlaySpace("net", 87), t0.Add(time.Hour))
	if !again.Full || again.FullName != "net" || again.FullPercent != 87 || !again.FullSince.Equal(t0) {
		t.Fatalf("summary = %+v, want full on net at 87%% since the first read", again)
	}
	off := summarise(again, overlaySpace("net", 40), t0.Add(2*time.Hour))
	if off.Full || off.FullName != "" || !off.FullSince.IsZero() {
		t.Fatalf("summary = %+v, want nothing left of the full state", off)
	}
	if on := summarise(off, overlaySpace("net", 95), t0.Add(3*time.Hour)); !on.FullSince.Equal(t0.Add(3 * time.Hour)) {
		t.Errorf("since = %v, want the time it turned full again", on.FullSince)
	}

	// The exact ratio decides, not the rounded percent: 89.9% is 89 and not enough.
	nearly := AddressSpace{Swarm: true, Overlays: []OverlayUsage{{ID: "n", Name: "n", Used: 899, Capacity: 1000, Percent: 89, State: netspace.StateGettingFull, Measured: true}}}
	if summarise(AddressSummary{}, nearly, t0).Full {
		t.Error("899 of 1000 turned the swarm full")
	}
	nearly.Overlays[0].Used = 900
	if !summarise(AddressSummary{}, nearly, t0).Full {
		t.Error("900 of 1000 did not turn the swarm full")
	}
}

func TestAddressSummaryWorst(t *testing.T) {
	pool := func(base string, used, capacity uint64) PoolUsage {
		return PoolUsage{PoolStat: netspace.PoolStat{
			Pool: netspace.Pool{Base: netip.MustParsePrefix(base), Size: 24}, Used: used, Capacity: capacity,
			Percent: netspace.PercentOf(used, capacity), State: netspace.StateOf(used, capacity),
		}, Source: PoolSourceSwarm}
	}
	ovl := func(name string, used, capacity uint64, ingress bool) OverlayUsage {
		return OverlayUsage{
			ID: name, Name: name, Used: used, Capacity: capacity, Percent: netspace.PercentOf(used, capacity),
			State: netspace.StateOf(used, capacity), Measured: capacity > 0, Ingress: ingress,
		}
	}
	v := AddressSpace{
		Swarm: true, Measured: true,
		Pools:    []PoolUsage{pool("10.0.0.0/16", 50, 256), pool("10.1.0.0/16", 100, 256)},
		Overlays: []OverlayUsage{ovl("ovl", 100, 256, false), ovl("ingress", 100, 256, true), ovl("unmeasured", 0, 0, false)},
	}
	// Tied at 100 of 256: the pool comes before the ingress network, and that before an overlay.
	w := summarise(AddressSummary{}, v, t0).Worst
	if w == nil || w.Kind != MeterPool || w.Name != "10.1.0.0/16" || w.Unit != UnitSubnets || w.Used != 100 || w.Capacity != 256 || w.Percent != 39 || w.Estimated {
		t.Fatalf("worst = %+v, want the second pool", w)
	}
	v.Pools = nil
	if w := summarise(AddressSummary{}, v, t0).Worst; w == nil || w.Kind != MeterIngress || w.Name != "ingress" || w.Unit != UnitAddresses {
		t.Fatalf("worst = %+v, want the ingress network ahead of the overlay it ties with", w)
	}
	v.Overlays = append(v.Overlays, ovl("fuller", 101, 256, false))
	if w := summarise(AddressSummary{}, v, t0).Worst; w == nil || w.Kind != MeterOverlay || w.Name != "fuller" {
		t.Fatalf("worst = %+v, want the fuller overlay", w)
	}
	// Nothing measured: no worst meter, and the orphan count of the earlier summary is kept.
	none := summarise(AddressSummary{Orphaned: 3}, AddressSpace{Swarm: true, NotMeasured: NotMeasuredNoManager, Overlays: []OverlayUsage{{Name: "x"}}}, t0)
	if none.Worst != nil || none.Measured || none.Orphaned != 3 {
		t.Fatalf("summary = %+v, want no worst meter and 3 orphaned kept", none)
	}
}

func TestAddressSummaryEstimatedNeverFull(t *testing.T) {
	standalone := AddressSpace{Pools: []PoolUsage{{
		PoolStat: netspace.PoolStat{
			Pool: netspace.Pool{Base: netip.MustParsePrefix("172.17.0.0/16"), Size: 24}, Used: 9, Capacity: 10, Percent: 90, State: netspace.StateAlmostFull,
		},
		Source: PoolSourceDefaults, Estimated: true,
	}}}
	sum := summarise(AddressSummary{}, standalone, t0)
	if sum.Full {
		t.Fatalf("an estimated pool at 90%% made the host full: %+v", sum)
	}
	if w := sum.Worst; w == nil || w.Percent != 90 || w.State != netspace.StateAlmostFull || !w.Estimated || w.Kind != MeterPool {
		t.Fatalf("worst = %+v, want the estimated pool still shown at 90%% almost_full", w)
	}
	// Not even an estimated pool of a swarm, nor with the host full before.
	standalone.Swarm = true
	if sum := summarise(AddressSummary{Full: true, FullName: "old", FullPercent: 95}, standalone, t0); sum.Full {
		t.Fatalf("an estimated pool kept the environment full: %+v", sum)
	}
}

func TestAddressSummaryKeepsFullWithoutMeasurement(t *testing.T) {
	full := summarise(AddressSummary{}, overlaySpace("net", 95), t0)
	if !full.Full {
		t.Fatalf("setup: %+v", full)
	}
	// The manager stops answering: the pools are remembered with nothing counted and the overlays
	// are listed without numbers.
	silent := AddressSpace{Swarm: true, NotMeasured: NotMeasuredNoManager, Overlays: []OverlayUsage{{ID: "net-id", Name: "net", State: netspace.StateUnknown}}}
	kept := summarise(full, silent, t0.Add(time.Minute))
	if !kept.Full || kept.FullName != "net" || kept.FullPercent != 95 || !kept.FullSince.Equal(t0) {
		t.Fatalf("summary = %+v, want full kept as it was", kept)
	}
	// Answering, but that one network could not be measured: still not a measurement below 85.
	partial := overlaySpace("other", 10)
	partial.Overlays = append(partial.Overlays, OverlayUsage{ID: "net-id", Name: "net", State: netspace.StateUnknown})
	if kept := summarise(full, partial, t0.Add(time.Minute)); !kept.Full || kept.FullName != "net" {
		t.Fatalf("summary = %+v, want full kept while the network is unmeasured", kept)
	}
	// The network is gone: nothing is full any more.
	if gone := summarise(full, overlaySpace("other", 10), t0.Add(time.Minute)); gone.Full {
		t.Fatalf("summary = %+v, want not full once the network is gone", gone)
	}
	// A measurement below 85 turns it off.
	if low := summarise(kept, overlaySpace("net", 84), t0.Add(2*time.Minute)); low.Full {
		t.Fatalf("summary = %+v, want not full at 84%%", low)
	}
}

// A read of a real swarm feeds the summary, and the orphan check adds its count.
func TestAddressSummaryFromRead(t *testing.T) {
	s, as := addrSwarm(t, overlayNets(
		dockertest.FakeNetwork{ID: "hot-id", Name: "hot", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.5.0/24"}},
	))
	ctx := testsupport.Context(t)
	if _, ok := as.Summary(s.env.ID); ok {
		t.Fatal("a summary before any read")
	}
	setUsage(s, "hot", dockertest.FakeSubnetUsage{Subnet: "10.0.5.0/24", InUse: 231, Available: 25})
	if _, err := as.Read(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	sum, ok := as.Summary(s.env.ID)
	if !ok || !sum.Full || sum.FullName != "hot" || sum.FullPercent != 90 || sum.Worst == nil || sum.Worst.Name != "hot" || sum.Worst.Kind != MeterOverlay || sum.Worst.Used != 231 {
		t.Fatalf("summary = %+v, want full on hot at 90%%", sum)
	}
	// What is handed out is a copy.
	sum.Worst.Name = "changed"
	if again, _ := as.Summary(s.env.ID); again.Worst.Name != "hot" {
		t.Error("Summary hands out its own meter")
	}
	// Back under the line: still full until 85, then not.
	setUsage(s, "hot", dockertest.FakeSubnetUsage{Subnet: "10.0.5.0/24", InUse: 220, Available: 36})
	s.clk.Advance(DefaultClusterTTL + time.Second)
	if _, err := as.Read(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if sum, _ := as.Summary(s.env.ID); !sum.Full || sum.FullPercent != 85 {
		t.Fatalf("summary = %+v, want full still at 85%%", sum)
	}
	setUsage(s, "hot", dockertest.FakeSubnetUsage{Subnet: "10.0.5.0/24", InUse: 200, Available: 56})
	s.clk.Advance(DefaultClusterTTL + time.Second)
	if _, err := as.Read(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if sum, _ := as.Summary(s.env.ID); sum.Full {
		t.Fatalf("summary = %+v, want not full at 78%%", sum)
	}
	// The orphan count joins the summary without disturbing it.
	as.setOrphaned(s.env.ID, 2)
	if sum, _ := as.Summary(s.env.ID); sum.Orphaned != 2 || sum.Worst == nil {
		t.Fatalf("summary = %+v, want 2 orphaned beside the worst meter", sum)
	}
}

// fakeAfter stands in for time.After: each call is recorded and answers when the test says so.
type fakeAfter struct {
	mu    sync.Mutex
	chans []chan time.Time
	asked chan time.Duration
}

func newFakeAfter() *fakeAfter { return &fakeAfter{asked: make(chan time.Duration, 64)} }

func (f *fakeAfter) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.mu.Lock()
	f.chans = append(f.chans, ch)
	f.mu.Unlock()
	f.asked <- d
	return ch
}

// fire answers the n-th request (from 0).
func (f *fakeAfter) fire(n int) {
	f.mu.Lock()
	ch := f.chans[n]
	f.mu.Unlock()
	ch <- time.Time{}
}

func (f *fakeAfter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chans)
}

// runProbe records what a running reader does: reads started and in flight per environment, and the
// end of each pass. Nothing in it waits on a clock.
type runProbe struct {
	mu       sync.Mutex
	reads    map[string]int
	inflight int
	peak     int
	enter    func() // called as a read starts, with the probe's lock not held
	passes   chan struct{}
	done     chan string
}

func newRunProbe() *runProbe {
	return &runProbe{reads: map[string]int{}, passes: make(chan struct{}, 64), done: make(chan string, 64)}
}

func (p *runProbe) attach(as *AddressSpaces) {
	as.passHook = func() { p.passes <- struct{}{} }
	as.readHook = func(envID string) func() {
		p.mu.Lock()
		p.reads[envID]++
		p.inflight++
		p.peak = max(p.peak, p.inflight)
		enter := p.enter
		p.mu.Unlock()
		if enter != nil {
			enter()
		}
		return func() {
			p.mu.Lock()
			p.inflight--
			p.mu.Unlock()
			p.done <- envID
		}
	}
}

func (p *runProbe) count(envID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads[envID]
}

// quietEvents is a subscription that never delivers anything.
func quietEvents(context.Context, string) ([]dockerx.EventRow, <-chan dockerx.EventRow, func()) {
	return nil, make(chan dockerx.EventRow), func() {}
}

// runReader is the reader of the swarm rig with the test's ticks, timers and subscriptions, running
// until the test ends.
func runReader(t *testing.T, s *swarmRig, o AddressSpaceOptions, p *runProbe) *AddressSpaces {
	t.Helper()
	o.Now = s.clk.Now
	as := NewAddressSpaces(s.svc, s.agg, s.c, o)
	p.attach(as)
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		as.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	return as
}

func swarmWithNetwork(t *testing.T) *swarmRig {
	t.Helper()
	s, _ := addrSwarm(t, overlayNets(
		dockertest.FakeNetwork{ID: "o1", Name: "o1", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
	))
	return s
}

func TestAddressSpacesRunTick(t *testing.T) {
	s := swarmWithNetwork(t)
	// A disabled environment, and five more that cannot be read: more than the reader does at once.
	disabled := s.srv.NewEnvironment(t, "Disabled")
	s.srv.Exec(t, "UPDATE environments SET enabled = 0 WHERE id = ?", disabled.ID)
	var others []string
	for i := range 5 {
		others = append(others, s.srv.NewEnvironment(t, fmt.Sprintf("Other %d", i)).ID)
	}

	// The first four reads to start wait for each other, so that a limit above four would show.
	probe := newRunProbe()
	var started atomic.Int32
	barrier := make(chan struct{})
	probe.enter = func() {
		if started.Add(1) == addressParallel {
			close(barrier)
		}
		<-barrier
	}
	ticks := make(chan time.Time)
	runReader(t, s, AddressSpaceOptions{Ticks: ticks, Subscribe: quietEvents}, probe)

	<-probe.passes
	for _, id := range append([]string{s.env.ID}, others...) {
		if n := probe.count(id); n != 1 {
			t.Errorf("after the first pass environment %s was read %d times, want 1", id, n)
		}
	}
	if n := probe.count(disabled.ID); n != 0 {
		t.Errorf("the disabled environment was read %d times", n)
	}
	probe.mu.Lock()
	peak := probe.peak
	probe.mu.Unlock()
	if peak > addressParallel {
		t.Errorf("%d environments were read at once, want at most %d", peak, addressParallel)
	}

	// The pass ran the orphan check, which stamped the network it saw.
	rows, err := s.svc.st.RQ.ListNetworkSeen(testsupport.Context(t), s.env.ID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("network_seen = %v (%v), want the networks the pass saw", rows, err)
	}

	// One tick, one more read of each.
	ticks <- time.Time{}
	<-probe.passes
	if n := probe.count(s.env.ID); n != 2 {
		t.Errorf("after the second pass the environment was read %d times, want 2", n)
	}
	if n := probe.count(disabled.ID); n != 0 {
		t.Errorf("the disabled environment was read %d times", n)
	}
}

func TestAddressSpacesEventDebounce(t *testing.T) {
	s := swarmWithNetwork(t)
	other := s.srv.NewEnvironment(t, "Other")

	feeds := map[string]chan dockerx.EventRow{s.env.ID: make(chan dockerx.EventRow), other.ID: make(chan dockerx.EventRow)}
	subscribed := make(chan string, 8)
	subscribe := func(_ context.Context, id string) ([]dockerx.EventRow, <-chan dockerx.EventRow, func()) {
		subscribed <- id
		return nil, feeds[id], func() {}
	}
	after := newFakeAfter()
	probe := newRunProbe()
	as := runReader(t, s, AddressSpaceOptions{Ticks: make(chan time.Time), Subscribe: subscribe, After: after.After}, probe)
	<-probe.passes
	<-subscribed
	<-subscribed
	drain := func(n int) {
		for range n {
			<-probe.done
		}
	}
	drain(2) // the first pass read both
	if probe.count(s.env.ID) != 1 || probe.count(other.ID) != 1 {
		t.Fatalf("reads after the first pass: %d and %d, want 1 and 1", probe.count(s.env.ID), probe.count(other.ID))
	}

	// An event that says nothing about networks, containers or services starts nothing.
	feeds[s.env.ID] <- dockerx.EventRow{Type: "image", Action: "pull"}
	// A network event arms the debounce; a second one, a container event and an explicit trigger
	// within it ride on the same read.
	feeds[s.env.ID] <- dockerx.EventRow{Type: "network", Action: "create"}
	if d := <-after.asked; d != eventDebounce {
		t.Fatalf("debounce = %v, want %v", d, eventDebounce)
	}
	as.Trigger(s.env.ID)
	as.Trigger(s.env.ID)
	if n := after.count(); n != 1 {
		t.Fatalf("%d timers armed, want 1 for the whole burst", n)
	}
	if n := probe.count(s.env.ID); n != 1 {
		t.Fatalf("read %d times before the debounce ended, want 1", n)
	}

	after.fire(0)
	if id := <-probe.done; id != s.env.ID {
		t.Fatalf("read %s, want %s", id, s.env.ID)
	}
	if n := probe.count(s.env.ID); n != 2 {
		t.Fatalf("read %d times, want one extra, 2", n)
	}

	// The burst is over: the next event arms a new timer. An event of the other environment
	// reads that environment and never this one.
	feeds[other.ID] <- dockerx.EventRow{Type: "service", Action: "update"}
	<-after.asked
	after.fire(1)
	if id := <-probe.done; id != other.ID {
		t.Fatalf("read %s, want %s", id, other.ID)
	}
	if n := probe.count(s.env.ID); n != 2 {
		t.Errorf("an event of another environment read this one: %d reads, want 2", n)
	}
	if n := probe.count(other.ID); n != 2 {
		t.Errorf("the other environment was read %d times, want 2", n)
	}
	// A container event counts too.
	feeds[s.env.ID] <- dockerx.EventRow{Type: "container", Action: "die"}
	<-after.asked
	after.fire(2)
	if id := <-probe.done; id != s.env.ID {
		t.Fatalf("read %s, want %s", id, s.env.ID)
	}
}

func TestAddressSpacesForgetsRemoved(t *testing.T) {
	s := swarmWithNetwork(t)
	other := s.srv.NewEnvironment(t, "Other")
	ticks := make(chan time.Time)
	probe := newRunProbe()
	as := runReader(t, s, AddressSpaceOptions{Ticks: ticks, Subscribe: quietEvents}, probe)
	<-probe.passes
	// Both have something held, as a page load or an orphan check would leave.
	as.setOrphaned(other.ID, 1)
	for _, id := range []string{s.env.ID, other.ID} {
		if _, ok := as.Summary(id); !ok {
			t.Fatalf("no summary for %s after a pass", id)
		}
	}

	s.srv.Exec(t, "DELETE FROM environments WHERE id = ?", other.ID)
	ticks <- time.Time{}
	<-probe.passes
	if _, ok := as.Summary(other.ID); ok {
		t.Error("the summary of a removed environment is still held")
	}
	if _, ok := as.Summary(s.env.ID); !ok {
		t.Error("the summary of the environment that stays was dropped")
	}
	if n := probe.count(other.ID); n != 1 {
		t.Errorf("the removed environment was read %d times, want only the first pass's", n)
	}
	as.bmu.Lock()
	_, watched := as.watches[other.ID]
	as.bmu.Unlock()
	if watched {
		t.Error("the removed environment still has an event watcher")
	}
}
