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
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Field error codes of the user routes. The SPA maps each to its copy.
const (
	codeInvalidEmail       = "invalid_email"
	codeDisplayNameTooLong = "display_name_too_long"
)

// TeamRefBody names a team a user belongs to.
type TeamRefBody struct {
	ID   string `json:"id" doc:"The team's ID"`
	Name string `json:"name" doc:"The team's name"`
}

// UserSummaryBody is a user as an admin sees them in the users table.
type UserSummaryBody struct {
	ID                string        `json:"id" doc:"The user's ID"`
	Username          string        `json:"username" doc:"The lower-cased username"`
	DisplayName       string        `json:"displayName" doc:"The name shown for the user; empty when none was set"`
	Email             string        `json:"email" doc:"The user's email address; empty when none was set. Bayplan never sends mail to it"`
	InstanceAdmin     bool          `json:"instanceAdmin" doc:"True for an instance admin"`
	Status            string        `json:"status" enum:"active,disabled,locked,invite_pending,invite_expired,deleted" doc:"Locked, then disabled, then invite expired, then invite pending, then active"`
	LockedUntil       string        `json:"lockedUntil,omitempty" format:"date-time" doc:"While the account is locked: when the lock ends"`
	LastLoginAt       string        `json:"lastLoginAt,omitempty" format:"date-time" doc:"When the user last signed in; absent if they never have"`
	Teams             []TeamRefBody `json:"teams" doc:"The teams the user belongs to; the list fills in on the user's own page, and teamCount is what the users table shows"`
	TeamCount         int           `json:"teamCount" doc:"How many teams the user belongs to"`
	LastInstanceAdmin bool          `json:"lastInstanceAdmin" doc:"True for the only instance admin who can still sign in. Demoting, disabling or deleting them answers 409 last_instance_admin, so a screen can show those actions as unavailable"`
}

// UserDetailBody is a user as the user page shows them.
type UserDetailBody struct {
	UserSummaryBody
	CreatedAt string `json:"createdAt" format:"date-time" doc:"When the user was created"`
	UpdatedAt string `json:"updatedAt" format:"date-time" doc:"The version of the user a profile save is based on; send it back unchanged to PATCH"`
}

// UserDetailOutput wraps UserDetailBody for huma.
type UserDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UserDetailBody
}

// UserIDInput names one user in the path, with the caller's origin for the audit entry.
type UserIDInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	UserID    string `path:"userId" doc:"The user's ID"`
}

// LinkBody is a one-time link, shown once to the admin who asked for it.
type LinkBody struct {
	URL       string `json:"url" doc:"The address to give the user. The secret is in the part after #, which a browser never sends to a server"`
	ExpiresAt string `json:"expiresAt" format:"date-time" doc:"When the link stops working"`
}

// summaryOf builds the table row for a user.
func summaryOf(u sqlc.User, now time.Time, linkLive bool) UserSummaryBody {
	b := UserSummaryBody{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
		InstanceAdmin: u.InstanceAdmin != 0, Status: userStatus(u, now, linkLive),
		Teams: []TeamRefBody{},
	}
	if u.LockedUntil.Valid && u.LockedUntil.Int64 > now.UTC().UnixNano() {
		b.LockedUntil = time.Unix(0, u.LockedUntil.Int64).UTC().Format(time.RFC3339)
	}
	if u.LastLoginAt.Valid {
		b.LastLoginAt = time.Unix(0, u.LastLoginAt.Int64).UTC().Format(time.RFC3339)
	}
	return b
}

// CreateUserInput is a new user.
type CreateUserInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Username    string `json:"username" maxLength:"64" doc:"3 to 32 characters from a-z, 0-9, dot, dash and underscore; stored lower-case"`
		DisplayName string `json:"displayName,omitempty" maxLength:"256" doc:"Optional; at most 64 characters"`
		Email       string `json:"email,omitempty" maxLength:"254" doc:"Optional; a bare address. Bayplan never sends mail"`
	}
}

// CreateUserBody is the new user and the link that lets them choose a password.
type CreateUserBody struct {
	User UserSummaryBody `json:"user" doc:"The user that was created, with status invite_pending"`
	Link LinkBody        `json:"link" doc:"The one-time invite link; it cannot be shown again"`
}

