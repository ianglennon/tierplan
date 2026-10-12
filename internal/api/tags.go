// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
)

// TagsDeps are the dependencies of the tag browsing route.
type TagsDeps struct {
	// Resolver chooses the credential a repository is browsed with, from the same candidates
	// pulls use. Nil answers 503 registries_unavailable.
	Resolver *registry.Resolver
	// Tags lists a repository's tags. Nil answers 503 registries_unavailable.
	Tags *registry.TagService
	// Checker decides who may browse and with which credentials.
	Checker *rbac.Checker
	// Aggregator reaches the nodes of an environment, for the rows that say what runs there.
	Aggregator *envs.Aggregator
	// Audit records a refused request. Browsing itself is not audited (D-31).
	Audit *audit.Writer
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// TagNameBody is one tag of a repository.
type TagNameBody struct {
	Name string `json:"name" doc:"The tag, as the registry names it"`
}

// TagListBody is one page of a repository's tags.
type TagListBody struct {
	Repo       string                           `json:"repo" doc:"The repository below the registry host: acme/app, or library/nginx on Docker Hub"`
	Host       string                           `json:"host" doc:"The registry host: ghcr.io, or docker.io for Docker Hub"`
	Credential Nullable[ResolvedCredentialBody] `json:"credential" doc:"The stored credential the registry was asked with, or null when it was asked anonymously. Never the secret"`
	Tags       []TagNameBody                    `json:"tags" doc:"The tags of this page"`
	Hidden     int                              `json:"hidden" doc:"How many signature, attestation and cache tags match the filter text, whether or not they are hidden"`
	Total      int                              `json:"total" doc:"How many tags the list has once the hide switch and the filter text are applied"`
	More       bool                             `json:"more" doc:"Whether another page follows this one"`
	Page       int                              `json:"page" doc:"This page's number, from 0"`
	Capped     bool                             `json:"capped" doc:"True when the repository has more tags than the server reads (10,000); the list is then the first 10,000 by registry order"`
}

// BrowseTagsInput is the request of the tag browsing route.
type BrowseTagsInput struct {
	Ref   string `query:"ref" required:"true" maxLength:"512" doc:"The repository to browse, like ghcr.io/acme/app or nginx. A tag or digest on it is ignored. 422 ref_invalid"`
	EnvID string `query:"envId" maxLength:"64" doc:"An environment whose own credentials are considered as well as the instance's. The caller needs images.read on it (403 otherwise). Left out, only the instance credentials are"`
	Q     string `query:"q" maxLength:"128" doc:"Only tags whose name contains this text, ignoring case"`
	Hide  bool   `query:"hide" default:"true" doc:"Leave out signature, attestation and cache tags (sha256-<digest> with .sig, .att, .sbom, .sbom.json or .prov, anything ending .sig, .att or .sbom, buildcache, cache, and names ending -buildcache or .cache). On by default"`
	Page  int    `query:"page" minimum:"0" maximum:"100" default:"0" doc:"Which page, from 0; 100 tags to a page, newest version first and then by name, descending"`
	// UserAgent is read only to record a refusal in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// TagListOutput wraps TagListBody for huma.
type TagListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TagListBody
}

// RegisterTags registers the tag browsing route. It is always registered; without a
// TagService it answers 503 registries_unavailable.
func RegisterTags(r *httpx.Router, d TagsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "browseTags",
		Method:      http.MethodGet,
		Path:        "/api/registries/tags",
		Summary:     "List the tags of a repository",
		Description: "Lists a repository's tag names through the server, with the stored credential pulls would use for it among the ones the caller may see: the instance credentials and, with envId, " +
			"that environment's own. The tags come newest version first and then by name, descending, 100 to a page (page, from 0), filtered by the text q and without signature, attestation and cache tags unless hide is false. The answer names the credential, never holds it, and is cached for a minute per credential, so a caller never gets a list fetched with a credential they cannot use. " +
			"Needs images.read on at least one environment, and on envId when it is given (403). An unparsable reference is 422 ref_invalid. When the registry cannot list the tags the answer is 404 " +
			"repository_not_found, or 502 with the problem type needs_login (the registry wants a login and no stored credential matches), login_refused (it refused the matching credential), " +
			"registry_unreachable or registry_tls; errors names the registry as registry.host and registry.repo and the credential used, if any, as credential.id, credential.name, credential.scopeKind and credential.scopeName. Not audited.",
		Tags: []string{"registries"},
	}, rbac.SelfService, func(ctx context.Context, in *BrowseTagsInput) (*TagListOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Tags == nil || d.Resolver == nil || d.Checker == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemRegistriesUnavailable, "Registry browsing is not available on this server.")
		}
		// Authorise before anything is read: no cache is consulted for a caller who may not
		// browse (T-05-60).
		viewer, err := viewerOf(ctx, d.Checker, p)
		if err != nil {
			d.Log.Error("tag browsing: effective permissions", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		if !viewer.anyImages || (in.EnvID != "" && !viewer.holds(in.EnvID, rbac.PermImagesRead)) {
			return nil, d.refuse(ctx, p, in)
		}
		ref, err := registry.ParseRef(in.Ref)
		if err != nil {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRefInvalid, "Enter an image like nginx:1.27 or ghcr.io/acme/app:latest.")
		}
		visible := func(c registry.Candidate) bool { return viewer.sees(registry.Credential{EnvID: c.EnvID}) }
		cred, err := d.Resolver.ForBrowse(ctx, in.EnvID, visible, ref)
		if err != nil {
			d.Log.Error("tag browsing: choose a credential", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		page, err := d.Tags.List(ctx, cred, ref, in.Q, in.Hide, in.Page)
		if err != nil {
			return nil, d.tagProblem(ctx, ref, cred, err)
		}
		d.noteSignIn(ctx, cred, "")
		body := TagListBody{
			Repo: page.Repo, Host: page.Host, Tags: make([]TagNameBody, len(page.Tags)),
			Hidden: page.Hidden, Total: page.Total, More: page.More, Page: max(in.Page, 0), Capped: page.Capped,
		}
		for i, t := range page.Tags {
			body.Tags[i] = TagNameBody{Name: t.Name}
		}
		if cred != nil {
			body.Credential = Some(ResolvedCredentialBody{
				ID: cred.ID, Name: cred.Name, Scope: candidateScope(cred.Candidate), Host: cred.Candidate.Host, Prefix: cred.Prefix,
			})
		}
		return &TagListOutput{CacheControl: "no-store", Body: body}, nil
	})
}

