// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
)

const (
	problemRegistriesUnavailable = "registries_unavailable"
	problemCredentialNameTaken   = "credential_name_taken" //nolint:gosec // G101: a problem or target type, not a credential
	problemHostPrefixTaken       = "host_prefix_taken"
	problemHubFallbackTaken      = "hub_fallback_taken"
	problemCredentialNotFound    = "credential_not_found" //nolint:gosec // G101: a problem or target type, not a credential

	credentialTargetType = "registry_credential" //nolint:gosec // G101: a problem or target type, not a credential
)

// RegistriesDeps are the dependencies of the registry credential routes.
type RegistriesDeps struct {
	// Store keeps the credentials. Nil answers 503 registries_unavailable, so the routes
	// are always registered.
	Store *registry.Store
	// Envs names the environment of an environment credential.
	Envs  *envs.Service
	Audit *audit.Writer
	// Checker decides who may see and manage which credentials.
	Checker *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// CredentialRequest is the body of POST /api/registries.
type CredentialRequest struct {
	Name        string `json:"name,omitempty" maxLength:"256" doc:"What the credential is called, unique among the instance credentials. 1 to 64 characters: letters, digits, spaces and . _ ( ) -; 422 name_required or name_invalid"`
	Type        string `json:"type,omitempty" maxLength:"32" doc:"dockerhub, ghcr, gitlab or generic. The first three fill in the host; 422 type_invalid for anything else"`
	Host        string `json:"host,omitempty" maxLength:"512" doc:"The registry host, with a port if it has one, like registry.example.org:5000. Ignored for dockerhub and ghcr, registry.gitlab.com when left out for gitlab, required for generic: 422 host_required or host_invalid. No scheme and no path"`
	Prefix      string `json:"prefix,omitempty" maxLength:"512" doc:"The namespace the credential covers, like acme or acme/team: lower-case path segments with no slash at either end. Empty covers the whole registry. 422 prefix_invalid, and hub_prefix_required for docker.io with neither a prefix nor hubFallback"`
	Username    string `json:"username,omitempty" maxLength:"512" doc:"The registry user name; 422 username_required"`
	Secret      string `json:"secret,omitempty" maxLength:"4096" doc:"The password or token. It is sealed with the vault and never returned by any route; only its last 4 characters can be shown, and only when it has 12 or more. 422 secret_required"`
	HubFallback bool   `json:"hubFallback,omitempty" doc:"Use this credential for every Docker Hub pull no more specific credential matches. Docker Hub only (422 hub_fallback_not_hub), one per layer (409 hub_fallback_taken)"`
	Expires     string `json:"expires,omitempty" maxLength:"32" doc:"An optional day, YYYY-MM-DD. It lasts to the end of that day, UTC. 422 expiry_invalid, and expiry_past for a day that has gone"`
}

// CreateCredentialInput is the body of the create route.
type CreateCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      CredentialRequest
}

// CredentialPatchRequest is the body of PATCH /api/registries/{credentialId}. Every member is
// optional: one left out keeps what is stored, and one sent as an empty string clears a prefix
// or an expiry. There is no scope: a credential never moves between layers.
type CredentialPatchRequest struct {
	Name        *string `json:"name,omitempty" maxLength:"256" doc:"A new name, unique among the instance credentials. Same rules as when adding; 422 name_required or name_invalid"`
	Type        *string `json:"type,omitempty" maxLength:"32" doc:"dockerhub, ghcr, gitlab or generic; 422 type_invalid for anything else"`
	Host        *string `json:"host,omitempty" maxLength:"512" doc:"A new registry host. Ignored for dockerhub and ghcr; 422 host_required or host_invalid"`
	Prefix      *string `json:"prefix,omitempty" maxLength:"512" doc:"A new namespace; an empty string covers the whole registry. 422 prefix_invalid, and hub_prefix_required for docker.io with neither a prefix nor hubFallback"`
	Username    *string `json:"username,omitempty" maxLength:"512" doc:"A new registry user name; 422 username_required"`
	Secret      *string `json:"secret,omitempty" maxLength:"4096" doc:"A replacement password or token. Left out or empty keeps the stored secret, which is never read back. A new one is sealed with the vault and replaces it. It is never returned by any route"`
	HubFallback *bool   `json:"hubFallback,omitempty" doc:"Whether it is used for every Docker Hub pull no more specific credential matches. Docker Hub only (422 hub_fallback_not_hub), one per layer (409 hub_fallback_taken)"`
	Expires     *string `json:"expires,omitempty" maxLength:"32" doc:"A day, YYYY-MM-DD, or an empty string for none. 422 expiry_invalid, and expiry_past for a day that has gone (the credential's own unchanged day is never refused)"`
}

