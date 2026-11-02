// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import (
	"cmp"
	"encoding/binary"
	"net/netip"
	"slices"
)

// interval is a half-open run [lo, hi) of indexes: block numbers of a pool, or addresses.
type interval struct{ lo, hi uint64 }

// v4 is the address as a number. It is only meaningful for an IPv4 address.
func v4(addr netip.Addr) uint64 {
	b := addr.As4()
	return uint64(binary.BigEndian.Uint32(b[:]))
}

// addrOf is the IPv4 address with the number n. n must be below 2^32.
func addrOf(n uint64) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(n >> 24 & 0xff), byte(n >> 16 & 0xff), byte(n >> 8 & 0xff), byte(n & 0xff)})
}

// span is the half-open run of addresses a prefix covers, as numbers. The prefix must be an
// IPv4 prefix; host bits are ignored.
func span(p netip.Prefix) (lo, hi uint64) {
	p = p.Masked()
	lo = v4(p.Addr())
	return lo, lo + 1<<(32-p.Bits())
}

// isV4 reports whether p is a valid IPv4 prefix, the only kind counted.
func isV4(p netip.Prefix) bool { return p.IsValid() && p.Addr().Is4() }

// merge sorts the intervals and joins those that overlap or touch, dropping empty ones. It
// reorders iv in place and returns a slice of it.
func merge(iv []interval) []interval {
	iv = slices.DeleteFunc(iv, func(x interval) bool { return x.hi <= x.lo })
	slices.SortFunc(iv, func(a, b interval) int { return cmp.Or(cmp.Compare(a.lo, b.lo), cmp.Compare(a.hi, b.hi)) })
	out := iv[:0]
	for _, x := range iv {
		if n := len(out); n > 0 && x.lo <= out[n-1].hi {
			out[n-1].hi = max(out[n-1].hi, x.hi)
			continue
		}
		out = append(out, x)
	}
	return out
}

// unionLen is how many indexes the intervals cover together.
func unionLen(iv []interval) uint64 {
	var n uint64
	for _, x := range merge(iv) {
		n += x.hi - x.lo
	}
	return n
}
