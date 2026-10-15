// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"time"
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

// ContainerStatsOnce reads one stats sample of a container.
func (c *Client) ContainerStatsOnce(ctx context.Context, id string) (StatsSample, error) {
	return StatsSample{}, errors.New("not implemented")
}

// DeriveStats turns the reading before and the reading now into a point.
func DeriveStats(prev *StatsSample, cur StatsSample) StatsPoint {
	return StatsPoint{}
}
