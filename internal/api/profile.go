// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// ProfileBody is the signed-in user's own profile.
type ProfileBody struct {
	Username    string `json:"username" doc:"The lower-cased username. It can never be changed"`
	DisplayName string `json:"displayName" doc:"The name shown for the user; empty when none was set"`
	Email       string `json:"email" doc:"The user's email address; empty when none was set. Bayplan never sends mail to it"`
	UpdatedAt   string `json:"updatedAt" format:"date-time" doc:"The version of the profile a save is based on; send it back unchanged to PATCH"`
}

// ProfileOutput wraps ProfileBody for huma.
type ProfileOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ProfileBody
}

// UpdateMyProfileInput is a user's edit of their own profile. The username is
// not a field: it never changes, and a body that names it is refused.
type UpdateMyProfileInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		DisplayName *string   `json:"displayName,omitempty" maxLength:"256" doc:"The name shown for you; at most 64 characters. Send an empty string to clear it; leave it out to keep it"`
		Email       *string   `json:"email,omitempty" maxLength:"254" doc:"A bare address, or empty to clear it; leave it out to keep it. Bayplan never sends mail"`
		UpdatedAt   time.Time `json:"updatedAt" format:"date-time" doc:"The updatedAt of the profile this edit is based on. If it changed since, the answer is 409 stale_change"`
	}
}

func profileOutput(u access.UserDetail) *ProfileOutput {
	return &ProfileOutput{CacheControl: "no-store", Body: ProfileBody{
		Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
		UpdatedAt: u.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}}
}

// registerProfile registers the signed-in user's own profile (D-24). Both
// routes act on the caller's own row only: the user ID is never read from the
// request, so there is nobody else's profile to reach.
func registerProfile(r *httpx.Router, d AccessDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getMyProfile",
		Method:      http.MethodGet,
		Path:        "/api/me/profile",
		Summary:     "Read the signed-in user's own profile",
		Description: "The username, display name and email, with the updatedAt a save must be based on.",
		Tags:        []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*ProfileOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		u, err := d.Access.GetUser(ctx, p.ID)
		if err != nil {
			return nil, lifecycleError(d, "read profile", err)
		}
		return profileOutput(u), nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateMyProfile",
		Method:      http.MethodPatch,
		Path:        "/api/me/profile",
		Summary:     "Edit the signed-in user's own display name and email",
		Description: "Send displayName, email or both, with the updatedAt you read. If the profile changed since, nothing is " +
			"saved and the answer is 409 stale_change. The username can never be changed and is not accepted. " +
			"An invalid field answers 422 on body.displayName or body.email. Saving what is already there changes nothing. " +
			"Audited as user.updated with the caller as actor and target.",
		Tags: []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, in *UpdateMyProfileInput) (*ProfileOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		err := d.Access.UpdateProfile(ctx, p.AuditActor(), p.ID, access.ProfileInput{
			DisplayName: in.Body.DisplayName, Email: in.Body.Email, UpdatedAt: in.Body.UpdatedAt,
		}, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if errors.Is(err, access.ErrInvalidEmail) || errors.Is(err, access.ErrDisplayNameTooLong) {
			return nil, userFieldProblems(err)
		}
		if err != nil {
			return nil, lifecycleError(d, "update profile", err)
		}
		u, err := d.Access.GetUser(ctx, p.ID)
		if err != nil {
			return nil, lifecycleError(d, "read profile", err)
		}
		return profileOutput(u), nil
	})
}
