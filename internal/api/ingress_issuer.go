// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The issuer change routes (Phase 20, DOM-05, D-25): changing the Let's Encrypt directory of a domain retires every ACME
// certificate under it that came from the other directory and orders new ones, so an Admin first asks what the change would
// do (a preview that counts the certificates and lists the uploads it leaves alone), then confirms with that count. The
// count is worked out again inside the write, so a certificate added or retired in between refuses the change.

const problemIngressIssuerCountMismatch = "ingress_issuer_count_mismatch"

// PreviewIngressDomainIssuerInput is the path and query of the preview.
type PreviewIngressDomainIssuerInput struct {
	EnvironmentPathInput
	DomainID string `path:"domainId" doc:"The domain's ID"`
	Issuer   string `query:"issuer" required:"true" enum:"staging,production" doc:"The directory the domain would change to"`
}

// IngressIssuerPreviewBody is what an issuer change would do.
type IngressIssuerPreviewBody struct {
	Issuer       string   `json:"issuer" enum:"staging,production" doc:"The directory the domain would change to"`
	Certificates int      `json:"certificates" doc:"How many live Let's Encrypt certificates under the domain come from the other directory: the change retires each and orders a new one from the new directory"`
	Uploads      []string `json:"uploads" doc:"The hostnames of the uploaded certificates under the domain, sorted. The change leaves them alone"`
	ConfirmCount int      `json:"confirmCount" doc:"The count to send back as confirmCount to make the change; it is checked again when the change is made"`
}

// IngressIssuerPreviewOutput is the preview, never cached.
type IngressIssuerPreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressIssuerPreviewBody
}

// ChangeIngressDomainIssuerInput is the path and body of the change.
type ChangeIngressDomainIssuerInput struct {
	EnvironmentPathInput
	DomainID string `path:"domainId" doc:"The domain's ID"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Issuer       string `json:"issuer" enum:"staging,production" doc:"The directory to change to"`
		ConfirmCount int    `json:"confirmCount" minimum:"0" doc:"The number of certificates the preview showed. 409 ingress_issuer_count_mismatch, with the number now in errors[0].value, when it is not the number now"`
	}
}

// registerIngressIssuer registers the preview and the change of a domain's issuer. Both need ingress.manage on the
// environment, and a domain id of another environment is a 404 exactly like one that does not exist.
func registerIngressIssuer(r *httpx.Router, d IngressCertDeps) {
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Per-hostname certificates are not available on this server.")
	}
	ready := func() bool { return d.Manager != nil && d.Domains != nil }
	notFound := func() error {
		return httpx.Problem(http.StatusNotFound, problemIngressDomainNotFound, "No such domain in this environment.")
	}
	internal := func(what string, err error) error {
		d.Log.Error("ingress issuer request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "previewIngressDomainIssuer",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/domains/{domainId}/issuer",
		Summary:     "Preview an issuer change",
		Description: "Says what changing the domain's Let's Encrypt directory to the one in the query would do, and writes nothing: how many live certificates under the domain come from the " +
			"other directory (each is retired and ordered again from the new one) and which uploaded certificates it leaves alone. confirmCount is the number to send back to make the change. " +
			"404 " + problemIngressDomainNotFound + " for a domain that is not this environment's. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *PreviewIngressDomainIssuerInput) (*IngressIssuerPreviewOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		p, err := d.Domains.PreviewIssuerChange(ctx, in.EnvID, in.DomainID, ingress.Issuer(in.Issuer))
		switch {
		case err == nil:
		case errors.Is(err, ingressstore.ErrDomainNotFound):
			return nil, notFound()
		default:
			return nil, internal("preview the issuer change", err)
		}
		out := &IngressIssuerPreviewOutput{CacheControl: "no-store"}
		out.Body = IngressIssuerPreviewBody{Issuer: string(p.Target), Certificates: p.Certificates, Uploads: nonNil(p.Uploads), ConfirmCount: p.Certificates}
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "changeIngressDomainIssuer",
		Method:      http.MethodPut,
		Path:        "/api/env/{envId}/ingress/domains/{domainId}/issuer",
		Summary:     "Change the issuer of a domain",
		Description: "Changes the domain's Let's Encrypt directory, only when confirmCount is the number of certificates the change affects now: the preview's count, checked again inside the write. " +
			"409 " + problemIngressIssuerCountMismatch + " with the number now in errors[0].value when a certificate was added or retired since, and nothing changes. The certificates under the domain from " +
			"the other directory are retired and ordered again by the certificate manager; uploads are left alone. Audited as ingress.domain_issuer_changed. Asking for the issuer the domain already has " +
			"changes nothing and writes no audit row. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *ChangeIngressDomainIssuerInput) (*IngressDomainOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		dom, changed, err := d.Domains.SetDomainIssuer(ctx, in.EnvID, in.DomainID, ingress.Issuer(in.Body.Issuer), in.Body.ConfirmCount, p.ID,
			func(ctx context.Context, tx *sql.Tx, dom ingress.Domain, pv ingressstore.IssuerChangePreview, previous ingress.Issuer) error {
				return d.Audit.RecordTx(ctx, tx, audit.Entry{
					Actor: p.AuditActor(), Action: audit.ActionIngressDomainIssuerChanged, TargetType: ingressDomainTarget, TargetID: dom.ID, EnvID: in.EnvID,
					Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
					Details: map[string]string{
						"suffix": dom.Suffix, "issuer": string(dom.Issuer), "previous": string(previous),
						"certificates": strconv.Itoa(pv.Certificates), "uploads": strconv.Itoa(len(pv.Uploads)),
					},
				})
			})
		var mismatch *ingressstore.IssuerCountMismatchError
		switch {
		case err == nil:
		case errors.Is(err, ingressstore.ErrDomainNotFound):
			return nil, notFound()
		case errors.As(err, &mismatch):
			return nil, &huma.ErrorModel{
				Type: problemIngressIssuerCountMismatch, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
				Detail: "The number of certificates this change affects is now " + strconv.Itoa(mismatch.Current) + ", not " + strconv.Itoa(in.Body.ConfirmCount) +
					". Review the preview again and confirm the new number.",
				Errors: []*huma.ErrorDetail{{Location: "body.confirmCount", Message: "the count changed", Value: mismatch.Current}},
			}
		default:
			return nil, internal("change the issuer", err)
		}
		if changed {
			d.Manager.Wake()
		}
		reserved, certificates := []string(nil), 0
		views, err := d.Domains.Domains(ctx, in.EnvID)
		if err != nil {
			// The change is made: the answer carries the domain without the counts rather than a 500.
			d.Log.Warn("ingress issuer change: the domain's counts could not be read", "error", err)
		}
		if i := slices.IndexFunc(views, func(v ingressstore.DomainView) bool { return v.ID == dom.ID }); i >= 0 {
			reserved, certificates = views[i].Reserved, views[i].Certificates
		}
		return &IngressDomainOutput{CacheControl: "no-store", Body: ingressDomainBody(dom, reserved, certificates)}, nil
	})
}
