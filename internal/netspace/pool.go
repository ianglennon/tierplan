// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

import (
	"math/bits"
	"net/netip"
)

// maxPoolSize is the largest per-network mask a pool can have: a /30 is the smallest block
// Docker hands out with room for a gateway and a container.
const maxPoolSize = 30

// Pool is an address pool: Base is the range, Size the mask of each network allocated from
// it. A 10.0.0.0/8 pool with Size 24 is 65,536 blocks of /24.
type Pool struct {
	Base netip.Prefix
	Size int
}

// Valid reports whether the pool can be counted: an IPv4 base, and a Size between the
// base's mask and 30. A pool read from an engine is checked before it is used, because the
// numbers come from a host Coaming does not control (T-10-03).
func (p Pool) Valid() bool {
	return isV4(p.Base) && p.Size >= p.Base.Bits() && p.Size <= maxPoolSize
}

// base is the base with its host bits cleared.
func (p Pool) base() netip.Prefix { return p.Base.Masked() }

// Capacity is how many blocks the pool holds: 2^(Size - base mask). An invalid pool holds
// none.
func (p Pool) Capacity() uint64 {
	if !p.Valid() {
		return 0
	}
	return 1 << (p.Size - p.Base.Bits())
}

// blockRange maps a subnet to the half-open run of block indexes it touches, clamped to the
// pool. A block is touched when any address of it is inside the subnet, so a /28 inside a
// /26 block touches that one block and a /16 over a /24 pool touches 256. A subnet outside
// the base is an empty run (hi <= lo). The pool and the subnet must be IPv4.
func (p Pool) blockRange(s netip.Prefix) (lo, hi uint64) {
	bLo, bHi := span(p.base())
	sLo, sHi := span(s)
	sLo, sHi = max(sLo, bLo), min(sHi, bHi)
	if sLo >= sHi {
		return 0, 0
	}
	size := uint64(1) << (32 - p.Size)
	return (sLo - bLo) / size, (sHi - bLo + size - 1) / size
}

// touched is the runs of blocks that the IPv4 subnets touch, one per subnet that touches any,
// not yet merged.
func (p Pool) touched(subnets []netip.Prefix) []interval {
	var iv []interval
	for _, s := range subnets {
		if !isV4(s) {
			continue
		}
		if lo, hi := p.blockRange(s); hi > lo {
			iv = append(iv, interval{lo, hi})
		}
	}
	return iv
}

// Used is how many blocks of the pool the subnets touch together. A subnet that straddles two
// blocks uses both, two subnets in one block use it once, and adjacent blocks are counted
// one by one. IPv6 subnets, invalid ones and ones outside the base change nothing. The work
// is proportional to the number of subnets, never to the size of the pool.
func (p Pool) Used(subnets []netip.Prefix) uint64 {
	if !p.Valid() {
		return 0
	}
	return unionLen(p.touched(subnets))
}

// LargestFree is the largest pool-aligned block of power-of-two size made only of free
// blocks: the prefix, how many blocks of the pool's Size it holds, and true. On a tie the
// lowest address wins. It is false when every block is used or the pool is invalid.
func (p Pool) LargestFree(subnets []netip.Prefix) (netip.Prefix, uint64, bool) {
	if !p.Valid() {
		return netip.Prefix{}, 0, false
	}
	capacity := p.Capacity()
	var bestStart, bestLen uint64
	gap := func(a, b uint64) {
		// The largest aligned power-of-two run inside [a, b), alignment relative to the base.
		for k := bits.Len64(b-a) - 1; k >= 0; k-- {
			n := uint64(1) << k
			if n <= bestLen {
				return
			}
			if s := (a + n - 1) &^ (n - 1); s+n <= b {
				bestStart, bestLen = s, n
				return
			}
		}
	}
	var next uint64
	for _, r := range merge(p.touched(subnets)) {
		if r.lo > next {
			gap(next, r.lo)
		}
		next = r.hi
	}
	if next < capacity {
		gap(next, capacity)
	}
	if bestLen == 0 {
		return netip.Prefix{}, 0, false
	}
	size := uint64(1) << (32 - p.Size)
	lo, _ := span(p.base())
	start := lo + bestStart*size
	return netip.PrefixFrom(addrOf(start), p.Size-(bits.Len64(bestLen)-1)), bestLen, true
}

// State is how full a pool is.
type State string

// The states of a meter (D-03).
const (
	StateNormal      State = "normal"
	StateGettingFull State = "getting_full"
	StateAlmostFull  State = "almost_full"
	// StateUnknown is a pool that could not be measured, or has no blocks.
	StateUnknown State = "unknown"
)

// StateOf decides the state from the exact ratio used/capacity, never from the rounded
// percent: 70% or more is getting full, 90% or more almost full. A capacity of zero is
// unknown.
func StateOf(used, capacity uint64) State {
	switch {
	case capacity == 0:
		return StateUnknown
	case used*100 >= capacity*ThresholdFull:
		return StateAlmostFull
	case used*100 >= capacity*ThresholdWarn:
		return StateGettingFull
	}
	return StateNormal
}

// PercentOf is used/capacity as a whole percent, rounded down, so 0.6999 reads 69 and is
// normal, and what a person reads never disagrees with the state. A capacity of zero is 0.
func PercentOf(used, capacity uint64) int {
	if capacity == 0 {
		return 0
	}
	return int(min(used*100/capacity, 100)) //nolint:gosec // G115: at most 100 on this line
}

// PoolStat is a pool and what is in use of it.
type PoolStat struct {
	Pool Pool
	// Used and Capacity are counted in blocks of the pool's Size.
	Used, Capacity uint64
	Percent        int
	State          State
	// LargestFree is the largest free aligned block and LargestFreeBlocks how many pool
	// blocks it holds; LargestFree is the zero prefix when no block is free.
	LargestFree       netip.Prefix
	LargestFreeBlocks uint64
}

// Stat measures the pool against the subnets of the networks in it. Its base is returned
// with host bits cleared. An invalid pool has capacity 0 and state unknown.
func Stat(p Pool, subnets []netip.Prefix) PoolStat {
	if p.Base.IsValid() {
		p.Base = p.Base.Masked()
	}
	st := PoolStat{Pool: p, Capacity: p.Capacity(), State: StateUnknown}
	if st.Capacity == 0 {
		return st
	}
	st.Used = p.Used(subnets)
	st.Percent = PercentOf(st.Used, st.Capacity)
	st.State = StateOf(st.Used, st.Capacity)
	st.LargestFree, st.LargestFreeBlocks, _ = p.LargestFree(subnets)
	return st
}
