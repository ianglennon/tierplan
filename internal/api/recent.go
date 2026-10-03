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
	"github.com/ianglennon/bayplan/internal/store/sqlc"
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

// RegisterRecent registers the signed-in user's last-used environments (D-31):
// the list, and the record of a use. They are kept on the server, per user, so the
// choice follows the user to another browser or device. Both are self-service: they
// are about the caller and nobody else, and the list only ever names environments
// the caller may read.
func RegisterRecent(r *httpx.Router, d RecentDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	internal := func(what string, err error) error {
		d.Log.Error("recent environments request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	// userOf is the caller as a user. Recent environments belong to people: a system
	// principal has none.
	userOf := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if p.Type != rbac.PrincipalUser {
			return nil, httpx.Problem(http.StatusForbidden, "", "Only a signed-in user has recent environments.")
		}
		return p, nil
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listRecentEnvironments", Method: http.MethodGet, Path: "/api/me/recent-environments",
		Summary: "The environments the signed-in user used last",
		Description: "Up to 3 environment IDs, the most recently used first and, for uses at the same instant, in ID order. " +
			"An environment that was removed, or that the user can no longer read, is left out.",
		Tags: []string{"environments"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*RecentEnvironmentsOutput, error) {
		p, err := userOf(ctx)
		if err != nil {
			return nil, err
		}
		_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
		if err != nil {
			return nil, internal("effective permissions", err)
		}
		ids, err := d.Store.RQ.ListRecentEnvironments(ctx, p.ID)
		if err != nil {
			return nil, internal("list recent environments", err)
		}
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if canRead(perEnv, id) {
				out = append(out, id)
			}
		}
		return &RecentEnvironmentsOutput{CacheControl: "no-store", Body: RecentEnvironmentsBody{EnvironmentIDs: out}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "recordRecentEnvironment", Method: http.MethodPut, Path: "/api/me/recent-environments/{envId}",
		Summary:       "Record that the signed-in user used an environment",
		DefaultStatus: http.StatusNoContent,
		Description: "Sets the environment's last-used time to now. Using the same environment again keeps one entry with the newer time. " +
			"Only the newest 3 per user are kept. An environment that does not exist and one the user cannot read both answer " +
			"404 environment_not_found, so the answer never says which exist.",
		Tags: []string{"environments"},
	}, rbac.SelfService, func(ctx context.Context, in *EnvironmentPathInput) (*struct{}, error) {
		p, err := userOf(ctx)
		if err != nil {
			return nil, err
		}
		_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
		if err != nil {
			return nil, internal("effective permissions", err)
		}
		// Unreadable and unknown are the same answer. The permission is checked
		// first and the registry second, so neither is reached for a caller who
		// may not read the environment.
		if !canRead(perEnv, in.EnvID) || !d.Envs.Known(ctx, in.EnvID) {
			return nil, httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		}
		tx, err := d.Store.Write.BeginTx(ctx, nil)
		if err != nil {
			return nil, internal("begin", err)
		}
		defer func() { _ = tx.Rollback() }() // a no-op once committed
		q := d.Store.Q.WithTx(tx)
		if err := q.UpsertRecentEnvironment(ctx, sqlc.UpsertRecentEnvironmentParams{
			UserID: p.ID, EnvID: in.EnvID, UsedAt: now().UTC().UnixNano(),
		}); err != nil {
			return nil, internal("record a use", err)
		}
		if err := q.TrimRecentEnvironments(ctx, p.ID); err != nil {
			return nil, internal("trim recent environments", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, internal("commit", err)
		}
		return nil, nil
	})
}
