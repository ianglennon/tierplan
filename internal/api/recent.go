// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
)

// RecentDeps are the dependencies of the recent-environments routes.
type RecentDeps struct {
	Store   *store.Store
	Envs    *envs.Service
	Checker *rbac.Checker
	// Now stamps a use. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RecentEnvironmentsBody lists the environments a user used last.
type RecentEnvironmentsBody struct {
	EnvironmentIDs []string `json:"environmentIds" doc:"Up to 3 environment IDs, most recently used first"`
}

// RecentEnvironmentsOutput wraps RecentEnvironmentsBody for huma.
type RecentEnvironmentsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RecentEnvironmentsBody
}

// RegisterRecent registers the signed-in user's last-used environments (stub).
func RegisterRecent(r *httpx.Router, d RecentDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listRecentEnvironments", Method: http.MethodGet, Path: "/api/me/recent-environments",
		Summary: "stub", Tags: []string{"environments"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*RecentEnvironmentsOutput, error) {
		return nil, httpx.Problem(http.StatusNotImplemented, "", "not implemented")
	})
	httpx.Register(r, huma.Operation{
		OperationID: "recordRecentEnvironment", Method: http.MethodPut, Path: "/api/me/recent-environments/{envId}",
		Summary: "stub", DefaultStatus: http.StatusNoContent, Tags: []string{"environments"},
	}, rbac.SelfService, func(ctx context.Context, in *EnvironmentPathInput) (*struct{}, error) {
		return nil, httpx.Problem(http.StatusNotImplemented, "", "not implemented")
	})
}
