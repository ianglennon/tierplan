// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// EnvironmentsDeps are the dependencies of the environment routes.
type EnvironmentsDeps struct {
	Envs    *envs.Service
	Checker *rbac.Checker
	Log     *slog.Logger
}

// RegisterEnvironments registers the environment routes.
func RegisterEnvironments(r *httpx.Router, d EnvironmentsDeps) {}
