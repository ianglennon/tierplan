// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The per-hostname certificate routes (Phase 20, CRT-03, CRT-05, CRT-06): an Admin lists the certificates of an
// environment, asks for one ahead of any claim, asks for another attempt now, and reads the weekly budget the
// queue waits on. Every route needs ingress.manage on the environment (its Admin), no route ever returns a private
// key or a sealed value, and every answer is no-store.

const (
	problemIngressCertNotFound = "ingress_cert_not_found"
	problemIngressCertsPaused  = "ingress_certs_paused"
	problemIngressCertBlocked  = "ingress_cert_blocked"
	problemIngressCertStopped  = "ingress_cert_stopped"

	// ingressCertTarget is the target type of every audit row of these routes; the target id is the certificate's id.
	ingressCertTarget = "ingress_certificate"
)

// IngressCertDeps are the dependencies of the per-hostname certificate routes.
type IngressCertDeps struct {
	// Manager asks for and retries certificates. Nil answers 503 ingress_unavailable on every route, so the routes are
	// always registered.
	Manager *ingresscert.Manager
	// Store reads the certificates.
	Store *ingresscert.Store
	// Domains is the ingress domain store the manager reads the claims and domains through.
	Domains *ingressstore.Store
	Audit   *audit.Writer
	// Checker decides which environments the caller may see, so a refusal names no stack of one they cannot.
	Checker *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
	// Now is the clock the Retry-After of a blocked retry counts from. Nil means time.Now.
	Now func() time.Time
}

// IngressCertQueueBody says why a queued certificate waits.
type IngressCertQueueBody struct {
	Code       string     `json:"code" doc:"Why the certificate waits: no_account, rate_limit_new, rate_limit_duplicate, rate_limit_orders, ca_hold or concurrency"`
	Reason     string     `json:"reason" doc:"The reason in one sentence, built from the figures below; it never carries the certificate authority's own words"`
	Used       int        `json:"used" doc:"How many of the budget is used"`
	Cap        int        `json:"cap" doc:"Where TierPlan stops ordering, which is below the certificate authority's own limit"`
	Domain     string     `json:"domain,omitempty" doc:"The registered domain the budget is counted for"`
	NextSlotAt *time.Time `json:"nextSlotAt,omitempty" doc:"When a slot opens, left out when none is known"`
}

// IngressCertErrorBody is the last failed attempt of a certificate.
type IngressCertErrorBody struct {
	Class  string     `json:"class" doc:"The class of the failure, such as dns_token_invalid or rate_limited"`
	Hint   string     `json:"hint" doc:"One fixed sentence for a person about this class of failure"`
	Detail string     `json:"detail,omitempty" doc:"What the certificate authority or DNS provider answered, capped in length. Only Admins read it here"`
	At     *time.Time `json:"at,omitempty" doc:"When the attempt failed"`
}

