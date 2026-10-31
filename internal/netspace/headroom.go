// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

// PresetMasks are the subnet sizes the picker offers, from the largest to the smallest.
var PresetMasks = []int{24, 25, 26, 27, 28}

// reservedAddrs are the addresses of a subnet no task can use: the network, the broadcast and
// the gateway address.
const reservedAddrs = 3

// Usable is how many addresses of a subnet of the mask a task or service can take: the size
// less the network, broadcast and gateway addresses. A /24 gives 253.
func Usable(mask int) int {
	if mask < 0 || mask > 32 {
		return 0
	}
	return max(1<<(32-mask)-reservedAddrs, 0)
}

// Needed is how many addresses a swarm overlay needs for the given number of tasks, services
// and nodes: ceil((2 x tasks + services + nodes) x 1.25) + 3. Twice the tasks covers a
// start-first update, where old and new tasks run together; a service takes a virtual IP; a
// node takes an endpoint for the load balancer; the 1.25 is room to scale; the 3 are the
// reserved addresses. Checked against a 3-replica service that used 8 addresses (RESEARCH F3).
// The arithmetic is on integers so the ceiling is exact.
func Needed(tasks, services, nodes int) int {
	base := 2*tasks + services + nodes
	return (base*5+3)/4 + reservedAddrs
}

// RecommendMask is the smallest preset subnet whose usable addresses hold needed, and false
// when not even a /24 does.
func RecommendMask(needed int) (int, bool) {
	for i := len(PresetMasks) - 1; i >= 0; i-- {
		if m := PresetMasks[i]; Usable(m) >= needed {
			return m, true
		}
	}
	return 0, false
}
