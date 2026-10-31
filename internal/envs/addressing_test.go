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
