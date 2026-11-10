// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The sources a served certificate can come from. SourceLetsEncrypt is produced
// once TierPlan obtains a certificate itself; it is declared now so the enum the
// contract publishes does not change.
const (
	SourceSelfSigned  = "self_signed"
	SourceCertFiles   = "cert_files"
	SourceLetsEncrypt = "lets_encrypt"
)

// problemHTTPSUnavailable answers a status request when the listener has no
// readable certificate to describe.
const problemHTTPSUnavailable = "https_unavailable"

// ServedCertificate is the certificate the live listener serves: where it came
// from and its parsed public leaf. A nil Leaf means none could be read.
type ServedCertificate struct {
	Source string
	Leaf   *x509.Certificate
}

// HTTPSDeps are what the HTTPS routes need. Served reads the listener's live
// certificate on every call, so the answer is what a browser receives now.
type HTTPSDeps struct {
	Served func() ServedCertificate
	// CertFiles is true when TIERPLAN_TLS_CERT_FILE and its key fix the certificate.
	CertFiles bool
	Log       *slog.Logger
}

// HTTPSCertificateBody describes the certificate in use. It carries public
// certificate fields only.
type HTTPSCertificateBody struct {
	Source    string    `json:"source" enum:"self_signed,cert_files,lets_encrypt" doc:"Where the certificate comes from: the one TierPlan generated, the files you supplied, or Let's Encrypt"`
	Issuer    string    `json:"issuer" doc:"Who issued it: the issuer's common name, else its organisation, else empty"`
	Names     []string  `json:"names" doc:"Every name the certificate covers, as it carries them: DNS names first, then IP addresses"`
	NotBefore time.Time `json:"notBefore" doc:"When it becomes valid"`
	NotAfter  time.Time `json:"notAfter" doc:"When it expires"`
}

// HTTPSStatusBody is the HTTPS status.
type HTTPSStatusBody struct {
	Certificate HTTPSCertificateBody `json:"certificate" doc:"The certificate the listener serves now"`
	CertFiles   bool                 `json:"certFiles" doc:"Whether certificate files from the environment fix the certificate"`
}

// HTTPSStatusOutput wraps HTTPSStatusBody for huma.
type HTTPSStatusOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         HTTPSStatusBody
}

// certificateBody builds the public description of a served certificate.
func certificateBody(s ServedCertificate) HTTPSCertificateBody {
	leaf := s.Leaf
	names := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	names = append(names, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	issuer := leaf.Issuer.CommonName
	if issuer == "" && len(leaf.Issuer.Organization) > 0 {
		issuer = leaf.Issuer.Organization[0]
	}
	return HTTPSCertificateBody{
		Source: s.Source, Issuer: issuer, Names: names,
		NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(),
	}
}

// RegisterHTTPS wires the HTTPS routes.
func RegisterHTTPS(r *httpx.Router, d HTTPSDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getHTTPSStatus",
		Method:      http.MethodGet,
		Path:        "/api/https/status",
		Summary:     "Get the certificate the server serves",
		Description: "The certificate the listener serves right now, read from the live listener and never from a stored copy: " +
			"its source (self_signed, cert_files or lets_encrypt), issuer, every name it covers and its validity. " +
			"Only public certificate fields are returned. 503 " + problemHTTPSUnavailable + " when no certificate can be read. Needs settings.read.",
		Tags: []string{"https"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}, func(context.Context, *struct{}) (*HTTPSStatusOutput, error) {
		var served ServedCertificate
		if d.Served != nil {
			served = d.Served()
		}
		if served.Leaf == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemHTTPSUnavailable, "The server has no readable certificate.")
		}
		return &HTTPSStatusOutput{
			CacheControl: "no-store",
			Body:         HTTPSStatusBody{Certificate: certificateBody(served), CertFiles: d.CertFiles},
		}, nil
	})
}
