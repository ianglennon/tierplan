// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import (
	"errors"
	"net/netip"
	"testing"
)

// The lowest free block (plan 10-13, NET-05, D-15). Each case is a table row with the answer
// worked out by hand in its name.

func TestLowestFreePacking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		occupied []string
		mask     int
		want     string
	}{
		{"an empty pool gives its first block", nil, 26, "10.0.0.0/26"},
		{"a taken /24 pushes a /26 into the next /24", []string{"10.0.0.0/24"}, 26, "10.0.1.0/26"},
		{"the lower half of a /24 taken leaves the third /26", []string{"10.0.0.0/25"}, 26, "10.0.0.128/26"},
		{"a /28 blocks its whole /26", []string{"10.0.0.32/28"}, 26, "10.0.0.64/26"},
		{"a /24 skips a /26 inside its first block", []string{"10.0.0.0/26"}, 24, "10.0.1.0/24"},
		{"blocks are found past a run of taken ones", []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/26"}, 26, "10.0.2.64/26"},
		{"the same run gives the next /24 for a /24", []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/26"}, 24, "10.0.3.0/24"},
		{"a gap below a taken block is used", []string{"10.0.1.0/24"}, 24, "10.0.0.0/24"},
		{"IPv6 subnets change nothing", []string{"fd00::/64"}, 24, "10.0.0.0/24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, p, err := LowestFree([]Pool{pool("10.0.0.0/16", 24)}, pfx(t, tc.occupied...), nil, tc.mask)
			if err != nil {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if got.String() != tc.want || p != pool("10.0.0.0/16", 24) {
				t.Errorf("got %v in %v, want %s in 10.0.0.0/16", got, p, tc.want)
			}
		})
	}
}

func TestLowestFreeAvoid(t *testing.T) {
	t.Parallel()
	pools := []Pool{pool("10.0.0.0/16", 24)}
	got, _, err := LowestFree(pools, nil, pfx(t, "10.0.0.0/24", "10.0.1.0/24"), 24)
	if err != nil || got.String() != "10.0.2.0/24" {
		t.Fatalf("avoid: got %v, %v, want 10.0.2.0/24", got, err)
	}
	// Avoid and occupied together.
	got, _, err = LowestFree(pools, pfx(t, "10.0.0.0/24"), pfx(t, "10.0.1.0/24"), 24)
	if err != nil || got.String() != "10.0.2.0/24" {
		t.Fatalf("occupied and avoid: got %v, %v, want 10.0.2.0/24", got, err)
	}
}

func TestLowestFreeNoBlock(t *testing.T) {
	t.Parallel()
	two := []Pool{pool("10.0.0.0/24", 24), pool("10.5.0.0/24", 24)}

	// The second pool is used when the first is full.
	got, p, err := LowestFree(two, pfx(t, "10.0.0.0/24"), nil, 24)
	if err != nil || got.String() != "10.5.0.0/24" || p != two[1] {
		t.Fatalf("second pool: got %v in %v, %v, want 10.5.0.0/24 in the second pool", got, p, err)
	}
	// Both full.
	if _, _, err := LowestFree(two, pfx(t, "10.0.0.0/24", "10.5.0.0/25", "10.5.0.128/25"), nil, 24); !errors.Is(err, ErrNoFreeBlock) {
		t.Fatalf("full pools: err = %v, want ErrNoFreeBlock", err)
	}
	// A mask wider than the pool base skips the pool.
	if _, _, err := LowestFree([]Pool{pool("10.0.0.0/24", 24)}, nil, nil, 16); !errors.Is(err, ErrNoFreeBlock) {
		t.Fatalf("a /16 in a /24 pool: err = %v, want ErrNoFreeBlock", err)
	}
	// It is skipped, not fatal: a later pool that can hold it is used.
	got, _, err = LowestFree([]Pool{pool("10.0.0.0/24", 24), pool("10.8.0.0/16", 24)}, nil, nil, 16)
	if err != nil || got.String() != "10.8.0.0/16" {
		t.Fatalf("a /16 past a small pool: got %v, %v, want 10.8.0.0/16", got, err)
	}
	// No pools, an IPv6 pool and an impossible mask have no block.
	for name, pools := range map[string][]Pool{"none": nil, "ipv6": {{Base: netip.MustParsePrefix("fd00::/48"), Size: 64}}} {
		if _, _, err := LowestFree(pools, nil, nil, 24); !errors.Is(err, ErrNoFreeBlock) {
			t.Errorf("%s: err = %v, want ErrNoFreeBlock", name, err)
		}
	}
	if _, _, err := LowestFree(two, nil, nil, 33); !errors.Is(err, ErrNoFreeBlock) {
		t.Errorf("a /33: err = %v, want ErrNoFreeBlock", err)
	}
}

func TestLowestFreeLargePool(t *testing.T) {
	t.Parallel()
	// Half of a /8 taken: the answer is the first /26 of the upper half, found by one jump and
	// not by stepping over eight million blocks.
	got, _, err := LowestFree([]Pool{pool("10.0.0.0/8", 24)}, pfx(t, "10.0.0.0/9"), nil, 26)
	if err != nil || got.String() != "10.128.0.0/26" {
		t.Fatalf("got %v, %v, want 10.128.0.0/26", got, err)
	}
	// Many scattered subnets: still one pass over the occupied runs.
	var occ []netip.Prefix
	for i := range 2000 {
		occ = append(occ, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 0}), 24))
	}
	got, _, err = LowestFree([]Pool{pool("10.0.0.0/8", 24)}, occ, nil, 24)
	if err != nil || got.String() != "10.7.208.0/24" {
		t.Fatalf("got %v, %v, want 10.7.208.0/24", got, err)
	}
	// A subnet that straddles the end of the pool leaves nothing above it.
	if _, _, err := LowestFree([]Pool{pool("10.0.0.0/24", 24)}, pfx(t, "10.0.0.0/16"), nil, 28); !errors.Is(err, ErrNoFreeBlock) {
		t.Fatalf("a /16 over a /24 pool: err = %v, want ErrNoFreeBlock", err)
	}
}