// UpdateCredentialInput is the request of the update route.
type UpdateCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent    string `header:"User-Agent" hidden:"true"`
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
	Body         CredentialPatchRequest
}

// DeleteCredentialInput is the request of the delete route.
type DeleteCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent    string `header:"User-Agent" hidden:"true"`
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
}

// CredentialScopeBody says where a credential applies.
type CredentialScopeBody struct {
	Kind    string  `json:"kind" enum:"instance,environment" doc:"instance: every environment. environment: one environment only"`
	EnvID   *string `json:"envId" nullable:"true" doc:"The environment of an environment credential; null for an instance credential"`
	EnvName *string `json:"envName" nullable:"true" doc:"The name of that environment; null for an instance credential"`
}

// CredentialTestBody is the outcome of the last test of a credential.
type CredentialTestBody struct {
	At     time.Time `json:"at" format:"date-time" doc:"When it was tested"`
	OK     bool      `json:"ok" doc:"Whether the registry accepted it"`
	Detail string    `json:"detail" doc:"A short sentence about the outcome. Never the secret"`
}

// CredentialFailureBody is the last time a pull or a browse failed to sign in with the credential.
type CredentialFailureBody struct {
	At   time.Time `json:"at" format:"date-time" doc:"When it failed"`
	Code string    `json:"code" doc:"A stable code, like unauthorized or denied"`
}

// CredentialBody is a registry credential as the browser sees it. It has no secret field:
// the secret is write-only (D-05), and last4 is all that is left of it.
type CredentialBody struct {
	ID          string                          `json:"id" doc:"The credential's ID"`
	Scope       CredentialScopeBody             `json:"scope" doc:"Where it applies"`
	Name        string                          `json:"name" doc:"Its name, unique within its scope"`
	Type        string                          `json:"type" enum:"dockerhub,ghcr,gitlab,generic" doc:"The registry preset"`
	Host        string                          `json:"host" doc:"The registry host, lower case, with a port if it has one"`
	Prefix      string                          `json:"prefix" doc:"The namespace it covers; empty for the whole registry"`
	Username    string                          `json:"username" doc:"The registry user name"`
	Last4       string                          `json:"last4" doc:"The last 4 characters of the secret, or empty when the secret is shorter than 12 characters. The page shows •••• before it"`
	HubFallback bool                            `json:"hubFallback" doc:"Whether it is used for every Docker Hub pull no more specific credential matches"`
	ExpiresAt   *string                         `json:"expiresAt" nullable:"true" format:"date-time" doc:"The end of the last day it is valid; null when it does not expire"`
	LastTest    Nullable[CredentialTestBody]    `json:"lastTest" doc:"The last test of the credential; null when it was never tested"`
	LastFailure Nullable[CredentialFailureBody] `json:"lastFailure" doc:"The last sign-in failure with it; null when there was none"`
	CanManage   bool                            `json:"canManage" doc:"Whether the caller may change or delete it"`
	CreatedAt   time.Time                       `json:"createdAt" format:"date-time" doc:"When it was added"`
	UpdatedAt   time.Time                       `json:"updatedAt" format:"date-time" doc:"When it was last changed"`
}

// CredentialOutput wraps CredentialBody for huma.
type CredentialOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CredentialBody
}

// CredentialListBody is the credentials the caller may see.
type CredentialListBody struct {
	Items []CredentialBody `json:"items" doc:"Instance credentials first, then each environment's, by host and prefix"`
}

// CredentialListOutput wraps CredentialListBody for huma.
type CredentialListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CredentialListBody
}

// CredentialPathInput names one credential.
type CredentialPathInput struct {
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
}

// credentialViewer is what one caller may see and manage.
type credentialViewer struct {
	global    []rbac.Perm
	perEnv    map[string][]rbac.Perm
	anyImages bool
}

func (v credentialViewer) holds(envID string, p rbac.Perm) bool {
	return slices.Contains(v.perEnv[envID], p) || slices.Contains(v.perEnv[rbac.AllEnvironments], p)
}

