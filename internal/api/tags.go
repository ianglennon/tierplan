// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sync/errgroup"

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
	// Envs names the environments, for Running in.
	Envs *envs.Service
	// Audit records a refused request. Browsing itself is not audited (D-31).
	Audit *audit.Writer
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// PlatformBody is one image of a multi-platform tag.
type PlatformBody struct {
	OS      string `json:"os" doc:"The operating system, like linux or windows"`
	Arch    string `json:"arch" doc:"The CPU architecture by its OCI name, like amd64, arm64 or arm"`
	Variant string `json:"variant" doc:"The architecture variant, like v7; empty when there is none"`
	Digest  string `json:"digest" doc:"The digest of this platform's manifest"`
	Size    int64  `json:"size" doc:"The compressed size of the image in bytes, or 0 when the registry does not say"`
}

func platformBodies(ps []registry.Platform) []PlatformBody {
	out := make([]PlatformBody, len(ps))
	for i, p := range ps {
		out[i] = PlatformBody{OS: p.OS, Arch: p.Arch, Variant: p.Variant, Digest: p.Digest, Size: p.Size}
	}
	return out
}

// TagNameBody is one tag of a repository.
type TagNameBody struct {
	Name      string         `json:"name" doc:"The tag, as the registry names it"`
	Digest    string         `json:"digest,omitempty" doc:"The digest the tag points at, for a repository whose registry lists it with the names (Docker Hub). Left out otherwise: ask for the details of the tags on screen"`
	Created   string         `json:"created,omitempty" format:"date-time" doc:"When the tag was last pushed, with the same limit as the digest"`
	Platforms []PlatformBody `json:"platforms,omitempty" doc:"The platforms of the tag, with the same limit as the digest"`
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
	Capped     bool                             `json:"capped" doc:"True when the repository has more tags than the server reads; the list is then the first limit tags, by registry order or, on Docker Hub, the newest"`
	Limit      int                              `json:"limit" doc:"How many tags the server reads of one repository: 10,000, or 2,000 on Docker Hub"`
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

// TagDetailsMax is how many tags one details request may ask about (T-05-75).
const TagDetailsMax = 25

// TagDetailsRequest asks for the details of the tags on screen.
type TagDetailsRequest struct {
	Ref   string   `json:"ref" required:"true" maxLength:"512" doc:"The repository, like ghcr.io/acme/app or nginx. A tag or digest on it is ignored. 422 ref_invalid"`
	EnvID string   `json:"envId,omitempty" maxLength:"64" doc:"An environment whose own credentials are considered as well as the instance's, and whose nodes' architecture the created date prefers. The caller needs images.read on it (403 otherwise)"`
	Tags  []string `json:"tags" required:"true" minItems:"1" maxItems:"25" doc:"The tags to read, at most 25, as the list named them"`
}

// RunningInBody is an environment that runs a tag.
type RunningInBody struct {
	EnvID   string `json:"envId" doc:"The environment's ID"`
	EnvName string `json:"envName" doc:"The environment's name"`
}

// TagDetailBody is what the server learned about one tag.
type TagDetailBody struct {
	Tag       string          `json:"tag" doc:"The tag asked about"`
	Digest    string          `json:"digest" doc:"The digest the tag points at; the index digest for a multi-platform tag. Empty when the registry could not be asked"`
	Created   string          `json:"created,omitempty" format:"date-time" doc:"When the image was built, from its config (Docker Hub: when the tag was pushed). Left out when it could not be read"`
	Platforms []PlatformBody  `json:"platforms" doc:"The runnable platforms of the tag, without attestation and unknown/unknown entries; one for a single-platform image, none when the registry could not be read"`
	RunningIn []RunningInBody `json:"runningIn" doc:"The environments the caller can read whose running containers run this tag's digest, matched by digest and not by tag text, from each environment's last snapshot; environments that are offline are skipped. Only filled on the Registries page: left empty when envId is given"`
	Code      string          `json:"code" enum:",date_unavailable,tag_not_found,needs_login,login_refused,registry_unreachable,registry_tls,rate_limited" doc:"Empty when everything was read; date_unavailable when the date could not be; tag_not_found when the registry no longer has the tag; otherwise why the registry could not be read"`
}

// TagDetailsBody answers a details request, in the order of the tags asked.
type TagDetailsBody struct {
	Items []TagDetailBody `json:"items" doc:"One entry per tag asked about, in the same order"`
}

// TagDetailsInput is the request of the tag details route.
type TagDetailsInput struct {
	Body TagDetailsRequest
	// UserAgent is read only to record a refusal in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// TagDetailsOutput wraps TagDetailsBody for huma.
type TagDetailsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TagDetailsBody
}

// RegisterTags registers the tag browsing routes. It is always registered; without a
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
		_, ref, cred, err := d.open(ctx, p, in.EnvID, in.Ref, in.UserAgent, tagsTarget)
		if err != nil {
			return nil, err
		}
		page, err := d.Tags.List(ctx, cred, ref, in.Q, in.Hide, in.Page)
		if err != nil {
			return nil, d.tagProblem(ctx, ref, cred, err)
		}
		d.noteSignIn(ctx, cred, "")
		body := TagListBody{
			Repo: page.Repo, Host: page.Host, Tags: make([]TagNameBody, len(page.Tags)),
			Hidden: page.Hidden, Total: page.Total, More: page.More, Page: max(in.Page, 0), Capped: page.Capped, Limit: page.Limit,
		}
		for i, t := range page.Tags {
			body.Tags[i] = TagNameBody{Name: t.Name, Digest: t.Digest, Created: formatTagTime(t.Created), Platforms: platformBodies(t.Platforms)}
		}
		if cred != nil {
			body.Credential = Some(ResolvedCredentialBody{
				ID: cred.ID, Name: cred.Name, Scope: candidateScope(cred.Candidate), Host: cred.Candidate.Host, Prefix: cred.Prefix,
			})
		}
		return &TagListOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "browseTagDetails",
		Method:      http.MethodPost,
		Path:        "/api/registries/tags/details",
		Summary:     "Read the digest, date and platforms of tags",
		Description: "Reads the digest, the created date and the platforms of up to 25 tags of a repository, for the rows the person can see. The server asks the registry with the same credential and the same authorisation as the tag list: " +
			"images.read on at least one environment, and on envId when it is given (403, audited), an unparsable reference is 422 ref_invalid, and 26 tags are 422. Docker Hub is read through its tags API, which costs no pulls; any other registry through a manifest HEAD and, per digest, the manifest and the config of one platform " +
			"(linux/amd64, else the environment's node architecture, else the first). Results are cached per credential, repository and digest. A tag that could not be read carries a code, never a failure of the request. Not audited.",
		Tags: []string{"registries"},
	}, rbac.SelfService, func(ctx context.Context, in *TagDetailsInput) (*TagDetailsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		req := in.Body
		if len(req.Tags) == 0 || len(req.Tags) > TagDetailsMax {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, "", "Ask about between 1 and 25 tags.")
		}
		viewer, ref, cred, err := d.open(ctx, p, req.EnvID, req.Ref, in.UserAgent, tagDetailsTarget)
		if err != nil {
			return nil, err
		}
		for _, tag := range req.Tags {
			if tag == "" || len(tag) > 128 {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, "", "A tag is between 1 and 128 characters.")
			}
		}
		details := d.Tags.Details(ctx, cred, ref, req.Tags, d.nodeArch(ctx, req.EnvID))
		var running []envRunning
		if req.EnvID == "" {
			running = d.runningEnvironments(ctx, viewer)
		}
		body := TagDetailsBody{Items: make([]TagDetailBody, len(details))}
		for i, t := range details {
			body.Items[i] = TagDetailBody{
				Tag: t.Tag, Digest: t.Digest, Created: formatTagTime(t.Created), Platforms: platformBodies(t.Platforms), Code: t.Code,
				RunningIn: runningIn(running, ref.Repo(), t),
			}
			if t.Code == registry.CodeLoginRefused {
				d.noteSignIn(ctx, cred, dockerx.PullCodeUnauthorized)
			}
		}
		return &TagDetailsOutput{CacheControl: "no-store", Body: body}, nil
	})
}

