// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The upload route (Phase 20, CRT-08, D-22, D-23): an Admin hands TierPlan a certificate chain and its private key for a
// hostname, and a later call returns the hostname to Let's Encrypt.
//
// The private key field carries no maxLength in the schema on purpose, exactly like the DNS token of the domain routes:
// a schema violation is answered with the offending value, which would echo the key. The operation bounds the whole body
// with MaxBodyBytes instead, and the handler bounds each field at ingresscert.MaxUploadBytes before anything is parsed.
// No answer, problem, audit row or log line of these routes holds the key: a refusal is a fixed sentence per code.

const (
	// maxIngressCertUploadBody bounds an upload's body: a chain and a key, each at most MaxUploadBytes, and the hostname
	// and the JSON around them.
	maxIngressCertUploadBody = 2*ingresscert.MaxUploadBytes + 4096
)

// UploadIngressCertInput is the path and body of an upload.
type UploadIngressCertInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Hostname       string `json:"hostname" maxLength:"253" doc:"The hostname the certificate is for, under a domain registered for this environment and not held by another environment. It is lower-cased and a trailing dot is dropped"`
		CertificatePEM string `json:"certificatePem" doc:"The certificate chain as PEM certificates, the server certificate first, at most 256 KiB. 422 ingress_cert_chain_unparsable when it is not, ingress_cert_host_not_covered, ingress_cert_expired or ingress_cert_not_yet_valid when the server certificate does not fit the hostname or the time"`
		PrivateKeyPEM  string `json:"privateKeyPem" doc:"The unencrypted private key as PEM (RSA 2048 to 8192 bits, or ECDSA on P-256, P-384 or P-521), at most 256 KiB. It is stored sealed and never returned"`
	}
}

// IngressCertUploadedBody is the certificate an upload made, with the warnings the upload carries.
type IngressCertUploadedBody struct {
	IngressCertBody
	Warnings []string `json:"warnings" doc:"What to know about the accepted certificate, such as chain_untrusted when its chain does not verify against the system's roots (an internal CA is a valid reason to upload)"`
}

// IngressCertUploadedOutput is the certificate an upload made, never cached.
type IngressCertUploadedOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressCertUploadedBody
}

