// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// RecentDeps are the dependencies of the recent-environments routes.
type RecentDeps struct {
	Store   *store.Store
	Envs    *envs.Service
	Checker *rbac.Checker
	// Refusals records the permission.denied entry of the 404 given for an
	// environment the caller has no grant on (D-08). It is the router's refusal
	// auditor, so the entry is coalesced like the policy gate's own and a flood of
	// probes cannot fill the log. Nil means the refusal is not audited.
	Refusals httpx.Auditor
	// Now stamps a use. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

const (
	// recentRecordRoute is the route as the policy gate names it in a refusal's
	// target (method and chi pattern), so this refusal reads like the gate's.
	recentRecordRoute = "PUT /api/me/recent-environments/{envId}"
	// reasonEnvNotVisible is the reason httpx's policy gate gives the same refusal.
	reasonEnvNotVisible = "environment_not_visible"
	// maxAuditedEnvID is how much of the URL's environment ID a refusal records; it
	// is the size the policy gate and audit.Writer cut to.
	maxAuditedEnvID = 128
)

// RecordRecentInput is the environment a user used, and the caller's origin for the
// audit entry of a refusal.
type RecordRecentInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
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
	}, rbac.SelfService, func(ctx context.Context, in *RecordRecentInput) (*struct{}, error) {
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
		if !canRead(perEnv, in.EnvID) {
			// A caller with no grant is audited exactly as the policy gate audits
			// it on an /api/env route (D-08), through the same coalescer, so probing
			// IDs through here leaves the same trace. A readable environment that
			// is gone is not: the gate does not audit that either.
			if d.Refusals != nil {
				envID := in.EnvID
				if len(envID) > maxAuditedEnvID {
					envID = envID[:maxAuditedEnvID]
				}
				if err := d.Refusals.Record(ctx, audit.Entry{
					Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
					Reason: reasonEnvNotVisible, TargetType: "route", TargetID: recentRecordRoute, EnvID: envID,
					IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
				}); err != nil {
					// A refusal nobody can see must not look like a normal one.
					return nil, internal("audit the refusal", err)
				}
			}
			return nil, httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		}
		if !d.Envs.Known(ctx, in.EnvID) {
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