// nodeArch is the CPU architecture the nodes of an environment run, by its OCI name, from the
// environment's last snapshot; empty when there is no environment, it cannot be read, or no
// node reported one. It only chooses which image a created date is read from.
func (d TagsDeps) nodeArch(ctx context.Context, envID string) string {
	if envID == "" || d.Aggregator == nil {
		return ""
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return ""
	}
	for _, n := range agg.Nodes {
		if n.Arch != "" {
			return n.Arch
		}
	}
	return ""
}

// The routes a refusal is audited under.
const (
	tagsTarget       = "GET /api/registries/tags"
	tagDetailsTarget = "POST /api/registries/tags/details"
)

// formatTagTime writes a time as the API does, or "" for none.
func formatTagTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// open authorises a caller to browse and chooses the credential the registry is asked with.
// It is the first thing both tag routes do: nothing is read, and no cache is consulted, for a
// caller who may not browse (T-05-60, D-24a). A refusal is audited under target.
func (d TagsDeps) open(ctx context.Context, p *rbac.Principal, envID, refText, userAgent, target string) (credentialViewer, registry.Ref, *registry.ResolvedCredential, error) {
	if d.Tags == nil || d.Resolver == nil || d.Checker == nil {
		return credentialViewer{}, registry.Ref{}, nil, httpx.Problem(http.StatusServiceUnavailable, problemRegistriesUnavailable, "Registry browsing is not available on this server.")
	}
	viewer, err := viewerOf(ctx, d.Checker, p)
	if err != nil {
		d.Log.Error("tag browsing: effective permissions", "error", err)
		return credentialViewer{}, registry.Ref{}, nil, huma.Error500InternalServerError("internal error")
	}
	if !viewer.anyImages || (envID != "" && !viewer.holds(envID, rbac.PermImagesRead)) {
		return credentialViewer{}, registry.Ref{}, nil, d.refuse(ctx, p, envID, userAgent, target)
	}
	ref, err := registry.ParseRef(refText)
	if err != nil {
		return credentialViewer{}, registry.Ref{}, nil, httpx.Problem(http.StatusUnprocessableEntity, problemRefInvalid, "Enter an image like nginx:1.27 or ghcr.io/acme/app:latest.")
	}
	visible := func(c registry.Candidate) bool { return viewer.sees(registry.Credential{EnvID: c.EnvID}) }
	cred, err := d.Resolver.ForBrowse(ctx, envID, visible, ref)
	if err != nil {
		d.Log.Error("tag browsing: choose a credential", "error", err)
		return credentialViewer{}, registry.Ref{}, nil, huma.Error500InternalServerError("internal error")
	}
	return viewer, ref, cred, nil
}

