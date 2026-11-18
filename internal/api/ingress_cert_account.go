// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The install-wide switches of the per-hostname certificates (Phase 20, D-32, D-36, D-37): one acknowledgement of the
// certificate authority's terms, after which TierPlan may register the ingress ACME accounts it needs, and the resume
// that lifts the pause a restore puts on issuing. They are instance settings, not an environment's, so each needs
// settings.write with global scope (the GET settings.read), exactly like their HTTPS counterparts: an environment
// Admin, who holds ingress.manage, is refused. They are audited as https.* for the same reason.

const (
	problemIngressTermsRequired  = "ingress_terms_required"
	problemIngressCertsNotPaused = "ingress_certs_not_paused"
)

// IngressCertAccountDirectoryBody is one directory the certificates are ordered from.
type IngressCertAccountDirectoryBody struct {
	Issuer         string `json:"issuer" enum:"production,staging" doc:"The Let's Encrypt directory this is"`
	URL            string `json:"url" doc:"The directory's address"`
	TermsOfService string `json:"termsOfService" doc:"The URL of the terms the directory advertises; empty when it names none or could not be read just now"`
}

// IngressCertAccountBody is the install-wide state of the per-hostname certificates.
type IngressCertAccountBody struct {
	TermsAccepted   bool                              `json:"termsAccepted" doc:"True once an Admin acknowledged the terms of the directories below. No certificate is ordered before that"`
	TermsAcceptedAt *time.Time                        `json:"termsAcceptedAt,omitempty" doc:"When the terms were acknowledged, left out before"`
	TermsAcceptedBy string                            `json:"termsAcceptedBy,omitempty" doc:"The ID of the user who acknowledged them, left out before"`
	Directories     []IngressCertAccountDirectoryBody `json:"directories" doc:"The directories certificates are ordered from, production then staging, with the terms each advertises"`
	Paused          bool                              `json:"paused" doc:"True while issuing is paused, as it is after a restore until an Admin resumes it"`
	PausedReason    string                            `json:"pausedReason" doc:"Why issuing is paused, such as restore; empty when it is not"`
}

// IngressCertAccountOutput is the install-wide state, never cached.
type IngressCertAccountOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressCertAccountBody
}

// IngressCertAccountReadInput is the (empty) input of the GET.
type IngressCertAccountReadInput struct{}

// AcceptIngressCertTermsInput is the body of the acknowledgement.
type AcceptIngressCertTermsInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		AgreeTos bool `json:"agreeTos" doc:"Must be true: the Admin agrees to the terms of service of the directories, on behalf of this install"`
	}
}