// IngressCertBody is one certificate as an Admin sees it. It never carries a private key or a sealed value.
type IngressCertBody struct {
	ID                  string                `json:"id" doc:"The certificate's ID, used in the paths of the routes below"`
	Hostname            string                `json:"hostname" doc:"The hostname the certificate covers"`
	Issuer              string                `json:"issuer,omitempty" enum:"staging,production" doc:"The Let's Encrypt directory the certificate comes from; left out for an uploaded certificate"`
	Staging             bool                  `json:"staging" doc:"True for a staging certificate, which browsers do not trust and TierPlan never serves"`
	Source              string                `json:"source" enum:"acme,upload" doc:"Whether TierPlan ordered the certificate or an Admin uploaded it"`
	State               string                `json:"state" doc:"One word for where it stands: queued, ordering, valid, uploaded, dns_proven_on_staging, renewal_failed, failed, expired, stopped or retired"`
	StateText           string                `json:"stateText" doc:"The state in words for a person"`
	Ready               bool                  `json:"ready" doc:"True when the certificate may be served now"`
	StoppedReason       string                `json:"stoppedReason,omitempty" doc:"Why a stopped certificate does nothing"`
	IssuedAt            *time.Time            `json:"issuedAt,omitempty" doc:"When the certificate was issued"`
	ExpiresAt           *time.Time            `json:"expiresAt,omitempty" doc:"When the certificate expires"`
	RenewAt             *time.Time            `json:"renewAt,omitempty" doc:"When the next renewal is planned"`
	RenewBasis          string                `json:"renewBasis,omitempty" doc:"What the renewal time is based on, such as the certificate authority's renewal information"`
	LastError           *IngressCertErrorBody `json:"lastError,omitempty" doc:"The last failed attempt, left out when there is none"`
	ConsecutiveFailures int                   `json:"consecutiveFailures" doc:"How many attempts in a row have failed"`
	NextAttemptAt       *time.Time            `json:"nextAttemptAt,omitempty" doc:"When the next attempt is made"`
	RetryAfterUntil     *time.Time            `json:"retryAfterUntil,omitempty" doc:"Until when the certificate authority asked TierPlan to wait"`
	Queue               *IngressCertQueueBody `json:"queue,omitempty" doc:"Why the certificate waits, left out when it waits for nothing"`
	RequestedAt         *time.Time            `json:"requestedAt,omitempty" doc:"When an Admin last asked for the certificate ahead of a claim"`
	RequestedBy         string                `json:"requestedBy,omitempty" doc:"The ID of the user who asked for it"`
	UnreferencedSince   *time.Time            `json:"unreferencedSince,omitempty" doc:"Since when no stack and no request refers to the hostname"`
	RetiredAt           *time.Time            `json:"retiredAt,omitempty" doc:"When the certificate was retired, left out while it is live"`
	RetiredReason       string                `json:"retiredReason,omitempty" doc:"Why it was retired"`
	CreatedAt           time.Time             `json:"createdAt" doc:"When the certificate's row was made"`
}

// IngressCertsBody is the certificates of one environment.
type IngressCertsBody struct {
	Items []IngressCertBody `json:"items" doc:"The environment's certificates, live and retired, by hostname"`
}

// IngressCertBudgetBody is the weekly new-certificate budget of one registered domain on one issuer.
type IngressCertBudgetBody struct {
	RegisteredDomain string     `json:"registeredDomain" doc:"The registered domain the certificate authority counts"`
	Issuer           string     `json:"issuer" enum:"staging,production" doc:"The directory the budget is on"`
	Used             int        `json:"used" doc:"New certificates in the last 7 days plus orders in flight, TierPlan's own certificate included on production"`
	Cap              int        `json:"cap" doc:"Where TierPlan stops ordering new certificates"`
	Limit            int        `json:"limit" doc:"The certificate authority's own limit"`
	Headroom         int        `json:"headroom" doc:"How many more new certificates TierPlan will order now"`
	NextSlotAt       *time.Time `json:"nextSlotAt,omitempty" doc:"When a slot opens; left out while there is headroom or nothing in the window will expire"`
	Domains          []string   `json:"domains" doc:"This environment's domains under the registered domain, sorted"`
	Shared           bool       `json:"shared" doc:"True when another domain, in any environment, or TierPlan's own address is under the same registered domain, so the budget is not this environment's alone"`
}

// IngressCertBudgetsBody is the budget rows of one environment.
type IngressCertBudgetsBody struct {
	Items []IngressCertBudgetBody `json:"items" doc:"One row per registered domain and issuer in use, by domain"`
}

// IngressCertOutput is one certificate, never cached.
type IngressCertOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressCertBody
}

// IngressCertsOutput is the list of certificates, never cached.
type IngressCertsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressCertsBody
}

// IngressCertBudgetOutput is the budget, never cached.
type IngressCertBudgetOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressCertBudgetsBody
}

// ListIngressCertsInput is the path of the list route.
type ListIngressCertsInput struct {
	EnvironmentPathInput
}