// refuse audits a browse the caller has no right to and answers 403.
func (d TagsDeps) refuse(ctx context.Context, p *rbac.Principal, in *BrowseTagsInput) error {
	if d.Audit != nil {
		if err := d.Audit.Record(ctx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
			Reason: rbac.ReasonForbidden, TargetType: "route", TargetID: "GET /api/registries/tags", EnvID: in.EnvID,
			IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{"permission": string(rbac.PermImagesRead)},
		}); err != nil {
			d.Log.Error("could not audit a refused tag browse", "error", err)
			return huma.Error500InternalServerError("internal error")
		}
	}
	return httpx.Problem(http.StatusForbidden, "", "You do not have permission to browse tags.")
}

// tagProblem answers a failed listing with the problem type the SPA words, and names the
// credential the registry was asked with. What the registry said is never carried or logged.
func (d TagsDeps) tagProblem(ctx context.Context, ref registry.Ref, cred *registry.ResolvedCredential, err error) error {
	code := registry.TagErrorCode(err)
	if code == "" {
		if errors.Is(err, context.Canceled) {
			return httpx.Problem(http.StatusServiceUnavailable, "", "The request was cancelled.")
		}
		d.Log.Error("tag browsing: list tags", "host", ref.Domain, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	if code == registry.CodeLoginRefused {
		d.noteSignIn(ctx, cred, dockerx.PullCodeUnauthorized)
	}
	status := http.StatusBadGateway
	if code == registry.CodeRepositoryNotFound {
		status = http.StatusNotFound
	}
	m := &huma.ErrorModel{
		Type: code, Title: http.StatusText(status), Status: status,
		Detail: "The registry " + ref.Domain + " could not list the tags of " + ref.Path + ".",
	}
	m.Errors = []*huma.ErrorDetail{
		{Location: "registry.host", Message: ref.Domain},
		{Location: "registry.repo", Message: ref.Path},
	}
	if cred != nil {
		scope := candidateScope(cred.Candidate)
		name := ""
		if scope.EnvName != nil {
			name = *scope.EnvName
		}
		m.Errors = append(m.Errors,
			&huma.ErrorDetail{Location: "credential.id", Message: cred.ID},
			&huma.ErrorDetail{Location: "credential.name", Message: cred.Name},
			&huma.ErrorDetail{Location: "credential.scopeKind", Message: scope.Kind},
			&huma.ErrorDetail{Location: "credential.scopeName", Message: name},
		)
	}
	return m
}

// noteSignIn keeps the credential's bookkeeping (D-07): a refused sign-in records the failure
// with failure, and a listing that worked (failure empty) clears it. A failed write is logged
// and changes nothing else.
func (d TagsDeps) noteSignIn(ctx context.Context, cred *registry.ResolvedCredential, failure string) {
	if cred == nil {
		return
	}
	var err error
	if failure == "" {
		err = d.Resolver.ClearFailure(ctx, cred.ID)
	} else {
		err = d.Resolver.RecordFailure(ctx, cred.ID, failure)
	}
	if err != nil {
		d.Log.Error("tag browsing: could not update a credential's last failure", "credential", cred.ID, "error", err)
	}
}
