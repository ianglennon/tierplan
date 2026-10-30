// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"net/http"
	"regexp"
	"time"

	"github.com/moby/moby/api/types/container"
)

var _ = registerFakeHandler((*Fake).serveStats)

// statsRoute matches /containers/{id}/stats.
var statsRoute = regexp.MustCompile(`^/containers/([^/]+)/stats$`)

// statsEpoch is the Read time of the first sample a FakeStats answers.
var statsEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// FakeStats is the resource use a container reports. Every call to its stats route is
// one more sample: each counter grows by its PerCall step, and the sample's Read time
// moves ReadStep on, so a test that reads twice gets rates it can compute by hand. The
// zero value reports a cgroup v2 container with four CPUs, no limit and null block I/O,
// as the engine does in dind (RESEARCH E21).
type FakeStats struct {
	// OnlineCPUs is cpu_stats.online_cpus; zero is 4.
	OnlineCPUs uint32
	// CPUPerCall and SystemPerCall are how much the container's CPU time and the host's
	// grow with every sample, in nanoseconds. A zero SystemPerCall is one second.
	CPUPerCall, SystemPerCall uint64
	// MemUsage and InactiveFile are memory_stats.usage and the inactive file cache, which
	// the engine reports under inactive_file on cgroup v2 and total_inactive_file on v1.
	MemUsage, InactiveFile uint64
	// CgroupV1 reports the inactive file cache under total_inactive_file.
	CgroupV1 bool
	// MemLimit is memory_stats.limit. A container with no limit reports the host's RAM
	// here, so a test sets it to prove the limit is never read from it.
	MemLimit uint64
	// NetRxPerCall and NetTxPerCall grow the received and sent bytes of every sample,
	// split evenly across two interfaces; both zero reports no network at all.
	NetRxPerCall, NetTxPerCall uint64
	// BlkReadPerCall and BlkWritePerCall grow the block I/O bytes. Both zero leaves
	// io_service_bytes_recursive null, as a dind engine does.
	BlkReadPerCall, BlkWritePerCall uint64
	// ReadStep is how far the Read time moves per sample; zero is two seconds.
	ReadStep time.Duration
	// Delay is how long each answer takes, ended early when the caller gives up, so a
	// test can hold requests in flight.
	Delay time.Duration
	// Error makes the route answer 500 with this message.
	Error string
}

// fakeStatsState is what the fake keeps about the stats route.
type fakeStatsState struct {
	calls                 map[string]int
	queries               []string
	inflight, maxInflight int
}

// StatsCalls is how many samples the fake has answered for a container, by ID.
func (f *Fake) StatsCalls(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats.calls[id]
}

// MaxConcurrentStats is the most stats requests the fake has had in flight at once.
func (f *Fake) MaxConcurrentStats() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats.maxInflight
}

// StatsQueries are the raw queries of the stats requests, in order.
func (f *Fake) StatsQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stats.queries...)
}

// serveStats answers GET /containers/{id}/stats with the next sample of the container's
// FakeStats. An ID the daemon does not know is the 404 a real one gives, and a container
// that is not running answers the empty document the engine gives it. PolicyDeny answers the
// agent's refusal.
func (f *Fake) serveStats(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	m := statsRoute.FindStringSubmatch(p)
	if m == nil || r.Method != http.MethodGet {
		return false
	}
	c, known := cfg.resolveContainer(m[1])
	f.mu.Lock()
	st := &f.stats
	if st.calls == nil {
		st.calls = map[string]int{}
	}
	st.queries = append(st.queries, r.URL.RawQuery)
	st.inflight++
	st.maxInflight = max(st.maxInflight, st.inflight)
	f.mu.Unlock()
	f.await.notify()
	defer func() {
		f.mu.Lock()
		st.inflight--
		f.mu.Unlock()
		f.await.notify()
	}()
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + m[1]})
		return true
	}
	s := cfg.Stats[c.ID]
	if s.Delay > 0 {
		select {
		case <-time.After(s.Delay):
		case <-r.Context().Done():
			return true
		}
	}
	if s.Error != "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": s.Error})
		return true
	}
	f.mu.Lock()
	st.calls[c.ID]++
	n := uint64(st.calls[c.ID]) //nolint:gosec // a call count is never negative
	f.mu.Unlock()

	// The engine answers a container that is not running with an empty document: no read
	// time and every counter zero.
	if c.State != "running" {
		writeJSON(w, http.StatusOK, container.StatsResponse{ID: c.ID})
		return true
	}
	writeJSON(w, http.StatusOK, s.sample(c.ID, n))
	return true
}

// sample is the n-th (1-based) sample of a container.
func (s FakeStats) sample(id string, n uint64) container.StatsResponse {
	step := s.ReadStep
	if step == 0 {
		step = 2 * time.Second
	}
	cpus := s.OnlineCPUs
	if cpus == 0 {
		cpus = 4
	}
	sysStep := s.SystemPerCall
	if sysStep == 0 {
		sysStep = 1_000_000_000
	}
	key := "inactive_file"
	if s.CgroupV1 {
		key = "total_inactive_file"
	}
	out := container.StatsResponse{
		ID:   id,
		Read: statsEpoch.Add(time.Duration(n) * step), //nolint:gosec // a call count is far below overflow
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: n * s.CPUPerCall},
			SystemUsage: n * sysStep,
			OnlineCPUs:  cpus,
		},
		MemoryStats: container.MemoryStats{
			Usage: s.MemUsage,
			Limit: s.MemLimit,
			Stats: map[string]uint64{key: s.InactiveFile, "anon": s.MemUsage - min(s.MemUsage, s.InactiveFile)},
		},
	}
	if s.NetRxPerCall > 0 || s.NetTxPerCall > 0 {
		half := func(v uint64) (uint64, uint64) { return v / 2, v - v/2 }
		rx0, rx1 := half(n * s.NetRxPerCall)
		tx0, tx1 := half(n * s.NetTxPerCall)
		out.Networks = map[string]container.NetworkStats{
			"eth0": {RxBytes: rx0, TxBytes: tx0},
			"eth1": {RxBytes: rx1, TxBytes: tx1},
		}
	}
	if s.BlkReadPerCall > 0 || s.BlkWritePerCall > 0 {
		out.BlkioStats.IoServiceBytesRecursive = []container.BlkioStatEntry{
			{Major: 8, Minor: 0, Op: "read", Value: n * s.BlkReadPerCall},
			{Major: 8, Minor: 0, Op: "write", Value: n * s.BlkWritePerCall},
			{Major: 8, Minor: 0, Op: "total", Value: n * (s.BlkReadPerCall + s.BlkWritePerCall)},
		}
	}
	return out
}
