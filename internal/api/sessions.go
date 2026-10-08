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

// Problem types of the session routes. The SPA maps each to its copy.
const (
	// problemCurrentSession is an attempt to revoke the session making the request.
	problemCurrentSession = "current_session"
	// problemSessionNotFound is a session handle that is not one of the user's.
	problemSessionNotFound = "session_not_found"
)

// SessionBody is one signed-in session. The session ID and its hash never appear:
// a session is named by its public ID, which a browser cannot present.
type SessionBody struct {
	ID         string `json:"id" doc:"The session's public ID, to revoke it by. It is not the cookie and cannot be used to sign in"`
	UserAgent  string `json:"userAgent" doc:"The user agent the session was created with; empty when the browser sent none"`
	IP         string `json:"ip" doc:"The address the session was created from; empty when unknown"`
	CreatedAt  string `json:"createdAt" format:"date-time" doc:"When the user signed in"`
	LastSeenAt string `json:"lastSeenAt" format:"date-time" doc:"When the session last made a request"`
	Current    bool   `json:"current" doc:"True for the session making this request. Never true when an admin looks at another user's sessions"`
}

// SessionsOutput is a user's sessions, most recently active first.
type SessionsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []SessionBody
}

// SessionsEndedBody says how many sessions a bulk revoke ended.
type SessionsEndedBody struct {
	Ended int `json:"ended" doc:"How many sessions ended"`
}

// SessionsEndedOutput wraps SessionsEndedBody for huma.
type SessionsEndedOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SessionsEndedBody
}

// RevokeSessionInput names one of the caller's own sessions.
type RevokeSessionInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	PublicID  string `path:"publicId" maxLength:"64" doc:"The session's public ID, as the session list gives it"`
}

// UserAgentInput carries only the caller's origin for the audit entry.
type UserAgentInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

func sessionsOutput(list []access.SessionInfo) *SessionsOutput {
	out := &SessionsOutput{CacheControl: "no-store", Body: make([]SessionBody, 0, len(list))}
	for _, s := range list {
		out.Body = append(out.Body, SessionBody{
			ID: s.PublicID, UserAgent: s.UserAgent, IP: s.IP, Current: s.Current,
			CreatedAt:  s.CreatedAt.UTC().Format(time.RFC3339),
			LastSeenAt: s.LastSeenAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

// sessionError answers the errors the session routes share.
func sessionError(d AccessDeps, op string, err error) error {
	switch {
	case errors.Is(err, access.ErrCurrentSession):
		return httpx.Problem(http.StatusConflict, problemCurrentSession, "That is the session you are using. Sign out to end it.")
	case errors.Is(err, access.ErrSessionNotFound):
		return httpx.Problem(http.StatusNotFound, problemSessionNotFound, "That session has already ended.")
	}
	return lifecycleError(d, op, err)
}

// registerSessions registers the session routes: the caller's own (self-service,
// every one scoped by the caller's user ID) and an admin's view of a user's.
func registerSessions(r *httpx.Router, d AccessDeps) {
	manage := rbac.Requirement{Perm: rbac.PermUsersManage, Scope: rbac.ScopeGlobal}
	self := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil || len(p.SessionIDHash) == 0 {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		return p, nil
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listMySessions",
		Method:      http.MethodGet,
		Path:        "/api/me/sessions",
		Summary:     "List the signed-in user's sessions",
		Description: "Every browser the caller is signed in on, most recently active first, with the device, address and " +
			"last activity, and current set on the one making this request.",
		Tags: []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*SessionsOutput, error) {
		p, err := self(ctx)
		if err != nil {
			return nil, err
		}
		list, err := d.Access.ListSessions(ctx, p.ID, p.SessionIDHash)
		if err != nil {
			return nil, sessionError(d, "list sessions", err)
		}
		return sessionsOutput(list), nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "revokeMySession",
		Method:        http.MethodDelete,
		Path:          "/api/me/sessions/{publicId}",
		Summary:       "Sign one of the caller's other sessions out",
		DefaultStatus: http.StatusNoContent,
		Description: "That browser's next request answers 401. The session making this request cannot be revoked here: " +
			"409 current_session. A handle that is not one of the caller's sessions answers 404 session_not_found. " +
			"Audited as session.revoked.",
		Tags: []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, in *RevokeSessionInput) (*struct{}, error) {
		p, err := self(ctx)
		if err != nil {
			return nil, err
		}
		if err := d.Access.RevokeSession(ctx, p.AuditActor(), p.ID, in.PublicID, p.SessionIDHash,
			access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent)); err != nil {
			return nil, sessionError(d, "revoke session", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "revokeMyOtherSessions",
		Method:      http.MethodPost,
		Path:        "/api/me/sessions/revoke-others",
		Summary:     "Sign out everywhere else",
		Description: "Ends every session of the caller except the one making this request, and says how many ended. " +
			"Audited as user.sessions_revoked.",
		Tags: []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, in *UserAgentInput) (*SessionsEndedOutput, error) {
		p, err := self(ctx)
		if err != nil {
			return nil, err
		}
		n, err := d.Access.RevokeOtherSessions(ctx, p.AuditActor(), p.ID, p.SessionIDHash,
			access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, sessionError(d, "revoke other sessions", err)
		}
		return &SessionsEndedOutput{CacheControl: "no-store", Body: SessionsEndedBody{Ended: n}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listUserSessions",
		Method:      http.MethodGet,
		Path:        "/api/users/{userId}/sessions",
		Summary:     "List a user's sessions",
		Description: "For an admin on a user's page. No session is marked current. An unknown or deleted user answers 404 user_not_found.",
		Tags:        []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*SessionsOutput, error) {
		list, err := d.Access.ListSessions(ctx, in.UserID, nil)
		if err != nil {
			return nil, sessionError(d, "list user sessions", err)
		}
		return sessionsOutput(list), nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "revokeUserSessions",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/sessions/revoke-all",
		Summary:     "End all of a user's sessions",
		Description: "Signs the user out everywhere at once and says how many ended. Nothing is written when they were not signed in " +
			"anywhere. An unknown or deleted user answers 404 user_not_found. Audited as user.sessions_revoked.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*SessionsEndedOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		n, err := d.Access.RevokeAllSessions(ctx, p.AuditActor(), in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, sessionError(d, "revoke user sessions", err)
		}
		return &SessionsEndedOutput{CacheControl: "no-store", Body: SessionsEndedBody{Ended: n}}, nil
	})
}
