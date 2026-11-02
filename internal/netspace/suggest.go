// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import (
	"errors"
	"net/netip"
	"sort"
)

// ErrNoFreeBlock is returned by LowestFree when no pool has a free block of the size asked for.
var ErrNoFreeBlock = errors.New("no free block of that size is left in the pools")

// LowestFree is the lowest block of the given mask that lies inside one of the pools and
// touches no subnet of occupied or avoid, with the pool it was found in. Pools are tried in
// the order given and blocks are aligned to the mask, so the answer packs low and leaves the
// largest free runs above it. IPv6 and invalid prefixes in occupied and avoid change nothing,
// and a pool whose base is IPv6 or narrower than the mask is skipped. ErrNoFreeBlock is
// returned when no pool has a block free.
//
// The search jumps over each occupied run instead of stepping block by block: a /8 pool
// asked for /28 blocks holds a million candidates, and the work here is proportional to the
// number of occupied subnets.
func LowestFree(pools []Pool, occupied, avoid []netip.Prefix, mask int) (netip.Prefix, Pool, error) {
	if mask < 0 || mask > 32 {
		return netip.Prefix{}, Pool{}, ErrNoFreeBlock
	}
	var iv []interval
	for _, list := range [][]netip.Prefix{occupied, avoid} {
		for _, s := range list {
			if !isV4(s) {
				continue
			}
			lo, hi := span(s)
			iv = append(iv, interval{lo, hi})
		}
	}
	busy := merge(iv)
	size := uint64(1) << (32 - mask)
	for _, p := range pools {
		if !isV4(p.Base) || p.Base.Bits() > mask {
			continue
		}
		lo, hi := span(p.base())
		cand := lo
		for cand+size <= hi {
			// The first busy run that ends after the candidate starts.
			i := sort.Search(len(busy), func(k int) bool { return busy[k].hi > cand })
			if i == len(busy) || busy[i].lo >= cand+size {
				return netip.PrefixFrom(addrOf(cand), mask), p, nil
			}
			// Jump to the first aligned block past the run.
			cand = (busy[i].hi + size - 1) / size * size
		}
	}
	return netip.Prefix{}, Pool{}, ErrNoFreeBlock
}
