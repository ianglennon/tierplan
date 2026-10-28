// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

const (
	problemGitCredentialInUse = "credential_in_use" //nolint:gosec // G101: a problem type, not a credential
	problemRepositoryNotFound = "repository_not_found"

	gitCredentialTargetType = "git_credential" //nolint:gosec // G101: a target type, not a credential
	gitRepoTargetType       = "git_repo"
)

// GitCredentialRequest is the body of adding or changing a git credential.
type GitCredentialRequest struct {
	Name     string `json:"name,omitempty" maxLength:"256" doc:"What the credential is called, unique among the credentials of its scope. 1 to 64 characters: letters, digits, spaces and . _ ( ) -; 422 name_required or name_invalid"`
	Type     string `json:"type,omitempty" maxLength:"32" doc:"https for a token, ssh for a deploy key; 422 type_invalid for anything else"`
	Username string `json:"username,omitempty" maxLength:"512" doc:"The user name the git server expects with a token (required for https: 422 username_required). Your host decides what it needs here. Defaults to git for an ssh key"`
	Secret   string `json:"secret,omitempty" maxLength:"65536" doc:"The token (https) or the private key (ssh). It is sealed with the vault and never returned by any route: an https token shows back only as its last 4 characters, and only when it has 12 or more; an ssh key as its public key and SHA256 fingerprint. Required when adding (422 secret_required); left out or empty when changing keeps the saved one. 422 secret_invalid for a token with spaces or control characters or over 4096 characters"`
}

// GitCredentialBody is a git credential as the browser sees it. It has no secret field: the token
// or the private key is write-only, and last4 or the public key and fingerprint are all that is
// left of it.
type GitCredentialBody struct {
	ID          string                          `json:"id" doc:"The credential's ID"`
	Scope       CredentialScopeBody             `json:"scope" doc:"Where it can be used: every environment, or one"`
	Name        string                          `json:"name" doc:"Its name, unique within its scope"`
	Type        string                          `json:"type" enum:"https,ssh" doc:"https for a token, ssh for a deploy key"`
	Username    string                          `json:"username" doc:"The user name sent with a token; git for an ssh key unless set"`
	Last4       string                          `json:"last4" doc:"The last 4 characters of an https token, or empty when it is shorter than 12 characters and for an ssh key. The page shows •••• before it"`
	PublicKey   string                          `json:"publicKey" doc:"For an ssh credential, the public key in authorized_keys form, to add to the repository's deploy keys; empty for https"`
	Fingerprint string                          `json:"fingerprint" doc:"For an ssh credential, the SHA256 fingerprint of that key; empty for https"`
	UsedBy      int                             `json:"usedBy" doc:"How many repositories use it"`
	LastUseAt   *string                         `json:"lastUseAt" nullable:"true" format:"date-time" doc:"When a test last signed in with it; null when it never did"`
	LastFailure Nullable[CredentialFailureBody] `json:"lastFailure" doc:"The last time the git server refused it and has not accepted it since; null when there was none"`
	ReadOnly    bool                            `json:"readOnly" doc:"True when the caller may see the credential but not change or delete it"`
	CreatedAt   time.Time                       `json:"createdAt" format:"date-time" doc:"When it was added"`
	UpdatedAt   time.Time                       `json:"updatedAt" format:"date-time" doc:"When it was last changed"`
}

// GitCredentialOutput wraps GitCredentialBody for huma.
type GitCredentialOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitCredentialBody
}

// GitCredentialListBody is the credentials the caller may see.
type GitCredentialListBody struct {
	Items []GitCredentialBody `json:"items" doc:"Instance credentials first, then each environment's, by name"`
}

// GitCredentialListOutput wraps GitCredentialListBody for huma.
type GitCredentialListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitCredentialListBody
}

// CreateGitCredentialInput is the request of the instance create route.
type CreateGitCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      GitCredentialRequest
}

// CreateEnvGitCredentialInput is the request of the environment create route.
type CreateEnvGitCredentialInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      GitCredentialRequest
}