// SwitchIngressCertInput is the path of a switch back to ACME.
type SwitchIngressCertInput struct {
	EnvironmentPathInput
	CertID string `path:"certId" doc:"The uploaded certificate's ID"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// uploadProblem is the problem of a refused upload: its code and a fixed sentence, never a byte of the input.
func uploadProblem(e *ingresscert.UploadError) error {
	return httpx.Problem(http.StatusUnprocessableEntity, e.Code, e.Error())
}

// registerIngressCertUpload registers the upload and the switch back to ACME. Both need ingress.manage on the environment.
func registerIngressCertUpload(r *httpx.Router, d IngressCertDeps) {
	// RegisterIngressCerts has filled in Log and Now.
	now := d.Now
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Per-hostname certificates are not available on this server.")
	}
	ready := func() bool { return d.Manager != nil && d.Store != nil && d.Domains != nil }
	internal := func(what string, err error) error {
		d.Log.Error("ingress certificate upload failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID:   "uploadIngressCertificate",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/ingress/certificates/upload",
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  maxIngressCertUploadBody,
		Summary:       "Upload a certificate and its private key for a hostname",
		Description: "Makes an uploaded certificate the hostname's certificate. The hostname follows the rules of a request ahead (422 " + ingress.CodeDomainNotAllowed + ", " + ingress.CodeHostInvalid + ", 409 " + ingress.CodeHostReserved + " " +
			"and " + ingress.CodeHostTaken + ", which names nothing of the other environment). The chain and key are refused with 422 and a stable code and a fixed sentence: " +
			ingresscert.UploadKeyMismatch + ", " + ingresscert.UploadHostNotCovered + ", " + ingresscert.UploadExpired + ", " + ingresscert.UploadNotYetValid + ", " + ingresscert.UploadChainUnparsable + ", " +
			ingresscert.UploadKeyUnsupported + ", " + ingresscert.UploadKeyEncrypted + " and " + ingresscert.UploadTooLarge + ". A chain that does not verify against the system's roots is accepted with " +
			"the warning " + ingresscert.WarningChainUntrusted + ". A live certificate of the hostname, whatever its source, is replaced. The key is stored sealed and is never returned, audited or logged. " +
			"Audited as ingress.cert_uploaded. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *UploadIngressCertInput) (*IngressCertUploadedOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		// Each field is bounded before anything is parsed or read from the store.
		if len(in.Body.CertificatePEM) > ingresscert.MaxUploadBytes || len(in.Body.PrivateKeyPEM) > ingresscert.MaxUploadBytes {
			return nil, uploadProblem(&ingresscert.UploadError{Code: ingresscert.UploadTooLarge})
		}
		var canView func(string) bool
		if d.Checker != nil {
			acc, err := d.Checker.Access(ctx, p)
			if err != nil {
				return nil, internal("read the caller's access", err)
			}
			canView = func(envID string) bool { return acc.For(envID).Role != rbac.RoleNone }
		}
		world, err := d.Domains.World(ctx, in.EnvID, canView)
		if err != nil {
			return nil, internal("read the ingress world", err)
		}
		c, warnings, err := d.Store.Upload(ctx, ingresscert.UploadInput{
			EnvID: in.EnvID, Hostname: in.Body.Hostname, Actor: p.ID,
			ChainPEM: []byte(in.Body.CertificatePEM), KeyPEM: []byte(in.Body.PrivateKeyPEM), World: world,
		}, nil, func(ctx context.Context, tx *sql.Tx, c ingresscert.Cert, replaced string, warnings []string) error {
			warning := ""
			if len(warnings) > 0 {
				warning = warnings[0]
			}
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionIngressCertUploaded, TargetType: ingressCertTarget, TargetID: c.ID, EnvID: in.EnvID,
				Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
				Details: map[string]string{"hostname": c.Hostname, "replaced": replaced, "warning": warning},
			})
		})
		var (
			refusedUpload  *ingresscert.UploadError
			refusedRequest *ingresscert.RequestRefusedError
		)
		switch {
		case err == nil:
		case errors.As(err, &refusedUpload):
			return nil, uploadProblem(refusedUpload)
		case errors.As(err, &refusedRequest):
			return nil, ingressCertRefusal(refusedRequest)
		default:
			// The error carries no input byte: store errors name the hostname and the operation only.
			return nil, internal("store the uploaded certificate", err)
		}
		d.Manager.Wake()
		out := &IngressCertUploadedOutput{CacheControl: "no-store"}
		out.Body.IngressCertBody = ingressCertBody(c, now())
		out.Body.Warnings = nonNil(warnings)
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "switchIngressCertificateToACME",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/certificates/{certId}/acme",
		Summary:     "Return an uploaded hostname to Let's Encrypt",
		Description: "Retires the uploaded certificate, which keeps serving until its replacement is ready or it expires, and queues a Let's Encrypt certificate for the hostname from the " +
			"issuer its domain has now. Answers 200 with the new queued certificate. 404 " + problemIngressCertNotFound + " for a certificate that is not a live upload of this environment " +
			"(an ACME certificate, a retired one, another environment's or an unknown id). 409 " + problemIngressDomainNotFound + " when the domain the upload was under is no longer registered. Audited as ingress.cert_switched_to_acme. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *SwitchIngressCertInput) (*IngressCertOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		notFound := httpx.Problem(http.StatusNotFound, problemIngressCertNotFound, "No such uploaded certificate in this environment.")
		cur, err := d.Store.Cert(ctx, in.CertID)
		switch {
		case errors.Is(err, ingresscert.ErrCertNotFound):
			return nil, notFound
		case err != nil:
			return nil, internal("read the certificate", err)
		case cur.EnvID != in.EnvID, cur.Source != ingresscert.SourceUpload, !cur.RetiredAt.IsZero():
			return nil, notFound
		}
		// D-28 leaves a deleted domain's certificates in place, so the domain may be gone.
		dom, err := d.Domains.DomainByID(ctx, cur.DomainID)
		switch {
		case errors.Is(err, ingressstore.ErrDomainNotFound):
			return nil, httpx.Problem(http.StatusConflict, problemIngressDomainNotFound,
				"The domain this certificate was under is no longer registered. Register it again before returning the hostname to Let's Encrypt.")
		case err != nil:
			return nil, internal("read the domain of the certificate", err)
		}
		c, err := d.Store.SwitchToACME(ctx, in.EnvID, in.CertID, p.ID, string(dom.Issuer), func(ctx context.Context, tx *sql.Tx, c ingresscert.Cert) error {
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionIngressCertSwitchedToACME, TargetType: ingressCertTarget, TargetID: c.ID, EnvID: in.EnvID,
				Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
				Details: map[string]string{"hostname": c.Hostname, "issuer": c.Issuer},
			})
		})
		switch {
		case err == nil:
		case errors.Is(err, ingresscert.ErrCertNotFound):
			return nil, notFound
		default:
			return nil, internal("switch the certificate to ACME", err)
		}
		d.Manager.Wake()
		return &IngressCertOutput{CacheControl: "no-store", Body: ingressCertBody(c, now())}, nil
	})
}