// CreateUserOutput wraps CreateUserBody for huma.
type CreateUserOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CreateUserBody
}

// ListUsersOutput is the users table.
type ListUsersOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []UserSummaryBody
}

// summaryOfAccess builds the table row for a user from the access service's view.
func summaryOfAccess(u access.UserSummary) UserSummaryBody {
	b := UserSummaryBody{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
		InstanceAdmin: u.InstanceAdmin, Status: u.Status, Teams: []TeamRefBody{}, TeamCount: u.TeamCount,
		LastInstanceAdmin: u.LastInstanceAdmin,
	}
	if u.LockedUntil != nil {
		b.LockedUntil = u.LockedUntil.UTC().Format(time.RFC3339)
	}
	if u.LastLoginAt != nil {
		b.LastLoginAt = u.LastLoginAt.UTC().Format(time.RFC3339)
	}
	return b
}

// detailOf builds the user page's view of a user.
func detailOf(u access.UserDetail) UserDetailBody {
	return UserDetailBody{
		UserSummaryBody: summaryOfAccess(u.UserSummary),
		CreatedAt:       u.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       u.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// lifecycleError answers the errors the account lifecycle routes share.
func lifecycleError(d AccessDeps, op string, err error) error {
	switch {
	case errors.Is(err, access.ErrNotFound):
		return httpx.Problem(http.StatusNotFound, problemUserNotFound, "That user doesn't exist.")
	case errors.Is(err, access.ErrLastInstanceAdmin):
		return httpx.Problem(http.StatusConflict, problemLastInstanceAdmin,
			"Bayplan needs at least one active instance admin. Make someone else an instance admin first.")
	case errors.Is(err, access.ErrUserDisabled):
		return httpx.Problem(http.StatusConflict, problemUserDisabled,
			"That user is disabled. Enable them first, then reset their password.")
	case errors.Is(err, access.ErrStaleChange):
		return httpx.Problem(http.StatusConflict, problemStaleChange,
			"This changed while you were looking at it. Reload the page and try again.")
	}
	d.Log.Error(op+" failed", "error", err)
	return huma.Error500InternalServerError("internal error")
}

// RegenerateInviteInput names the user whose invite is replaced.
type RegenerateInviteInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	UserID    string `path:"userId" doc:"The user's ID"`
}

// RegenerateInviteOutput wraps the new link for huma.
type RegenerateInviteOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         LinkBody
}

