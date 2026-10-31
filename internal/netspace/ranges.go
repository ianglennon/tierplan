// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

import "net/netip"

// CommonRanges are the ranges many home networks and VPNs use, which a subnet is warned about
// (D-16 item 4). swarmPoolIsTenSlashEight adds 10.0.0.0/8, which is listed only when it is the
// swarm's own pool, as D-16 words it.
//
//   - 192.168.0.0/16: the default of nearly every home router and many office LANs.
//   - 172.16.0.0/16, 172.17.0.0/16 and 172.31.0.0/16: the corners of 172.16.0.0/12 that VPN
//     clients, cloud VPCs and Docker's own default bridge (172.17.0.0/16) take first. The rest
//     of the /12 is not listed, so the pool Docker hands out from stays usable.
//   - 10.0.0.0/8: the corporate and VPN range, listed only for a swarm whose pool it is.
func CommonRanges(swarmPoolIsTenSlashEight bool) []netip.Prefix {
	out := []netip.Prefix{
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/16"),
		netip.MustParsePrefix("172.17.0.0/16"),
		netip.MustParsePrefix("172.31.0.0/16"),
	}
	if swarmPoolIsTenSlashEight {
		out = append(out, netip.MustParsePrefix("10.0.0.0/8"))
	}
	return out
}
