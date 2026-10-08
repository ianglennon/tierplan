// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// registerUserLifecycle registers the routes of one user's page: read, disable
// and enable. All need users.manage.
func registerUserLifecycle(r *httpx.Router, d AccessDeps) {
	manage := rbac.Requirement{Perm: rbac.PermUsersManage, Scope: rbac.ScopeGlobal}

	httpx.Register(r, huma.Operation{
		OperationID: "getUser",
		Method:      http.MethodGet,
		Path:        "/api/users/{userId}",
		Summary:     "Read one user",
		Description: "The user as the user page shows them. An unknown or deleted user answers 404 user_not_found.",
		Tags:        []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*UserDetailOutput, error) {
		return readUser(ctx, d, in.UserID)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "disableUser",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/disable",
		Summary:     "Disable a user",
		Description: "Ends every session of the user at once and refuses their sign-in. Their grants and team " +
			"memberships are kept and the automations they own pause. Answers the user as they now are. " +
			"Disabling a disabled user changes nothing. " +
			"The last active instance admin cannot be disabled: 409 last_instance_admin.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*UserDetailOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := d.Access.DisableUser(ctx, p.AuditActor(), in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent)); err != nil {
			return nil, lifecycleError(d, "disable user", err)
		}
		return readUser(ctx, d, in.UserID)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "enableUser",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/enable",
		Summary:     "Enable a user",
		Description: "Lets a disabled user sign in again with the grants and teams they had. Enabling an enabled user changes nothing.",
		Tags:        []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*UserDetailOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := d.Access.EnableUser(ctx, p.AuditActor(), in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent)); err != nil {
			return nil, lifecycleError(d, "enable user", err)
		}
		return readUser(ctx, d, in.UserID)
	})
}

// readUser answers a lifecycle route with the user as it is after the change, so
// the page can show the new state without a second request.
func readUser(ctx context.Context, d AccessDeps, id string) (*UserDetailOutput, error) {
	u, err := d.Access.GetUser(ctx, id)
	if err != nil {
		return nil, lifecycleError(d, "read user", err)
	}
	return &UserDetailOutput{CacheControl: "no-store", Body: detailOf(u)}, nil
}
