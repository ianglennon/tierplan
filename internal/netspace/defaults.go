// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import "net/netip"

// The meter thresholds, in whole percent of a pool's blocks in use (D-03). They are fixed on
// purpose: a pool at 70% is getting full, and at 90% it is almost full.
const (
	ThresholdWarn = 70
	ThresholdFull = 90
)

// DefaultSwarmSubnetSize is the mask a swarm gives each network when it was initialised
// without --default-addr-pool-mask-length.
const DefaultSwarmSubnetSize = 24

// DefaultSwarmPool is the pool a swarm uses when it was initialised without
// --default-addr-pool: its /info reports none, and the swarm allocates from 10.0.0.0/8 at
// DefaultSwarmSubnetSize (RESEARCH F2, reproduced on Engine 29.8.2).
var DefaultSwarmPool = netip.MustParsePrefix("10.0.0.0/8")

// DockerDefaultPools are the local pools a standalone daemon uses when it has no
// default-address-pools setting: seven pools, in the order the daemon tries them. Source:
// moby daemon/libnetwork/ipamutils/utils.go (RESEARCH F1). They are Docker's documented
// defaults and not a measurement, so a caller that shows them says so (Assumption A5: the
// list can change between engine releases; the reading of /info DefaultAddressPools
// replaces it whenever the daemon reports one).
var DockerDefaultPools = []Pool{
	{Base: netip.MustParsePrefix("172.17.0.0/16"), Size: 16},
	{Base: netip.MustParsePrefix("172.18.0.0/16"), Size: 16},
	{Base: netip.MustParsePrefix("172.19.0.0/16"), Size: 16},
	{Base: netip.MustParsePrefix("172.20.0.0/14"), Size: 16},
	{Base: netip.MustParsePrefix("172.24.0.0/14"), Size: 16},
	{Base: netip.MustParsePrefix("172.28.0.0/14"), Size: 16},
	{Base: netip.MustParsePrefix("192.168.0.0/16"), Size: 20},
}