// RequestIngressCertInput is the path and body of a request ahead.
type RequestIngressCertInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Hostname string `json:"hostname" maxLength:"253" doc:"The hostname to get a certificate for, under a domain registered for this environment and not held by a stack of another environment. It is lower-cased and a trailing dot is dropped"`
	}
}

// RetryIngressCertInput is the path of a retry.
type RetryIngressCertInput struct {
	EnvironmentPathInput
	CertID string `path:"certId" doc:"The certificate's ID"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// IngressCertBudgetInput is the path of the budget route.
type IngressCertBudgetInput struct {
	EnvironmentPathInput
}

// ingressCertBody maps a certificate onto the contract. It is pure: it reads no clock but now, and it copies no
// private key because the row's key is never read here.
func ingressCertBody(c ingresscert.Cert, now time.Time) IngressCertBody {
	state := ingresscert.StateOf(c, now)
	b := IngressCertBody{
		ID: c.ID, Hostname: c.Hostname, Issuer: c.Issuer, Staging: c.Issuer == ingresscert.IssuerStaging, Source: c.Source,
		State: state, StateText: ingresscert.StateText(state), Ready: c.Ready, StoppedReason: c.StoppedReason,
		IssuedAt: timePtr(c.IssuedAt), ExpiresAt: timePtr(c.NotAfter), RenewAt: timePtr(c.RenewAt), RenewBasis: c.RenewBasis,
		ConsecutiveFailures: c.ConsecutiveFailures, NextAttemptAt: timePtr(c.NextAttemptAt), RetryAfterUntil: timePtr(c.RetryAfterUntil),
		RequestedAt: timePtr(c.RequestedAt), RequestedBy: c.RequestedBy, UnreferencedSince: timePtr(c.UnreferencedSince),
		RetiredAt: timePtr(c.RetiredAt), RetiredReason: c.RetiredReason, CreatedAt: c.CreatedAt.UTC(),
	}
	if c.LastErrorClass != "" {
		b.LastError = &IngressCertErrorBody{
			Class: c.LastErrorClass, Hint: ingresscert.FailureHint(c.LastErrorClass), Detail: c.LastErrorDetail, At: timePtr(c.LastErrorAt),
		}
	}
	if c.QueueCode != "" {
		b.Queue = &IngressCertQueueBody{
			Code: c.QueueCode, Reason: ingresscert.QueueReason(c), Used: c.QueueUsed, Cap: c.QueueCap,
			Domain: c.QueueDomain, NextSlotAt: timePtr(c.QueueNextSlotAt),
		}
	}
	return b
}

// ingressCertRefusal maps a refusal of a request ahead onto its ingress problem code and status. The sentence is the
// policy's, worded so it names nothing the caller may not see.
func ingressCertRefusal(e *ingresscert.RequestRefusedError) error {
	status := http.StatusUnprocessableEntity
	switch e.Problem.Code {
	case ingress.CodeHostTaken, ingress.CodeHostReserved:
		status = http.StatusConflict
	}
	return httpx.Problem(status, e.Problem.Code, e.Problem.Message)
}

// ingressCertBlocked is the 429 of a retry inside a CA wait or the failure guard: the reason is the first error's
// message, the instant it reopens, in RFC 3339, its value, and Retry-After says the wait in whole seconds.
func ingressCertBlocked(e *ingresscert.BlockedError, now time.Time) error {
	until := e.Until.UTC()
	detail := "The certificate authority asked TierPlan to wait before another attempt."
	if e.Reason == acme.BlockFailureGuard {
		detail = "Three attempts for this certificate failed within the hour, so new attempts are held back."
	}
	detail += " Try again after " + until.Format(time.RFC3339) + "."
	secs := int64(math.Ceil(until.Sub(now).Seconds()))
	if secs < 1 {
		secs = 1
	}
	return huma.ErrorWithHeaders(&huma.ErrorModel{
		Type: problemIngressCertBlocked, Title: http.StatusText(http.StatusTooManyRequests), Status: http.StatusTooManyRequests, Detail: detail,
		Errors: []*huma.ErrorDetail{{Location: "reason", Message: e.Reason, Value: until.Format(time.RFC3339)}},
	}, http.Header{"Retry-After": {strconv.FormatInt(secs, 10)}})
}

// RegisterIngressCerts registers the per-hostname certificate routes: list, request ahead, retry now and the budget.
// Every one needs ingress.manage on the environment (its Admin) and every certificate id is looked up inside that
// environment, so an id of another environment is a 404, exactly like one that does not exist. The routes are always
// registered; without a manager they answer 503 ingress_unavailable.
func RegisterIngressCerts(r *httpx.Router, d IngressCertDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Per-hostname certificates are not available on this server.")
	}
	ready := func() bool { return d.Manager != nil && d.Store != nil }
	internal := func(what string, err error) error {
		d.Log.Error("ingress certificate request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	audited := func(ctx context.Context, p *rbac.Principal, userAgent, action, envID string, c ingresscert.Cert, details map[string]string) audit.Entry {
		return audit.Entry{
			Actor: p.AuditActor(), Action: action, TargetType: ingressCertTarget, TargetID: c.ID, EnvID: envID,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
		}
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listIngressCertificates",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/certificates",
		Summary:     "List the certificates of an environment",
		Description: "Every per-hostname certificate of the environment, live and retired, by hostname: its issuer (staging flagged), source, state and the state in words, whether it is ready to serve, " +
			"when it was issued and expires, when it renews next and on what basis, its last failure (class, a fixed hint, the capped detail and the time), the failures in a row, when the next attempt is, " +
			"how long the certificate authority asked TierPlan to wait, why a queued certificate waits (a code, one sentence, the figures and the next slot), who asked for it and when, since when nothing " +
			"refers to it, and why a retired one was retired. No private key and no sealed value is ever returned. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *ListIngressCertsInput) (*IngressCertsOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		certs, err := d.Store.EnvCerts(ctx, in.EnvID)
		if err != nil {
			return nil, internal("list the certificates", err)
		}
		at := now()
		out := &IngressCertsOutput{CacheControl: "no-store"}
		out.Body.Items = make([]IngressCertBody, 0, len(certs))
		for _, c := range certs {
			out.Body.Items = append(out.Body.Items, ingressCertBody(c, at))
		}
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "requestIngressCertificate",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/ingress/certificates",
		DefaultStatus: http.StatusCreated,
		Summary:       "Request a certificate ahead of any claim",
		Description: "Asks for the certificate of a hostname before any stack holds the name. The hostname must be under a domain registered for the environment: 422 " + ingress.CodeDomainNotAllowed +
			" otherwise, 422 " + ingress.CodeHostInvalid + " when it is not a hostname a certificate can be ordered for, 409 " + ingress.CodeHostReserved + " when an Admin reserved it, and 409 " +
			ingress.CodeHostTaken + " when another environment holds it, in words that name nothing the caller may not see. The certificate is queued and ordered by the next round; asking again for a " +
			"hostname that already has one reuses it and starts its grace period again, and a certificate stopped for being unreferenced is brought back. Requests are accepted while issuing is paused: " +
			"nothing is ordered until it resumes. Audited as ingress.cert_requested. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *RequestIngressCertInput) (*IngressCertOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		var canView func(string) bool
		if d.Checker != nil {
			acc, err := d.Checker.Access(ctx, p)
			if err != nil {
				return nil, internal("read the caller's access", err)
			}
			canView = func(envID string) bool { return acc.For(envID).Role != rbac.RoleNone }
		}
		c, _, err := d.Manager.RequestAhead(ctx, ingresscert.RequestInput{
			EnvID: in.EnvID, Hostname: in.Body.Hostname, Actor: p.ID, CanView: canView,
		}, func(ctx context.Context, tx *sql.Tx, c ingresscert.Cert) error {
			details := map[string]string{"hostname": c.Hostname, "issuer": c.Issuer}
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionIngressCertRequested, in.EnvID, c, details))
		})
		var refused *ingresscert.RequestRefusedError
		if errors.As(err, &refused) {
			return nil, ingressCertRefusal(refused)
		}
		if err != nil {
			return nil, internal("request the certificate", err)
		}
		return &IngressCertOutput{CacheControl: "no-store", Body: ingressCertBody(c, now())}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "retryIngressCertificate",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/ingress/certificates/{certId}/retry",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Try a certificate again now",
		Description: "Queues the certificate with its next attempt now, without waiting for its backoff. The order is placed by the next round, so 202. 404 " + problemIngressCertNotFound +
			" for a certificate that is not a live one of this environment that TierPlan orders (another environment's, retired, uploaded or gone); 409 " + problemIngressCertsPaused +
			" while issuing is paused after a restore; 409 " + problemIngressCertStopped + " for a stopped certificate, which waits for a claim or a request, not a retry; 429 " + problemIngressCertBlocked +
			" while the certificate authority has asked TierPlan to wait or three attempts failed within the hour (reason told_to_wait or failure_guard, the instant it reopens in errors[0].value, and a " +
			"Retry-After header in seconds). A certificate whose order is already in flight is left alone and answers 202. Audited as ingress.cert_retried. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *RetryIngressCertInput) (*IngressNoContentOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		err := d.Manager.RetryNow(ctx, in.EnvID, in.CertID, func(ctx context.Context, tx *sql.Tx, c ingresscert.Cert) error {
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionIngressCertRetried, in.EnvID, c, map[string]string{"hostname": c.Hostname}))
		})
		var blocked *ingresscert.BlockedError
		switch {
		case err == nil:
			return &IngressNoContentOutput{CacheControl: "no-store"}, nil
		case errors.Is(err, ingresscert.ErrPaused):
			return nil, httpx.Problem(http.StatusConflict, problemIngressCertsPaused, "Issuing is paused after a restore. An Admin resumes it from the HTTPS settings.")
		case errors.Is(err, ingresscert.ErrCertNotFound):
			return nil, httpx.Problem(http.StatusNotFound, problemIngressCertNotFound, "No such certificate in this environment.")
		case errors.Is(err, ingresscert.ErrCertStopped):
			return nil, httpx.Problem(http.StatusConflict, problemIngressCertStopped, "This certificate is stopped and waits for a stack to claim the hostname or for a new request, not for a retry.")
		case errors.As(err, &blocked):
			return nil, ingressCertBlocked(blocked, now())
		}
		return nil, internal("retry the certificate", err)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getIngressCertificateBudget",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/certificates/budget",
		Summary:     "Read the weekly new-certificate budget",
		Description: "One row per registered domain and issuer the environment's domains use: the new certificates used in the last 7 days (orders in flight and TierPlan's own certificate included), where " +
			"TierPlan stops (cap), the certificate authority's own limit, the headroom, when the next slot opens, the environment's own domains under it, and whether the budget is shared with another " +
			"domain or TierPlan's own address, which is never named. The counts are install-wide. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressCertBudgetInput) (*IngressCertBudgetOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if !ready() {
			return nil, unavailable()
		}
		rows, err := d.Manager.Budget(ctx, in.EnvID)
		if err != nil {
			return nil, internal("read the budget", err)
		}
		out := &IngressCertBudgetOutput{CacheControl: "no-store"}
		out.Body.Items = make([]IngressCertBudgetBody, 0, len(rows))
		for _, row := range rows {
			out.Body.Items = append(out.Body.Items, IngressCertBudgetBody{
				RegisteredDomain: row.RegisteredDomain, Issuer: row.Issuer, Used: row.Used, Cap: row.Cap, Limit: row.Limit,
				Headroom: row.Headroom, NextSlotAt: timePtr(row.NextSlot), Domains: nonNil(row.Domains), Shared: row.Shared,
			})
		}
		return out, nil
	})
}
