// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// NodesDeps are the dependencies of the swarm node routes.
type NodesDeps struct {
	Aggregator      *envs.Aggregator
	Clusters        *envs.Clusters
	Audit           *audit.Writer
	Checker         *rbac.Checker
	Agents          *agents.Service
	Live            AgentSessions
	SelfContainerID string
	Log             *slog.Logger
}

// RegisterNodes mounts the swarm node routes.
func RegisterNodes(r *httpx.Router, d NodesDeps) {}
