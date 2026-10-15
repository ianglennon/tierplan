// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

type ServiceLogsDeps struct {
	Clusters          *envs.Clusters
	Audit             *audit.Writer
	Checker           *rbac.Checker
	Heartbeat         time.Duration
	MaxStreamsPerUser int
	MaxLifetime       time.Duration
	Now               func() time.Time
	Log               *slog.Logger
}

func RegisterServiceLogs(r *httpx.Router, d ServiceLogsDeps) {}
