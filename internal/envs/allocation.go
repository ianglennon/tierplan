// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"net/netip"
	"strings"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/netspace"
)

// SmallSubnetSetting is the environment's small stack subnet opt-in: whether a stack deploy gives
// its new networks a block of the mask instead of a whole pool block (D-20, D-21). An environment
// whose setting cannot be read answers off, which is how a deploy ran before the setting existed.
func (s *AddressSpaces) SmallSubnetSetting(ctx context.Context, envID string) (enabled bool, mask int) {
	if s == nil || s.svc == nil || s.svc.st == nil {
		return false, 0
	}
	row, err := s.svc.st.RQ.GetAddressSettings(ctx, envID)
	if err != nil {
		s.log.Warn("could not read the small subnet setting", "environment", envID, "error", err)
		return false, 0
	}
	return row.SmallSubnets != 0, int(row.SmallSubnetMask)
}

// AllocationInputs are what choosing a free block for a new network needs: the swarm's pools (a standalone
// host's own), every IPv4 subnet of every network on every node (of the host), and the destinations of the
// nodes' routes that are not Docker's own bridges.
type AllocationInputs struct {
	Pools    []netspace.Pool
	Occupied []netip.Prefix
	Avoid    []netip.Prefix
}

// AllocationInputs reads them. With fresh true the cached snapshot of every node is dropped first,
// so a network made a moment ago, through Coaming or not, is seen; a caller holds LockAllocation
// across the read and the create that follows. A swarm no manager answers for is ErrNoManager. A
// standalone host's pools are the ones a person entered, else the daemon's, else Docker's defaults, and
// only its local networks count.
func (s *AddressSpaces) AllocationInputs(ctx context.Context, envID string, fresh bool) (AllocationInputs, error) {
	if s == nil || s.agg == nil || s.clusters == nil {
		return AllocationInputs{}, ErrNoManager
	}
	var agg Aggregate
	var err error
	if fresh {
		agg, _, err = s.agg.FreshSnapshot(ctx, envID)
	} else {
		agg, _, err = s.agg.Snapshot(ctx, envID)
	}
	if err != nil {
		return AllocationInputs{}, err
	}
	swarm, err := s.clusters.IsSwarm(ctx, envID)
	if err != nil {
		return AllocationInputs{}, err
	}
	if !swarm {
		return s.standaloneInputs(ctx, envID, agg)
	}
	var pools dockerx.AddressPools
	if err := s.clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		pools, err = c.AddressPools(ctx)
		return err
	}); err != nil {
		return AllocationInputs{}, err
	}
	if !pools.SwarmKnown {
		return AllocationInputs{}, ErrNoManager
	}
	size := pools.SwarmSubnetSize
	if size == 0 {
		size = netspace.DefaultSwarmSubnetSize
	}
	bases := pools.SwarmPools
	if len(bases) == 0 {
		bases = []netip.Prefix{netspace.DefaultSwarmPool}
	}
	var in AllocationInputs
	for _, b := range bases {
		p := netspace.Pool{Base: b, Size: size}
		if !p.Valid() {
			s.log.Warn("a swarm reported an address pool that cannot be used", "environment", envID, "pool", b, "size", size)
			continue
		}
		in.Pools = append(in.Pools, p)
	}
	in.Occupied = networkSubnets(agg.Networks, func(NetworkRow) bool { return true })
	in.Avoid = s.routeAvoid(ctx, envID, agg)
	return in, nil
}

// standaloneInputs is AllocationInputs for a standalone host: the pools its daemon hands local networks out
// of, the subnets of its local networks (an overlay is a swarm's) and the destinations of its routes.
func (s *AddressSpaces) standaloneInputs(ctx context.Context, envID string, agg Aggregate) (AllocationInputs, error) {
	if len(agg.Nodes) == 0 {
		return AllocationInputs{}, ErrNodeUnavailable
	}
	defs, _, err := s.standalonePools(ctx, envID, agg.Nodes[0].ID)
	if err != nil {
		return AllocationInputs{}, err
	}
	var in AllocationInputs
	for _, p := range defs {
		if !p.Valid() {
			s.log.Warn("a daemon reported an address pool that cannot be used", "environment", envID, "pool", p.Base, "size", p.Size)
			continue
		}
		in.Pools = append(in.Pools, p)
	}
	in.Occupied = networkSubnets(agg.Networks, func(n NetworkRow) bool { return n.Scope != dockerx.ScopeSwarm })
	in.Avoid = s.routeAvoid(ctx, envID, agg)
	return in, nil
}

// routeAvoid are the destinations of the routes of the hosts whose route check is ok, without
// the routes of Docker's own bridges and without a route that is a Docker network's subnet (those
// are in Occupied already). With no route reader none is returned.
func (s *AddressSpaces) routeAvoid(ctx context.Context, envID string, agg Aggregate) []netip.Prefix {
	if s.routes == nil {
		return nil
	}
	nodes, err := s.routes.Get(ctx, envID)
	if err != nil {
		s.log.Debug("the host routes could not be read for an allocation", "environment", envID, "error", err)
		return nil
	}
	docker := map[netip.Prefix]bool{}
	for _, p := range networkSubnets(agg.Networks, func(NetworkRow) bool { return true }) {
		docker[p] = true
	}
	var out []netip.Prefix
	for _, n := range nodes {
		if n.State != RouteCheckOK {
			continue
		}
		for _, r := range n.Routes {
			if dockerBridge(r.Iface) || docker[r.Dst.Masked()] {
				continue
			}
			out = append(out, r.Dst)
		}
	}
	return out
}

// dockerBridge reports whether name is one of Docker's own bridges, whose routes are the networks
// Docker made and not a LAN or VPN of the host.
func dockerBridge(name string) bool {
	return name == "docker0" || name == "docker_gwbridge" || strings.HasPrefix(name, "br-")
}

// PoolList lists the pools as the messages word them: "10.0.0.0/8, 172.20.0.0/16".
func PoolList(pools []netspace.Pool) string {
	parts := make([]string, 0, len(pools))
	for _, p := range pools {
		parts = append(parts, p.Base.Masked().String())
	}
	return strings.Join(parts, ", ")
}