// gitViewer is what one caller may see and manage of the git connections.
type gitViewer struct {
	global  []rbac.Perm
	perEnv  map[string][]rbac.Perm
	anyRead bool
}

func (v gitViewer) holds(envID string, p rbac.Perm) bool {
	return slices.Contains(v.perEnv[envID], p) || slices.Contains(v.perEnv[rbac.AllEnvironments], p)
}

// admin reports an instance admin, who holds every global permission.
func (v gitViewer) admin() bool { return slices.Contains(v.global, rbac.PermGitInstance) }

// manages reports whether the caller may change connections of envID: the instance layer is for
// instance admins alone, an environment's for its Admin and instance admins.
func (v gitViewer) manages(envID string) bool {
	if envID == "" {
		return v.admin()
	}
	return v.admin() || v.holds(envID, rbac.PermGitManage)
}

// gitViewerOf reads what one caller may do, from the same effective permissions the policy
// enforces with.
func gitViewerOf(ctx context.Context, checker *rbac.Checker, p *rbac.Principal) (gitViewer, error) {
	global, perEnv, err := checker.EffectivePermissions(ctx, p)
	if err != nil {
		return gitViewer{}, err
	}
	v := gitViewer{global: global, perEnv: perEnv}
	for _, perms := range perEnv {
		if slices.Contains(perms, rbac.PermGitRead) {
			v.anyRead = true
			break
		}
	}
	return v, nil
}

// scopeBody says where a connection of envID applies.
func (d GitDeps) scopeBody(ctx context.Context, envID string) CredentialScopeBody {
	if envID == "" {
		return CredentialScopeBody{Kind: gitconn.LayerInstance}
	}
	id := envID
	return CredentialScopeBody{Kind: gitconn.LayerEnvironment, EnvID: &id, EnvName: d.envName(ctx, envID)}
}

// envName is the name of an environment, or nil for the instance layer or one that is gone.
func (d GitDeps) envName(ctx context.Context, envID string) *string {
	if envID == "" || d.Envs == nil {
		return nil
	}
	e, err := d.Envs.Get(ctx, envID)
	if err != nil {
		return nil
	}
	return &e.Name
}

