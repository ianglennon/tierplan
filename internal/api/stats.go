// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

var statsContainerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// StatsSource is what the stats stream reads: the aggregate of an environment, to find
// the node of a container, and that node's engine. *envs.Aggregator implements it.
type StatsSource interface {
	snapshotReader
	statsNodes
	// SwarmNodeTargets maps the swarm nodes Bayplan can reach now to the node the hub asks
	// (the service, services-list and node streams, plan 06-33).
	SwarmNodeTargets(ctx context.Context, envID string) (map[string]envs.NodeRef, error)
}

// StatsDeps are the dependencies of the container stats stream.
type StatsDeps struct {
	Aggregator StatsSource
	// Clusters reads the service, tasks and nodes of a swarm for the service, services-list
	// and node streams (06-33).
	Clusters *envs.Clusters
	// Hub samples the containers that are watched. One hub serves every viewer; the
	// server builds it once and closes it on shutdown. Required.
	Hub *StatsHub
	// Heartbeat is how often an idle stream gets a comment line and has its access checked
	// again. Zero means 15 seconds; tests shorten it.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent stats streams per user. Zero means 8.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour.
	MaxLifetime time.Duration
	// RoundWait is the longest a service, list or node stream waits for a container that is
	// slow before it sends the round without it. Zero means 3 seconds.
	RoundWait time.Duration
	// Log receives server-side failures, never a sample. Nil means slog.Default().
	Log *slog.Logger
}

// RegisterStats registers the live stats stream of a container under /api/env/{envId}/,
// and the streams of a service, of the rows of a services list or a containers list and of
// the nodes of a swarm (plan 06-33). They are mounted through Router.Guarded because the
// contract cannot describe a stream (D-47); the container streams need containers.read on
// the environment and the swarm ones services.read. The browser keeps the history; the
// server keeps nothing (D-30).
func RegisterStats(r *httpx.Router, d StatsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Heartbeat <= 0 {
		d.Heartbeat = defaultHeartbeat
	}
	if d.MaxStreamsPerUser <= 0 {
		d.MaxStreamsPerUser = defaultMaxStreams
	}
	if d.MaxLifetime <= 0 {
		d.MaxLifetime = defaultMaxStreamLifetime
	}
	if d.RoundWait <= 0 {
		d.RoundWait = statsRoundWait
	}
	need := rbac.Requirement{Perm: rbac.PermContainersRead, Scope: rbac.ScopeEnvironment}
	needServices := rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment}
	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/containers/{containerId}/stats/stream", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveStatsStream(w, req, d, lim)
	}))
	// The streams that total many containers share a limit of their own, so the tabs and
	// lists one person has open do not count against the container Stats tabs.
	agg := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	route := func(path string, need rbac.Requirement, h func(http.ResponseWriter, *http.Request, StatsDeps, *streamLimiter)) {
		r.Guarded(http.MethodGet, path, need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { h(w, req, d, agg) }))
	}
	route("/api/env/{envId}/services/{serviceId}/stats/stream", needServices, serveServiceStatsStream)
	route("/api/env/{envId}/services/stats/stream", needServices, serveServicesStatsStream)
	route("/api/env/{envId}/containers/stats/stream", need, serveContainersStatsStream)
	route("/api/env/{envId}/nodes/stats/stream", needServices, serveNodesStatsStream)
}

// statsReady is the first event of a stream: the container's own limits, which come from
// its host config and never from the stats (memory_stats.limit is the host's RAM when the
// container has none), and whether there is anything to sample.
type statsReady struct {
	LimitBytes *int64 `json:"limitBytes"`
	NanoCPUs   *int64 `json:"nanoCpus"`
	Running    bool   `json:"running"`
}

// statsSample is one sample event. t is the engine's read time in Unix milliseconds. cpu is
// percent of one core and null for the first sample; the four rates are bytes per second
// and null for the first sample too.
type statsSample struct {
	T        int64    `json:"t"`
	CPU      *float64 `json:"cpu"`
	Mem      uint64   `json:"mem"`
	NetRx    *float64 `json:"netRx"`
	NetTx    *float64 `json:"netTx"`
	BlkRead  *float64 `json:"blkRead"`
	BlkWrite *float64 `json:"blkWrite"`
}

func sampleEvent(p dockerx.StatsPoint) statsSample {
	s := statsSample{T: p.At.UnixMilli(), CPU: p.CPUPercent, Mem: p.MemBytes}
	if p.HasRates {
		s.NetRx, s.NetTx, s.BlkRead, s.BlkWrite = &p.NetRxRate, &p.NetTxRate, &p.BlkReadRate, &p.BlkWriteRate
	}
	return s
}

func positive(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

func serveStatsStream(w http.ResponseWriter, req *http.Request, d StatsDeps, lim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	containerID := chi.URLParam(req, "containerId")
	wantNode := req.URL.Query().Get("nodeId")
	if len(wantNode) > 128 || !statsContainerID.MatchString(containerID) {
		writeJSONProblem(w, http.StatusUnprocessableEntity, problemValidationFailed, "containerId or nodeId is not valid")
		return
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemRateLimited,
			fmt.Sprintf("You already have %d stats streams open. Close one and try again.", lim.max))
		return
	}
	defer release()

	// The container is read fresh: its state says whether there is anything to sample, and
	// its host config is where its limits are.
	detail, _, nodeID, _, err := inspectOn(ctx, d.Aggregator, d.Log, env.ID, wantNode, containerID)
	if err != nil {
		writeProblemError(w, err)
		return
	}

	// The viewer joins the sampler before the stream says it is ready, so a ready event
	// means a sample is on its way. The sampler is keyed by the full ID, so a viewer by
	// name and a viewer by ID share one.
	var updates <-chan StatsUpdate
	if detail.Running {
		var leave func()
		updates, leave = d.Hub.Subscribe(env.ID, nodeID, detail.ID)
		defer leave()
	}

	rc, send := startSSE(w, d.Log)
	if rc == nil {
		return
	}
	if !send("ready", "", statsReady{LimitBytes: positive(detail.MemoryLimit), NanoCPUs: positive(detail.NanoCPUs), Running: detail.Running}) {
		return
	}
	if !detail.Running {
		send("closed", "", map[string]string{"reason": "not_running"})
		return
	}

	beat := time.NewTicker(d.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(d.MaxLifetime)
	defer life.Stop()
	var silent, stopped bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			// The browser reconnects, and the history it holds is its own.
			return
		case u, open := <-updates:
			if !open {
				return // the hub is closing
			}
			switch u.Kind {
			case StatsKindNotReporting:
				silent = true
				if !send("node", "", map[string]string{"state": "not_reporting"}) {
					return
				}
			case StatsKindStopped:
				stopped = true
				if !send("state", "", map[string]string{"state": "stopped"}) {
					return
				}
			case StatsKindSample:
				if silent {
					silent = false
					if !send("node", "", map[string]string{"state": "reporting"}) {
						return
					}
				}
				if stopped {
					stopped = false
					if !send("state", "", map[string]string{"state": "running"}) {
						return
					}
				}
				if !send("sample", "", sampleEvent(u.Point)) {
					return
				}
			}
		case <-beat.C:
			if !httpx.StillAuthorized(ctx) {
				send("closed", "", map[string]string{"reason": "access_lost"})
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
