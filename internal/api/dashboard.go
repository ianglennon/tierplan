// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
)

// SnapshotSource is what the dashboard routes read. *envs.Snapshots implements it.
type SnapshotSource interface {
	Get(ctx context.Context, envID string) (dockerx.Snapshot, envs.Status, error)
}

// DashboardDeps are the dependencies of the summary and list routes.
type DashboardDeps struct {
	Snapshots SnapshotSource
	// SelfContainerID is Bayplan's own container ID, for the isSelf flag.
	SelfContainerID string
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RegisterDashboard registers the environment summary and the four list routes.
func RegisterDashboard(_ *httpx.Router, _ DashboardDeps) {}