// gitCredentialBody maps a stored credential to its body. readOnly is the caller's.
func (d GitDeps) gitCredentialBody(ctx context.Context, c gitconn.Credential, readOnly bool) GitCredentialBody {
	b := GitCredentialBody{
		ID: c.ID, Scope: d.scopeBody(ctx, c.EnvID), Name: c.Name, Type: c.Type, Username: c.Username, Last4: c.Last4,
		PublicKey: c.PublicKey, Fingerprint: c.Fingerprint, UsedBy: c.UsedBy, ReadOnly: readOnly,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	if c.LastUseAt != nil {
		s := c.LastUseAt.UTC().Format(time.RFC3339)
		b.LastUseAt = &s
	}
	if c.LastFailure != nil {
		b.LastFailure = Some(CredentialFailureBody{At: c.LastFailure.At, Code: c.LastFailure.Code})
	}
	return b
}

// gitCredentialAuditDetails is what the audit row of a credential change records: what was stored,
// never the token or the key or any part of it (T-09-12).
func gitCredentialAuditDetails(c gitconn.Credential) map[string]string {
	return map[string]string{
		"name": c.Name, "layer": c.Layer(), "type": c.Type, "username": c.Username, "fingerprint": c.Fingerprint,
	}
}

// gitFieldProblems answers a refused submission as one 422 validation_failed that names every
// field at fault, with its code as the message (the setup form's convention).
func gitFieldProblems(ve *gitconn.ValidationError) error {
	problems := make([]*huma.ErrorDetail, len(ve.Fields))
	for i, f := range ve.Fields {
		problems[i] = &huma.ErrorDetail{Location: "body." + f.Field, Message: f.Code}
	}
	return &huma.ErrorModel{
		Type: problemValidationFailed, Title: http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity, Detail: "validation failed", Errors: problems,
	}
}

// gitFail words an error of the store for the layer it happened in.
func (d GitDeps) gitFail(what string, err error) error {
	var ve *gitconn.ValidationError
	var inUse *gitconn.InUseError
	switch {
	case errors.As(err, &ve):
		return gitFieldProblems(ve)
	case errors.Is(err, gitconn.ErrNameTaken):
		return httpx.Problem(http.StatusConflict, problemNameTaken, "Another one in this scope already has that name.")
	case errors.As(err, &inUse):
		m := &huma.ErrorModel{
			Type: problemGitCredentialInUse, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
			Detail: "Repositories use this credential. Pick another credential for them first.",
			Errors: []*huma.ErrorDetail{{Location: "conflict.count", Message: strconv.Itoa(inUse.Count)}},
		}
		return m
	case errors.Is(err, gitconn.ErrNotFound):
		return httpx.Problem(http.StatusNotFound, problemCredentialNotFound, "not found")
	}
	d.Log.Error("git connection request failed", "step", what, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// gitAudit is the audit row of a change to a git connection, written in the write's transaction.
func (d GitDeps) gitAudit(ctx context.Context, tx *sql.Tx, p *rbac.Principal, userAgent, action, targetType, targetID, envID string, details map[string]string) error {
	return d.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: targetType, TargetID: targetID, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	})
}

// createGitCredential stores a credential of the instance layer (envID empty) or of one
// environment and audits it in the same transaction.
func (d GitDeps) createGitCredential(ctx context.Context, p *rbac.Principal, envID, userAgent string, req GitCredentialRequest) (*GitCredentialOutput, error) {
	in := gitconn.CredentialInput{Name: req.Name, Type: req.Type, Username: req.Username, Secret: req.Secret}
	if envID != "" {
		in.EnvID = &envID
	}
	c, err := d.Conns.CreateCredential(ctx, in, p.ID, func(ctx context.Context, tx *sql.Tx, c gitconn.Credential) error {
		return d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitCredentialCreated, gitCredentialTargetType, c.ID, envID, gitCredentialAuditDetails(c))
	})
	if err != nil {
		return nil, d.gitFail("create credential", err)
	}
	return &GitCredentialOutput{CacheControl: "no-store", Body: d.gitCredentialBody(ctx, c, false)}, nil
}

// registerGitCredentials registers the credential routes.
func registerGitCredentials(r *httpx.Router, d GitDeps) {
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	const secretDoc = "The token or private key is sealed with the vault, included in backups and re-wrapped by key rotation, and never returned by any route afterwards. " +
		"422 validation_failed names every field at fault with a stable code (name_required, name_invalid, type_invalid, username_required, username_invalid, secret_required, secret_invalid); " +
		"409 name_taken when another credential of the scope has the name. "

	httpx.Register(r, huma.Operation{
		OperationID:   "createGitCredentialInstance",
		Method:        http.MethodPost,
		Path:          "/api/git/credentials",
		Summary:       "Add an instance git credential",
		DefaultStatus: http.StatusCreated,
		Description: "Stores a git sign-in that repositories of every environment can pick. A repository uses a credential only when it names it: nothing is matched by address. " + secretDoc +
			"Needs git.instance, which only instance admins hold. Audited as git.credential_created without the secret.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitInstance), func(ctx context.Context, in *CreateGitCredentialInput) (*GitCredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Conns == nil {
			return nil, unavailable()
		}
		return d.createGitCredential(ctx, p, "", in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "createGitCredential",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/git/credentials",
		Summary:       "Add a git credential to an environment",
		DefaultStatus: http.StatusCreated,
		Description: "Stores a git sign-in that repositories of this environment can pick, and no other environment's. " + secretDoc +
			"Needs git.manage on the environment (its Admin, or an instance admin); an Operator or Viewer gets 403 and an environment the caller cannot see 404. " +
			"Audited as git.credential_created with the environment, without the secret.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *CreateEnvGitCredentialInput) (*GitCredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Conns == nil {
			return nil, unavailable()
		}
		return d.createGitCredential(ctx, p, in.EnvID, in.UserAgent, in.Body)
	})
}
