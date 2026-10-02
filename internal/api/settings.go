// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/store"
)

// SettingsDeps are the dependencies of the settings routes.
type SettingsDeps struct {
	Instance *instance.Instance
	Store    *store.Store
	Audit    *audit.Writer
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RegisterSettings registers GET and PATCH /api/settings.
func RegisterSettings(_ *httpx.Router, _ SettingsDeps) {}