// refuse audits a browse the caller has no right to and answers 403.
func (d TagsDeps) refuse(ctx context.Context, p *rbac.Principal, envID, userAgent, target string) error {
	if d.Audit != nil {
		if err := d.Audit.Record(ctx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
			Reason: rbac.ReasonForbidden, TargetType: "route", TargetID: target, EnvID: envID,
			IP: httpx.ClientIP(ctx), UserAgent: userAgent,
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

// envRunning is an environment the caller can read and the digests its containers run.
type envRunning struct {
	id, name string
	running  map[string][]RunningRef
}

// runningEnvironments reads, for each environment the viewer can read images in, which digests
// its containers run, from the environment's cached snapshot. An environment that cannot be
// read (offline, disabled, a node timed out) is left out, and so is every environment the
// viewer holds nothing on (T-05-64). They come in name order.
func (d TagsDeps) runningEnvironments(ctx context.Context, viewer credentialViewer) []envRunning {
	if d.Envs == nil || d.Aggregator == nil {
		return nil
	}
	all, err := d.Envs.List(ctx)
	if err != nil {
		d.Log.Error("tag browsing: list the environments", "error", err)
		return nil
	}
	var readable []envs.Environment
	for _, e := range all {
		if e.Enabled && viewer.holds(e.ID, rbac.PermImagesRead) {
			readable = append(readable, e)
		}
	}
	out := make([]envRunning, len(readable))
	var g errgroup.Group
	g.SetLimit(4)
	for i, e := range readable {
		g.Go(func() error {
			agg, _, err := d.Aggregator.Snapshot(ctx, e.ID)
			if err != nil {
				return nil //nolint:nilerr // an environment that cannot be read is skipped
			}
			out[i] = envRunning{id: e.ID, name: e.Name, running: runningDigests(agg)}
			return nil
		})
	}
	_ = g.Wait()
	out = slices.DeleteFunc(out, func(e envRunning) bool { return e.id == "" })
	slices.SortFunc(out, func(a, b envRunning) int { return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.id, b.id)) })
	return out
}

// runningIn lists the environments that run the tag whose details are t.
func runningIn(running []envRunning, repo string, t registry.TagDetail) []RunningInBody {
	out := []RunningInBody{}
	platformDigests := make([]string, len(t.Platforms))
	for i, p := range t.Platforms {
		platformDigests[i] = p.Digest
	}
	for _, e := range running {
		if isRunning(e.running, repo, t.Digest, platformDigests) {
			out = append(out, RunningInBody{EnvID: e.id, EnvName: e.name})
		}
	}
	return out
}
