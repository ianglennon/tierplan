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
	ID            string        `json:"id" doc:"The user's ID"`
	Username      string        `json:"username" doc:"The lower-cased username"`
	DisplayName   string        `json:"displayName" doc:"The name shown for the user; empty when none was set"`
	Email         string        `json:"email" doc:"The user's email address; empty when none was set. Bayplan never sends mail to it"`
	InstanceAdmin bool          `json:"instanceAdmin" doc:"True for an instance admin"`
	Status        string        `json:"status" enum:"active,disabled,locked,invite_pending,invite_expired,deleted" doc:"Locked, then disabled, then invite expired, then invite pending, then active"`
	LockedUntil   string        `json:"lockedUntil,omitempty" format:"date-time" doc:"While the account is locked: when the lock ends"`
	LastLoginAt   string        `json:"lastLoginAt,omitempty" format:"date-time" doc:"When the user last signed in; absent if they never have"`
	Teams         []TeamRefBody `json:"teams" doc:"The teams the user belongs to; the list fills in on the user's own page, and teamCount is what the users table shows"`
	TeamCount     int           `json:"teamCount" doc:"How many teams the user belongs to"`
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
