// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/store"
)

// RestartLoopDeps are the dependencies of the restart-loop mute and settings routes.
type RestartLoopDeps struct {
	Store    *store.Store
	Clusters *envs.Clusters
	Audit    *audit.Writer
	Log      *slog.Logger
	Now      func() int64
}

// RegisterRestartLoop registers the mute and settings routes. (scaffold)
func RegisterRestartLoop(r *httpx.Router, d RestartLoopDeps) {}
