// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// AccountDeps are the dependencies of the account and warnings routes.
type AccountDeps struct {
	Sessions *auth.Sessions
	Instance *instance.Instance
	Store    *store.Store
	Audit    *audit.Writer
	DataDir  datadir.Info
	Vault    *vault.Vault
	Log      *slog.Logger
}

// RegisterAccount registers password change and the instance warnings.
func RegisterAccount(_ *httpx.Router, _ AccountDeps) {}
