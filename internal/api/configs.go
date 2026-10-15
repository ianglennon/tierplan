// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
)

// ConfigsDeps are the dependencies of the swarm config routes.
type ConfigsDeps struct {
	// Aggregator reaches the manager a write goes to: WriteOnManager.
	Aggregator *envs.Aggregator
	// Clusters is the cluster reader the lists and the in-use check are served from.
	Clusters *envs.Clusters
	Audit    *audit.Writer
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RegisterConfigs mounts the swarm config routes.
func RegisterConfigs(r *httpx.Router, d ConfigsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
}

// configReadProblem turns the error of a read through a manager into the problem the caller is
// answered with.
func configReadProblem(d ConfigsDeps, envID string, err error) error { return nil }
