// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// HealthSource is what the global stream reads: the broker.
type HealthSource interface {
	Subscribe(ctx context.Context) (backlog []envs.HealthEvent, ch <-chan envs.HealthEvent, cancel func())
	SnapshotAll(ctx context.Context) ([]envs.Environment, error)
}

// EnvStreamDeps are the dependencies of GET /api/environments/stream.
type EnvStreamDeps struct {
	Broker  HealthSource
	Checker *rbac.Checker
	// Heartbeat, MaxStreamsPerUser and MaxLifetime default to 15 s, 4 and one hour.
	Heartbeat         time.Duration
	MaxStreamsPerUser int
	MaxLifetime       time.Duration
	Log               *slog.Logger
}

// RegisterEnvStream registers the global environments stream.
func RegisterEnvStream(r *httpx.Router, d EnvStreamDeps) {
	r.Guarded(http.MethodGet, "/api/environments/stream", rbac.SelfService, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeJSONProblem(w, http.StatusNotImplemented, "", "not implemented")
	}))
}
