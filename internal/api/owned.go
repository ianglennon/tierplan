// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/access"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
)

// OwnedBody is an automation a user owns: a git sync or a webhook that acts on
// its owner's authority (D-14).
type OwnedBody struct {
	Kind          string `json:"kind" doc:"The kind of automation, such as git.sync. Stable; the label is for people"`
	Label         string `json:"label" doc:"The kind named for people, such as Git sync"`
	ResourceID    string `json:"resourceId" doc:"The automation's ID within its kind"`
	Name          string `json:"name" doc:"The automation's name"`
	EnvID         string `json:"envId" doc:"The environment it acts on"`
	EnvName       string `json:"envName" doc:"The environment's name; its ID when the environment no longer exists"`
	State         string `json:"state" enum:"active,paused_lacks_permission,paused_disabled,paused_deleted" doc:"active, or why it is paused: the owner no longer holds the permission it needs, the owner is disabled, or the owner was deleted"`
	NeedsEnvAdmin bool   `json:"needsEnvAdmin" doc:"True when its last run used host-escalating compose keys, so its owner must hold Admin on the environment"`
}

func ownedBody(it access.OwnedItem) OwnedBody {
	return OwnedBody{
		Kind: it.Kind, Label: it.Label, ResourceID: it.ResourceID, Name: it.Name, EnvID: it.EnvID,
		EnvName: it.EnvName, State: it.State, NeedsEnvAdmin: it.NeedsEnvAdmin,
	}
}

// OwnedListOutput is a user's automations, by kind and ID.
type OwnedListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []OwnedBody
}

// OwnedOutput wraps one automation for huma.
type OwnedOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         OwnedBody
}

// UserRefBody is a user an automation could be handed to.
type UserRefBody struct {
	ID          string `json:"id" doc:"The user's ID"`
	Username    string `json:"username" doc:"The lower-cased username"`
	DisplayName string `json:"displayName" doc:"The name shown for the user; empty when none was set"`
}

// EligibleOwnersOutput lists who an automation could be handed to.
type EligibleOwnersOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []UserRefBody
}

// OwnedPathInput names one automation in the path.
type OwnedPathInput struct {
	Kind       string `path:"kind" maxLength:"64" doc:"The automation's kind, as the owned list gives it"`
	ResourceID string `path:"resourceId" maxLength:"256" doc:"The automation's ID within its kind"`
}

// TransferOwnedInput names the automation and its new owner.
type TransferOwnedInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent  string `header:"User-Agent" hidden:"true"`
	Kind       string `path:"kind" maxLength:"64" doc:"The automation's kind, as the owned list gives it"`
	ResourceID string `path:"resourceId" maxLength:"256" doc:"The automation's ID within its kind"`
	Body       struct {
		OwnerID string `json:"ownerId" minLength:"1" maxLength:"64" doc:"The ID of the user who takes it over"`
	}
}

// ownedError answers the errors the owned routes share.
func ownedError(d AccessDeps, op string, err error) error {
	switch {
	case errors.Is(err, access.ErrOwnedNotFound), errors.Is(err, access.ErrUnknownOwnedKind):
		return httpx.Problem(http.StatusNotFound, problemAutomationNotFound, "That automation doesn't exist.")
	case errors.Is(err, access.ErrNotEligible):
		return httpx.Problem(http.StatusUnprocessableEntity, problemOwnerNotEligible,
			"That user doesn't have enough access to run this automation. Choose someone else, or give them access first.")
	}
	return lifecycleError(d, op, err)
}

// registerOwned registers the routes that show a user's automations and hand them
// to someone who could run them (D-14, D-22). All need users.manage.
func registerOwned(r *httpx.Router, d AccessDeps) {
	manage := rbac.Requirement{Perm: rbac.PermUsersManage, Scope: rbac.ScopeGlobal}

	httpx.Register(r, huma.Operation{
		OperationID: "listUserOwned",
		Method:      http.MethodGet,
		Path:        "/api/users/{userId}/owned",
		Summary:     "List the automations a user owns",
		Description: "The git syncs and webhooks that run with the user's permissions, with the environment each acts on and " +
			"whether it is active or paused because its owner was disabled, deleted or no longer holds the permission it needs. " +
			"Empty when they own nothing. An unknown or deleted user answers 404 user_not_found.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *UserIDInput) (*OwnedListOutput, error) {
		if _, err := d.Access.GetUser(ctx, in.UserID); err != nil {
			return nil, ownedError(d, "read user", err)
		}
		items, err := d.Access.ListOwned(ctx, in.UserID)
		if err != nil {
			return nil, ownedError(d, "list owned", err)
		}
		out := &OwnedListOutput{CacheControl: "no-store", Body: make([]OwnedBody, 0, len(items))}
		for _, it := range items {
			out.Body = append(out.Body, ownedBody(it))
		}
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listEligibleOwners",
		Method:      http.MethodGet,
		Path:        "/api/owned/{kind}/{resourceId}/eligible-owners",
		Summary:     "List who an automation could be handed to",
		Description: "The active users, other than its owner, who hold what the automation needs on its environment right now: " +
			"Admin when it needs that, otherwise the permission of its kind. Sorted by username. The transfer checks again, so " +
			"this is a convenience. An unknown automation answers 404 automation_not_found.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *OwnedPathInput) (*EligibleOwnersOutput, error) {
		users, err := d.Access.EligibleOwners(ctx, in.Kind, in.ResourceID)
		if err != nil {
			return nil, ownedError(d, "list eligible owners", err)
		}
		out := &EligibleOwnersOutput{CacheControl: "no-store", Body: make([]UserRefBody, 0, len(users))}
		for _, u := range users {
			out.Body = append(out.Body, UserRefBody{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
		}
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "transferOwned",
		Method:      http.MethodPost,
		Path:        "/api/owned/{kind}/{resourceId}/transfer",
		Summary:     "Hand an automation to another user",
		Description: "The new owner must be able to run it now, which is checked again here: a user who could not answers " +
			"422 owner_not_eligible and nothing changes. A paused automation is active again under its new owner. Audited as " +
			"owner.transferred. Answers the automation as it now is. An unknown automation answers 404 automation_not_found.",
		Tags: []string{"users"},
	}, manage, func(ctx context.Context, in *TransferOwnedInput) (*OwnedOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := d.Access.TransferOwned(ctx, p.AuditActor(), in.Kind, in.ResourceID, in.Body.OwnerID,
			access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent)); err != nil {
			return nil, ownedError(d, "transfer owned", err)
		}
		it, err := d.Access.GetOwned(ctx, in.Kind, in.ResourceID)
		if err != nil {
			return nil, ownedError(d, "read owned", err)
		}
		return &OwnedOutput{CacheControl: "no-store", Body: ownedBody(it)}, nil
	})
}
