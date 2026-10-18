// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// One-shot stats of a container and what two readings make (plan 06-32, RES-11, RESEARCH
// Pattern 7, E21, Pitfall 14).

func statsFake(t *testing.T, s dockertest.FakeStats) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{{ID: "web-id", Names: []string{"/web"}, Image: "nginx:1.27", State: "running"}},
		Stats:      map[string]dockertest.FakeStats{"web-id": s},
	})
	return newClient(t, fake), fake
}

func TestContainerStatsOnceReadsOneSampleWithTheOneShotQuery(t *testing.T) {
	c, fake := statsFake(t, dockertest.FakeStats{
		OnlineCPUs: 8, CPUPerCall: 500_000_000, SystemPerCall: 4_000_000_000,
		MemUsage: 300 << 20, InactiveFile: 100 << 20, MemLimit: 64 << 30,
		NetRxPerCall: 1001, NetTxPerCall: 501, BlkReadPerCall: 4096, BlkWritePerCall: 8192,
	})
	s, err := c.ContainerStatsOnce(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUTotal != 500_000_000 || s.SystemCPU != 4_000_000_000 || s.OnlineCPUs != 8 {
		t.Errorf("cpu = %d %d %d", s.CPUTotal, s.SystemCPU, s.OnlineCPUs)
	}
	if s.MemUsage != 300<<20 || s.InactiveFile != 100<<20 {
		t.Errorf("memory = %d %d", s.MemUsage, s.InactiveFile)
	}
	if s.NetRx != 1001 || s.NetTx != 501 {
		t.Errorf("network is summed over both interfaces: rx %d tx %d", s.NetRx, s.NetTx)
	}
	if s.BlkRead != 4096 || s.BlkWrite != 8192 {
		t.Errorf("block io = read %d write %d, want the read and write entries only (not total)", s.BlkRead, s.BlkWrite)
	}
	if s.Read.IsZero() {
		t.Error("the sample has no read time")
	}
	qs := fake.StatsQueries()
	if len(qs) != 1 || qs[0] != "one-shot=true&stream=false" {
		t.Errorf("queries = %q, want the one-shot query the agent route allows", qs)
	}
}

func TestContainerStatsOnceTakesTheInactiveFileOfCgroupV1(t *testing.T) {
	c, _ := statsFake(t, dockertest.FakeStats{MemUsage: 200, InactiveFile: 50, CgroupV1: true})
	s, err := c.ContainerStatsOnce(context.Background(), "web-id")
	if err != nil {
		t.Fatal(err)
	}
	if s.InactiveFile != 50 {
		t.Errorf("inactive file = %d, want 50 from total_inactive_file", s.InactiveFile)
	}
}

func TestContainerStatsOnceCountsNullBlockIOAsZero(t *testing.T) {
	c, _ := statsFake(t, dockertest.FakeStats{MemUsage: 10})
	s, err := c.ContainerStatsOnce(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if s.BlkRead != 0 || s.BlkWrite != 0 || s.NetRx != 0 || s.NetTx != 0 {
		t.Errorf("a container with no blkio and no network = %+v, want zeros", s)
	}
}

func TestContainerStatsOnceMovesOnBetweenCalls(t *testing.T) {
	c, _ := statsFake(t, dockertest.FakeStats{CPUPerCall: 100, NetRxPerCall: 10})
	a, err := c.ContainerStatsOnce(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.ContainerStatsOnce(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if b.CPUTotal <= a.CPUTotal || b.NetRx <= a.NetRx || !b.Read.After(a.Read) {
		t.Errorf("the second sample did not move on: %+v then %+v", a, b)
	}
}

func TestContainerStatsOnceOfAnUnknownContainerIsNotFound(t *testing.T) {
	c, _ := statsFake(t, dockertest.FakeStats{})
	if _, err := c.ContainerStatsOnce(context.Background(), "nope"); err == nil || !dockerx.IsNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
}

func TestContainerStatsOnceReportsTheEngineFailure(t *testing.T) {
	c, _ := statsFake(t, dockertest.FakeStats{Error: "cgroup is gone"})
	if _, err := c.ContainerStatsOnce(context.Background(), "web"); err == nil {
		t.Error("an engine failure was swallowed")
	}
}

func f64(v float64) *float64 { return &v }

func TestDeriveStats(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := dockerx.StatsSample{
		Read: t0, CPUTotal: 1_000_000_000, SystemCPU: 10_000_000_000, OnlineCPUs: 4,
		MemUsage: 500, InactiveFile: 100, NetRx: 1000, NetTx: 2000, BlkRead: 4000, BlkWrite: 8000,
	}
	next := func(f func(*dockerx.StatsSample)) dockerx.StatsSample {
		s := base
		s.Read = t0.Add(2 * time.Second)
		f(&s)
		return s
	}
	for _, tc := range []struct {
		name string
		prev *dockerx.StatsSample
		cur  dockerx.StatsSample
		want dockerx.StatsPoint
	}{
		{
			name: "the first sample has memory and no cpu and no rates",
			cur:  base,
			want: dockerx.StatsPoint{At: t0, MemBytes: 400},
		},
		{
			name: "cpu is the docker stats formula, in percent of one core",
			prev: &base,
			// 0.5 s of the container's CPU time over 2 s of the host's: 0.5 / 2 x 4 CPUs x 100 = 100.
			cur: next(func(s *dockerx.StatsSample) { s.CPUTotal += 500_000_000; s.SystemCPU += 2_000_000_000 }),
			want: dockerx.StatsPoint{
				At: t0.Add(2 * time.Second), CPUPercent: f64(100), MemBytes: 400, HasRates: true,
			},
		},
		{
			name: "more than one core shows above 100",
			prev: &base,
			cur:  next(func(s *dockerx.StatsSample) { s.CPUTotal += 3_000_000_000; s.SystemCPU += 4_000_000_000 }),
			want: dockerx.StatsPoint{At: t0.Add(2 * time.Second), CPUPercent: f64(300), MemBytes: 400, HasRates: true},
		},
		{
			name: "an idle container is 0 percent, not missing",
			prev: &base,
			cur:  next(func(s *dockerx.StatsSample) { s.SystemCPU += 2_000_000_000 }),
			want: dockerx.StatsPoint{At: t0.Add(2 * time.Second), CPUPercent: f64(0), MemBytes: 400, HasRates: true},
		},
		{
			name: "no change in the host's CPU time leaves cpu absent",
			prev: &base,
			cur:  next(func(s *dockerx.StatsSample) { s.CPUTotal += 1000 }),
			want: dockerx.StatsPoint{At: t0.Add(2 * time.Second), MemBytes: 400, HasRates: true},
		},
		{
			name: "a counter that went back (the container restarted) leaves cpu absent and rates at zero",
			prev: &base,
			cur: next(func(s *dockerx.StatsSample) {
				s.CPUTotal, s.SystemCPU, s.NetRx, s.NetTx, s.BlkRead, s.BlkWrite = 10, base.SystemCPU+1_000_000_000, 5, 5, 5, 5
			}),
			want: dockerx.StatsPoint{At: t0.Add(2 * time.Second), MemBytes: 400, HasRates: true},
		},
		{
			name: "rates are bytes per second over the gap between the read times",
			prev: &base,
			cur: next(func(s *dockerx.StatsSample) {
				s.NetRx += 2000
				s.NetTx += 600
				s.BlkRead += 10_000
				s.BlkWrite += 3_000
			}),
			want: dockerx.StatsPoint{
				At: t0.Add(2 * time.Second), MemBytes: 400, HasRates: true,
				NetRxRate: 1000, NetTxRate: 300, BlkReadRate: 5000, BlkWriteRate: 1500,
			},
		},
		{
			name: "memory never goes below zero when the cache is bigger than the usage",
			cur:  dockerx.StatsSample{Read: t0, MemUsage: 10, InactiveFile: 50},
			want: dockerx.StatsPoint{At: t0, MemBytes: 0},
		},
		{
			name: "a read time that did not move on has no rates",
			prev: &base,
			cur:  func() dockerx.StatsSample { s := base; s.NetRx += 100; return s }(),
			want: dockerx.StatsPoint{At: t0, MemBytes: 400},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dockerx.DeriveStats(tc.prev, tc.cur)
			if !got.At.Equal(tc.want.At) || got.MemBytes != tc.want.MemBytes || got.HasRates != tc.want.HasRates {
				t.Errorf("point = %+v, want %+v", got, tc.want)
			}
			if (got.CPUPercent == nil) != (tc.want.CPUPercent == nil) {
				t.Fatalf("cpu = %v, want %v", got.CPUPercent, tc.want.CPUPercent)
			}
			if got.CPUPercent != nil && math.Abs(*got.CPUPercent-*tc.want.CPUPercent) > 1e-9 {
				t.Errorf("cpu = %v, want %v", *got.CPUPercent, *tc.want.CPUPercent)
			}
			for name, pair := range map[string][2]float64{
				"net rx": {got.NetRxRate, tc.want.NetRxRate}, "net tx": {got.NetTxRate, tc.want.NetTxRate},
				"block read": {got.BlkReadRate, tc.want.BlkReadRate}, "block write": {got.BlkWriteRate, tc.want.BlkWriteRate},
			} {
				if math.Abs(pair[0]-pair[1]) > 1e-9 {
					t.Errorf("%s rate = %v, want %v", name, pair[0], pair[1])
				}
			}
		})
	}
}