// ResumeIngressCertsInput is the (empty) input of the resume.
type ResumeIngressCertsInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// registerIngressCertAccount registers the account read, the acknowledgement of the terms and the resume. The routes are
// always registered; without a manager they answer 503 ingress_unavailable.
func registerIngressCertAccount(r *httpx.Router, d IngressCertDeps) {
	read := rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}
	write := rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Per-hostname certificates are not available on this server.")
	}
	ready := func() bool { return d.Manager != nil && d.Store != nil }
	internal := func(what string, err error) error {
		d.Log.Error("ingress certificate account request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	audited := func(ctx context.Context, p *rbac.Principal, userAgent, action string, details map[string]string) audit.Entry {
		return audit.Entry{
			Actor: p.AuditActor(), Action: action, TargetType: httpsAuditTarget, TargetID: httpsAuditTarget,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
		}
	}
	// accept serializes the check and the write of the acknowledgement, so two Admins accepting at once leave one
	// audit row and the first one's name and time.
	var accept sync.Mutex

	httpx.Register(r, huma.Operation{
		OperationID: "getIngressCertificateAccount",
		Method:      http.MethodGet,
		Path:        "/api/ingress/certificates/account",
		Summary:     "Read the install-wide state of the per-hostname certificates",
		Description: "Whether an Admin has acknowledged the certificate authority's terms (and when and by whom), the directories certificates are ordered from with the terms each one advertises " +
			"(empty when a directory cannot be read just now), and whether issuing is paused, with the reason. No certificate is ordered before the terms are acknowledged. Needs settings.read.",
		Tags: []string{"ingress"},
	}, read, func(ctx context.Context, _ *IngressCertAccountReadInput) (*IngressCertAccountOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		return ingressCertAccountOutput(ctx, d, internal)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "acceptIngressCertificateTerms",
		Method:      http.MethodPut,
		Path:        "/api/ingress/certificates/account",
		Summary:     "Acknowledge the certificate authority's terms for per-hostname certificates",
		Description: "Records, once for the whole install, that the Admin agrees to the terms of service of the directories listed by the GET, so TierPlan may register the ACME accounts it needs to order " +
			"certificates for ingress hostnames. Certificates queued for want of an account are ordered on the next round. 422 " + problemIngressTermsRequired + " when agreeTos is not true, " +
			"and nothing is recorded. Repeating it once accepted answers 200 and records nothing more. Audited as https.ingress_terms_accepted with the directories it covers. Needs settings.write.",
		Tags: []string{"ingress"},
	}, write, func(ctx context.Context, in *AcceptIngressCertTermsInput) (*IngressCertAccountOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		if !in.Body.AgreeTos {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemIngressTermsRequired,
				"Agree to the certificate authority's terms of service to get certificates for hostnames.")
		}
		accept.Lock()
		defer accept.Unlock()
		state, err := d.Store.State(ctx)
		if err != nil {
			return nil, internal("read the certificate state", err)
		}
		if state.TermsAcceptedAt.IsZero() {
			dirs := d.Manager.Directories(ctx)
			urls := make([]string, 0, len(dirs))
			for _, dir := range dirs {
				urls = append(urls, dir.URL)
			}
			details := map[string]string{"directories": strings.Join(urls, ",")}
			err := d.Store.AcceptTerms(ctx, p.ID, func(ctx context.Context, tx *sql.Tx) error {
				return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionHTTPSIngressTermsAccepted, details))
			})
			if err != nil {
				return nil, internal("record the acceptance of the terms", err)
			}
			d.Manager.Wake()
		}
		return ingressCertAccountOutput(ctx, d, internal)
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "resumeIngressCertificates",
		Method:        http.MethodPost,
		Path:          "/api/ingress/certificates/resume",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Resume per-hostname certificates after a restore",
		Description: "Lifts the pause a restore puts on ordering and renewing the per-hostname certificates, once the Admin has seen what was restored. Nothing is ordered by the call itself; the next round " +
			"does that, and it is woken now. 204. 409 " + problemIngressCertsNotPaused + " when nothing is paused. Audited as https.ingress_resumed with the reason that was lifted. Needs settings.write.",
		Tags: []string{"ingress"},
	}, write, func(ctx context.Context, in *ResumeIngressCertsInput) (*IngressNoContentOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		err := d.Manager.Resume(ctx, func(ctx context.Context, tx *sql.Tx, reason string) error {
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionHTTPSIngressResumed, map[string]string{"reason": reason}))
		})
		switch {
		case err == nil:
			return &IngressNoContentOutput{CacheControl: "no-store"}, nil
		case errors.Is(err, ingresscert.ErrNotPaused):
			return nil, httpx.Problem(http.StatusConflict, problemIngressCertsNotPaused, "Per-hostname certificates are not paused.")
		}
		return nil, internal("resume the certificates", err)
	})
}

// ingressCertAccountOutput reads the install-wide state and the directories and maps them onto the contract.
func ingressCertAccountOutput(ctx context.Context, d IngressCertDeps, internal func(string, error) error) (*IngressCertAccountOutput, error) {
	state, err := d.Store.State(ctx)
	if err != nil {
		return nil, internal("read the certificate state", err)
	}
	out := &IngressCertAccountOutput{CacheControl: "no-store"}
	out.Body = IngressCertAccountBody{
		TermsAccepted: !state.TermsAcceptedAt.IsZero(), TermsAcceptedAt: timePtr(state.TermsAcceptedAt), TermsAcceptedBy: state.TermsAcceptedBy,
		Paused: state.PausedReason != "", PausedReason: state.PausedReason, Directories: []IngressCertAccountDirectoryBody{},
	}
	for _, dir := range d.Manager.Directories(ctx) {
		out.Body.Directories = append(out.Body.Directories, IngressCertAccountDirectoryBody{Issuer: dir.Issuer, URL: dir.URL, TermsOfService: dir.TermsOfService})
	}
	return out, nil
}