// admin reports an instance admin, who holds every global permission.
func (v credentialViewer) admin() bool { return slices.Contains(v.global, rbac.PermRegistriesInstance) }

// sees is D-09: an instance admin sees everything; anyone who can read images on an
// environment sees the instance credentials, and an environment's own credentials when they
// can read that environment.
func (v credentialViewer) sees(c registry.Credential) bool {
	if v.admin() {
		return true
	}
	if c.EnvID == "" {
		return v.anyImages
	}
	return v.holds(c.EnvID, rbac.PermImagesRead)
}

func (v credentialViewer) canManage(c registry.Credential) bool {
	if c.EnvID == "" {
		return v.admin()
	}
	return v.admin() || v.holds(c.EnvID, rbac.Perm("registries.manage"))
}

// RegisterRegistries registers the registry credential routes that are not in an environment's
// path: create an instance credential, list and read the credentials the caller may see.
// The routes are always registered; without a Store they answer 503 registries_unavailable.
func RegisterRegistries(r *httpx.Router, d RegistriesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	fail := func(what string, err error) error {
		var ve *registry.ValidationError
		switch {
		case errors.As(err, &ve):
			return credentialFieldProblems(ve)
		case errors.Is(err, registry.ErrNameTaken):
			return conflictProblem(err, problemCredentialNameTaken, "Another instance credential already has that name.")
		case errors.Is(err, registry.ErrHostPrefixTaken):
			return conflictProblem(err, problemHostPrefixTaken, "Another instance credential already covers that registry and namespace.")
		case errors.Is(err, registry.ErrHubFallbackTaken):
			return conflictProblem(err, problemHubFallbackTaken, "Another instance credential is already used for all Docker Hub pulls.")
		case errors.Is(err, registry.ErrNotFound):
			return httpx.Problem(http.StatusNotFound, problemCredentialNotFound, "credential not found")
		}
		d.Log.Error("registry credential request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemRegistriesUnavailable, "Registry credentials are not available on this server.")
	}
	viewerOf := func(ctx context.Context, p *rbac.Principal) (credentialViewer, error) {
		global, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
		if err != nil {
			return credentialViewer{}, err
		}
		v := credentialViewer{global: global, perEnv: perEnv}
		for _, perms := range perEnv {
			if slices.Contains(perms, rbac.PermImagesRead) {
				v.anyImages = true
				break
			}
		}
		return v, nil
	}
	body := func(ctx context.Context, v credentialViewer, c registry.Credential) CredentialBody {
		return credentialBody(c, v.canManage(c), d.envName(ctx, c.EnvID))
	}

	httpx.Register(r, huma.Operation{
		OperationID:   "createInstanceCredential",
		Method:        http.MethodPost,
		Path:          "/api/registries",
		Summary:       "Add an instance registry credential",
		DefaultStatus: http.StatusCreated,
		Description: "Stores a sign-in for a registry host and an optional namespace, used for pulls and tag browsing on every environment. The secret is sealed with the vault, " +
			"included in backups and re-wrapped by key rotation, and never returned by any route afterwards: the answer carries only its last 4 characters, and only for a secret of 12 or more characters. " +
			"Needs registries.instance, which only instance admins hold. 422 validation_failed names every field at fault with a stable code (name_required, name_invalid, type_invalid, host_required, host_invalid, " +
			"prefix_invalid, username_required, secret_required, hub_prefix_required, hub_fallback_not_hub, expiry_invalid, expiry_past); 409 credential_name_taken, host_prefix_taken or hub_fallback_taken " +
			"when another instance credential clashes; the 409 names that credential in its errors list (location conflict.credential). Audited as registry.credential_created without the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *CreateCredentialInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		req := in.Body
		c, err := d.Store.Create(ctx, registry.CreateInput{
			Name: req.Name, Type: req.Type, Host: req.Host, Prefix: req.Prefix, Username: req.Username,
			Secret: req.Secret, HubFallback: req.HubFallback, Expires: req.Expires,
		}, p.ID, func(ctx context.Context, tx *sql.Tx, c registry.Credential) error {
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialCreated, TargetType: credentialTargetType, TargetID: c.ID,
				Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent, Details: credentialAuditDetails(c),
			})
		})
		if err != nil {
			return nil, fail("create credential", err)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: credentialBody(c, true, nil)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateInstanceCredential",
		Method:      http.MethodPatch,
		Path:        "/api/registries/{credentialId}",
		Summary:     "Change an instance registry credential",
		Description: "Changes the members that are sent and keeps the rest. The secret is write-only: leave it out or send it empty to keep the stored one, which is neither read nor returned; " +
			"send a new one and it is sealed again and replaces it, and the answer carries only its last 4 characters (12 or more characters). There is no scope: a credential never moves between layers. " +
			"Needs registries.instance. A credential of an environment answers 404 credential_not_found here, exactly like one that does not exist. 422 validation_failed names every field at fault " +
			"with the same codes as adding; 409 credential_name_taken, host_prefix_taken or hub_fallback_taken when another instance credential clashes, naming it in its errors list (location conflict.credential). " +
			"Audited as registry.credential_updated with a changed detail listing the names of the fields that changed, the secret only as the word secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *UpdateCredentialInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		cur, err := d.Store.Get(ctx, in.CredentialID)
		if err != nil {
			return nil, fail("get credential", err)
		}
		// This family manages the instance layer only. An environment's credential is not
		// reachable through it, whatever the caller holds.
		if cur.EnvID != "" {
			return nil, fail("get credential", registry.ErrNotFound)
		}
		c, _, err := d.Store.Update(ctx, in.CredentialID, mergeCredentialPatch(cur, in.Body), func(ctx context.Context, tx *sql.Tx, c registry.Credential, changed []string) error {
			details := credentialAuditDetails(c)
			details["changed"] = strings.Join(changed, ",")
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialUpdated, TargetType: credentialTargetType, TargetID: c.ID,
				Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent, Details: details,
			})
		})
		if err != nil {
			return nil, fail("update credential", err)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: credentialBody(c, true, nil)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteInstanceCredential",
		Method:        http.MethodDelete,
		Path:          "/api/registries/{credentialId}",
		Summary:       "Delete an instance registry credential",
		DefaultStatus: http.StatusNoContent,
		Description: "Removes the credential and its sealed secret. Pulls it would have matched fall back to the next matching credential, or to anonymous; nothing else refers to it. " +
			"Needs registries.instance. A credential that is already gone, and one of an environment, answer 404 credential_not_found, so a repeated delete is safe and writes nothing. " +
			"Audited as registry.credential_deleted without the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *DeleteCredentialInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		cur, err := d.Store.Get(ctx, in.CredentialID)
		if err != nil {
			return nil, fail("get credential", err)
		}
		if cur.EnvID != "" {
			return nil, fail("get credential", registry.ErrNotFound)
		}
		if _, err := d.Store.Delete(ctx, in.CredentialID, func(ctx context.Context, tx *sql.Tx, c registry.Credential) error {
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialDeleted, TargetType: credentialTargetType, TargetID: c.ID,
				Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent, Details: credentialAuditDetails(c),
			})
		}); err != nil {
			return nil, fail("delete credential", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listCredentials",
		Method:      http.MethodGet,
		Path:        "/api/registries",
		Summary:     "List the registry credentials the signed-in user may see",
		Description: "An instance admin sees every credential. Anyone who holds images.read on at least one environment sees the instance credentials, and an environment's own credentials " +
			"when they can read that environment; canManage says whether they may change a credential. A caller with no images.read anywhere gets an empty list. No secret is ever included.",
		Tags: []string{"registries"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*CredentialListOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		v, err := viewerOf(ctx, p)
		if err != nil {
			return nil, fail("effective permissions", err)
		}
		all, err := d.Store.List(ctx)
		if err != nil {
			return nil, fail("list credentials", err)
		}
		items := make([]CredentialBody, 0, len(all))
		for _, c := range all {
			if v.sees(c) {
				items = append(items, body(ctx, v, c))
			}
		}
		return &CredentialListOutput{CacheControl: "no-store", Body: CredentialListBody{Items: items}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getCredential",
		Method:      http.MethodGet,
		Path:        "/api/registries/{credentialId}",
		Summary:     "Read one registry credential",
		Description: "The credential as the list shows it. One the caller may not see answers exactly like one that does not exist: 404 credential_not_found.",
		Tags:        []string{"registries"},
	}, rbac.SelfService, func(ctx context.Context, in *CredentialPathInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		v, err := viewerOf(ctx, p)
		if err != nil {
			return nil, fail("effective permissions", err)
		}
		c, err := d.Store.Get(ctx, in.CredentialID)
		if err != nil {
			return nil, fail("get credential", err)
		}
		if !v.sees(c) {
			return nil, fail("get credential", registry.ErrNotFound)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: body(ctx, v, c)}, nil
	})
}

// mergeCredentialPatch lays the members of a patch over the stored credential. The secret is
// not part of the stored credential at all: it is the replacement, or empty to keep the one
// that is stored.
func mergeCredentialPatch(cur registry.Credential, patch CredentialPatchRequest) registry.UpdateInput {
	in := registry.UpdateInput{
		Name: cur.Name, Type: cur.Type, Host: cur.Host, Prefix: cur.Prefix, Username: cur.Username, HubFallback: cur.HubFallback,
	}
	if cur.ExpiresAt != nil {
		in.Expires = cur.ExpiresAt.UTC().Format(time.DateOnly)
	}
	for dst, src := range map[*string]*string{
		&in.Name: patch.Name, &in.Type: patch.Type, &in.Host: patch.Host, &in.Prefix: patch.Prefix,
		&in.Username: patch.Username, &in.Secret: patch.Secret, &in.Expires: patch.Expires,
	} {
		if src != nil {
			*dst = *src
		}
	}
	if patch.HubFallback != nil {
		in.HubFallback = *patch.HubFallback
	}
	return in
}

// conflictProblem answers a clash within the layer as a 409 that names the other credential
// in its errors list, so the page can say which one.
func conflictProblem(err error, ptype, detail string) error {
	m := &huma.ErrorModel{Type: ptype, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict, Detail: detail}
	var ce *registry.ConflictError
	if errors.As(err, &ce) && ce.With != "" {
		m.Errors = []*huma.ErrorDetail{{Location: "conflict.credential", Message: ce.With}}
	}
	return m
}

// envName is the name of an environment, or nil for an instance credential or one that is gone.
func (d RegistriesDeps) envName(ctx context.Context, envID string) *string {
	if envID == "" || d.Envs == nil {
		return nil
	}
	e, err := d.Envs.Get(ctx, envID)
	if err != nil {
		return nil
	}
	return &e.Name
}

// credentialBody maps a stored credential to its body. canManage is the caller's.
func credentialBody(c registry.Credential, canManage bool, envName *string) CredentialBody {
	b := CredentialBody{
		ID: c.ID, Scope: CredentialScopeBody{Kind: c.Layer(), EnvName: envName}, Name: c.Name, Type: c.Type, Host: c.Host,
		Prefix: c.Prefix, Username: c.Username, Last4: c.Last4, HubFallback: c.HubFallback, CanManage: canManage,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	if c.EnvID != "" {
		id := c.EnvID
		b.Scope.EnvID = &id
	}
	if c.ExpiresAt != nil {
		s := c.ExpiresAt.UTC().Format(time.RFC3339)
		b.ExpiresAt = &s
	}
	if c.LastTest != nil {
		b.LastTest = Some(CredentialTestBody{At: c.LastTest.At, OK: c.LastTest.OK, Detail: c.LastTest.Detail})
	}
	if c.LastFailure != nil {
		b.LastFailure = Some(CredentialFailureBody{At: c.LastFailure.At, Code: c.LastFailure.Code})
	}
	return b
}

// credentialAuditDetails is what the audit row of a credential change records: what was stored,
// never the secret or any part of it (T-05-23).
func credentialAuditDetails(c registry.Credential) map[string]string {
	expires := ""
	if c.ExpiresAt != nil {
		expires = c.ExpiresAt.UTC().Format(time.DateOnly)
	}
	return map[string]string{
		"name": c.Name, "layer": c.Layer(), "host": c.Host, "prefix": c.Prefix, "type": c.Type,
		"username": c.Username, "hub_fallback": strconv.FormatBool(c.HubFallback), "expires": expires,
	}
}

// credentialFieldProblems answers a refused submission as one 422 validation_failed that names
// every field at fault, with its code as the message (the setup form's convention).
func credentialFieldProblems(ve *registry.ValidationError) error {
	problems := make([]*huma.ErrorDetail, len(ve.Fields))
	for i, f := range ve.Fields {
		problems[i] = &huma.ErrorDetail{Location: "body." + f.Field, Message: f.Code}
	}
	return &huma.ErrorModel{
		Type: problemValidationFailed, Title: http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity, Detail: "validation failed", Errors: problems,
	}
}
