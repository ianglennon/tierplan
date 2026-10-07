// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// targetLink is the audit target type of a refused link. The target ID stays
// empty: the link is a secret and is never recorded, not even a hash of it.
const targetLink = "link"

// Audit reasons recorded with auth.link_refused.
const reasonInvalidLink = "invalid_link"

// LookupInviteInput is a link being opened.
type LookupInviteInput struct {
	Body struct {
		// maxLength caps the submission well above a real token (43 characters).
		Token string `json:"token" maxLength:"128" doc:"The token from the part of the link after #token="`
	}
}

// LookupInviteBody says what the link is for.
type LookupInviteBody struct {
	Kind         string `json:"kind" enum:"invite,reset" doc:"invite for a new user choosing a first password, reset for an existing user choosing a new one"`
	Username     string `json:"username" doc:"Whose link it is"`
	InstanceName string `json:"instanceName" doc:"The name of this Bayplan instance"`
}

// LookupInviteOutput wraps LookupInviteBody for huma.
type LookupInviteOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         LookupInviteBody
}

// AcceptInviteInput is a password chosen with a link.
type AcceptInviteInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Token    string `json:"token" maxLength:"128" doc:"The token from the part of the link after #token="`
		Password string `json:"password" maxLength:"1024" doc:"At least 12 characters, not on the common-password list and not containing the username"`
	}
}

// AcceptInviteOutput is an accepted link: no body, and no session.
type AcceptInviteOutput struct {
	CacheControl string `header:"Cache-Control"`
}

func registerInvite(r *httpx.Router, d AccessDeps) {
	anon := d.AnonAudit
	if anon == nil && d.Access != nil {
		anon = d.Access.Audit
	}
	internal := func(what string, err error) error {
		d.Log.Error("invite request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	// refuse records a refused link attempt and returns the answer. A failed
	// audit write fails the request: nothing is refused unrecorded.
	refuse := func(ctx context.Context, ua, reason string, status int, ptype, detail string) error {
		if err := anon.Record(ctx, audit.Entry{
			Actor:      audit.Actor{Type: audit.ActorAnonymous},
			Action:     audit.ActionAuthLinkRefused,
			TargetType: targetLink,
			Result:     audit.ResultFailure,
			Reason:     reason,
			IP:         httpx.ClientIP(ctx),
			UserAgent:  ua,
		}); err != nil {
			return internal("audit refused link", err)
		}
		return httpx.Problem(status, ptype, detail)
	}
	// limited spends one attempt of the caller's address before any work is done.
	limited := func(ctx context.Context, ua string) error {
		if d.Limiter == nil || d.Limiter.Allow(httpx.ClientIP(ctx)) {
			return nil
		}
		return refuse(ctx, ua, reasonRateLimited, http.StatusTooManyRequests, problemRateLimited, "Too many attempts. Wait a minute and try again.")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "lookupInvite",
		Method:      http.MethodPost,
		Path:        "/api/invite/lookup",
		Summary:     "Say what a one-time link is for",
		Description: "Public. The token is in the body, never in the address. Every link that cannot be used, whether " +
			"expired, used, replaced, unknown or belonging to a disabled or deleted user, answers the same 404 " +
			"invalid_link.",
		Tags: []string{"users"},
	}, rbac.Public, func(ctx context.Context, in *LookupInviteInput) (*LookupInviteOutput, error) {
		if err := limited(ctx, ""); err != nil {
			return nil, err
		}
		info, err := d.Access.LookupLink(ctx, in.Body.Token)
		if errors.Is(err, access.ErrInvalidLink) {
			return nil, refuse(ctx, "", reasonInvalidLink, http.StatusNotFound, problemInvalidLink, invalidLinkDetail)
		}
		if err != nil {
			return nil, internal("look up link", err)
		}
		return &LookupInviteOutput{
			CacheControl: "no-store",
			Body:         LookupInviteBody{Kind: info.Kind, Username: info.Username, InstanceName: d.Instance.Name()},
		}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "acceptInvite",
		Method:        http.MethodPost,
		Path:          "/api/invite/accept",
		Summary:       "Choose a password with a one-time link",
		DefaultStatus: http.StatusNoContent,
		Description: "Public. Spends the link and sets the user's password. It does not sign anyone in: the user signs in " +
			"afterwards with the password they chose. A link works once.",
		Tags: []string{"users"},
	}, rbac.Public, func(ctx context.Context, in *AcceptInviteInput) (*AcceptInviteOutput, error) {
		if err := limited(ctx, in.UserAgent); err != nil {
			return nil, err
		}
		err := d.Access.AcceptLink(ctx, in.Body.Token, in.Body.Password, httpx.ClientIP(ctx), in.UserAgent)
		switch {
		case errors.Is(err, access.ErrInvalidLink):
			return nil, refuse(ctx, in.UserAgent, reasonInvalidLink, http.StatusNotFound, problemInvalidLink, invalidLinkDetail)
		case errors.Is(err, auth.ErrPasswordTooShort):
			return nil, passwordProblem(codePasswordTooShort)
		case errors.Is(err, auth.ErrPasswordCommon):
			return nil, passwordProblem(codePasswordCommon)
		case err != nil:
			return nil, internal("accept link", err)
		}
		return &AcceptInviteOutput{CacheControl: "no-store"}, nil
	})
}

// passwordProblem is the 422 for a password the policy refuses, with the same
// stable codes as the setup wizard and the change-password dialog.
func passwordProblem(code string) error {
	return &huma.ErrorModel{
		Type:   problemValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: "validation failed",
		Errors: []*huma.ErrorDetail{{Location: "body.password", Message: code}},
	}
}
