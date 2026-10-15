// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
)

// SecretsDeps are the dependencies of the swarm secret routes.
type SecretsDeps struct {
	// Aggregator reaches the manager a write goes to: WriteOnManager.
	Aggregator *envs.Aggregator
	// Clusters is the cluster reader the lists and the in-use check are served from.
	Clusters *envs.Clusters
	Audit    *audit.Writer
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RegisterSecrets mounts the swarm secret routes.
func RegisterSecrets(r *httpx.Router, d SecretsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
}
