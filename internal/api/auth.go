// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
)

// AuthDeps are the dependencies of the sign-in routes.
type AuthDeps struct {
	Login    *auth.Login
	Sessions *auth.Sessions
	Instance *instance.Instance
	Store    *store.Store
	Checker  *rbac.Checker
	Log      *slog.Logger
}

// RegisterAuth registers sign-in, sign-out and /api/me. Not implemented yet.
func RegisterAuth(_ *httpx.Router, _ AuthDeps) {}