func registerUsers(r *httpx.Router, d AccessDeps) {
	registerUserLifecycle(r, d)
	httpx.Register(r, huma.Operation{
		OperationID: "listUsers",
		Method:      http.MethodGet,
		Path:        "/api/users",
		Summary:     "List users",
		Description: "Every user who is not deleted, sorted by username, with the status the users table shows: " +
			"locked, disabled, invite expired, invite pending or active, in that order of precedence.",
		Tags: []string{"users"},
	}, rbac.Requirement{Perm: rbac.PermUsersManage, Scope: rbac.ScopeGlobal}, func(ctx context.Context, _ *struct{}) (*ListUsersOutput, error) {
		users, err := d.Access.ListUsers(ctx)
		if err != nil {
			d.Log.Error("list users failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		out := &ListUsersOutput{CacheControl: "no-store", Body: make([]UserSummaryBody, 0, len(users))}
		for _, u := range users {
			out.Body = append(out.Body, summaryOfAccess(u))
		}
		return out, nil
	})
	httpx.Register(r, huma.Operation{
		OperationID:   "createUser",
		Method:        http.MethodPost,
		Path:          "/api/users",
		Summary:       "Create a user and a one-time invite link",
		DefaultStatus: http.StatusCreated,
		Description: "The user has no password. The response carries a link, valid for BAYPLAN_INVITE_TTL (72 hours " +
			"by default), that opens a page where they choose one. Bayplan sends no email: the admin hands the " +
			"link over. A username that belonged to a deleted user answers 409 username_reserved.",
		Tags: []string{"users"},
	}, rbac.Requirement{Perm: rbac.PermUsersManage, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *CreateUserInput) (*CreateUserOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		u, link, err := d.Access.CreateUser(ctx, p.AuditActor(), access.CreateUserInput{
			Username: in.Body.Username, DisplayName: in.Body.DisplayName, Email: in.Body.Email,
			IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		})
		switch {
		case errors.Is(err, access.ErrUsernameReserved):
			return nil, httpx.Problem(http.StatusConflict, problemUsernameReserved, "That username belonged to a deleted user and stays reserved.")
		case errors.Is(err, access.ErrUsernameTaken):
			return nil, httpx.Problem(http.StatusConflict, problemUsernameTaken, "That username is already taken.")
		case errors.Is(err, access.ErrInvalidUsername), errors.Is(err, access.ErrInvalidEmail), errors.Is(err, access.ErrDisplayNameTooLong):
			return nil, userFieldProblems(err)
		case err != nil:
			d.Log.Error("create user failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &CreateUserOutput{
			CacheControl: "no-store",
			Body: CreateUserBody{
				User: summaryOf(u, d.now(), true),
				Link: LinkBody{URL: linkURL(d.Instance, link.Token), ExpiresAt: link.ExpiresAt.UTC().Format(time.RFC3339)},
			},
		}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "regenerateInvite",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/invite",
		Summary:     "Replace a user's invite link",
		Description: "For a user who has not yet set a password. The new link replaces the previous one, which " +
			"stops working at once, and is valid for BAYPLAN_INVITE_TTL. A user who has already set a password " +
			"answers 409 not_invited; an unknown or deleted user answers 404 user_not_found.",
		Tags: []string{"users"},
	}, rbac.Requirement{Perm: rbac.PermUsersManage, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *RegenerateInviteInput) (*RegenerateInviteOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		_, link, err := d.Access.RegenerateInvite(ctx, p.AuditActor(), in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		switch {
		case errors.Is(err, access.ErrNotFound):
			return nil, httpx.Problem(http.StatusNotFound, problemUserNotFound, "That user doesn't exist.")
		case errors.Is(err, access.ErrNotInvited):
			return nil, httpx.Problem(http.StatusConflict, problemNotInvited, "That user has already set a password, so there is no invite to replace.")
		case err != nil:
			d.Log.Error("regenerate invite failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &RegenerateInviteOutput{
			CacheControl: "no-store",
			Body:         LinkBody{URL: linkURL(d.Instance, link.Token), ExpiresAt: link.ExpiresAt.UTC().Format(time.RFC3339)},
		}, nil
	})
}

// userFieldProblems turns the validation errors of a user submission into one
// 422 validation_failed that names every field at fault, with a stable code.
func userFieldProblems(err error) error {
	var problems []*huma.ErrorDetail
	add := func(location, code string) {
		problems = append(problems, &huma.ErrorDetail{Location: location, Message: code})
	}
	if errors.Is(err, access.ErrInvalidUsername) {
		add("body.username", codeInvalidUsername)
	}
	if errors.Is(err, access.ErrDisplayNameTooLong) {
		add("body.displayName", codeDisplayNameTooLong)
	}
	if errors.Is(err, access.ErrInvalidEmail) {
		add("body.email", codeInvalidEmail)
	}
	return &huma.ErrorModel{
		Type:   problemValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: "validation failed",
		Errors: problems,
	}
}

// SetInstanceAdminInput turns instance authority on or off for one user.
type SetInstanceAdminInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	UserID    string `path:"userId" doc:"The user's ID"`
	Body      struct {
		Enabled bool `json:"enabled" doc:"True to make the user an instance admin, false to remove it"`
	}
}

// UpdateUserInput is an admin's edit of a user's profile. The username is not a
// field: it never changes, and a body that names it is refused.
type UpdateUserInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	UserID    string `path:"userId" doc:"The user's ID"`
	Body      struct {
		DisplayName *string   `json:"displayName,omitempty" maxLength:"256" doc:"The name shown for the user; at most 64 characters. Send an empty string to clear it; leave it out to keep it"`
		Email       *string   `json:"email,omitempty" maxLength:"254" doc:"A bare address, or empty to clear it; leave it out to keep it. Bayplan never sends mail"`
		UpdatedAt   time.Time `json:"updatedAt" format:"date-time" doc:"The updatedAt of the user this edit is based on. If the user changed since, the answer is 409 stale_change"`
	}
}

// registerUserLifecycle registers the routes of one user's page: read, edit,
// disable, enable, delete, instance admin, unlock and password reset. All need
// users.manage.
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
		OperationID: "updateUser",
		Method:      http.MethodPatch,
		Path:        "/api/users/{userId}",
		Summary:     "Edit a user's display name and email",
		Description: "Send displayName, email or both, with the updatedAt you read. If the user changed since, nothing is " +
			"saved and the answer is 409 stale_change. The username can never be changed and is not accepted. " +
			"An invalid field answers 422 on body.displayName or body.email. Saving what is already there changes nothing.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *UpdateUserInput) (*UserDetailOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		err := d.Access.UpdateProfile(ctx, p.AuditActor(), in.UserID, access.ProfileInput{
			DisplayName: in.Body.DisplayName, Email: in.Body.Email, UpdatedAt: in.Body.UpdatedAt,
		}, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if errors.Is(err, access.ErrInvalidEmail) || errors.Is(err, access.ErrDisplayNameTooLong) {
			return nil, userFieldProblems(err)
		}
		if err != nil {
			return nil, lifecycleError(d, "update user", err)
		}
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

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteUser",
		Method:        http.MethodDelete,
		Path:          "/api/users/{userId}",
		Summary:       "Delete a user",
		DefaultStatus: http.StatusNoContent,
		Description: "Turns the user into a tombstone and cannot be undone: they are disabled, their password, email and " +
			"display name are cleared, instance admin is turned off, and their sessions, links, direct access and team " +
			"memberships are removed in one step. Their username stays reserved, so creating it again answers 409 " +
			"username_reserved. The automations they own pause. The last active instance admin cannot be deleted: 409 " +
			"last_instance_admin. A user who is already deleted answers 404 user_not_found.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := d.Access.DeleteUser(ctx, p.AuditActor(), in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent)); err != nil {
			return nil, lifecycleError(d, "delete user", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "setInstanceAdmin",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/instance-admin",
		Summary:     "Turn instance admin on or off for a user",
		Description: "Instance authority belongs to a user, never to a team. Setting what is already set changes nothing. " +
			"Turning it off for the last active instance admin answers 409 last_instance_admin. Answers the user as they now are.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *SetInstanceAdminInput) (*UserDetailOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := d.Access.SetInstanceAdmin(ctx, p.AuditActor(), in.UserID, in.Body.Enabled, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent)); err != nil {
			return nil, lifecycleError(d, "set instance admin", err)
		}
		return readUser(ctx, d, in.UserID)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "resetUserPassword",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/reset-password",
		Summary:     "Issue a password reset link",
		Description: "Ends every session of the user at once, in the same step that makes the link, and answers a one-time " +
			"link built like an invite's that lets them choose a new password. It is valid for BAYPLAN_INVITE_TTL (72 hours " +
			"by default) and replaces any link they held. Bayplan sends no email: the admin hands the link over. " +
			"A disabled user answers 409 user_disabled; an unknown or deleted user answers 404 user_not_found. " +
			"Audited as user.password_reset_issued with the number of sessions ended.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*RegenerateInviteOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		_, link, err := d.Access.IssueReset(ctx, p.AuditActor(), in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, lifecycleError(d, "reset password", err)
		}
		return &RegenerateInviteOutput{
			CacheControl: "no-store",
			Body:         LinkBody{URL: linkURL(d.Instance, link.Token), ExpiresAt: link.ExpiresAt.UTC().Format(time.RFC3339)},
		}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "unlockUser",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/unlock",
		Summary:     "Unlock a locked user",
		Description: "Clears the lock and the failed sign-in count, audited as auth.unlocked with the caller as actor. " +
			"A user who is not locked is left alone and nothing is audited. Answers the user as they now are.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*UserDetailOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := d.Access.UnlockUser(ctx, p.AuditActor(), in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent)); err != nil {
			return nil, lifecycleError(d, "unlock user", err)
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
