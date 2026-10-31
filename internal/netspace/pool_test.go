// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

import (
	"net/netip"
	"testing"
)

// Pool arithmetic (plan 10-01, NET-02, D-03, Pitfall 4). Every case is a table row, with the
// answer worked out by hand in its name.

func pfx(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("bad prefix %q in a test: %v", s, err)
		}
		out = append(out, p)
	}
	return out
}

func pool(base string, size int) Pool { return Pool{Base: netip.MustParsePrefix(base), Size: size} }

func TestPoolUsed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		pool    Pool
		subnets []string
		want    uint64
	}{
		{"nothing in the pool", pool("10.0.0.0/16", 24), nil, 0},
		{"adjacent blocks each count once and never merge into one", pool("10.0.0.0/16", 24), []string{"10.0.1.0/24", "10.0.2.0/24"}, 2},
		{"a subnet equal to one block counts that block once", pool("10.0.0.0/16", 24), []string{"10.0.7.0/24"}, 1},
		{"two small subnets in different blocks use two", pool("10.0.0.0/16", 26), []string{"10.0.5.0/28", "10.0.5.64/28"}, 2},
		{"two small subnets in one block use it once", pool("10.0.0.0/16", 26), []string{"10.0.5.0/28", "10.0.5.16/28"}, 1},
		{"a /16 in a /24-size pool uses 256", pool("10.0.0.0/8", 24), []string{"10.1.0.0/16"}, 256},
		{"host bits are ignored: 10.0.3.128/23 is 10.0.2.0/23", pool("10.0.0.0/16", 24), []string{"10.0.3.128/23"}, 2},
		{"a subnet that straddles two blocks uses both", pool("10.0.0.0/16", 25), []string{"10.0.0.64/26", "10.0.0.128/25"}, 2},
		{"a /25 over two /26 blocks uses both", pool("10.0.0.0/16", 26), []string{"10.0.0.64/25"}, 2},
		{"a subnet outside the base changes nothing", pool("10.0.0.0/16", 24), []string{"10.1.0.0/24", "192.168.1.0/24"}, 0},
		{"an IPv6 subnet changes nothing", pool("10.0.0.0/16", 24), []string{"fd00::/64", "10.0.1.0/24"}, 1},
		{"a duplicate subnet changes nothing", pool("10.0.0.0/16", 24), []string{"10.0.1.0/24", "10.0.1.0/24", "10.0.1.0/25"}, 1},
		{"a subnet wider than the pool uses every block", pool("10.0.0.0/16", 24), []string{"10.0.0.0/8"}, 256},
		{"a subnet that starts before the base and overlaps its start", pool("10.0.0.0/16", 24), []string{"9.255.0.0/8"}, 0},
		{"a subnet wider than the base from the same address", pool("10.1.0.0/16", 24), []string{"10.0.0.0/8"}, 256},
		{"a pool at /30 with one /9 uses 2^21 blocks without enumerating them", pool("10.0.0.0/8", 30), []string{"10.0.0.0/9"}, 2097152},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.pool.Used(pfx(t, tc.subnets...)); got != tc.want {
				t.Errorf("Used = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPoolCapacity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pool Pool
		want uint64
	}{
		{pool("10.0.0.0/8", 24), 65536},
		{pool("10.0.0.0/8", 30), 4194304},
		{pool("10.60.0.0/16", 26), 1024},
		{pool("172.20.0.0/14", 16), 4},
		{pool("192.168.0.0/16", 20), 16},
		{pool("10.0.0.0/24", 24), 1},
		{pool("fd00::/48", 64), 0},
		{pool("10.0.0.0/24", 23), 0},
		{pool("10.0.0.0/8", 31), 0},
	} {
		if got := tc.pool.Capacity(); got != tc.want {
			t.Errorf("Capacity of %s at /%d = %d, want %d", tc.pool.Base, tc.pool.Size, got, tc.want)
		}
	}
}

func TestPoolLargestFree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		pool    Pool
		subnets []string
		want    string
		blocks  uint64
	}{
		{"an empty pool is its own base", pool("10.0.0.0/22", 24), nil, "10.0.0.0/22", 4},
		{"the first block used leaves the upper half", pool("10.0.0.0/22", 24), []string{"10.0.0.0/24"}, "10.0.2.0/23", 2},
		{"a used block in the upper half leaves the lower half", pool("10.0.0.0/22", 24), []string{"10.0.2.0/24"}, "10.0.0.0/23", 2},
		{"a used middle block leaves the upper half, which is aligned", pool("10.0.0.0/22", 24), []string{"10.0.1.0/24"}, "10.0.2.0/23", 2},
		{"a free run that is not aligned yields its largest aligned part, lowest first", pool("10.0.0.0/22", 24), []string{"10.0.0.0/24", "10.0.3.0/24"}, "10.0.1.0/24", 1},
		{"a tie goes to the lowest address", pool("10.0.0.0/21", 24), []string{"10.0.2.0/23", "10.0.6.0/23"}, "10.0.0.0/23", 2},
		{"a bigger run later beats a smaller one earlier", pool("10.0.0.0/21", 24), []string{"10.0.2.0/23"}, "10.0.4.0/22", 4},
		{"a fresh swarm: ingress and three overlays leave the upper half", pool("10.0.0.0/8", 24), []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"}, "10.128.0.0/9", 32768},
		{"a small subnet uses its whole block", pool("10.0.0.0/22", 24), []string{"10.0.0.5/32"}, "10.0.2.0/23", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, blocks, ok := tc.pool.LargestFree(pfx(t, tc.subnets...))
			if !ok || got.String() != tc.want || blocks != tc.blocks {
				t.Errorf("LargestFree = %s, %d, %v; want %s, %d, true", got, blocks, ok, tc.want, tc.blocks)
			}
		})
	}

	t.Run("a full pool has none", func(t *testing.T) {
		t.Parallel()
		got, blocks, ok := pool("10.0.0.0/22", 24).LargestFree(pfx(t, "10.0.0.0/22"))
		if ok || got.IsValid() || blocks != 0 {
			t.Errorf("LargestFree of a full pool = %s, %d, %v; want none", got, blocks, ok)
		}
	})
	t.Run("an invalid pool has none", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := pool("fd00::/48", 64).LargestFree(nil); ok {
			t.Error("LargestFree of an IPv6 pool found a block")
		}
	})
}

