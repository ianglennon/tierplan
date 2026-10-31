// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"github.com/moby/moby/client"
)

// DaemonPool is one entry of a daemon's default-address-pools setting: the range and the mask
// of each network allocated from it.
type DaemonPool struct {
	Base netip.Prefix
	Size int
}

// AddressPools are the address pools an engine reports about itself.
type AddressPools struct {
	// SwarmKnown is true when the engine is a swarm manager that reports its cluster; the two
	// fields below mean something only then. A worker, and an engine outside a swarm, report no
	// cluster.
	SwarmKnown bool
	// SwarmPools are the swarm's default address pools. Empty means the swarm was initialised
	// without one and allocates from 10.0.0.0/8; SwarmSubnetSize is the mask of each network, 0
	// when the engine did not say.
	SwarmPools      []netip.Prefix
	SwarmSubnetSize int
	// DaemonPools are the daemon's default-address-pools setting, the pools local networks are
	// allocated from. Empty when the daemon has none configured, which is when it uses Docker's
	// built-in defaults: the engine omits the field.
	DaemonPools []DaemonPool
}

// AddressPools reads the pools from one call to the engine's info. It never asks for the
// swarm itself (GET /swarm): that answer carries the join tokens, and the info has the pools
// without them (T-10-01). The swarm part is the manager's: call it through a manager.
func (c *Client) AddressPools(ctx context.Context) (AddressPools, error) {
	res, err := c.cur().Info(ctx, client.InfoOptions{})
	if err != nil {
		return AddressPools{}, fmt.Errorf("read the address pools from %s: %w", c.t.Describe(), err)
	}
	var out AddressPools
	if cl := res.Info.Swarm.Cluster; cl != nil {
		out.SwarmKnown = true
		out.SwarmPools = slices.Clone(cl.DefaultAddrPool)
		out.SwarmSubnetSize = int(cl.SubnetSize)
	}
	for _, p := range res.Info.DefaultAddressPools {
		out.DaemonPools = append(out.DaemonPools, DaemonPool{Base: p.Base, Size: p.Size})
	}
	return out, nil
}
