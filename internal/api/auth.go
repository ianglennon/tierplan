// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
)

// problemInvalidCredentials is the stable type of every failed sign-in. The SPA
// shows UI-SPEC's "Invalid username or password." for it, including while the
// account is locked (D-30).
const problemInvalidCredentials = "invalid_credentials" //nolint:gosec // a problem type name, not a credential

// invalidCredentialsDetail is the one message every failed sign-in carries.
// Unknown user, wrong password, locked, disabled and rate limited must not be
// distinguishable by anyone but the audit log (D-30).
const invalidCredentialsDetail = "Invalid username or password."

// AuthDeps are the dependencies of the sign-in routes.
type AuthDeps struct {
	Login    *auth.Login
	Sessions *auth.Sessions
	Instance *instance.Instance
	Store    *store.Store
	Checker  *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// LoginInput is a sign-in attempt. The length caps bound what the server reads;
// they say nothing about which names exist.
type LoginInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Username string `json:"username" maxLength:"64" doc:"The account name; matched case-insensitively"`
		Password string `json:"password" maxLength:"1024" doc:"The account password"`
		Remember bool   `json:"remember,omitempty" doc:"Keep this browser signed in for the remember lifetime instead of until it has been idle for the idle timeout"`
	}
}

// UserBody is a user as the SPA needs to show them.
type UserBody struct {
	ID       string `json:"id" doc:"The user's ID"`
	Username string `json:"username" doc:"The lower-cased username"`
	Role     string `json:"role" doc:"The user's global role, empty when they have none"`
}

// LoginBody is the signed-in state returned by a sign-in.
type LoginBody struct {
	User      UserBody `json:"user" doc:"The signed-in user"`
	CSRFToken string   `json:"csrfToken" doc:"Sent as X-CSRF-Token on every mutating request"`
}

// LoginOutput carries the session cookie with the body.
type LoginOutput struct {
	SetCookie    http.Cookie `header:"Set-Cookie"`
	CacheControl string      `header:"Cache-Control"`
	Body         LoginBody
}

// LogoutInput is a sign-out. It has no body.
type LogoutInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// LogoutOutput clears the session cookie.
type LogoutOutput struct {
	SetCookie    http.Cookie `header:"Set-Cookie"`
	CacheControl string      `header:"Cache-Control"`
}

// PermissionsBody is what a user may do (D-50). The UI hides or disables what
// is not listed; the server remains the only enforcer.
type PermissionsBody struct {
	Global       []string            `json:"global" doc:"Installation-wide permissions"`
	Environments map[string][]string `json:"environments" doc:"Permissions per environment ID; the key * holds those that apply to every environment"`
}

// MeBody is the signed-in user's own state.
type MeBody struct {
	User        UserBody        `json:"user" doc:"The signed-in user"`
	CSRFToken   string          `json:"csrfToken" doc:"Sent as X-CSRF-Token on every mutating request"`
	Permissions PermissionsBody `json:"permissions" doc:"Effective permissions of the signed-in user"`
}

// MeOutput wraps MeBody for huma.
type MeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         MeBody
}

// RegisterAuth registers sign-in, sign-out and /api/me. Sign-in is public by
// necessity. Sign-out and /api/me are self-service: any signed-in user, about
// themselves (D-44).
func RegisterAuth(r *httpx.Router, d AuthDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	internal := func(what string, err error) error {
		d.Log.Error("auth request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	signedIn := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil || len(p.SessionIDHash) == 0 {
			// The policy refuses anonymous callers first; this is the backstop.
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		return p, nil
	}
	// roleOf is the user's global role, empty when they hold none.
	roleOf := func(ctx context.Context, userID string) (string, error) {
		role, err := d.Store.RQ.GetUserGlobalRole(ctx, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return role, err
	}

	httpx.Register(r, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/api/auth/login",
		Summary:     "Sign in with a username and password",
		Description: "Starts a server-side session and sets its cookie. Every failure, whether the user is unknown, the " +
			"password is wrong, the account is locked or disabled or the address is rate limited, answers 401 " +
			"invalid_credentials with the same body (D-30). A session the browser already holds is ended first.",
		Tags: []string{"auth"},
	}, rbac.Public, func(ctx context.Context, in *LoginInput) (*LoginOutput, error) {
		prev := []byte(nil)
		if p := httpx.PrincipalFrom(ctx); p != nil {
			prev = p.SessionIDHash // rotation: the browser's old session ends with this sign-in (D-32)
		}
		res, err := d.Login.Login(ctx, auth.LoginInput{
			Username:            in.Body.Username,
			Password:            in.Body.Password,
			Remember:            in.Body.Remember,
			IP:                  httpx.ClientIP(ctx),
			UserAgent:           in.UserAgent,
			PreviousSessionHash: prev,
		})
		if errors.Is(err, auth.ErrInvalidCredentials) {
			return nil, httpx.Problem(http.StatusUnauthorized, problemInvalidCredentials, invalidCredentialsDetail)
		}
		if err != nil {
			return nil, internal("sign in", err)
		}
		role, err := roleOf(ctx, res.User.ID)
		if err != nil {
			return nil, internal("look up role", err)
		}
		return &LoginOutput{
			SetCookie:    d.Sessions.Cookie(res.RawSessionID, in.Body.Remember, d.Instance.CookieScope()),
			CacheControl: "no-store",
			Body: LoginBody{
				User:      UserBody{ID: res.User.ID, Username: res.User.Username, Role: role},
				CSRFToken: d.Sessions.CSRFToken(res.IDHash),
			},
		}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "logout",
		Method:        http.MethodPost,
		Path:          "/api/auth/logout",
		Summary:       "Sign out of this browser",
		Description:   "Ends only the session this request was made with and clears its cookie. The user's other devices stay signed in (D-33).",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, in *LogoutInput) (*LogoutOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		if err := d.Login.Logout(ctx, p, httpx.ClientIP(ctx), in.UserAgent); err != nil {
			return nil, internal("sign out", err)
		}
		return &LogoutOutput{SetCookie: auth.ClearCookie(d.Instance.CookieScope()), CacheControl: "no-store"}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getMe",
		Method:      http.MethodGet,
		Path:        "/api/me",
		Summary:     "Get the signed-in user, their CSRF token and their effective permissions",
		Tags:        []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*MeOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		role, err := roleOf(ctx, p.ID)
		if err != nil {
			return nil, internal("look up role", err)
		}
		global, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
		if err != nil {
			return nil, internal("effective permissions", err)
		}
		perms := PermissionsBody{Global: names(global), Environments: make(map[string][]string, len(perEnv))}
		for env, list := range perEnv {
			perms.Environments[env] = names(list)
		}
		return &MeOutput{
			CacheControl: "no-store",
			Body: MeBody{
				User:        UserBody{ID: p.ID, Username: p.Name, Role: role},
				CSRFToken:   d.Sessions.CSRFToken(p.SessionIDHash),
				Permissions: perms,
			},
		}, nil
	})
}

// names turns permissions into strings, never nil, so the JSON is [] not null.
func names(perms []rbac.Perm) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, string(p))
	}
	return out
}