func TestStateOfThresholds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		used, capacity uint64
		percent        int
		state          State
	}{
		{0, 0, 0, StateUnknown},
		{5, 0, 0, StateUnknown},
		{0, 10000, 0, StateNormal},
		{6999, 10000, 69, StateNormal},
		{7000, 10000, 70, StateGettingFull},
		{8999, 10000, 89, StateGettingFull},
		{9000, 10000, 90, StateAlmostFull},
		{9999, 10000, 99, StateAlmostFull},
		{10000, 10000, 100, StateAlmostFull},
		{717, 1024, 70, StateGettingFull},
		{716, 1024, 69, StateNormal},
	} {
		if got := StateOf(tc.used, tc.capacity); got != tc.state {
			t.Errorf("StateOf(%d, %d) = %s, want %s", tc.used, tc.capacity, got, tc.state)
		}
		if got := PercentOf(tc.used, tc.capacity); got != tc.percent {
			t.Errorf("PercentOf(%d, %d) = %d, want %d", tc.used, tc.capacity, got, tc.percent)
		}
	}
}

// TestStateNeverDisagreesWithThePercent runs every used count of a pool of 1024 blocks: the
// state is decided on the exact ratio, and the percent a person reads is that ratio rounded
// down, so 70 or more is never normal and 69 or less is never getting full.
func TestStateNeverDisagreesWithThePercent(t *testing.T) {
	t.Parallel()
	const capacity = 1024
	for used := uint64(0); used <= capacity; used++ {
		pc, st := PercentOf(used, capacity), StateOf(used, capacity)
		switch {
		case pc >= ThresholdFull && st != StateAlmostFull,
			pc >= ThresholdWarn && pc < ThresholdFull && st != StateGettingFull,
			pc < ThresholdWarn && st != StateNormal:
			t.Fatalf("used %d of %d reads %d%% but is %s", used, capacity, pc, st)
		}
	}
}

func TestPoolValid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pool Pool
		want bool
	}{
		{"a swarm default", pool("10.0.0.0/8", 24), true},
		{"a size equal to the base mask", pool("10.0.0.0/24", 24), true},
		{"the smallest block there is", pool("10.0.0.0/8", 30), true},
		{"an IPv6 base", pool("fd00::/48", 64), false},
		{"a size smaller than the base mask", pool("10.0.0.0/24", 16), false},
		{"a size above 30", pool("10.0.0.0/8", 31), false},
		{"a zero pool", Pool{}, false},
	} {
		if got := tc.pool.Valid(); got != tc.want {
			t.Errorf("%s: Valid = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStat(t *testing.T) {
	t.Parallel()
	p := pool("10.0.0.0/24", 26)
	for _, tc := range []struct {
		subnets []string
		used    uint64
		percent int
		state   State
		free    string
	}{
		{nil, 0, 0, StateNormal, "10.0.0.0/24"},
		{[]string{"10.0.0.0/26"}, 1, 25, StateNormal, "10.0.0.128/25"},
		{[]string{"10.0.0.0/26", "10.0.0.64/26", "10.0.0.128/26"}, 3, 75, StateGettingFull, "10.0.0.192/26"},
		{[]string{"10.0.0.0/24"}, 4, 100, StateAlmostFull, ""},
	} {
		st := Stat(p, pfx(t, tc.subnets...))
		free := ""
		if st.LargestFree.IsValid() {
			free = st.LargestFree.String()
		}
		if st.Used != tc.used || st.Capacity != 4 || st.Percent != tc.percent || st.State != tc.state || free != tc.free {
			t.Errorf("Stat with %v = %+v, want used %d, %d%%, %s, free %q", tc.subnets, st, tc.used, tc.percent, tc.state, tc.free)
		}
	}
	if st := Stat(pool("fd00::/48", 64), nil); st.State != StateUnknown || st.Capacity != 0 {
		t.Errorf("Stat of an invalid pool = %+v, want unknown with no capacity", st)
	}
	if st := Stat(pool("10.0.0.77/24", 26), nil); st.Pool.Base.String() != "10.0.0.0/24" {
		t.Errorf("Stat keeps the base %s, want it with host bits cleared", st.Pool.Base)
	}
}
