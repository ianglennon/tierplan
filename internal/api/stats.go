// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// StatsSource is what the stats stream reads: the aggregate of an environment, to find the
// node of a container, and that node's engine. *envs.Aggregator implements it.
type StatsSource interface {
	snapshotReader
	statsNodes
}

// StatsDeps are the dependencies of the container stats stream.
type StatsDeps struct {
	Aggregator StatsSource
	// Hub samples the containers that are watched. One hub serves every viewer.
	Hub *StatsHub
	// Heartbeat is how often an idle stream gets a comment line and has its access checked
	// again. Zero means 15 seconds; tests shorten it.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent stats streams per user. Zero means 8.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour.
	MaxLifetime time.Duration
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RegisterStats registers the live stats stream of a container under /api/env/{envId}/.
func RegisterStats(r *httpx.Router, d StatsDeps) {
	need := rbac.Requirement{Perm: rbac.PermContainersRead, Scope: rbac.ScopeEnvironment}
	r.Guarded(http.MethodGet, "/api/env/{envId}/containers/{containerId}/stats/stream", need, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
}
