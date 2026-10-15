// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// StatsSample is one raw one-shot stats reading of a container: the counters the engine
// reports, with nothing derived. The server keeps only the last one per container, to
// turn two readings into rates; nothing is stored (D-30).
type StatsSample struct {
	// Read is when the engine took the sample.
	Read time.Time
	// CPUTotal is the container's CPU time in nanoseconds, SystemCPU the host's, and
	// OnlineCPUs how many CPUs the host has online.
	CPUTotal, SystemCPU uint64
	OnlineCPUs          uint32
	// MemUsage is memory_stats.usage and InactiveFile the file cache the engine counts in
	// it (inactive_file on cgroup v2, total_inactive_file on v1).
	MemUsage, InactiveFile uint64
	// NetRx and NetTx are the bytes received and sent, summed over every interface.
	NetRx, NetTx uint64
	// BlkRead and BlkWrite are the block I/O bytes; a null blkio is zero.
	BlkRead, BlkWrite uint64
}

// StatsPoint is what two readings make: the numbers a chart draws.
type StatsPoint struct {
	// At is the Read time of the newer reading.
	At time.Time
	// CPUPercent is percent of one core, nil when it cannot be told from the readings.
	CPUPercent *float64
	// MemBytes is the usage without the file cache the engine can drop.
	MemBytes uint64
	// HasRates is false for the first reading of a run, which has nothing to take a rate
	// from; the four rates are then zero and mean nothing.
	HasRates bool
	// NetRxRate, NetTxRate, BlkReadRate and BlkWriteRate are bytes per second.
	NetRxRate, NetTxRate, BlkReadRate, BlkWriteRate float64
}

// ContainerStatsOnce reads one stats sample of a container, with the stream off and the
// one-shot switch on: the engine answers at once with the counters as they are, and the
// request holds nothing open. The first sample of a container has no CPU time to compare
// with, so the caller keeps the last one and derives a point from two (DeriveStats). A
// container that does not exist is an error IsNotFound recognises.
func (c *Client) ContainerStatsOnce(ctx context.Context, id string) (StatsSample, error) {
	res, err := c.cur().ContainerStats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return StatsSample{}, fmt.Errorf("read stats of a container on %s: %w", c.t.Describe(), err)
	}
	defer func() { _ = res.Body.Close() }()
	var in container.StatsResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxStatsBody)).Decode(&in); err != nil {
		return StatsSample{}, fmt.Errorf("decode stats of a container on %s: %w", c.t.Describe(), err)
	}
	return sampleOf(in), nil
}

// maxStatsBody is the most bytes of a stats answer that are read; a real one is a few KiB.
const maxStatsBody = 1 << 20

// sampleOf keeps the counters of the engine's stats document that a chart needs.
func sampleOf(in container.StatsResponse) StatsSample {
	s := StatsSample{
		Read:     in.Read,
		CPUTotal: in.CPUStats.CPUUsage.TotalUsage, SystemCPU: in.CPUStats.SystemUsage, OnlineCPUs: in.CPUStats.OnlineCPUs,
		MemUsage: in.MemoryStats.Usage,
	}
	// cgroup v1 reports the cache under total_inactive_file (inactive_file there leaves out
	// the children); cgroup v2 has inactive_file alone.
	if v, ok := in.MemoryStats.Stats["total_inactive_file"]; ok {
		s.InactiveFile = v
	} else {
		s.InactiveFile = in.MemoryStats.Stats["inactive_file"]
	}
	for _, n := range in.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}
	// blkio_stats fields are null on some engines (RESEARCH E21): a nil slice adds nothing.
	for _, e := range in.BlkioStats.IoServiceBytesRecursive {
		switch {
		case strings.EqualFold(e.Op, "read"):
			s.BlkRead += e.Value
		case strings.EqualFold(e.Op, "write"):
			s.BlkWrite += e.Value
		}
	}
	return s
}

// DeriveStats turns the reading before and the reading now into a point. CPU percent is
// the docker stats formula, the container's CPU time over the host's, times the online
// CPUs, times 100, so 100 is one core. It is absent for the first reading, when the host's
// CPU time did not move and when a counter went back (the container restarted); an idle
// container is a genuine 0. Memory is the usage without the file cache. Rates are the
// growth of the byte counters per second of the engine's own read times; a counter that
// went back counts as no growth. The limit is deliberately not here: memory_stats.limit is
// the host's RAM when the container has none (RESEARCH E21).
func DeriveStats(prev *StatsSample, cur StatsSample) StatsPoint {
	p := StatsPoint{At: cur.Read, MemBytes: cur.MemUsage - min(cur.MemUsage, cur.InactiveFile)}
	if prev == nil {
		return p
	}
	if cur.SystemCPU > prev.SystemCPU && cur.CPUTotal >= prev.CPUTotal {
		cpuDelta := float64(cur.CPUTotal - prev.CPUTotal)
		sysDelta := float64(cur.SystemCPU - prev.SystemCPU)
		pct := cpuDelta / sysDelta * float64(cur.OnlineCPUs) * 100
		p.CPUPercent = &pct
	}
	secs := cur.Read.Sub(prev.Read).Seconds()
	if secs <= 0 {
		return p
	}
	p.HasRates = true
	rate := func(now, before uint64) float64 {
		if now < before {
			return 0
		}
		return float64(now-before) / secs
	}
	p.NetRxRate, p.NetTxRate = rate(cur.NetRx, prev.NetRx), rate(cur.NetTx, prev.NetTx)
	p.BlkReadRate, p.BlkWriteRate = rate(cur.BlkRead, prev.BlkRead), rate(cur.BlkWrite, prev.BlkWrite)
	return p
}
