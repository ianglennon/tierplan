// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The ingress domain routes (Phase 18, DOM-01 to DOM-04): an Admin registers a domain with the DNS token that
// validates its certificates, replaces the token, deletes a domain nothing claims, and reserves exact hostnames under
// it. None of them ever returns the token, and every answer is no-store.
//
// The token field carries no maxLength in the schema on purpose: a schema violation is answered with the offending
// value, which would echo a secret. The handler bounds it at maxIngressTokenBytes instead and never puts it in a detail.

const (
	problemIngressUnavailable    = "ingress_unavailable"
	problemIngressDomainNotFound = "ingress_domain_not_found"
	problemIngressDomainExists   = "ingress_domain_exists"
	problemIngressDomainInUse    = "ingress_domain_in_use"
	// problemIngressDomainCleanupPending is a delete or a token replacement refused while TierPlan still holds DNS
	// records made with the domain's token (D-27).
	problemIngressDomainCleanupPending = "ingress_domain_cleanup_pending"
	problemIngressTokenMissing         = "ingress_token_missing"  //nolint:gosec // G101: a problem type, not a credential
	problemIngressTokenTooLong         = "ingress_token_too_long" //nolint:gosec // G101: a problem type, not a credential
	problemIngressHostnameInvalid      = "ingress_hostname_invalid"
	problemIngressHostnameNotUnder     = "ingress_hostname_not_under_domain"
	problemIngressHostnameClaimed      = "ingress_hostname_claimed"

	// ingressDomainTarget is the target type of every audit row of these routes.
	ingressDomainTarget = "ingress_domain"

	// maxIngressTokenBytes bounds a DNS token in code. Real provider tokens are a few dozen characters; the bound only
	// stops a body that is a megabyte of nothing.
	maxIngressTokenBytes = 4096
	// ingressTokenTestBudget is the longest the token test may take whatever the provider does.
	ingressTokenTestBudget = 30 * time.Second
)

// IngressDeps are the dependencies of the ingress domain routes.
type IngressDeps struct {
	// Store keeps the domains and reservations. Nil answers 503 ingress_unavailable, so the routes are always registered.
	Store *ingressstore.Store
	// DNSProviders are the DNS provider factories by provider id, the same map the Let's Encrypt manager uses, so one
	// Go option redirects the token to a fake for both. A provider with no factory answers 503 ingress_unavailable.
	DNSProviders map[string]dns.Factory
	Audit        *audit.Writer
	// Checker decides which environments the caller may see, so a refusal names no stack of one they cannot.
	Checker *rbac.Checker
	// CertSweep asks the certificate manager for a full sweep of the DNS records it still holds, so the cleanup a
	// refused delete or token replacement waits for starts at once. It is wired to the manager's SweepNow; nil does
	// nothing.
	CertSweep func()
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// IngressDomainBody is a domain as an Admin sees it. It carries whether a token is saved and when it last changed, never
// the token.
type IngressDomainBody struct {
	ID             string     `json:"id" doc:"The domain's ID, used in the paths of the routes below"`
	Suffix         string     `json:"suffix" doc:"The domain stacks of this environment may serve hostnames under, such as apps.example.com. A hostname must be strictly below it"`
	Issuer         string     `json:"issuer" enum:"staging,production" doc:"The Let's Encrypt directory certificates for this domain come from. Staging certificates are not trusted by browsers"`
	DNSProvider    string     `json:"dnsProvider" doc:"The DNS provider that validates the domain, such as cloudflare"`
	TokenSet       bool       `json:"tokenSet" doc:"True when a DNS token is saved. The token is never returned"`
	TokenUpdatedAt *time.Time `json:"tokenUpdatedAt,omitempty" doc:"When the DNS token was last saved or replaced; left out when none is saved"`
	CreatedBy      string     `json:"createdBy" doc:"The ID of the user who registered the domain"`
	CreatedAt      time.Time  `json:"createdAt" doc:"When the domain was registered"`
	UpdatedAt      time.Time  `json:"updatedAt" doc:"When the domain last changed"`
	Reserved       []string   `json:"reserved" doc:"The hostnames held back under the domain, sorted. No stack can claim one"`
	Certificates   int        `json:"certificates" doc:"The live certificates under the domain. If the domain is deleted they stop renewing and expire"`
}

// IngressDomainsBody is the list of the domains of one environment.
type IngressDomainsBody struct {
	Items []IngressDomainBody `json:"items" doc:"The environment's domains, by suffix"`
}

// IngressDomainOutput is one domain, never cached.
type IngressDomainOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressDomainBody
}

// IngressDomainsOutput is the list of domains, never cached.
type IngressDomainsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressDomainsBody
}

// IngressNoContentOutput is an answer with no body that must still not be cached.
type IngressNoContentOutput struct {
	CacheControl string `header:"Cache-Control"`
}

// IngressDomainDeletedBody is what a deletion that left certificates behind says.
type IngressDomainDeletedBody struct {
	CertificatesLeft int `json:"certificatesLeft" doc:"The live certificates the deletion left behind. They stop renewing and are deleted when they expire"`
}

// IngressDomainDeletedOutput is the answer of the domain delete: 200 with the count when certificates are left to expire,
// 204 with no body when none are, as before certificates existed. Never cached.
type IngressDomainDeletedOutput struct {
	Status       int
	CacheControl string `header:"Cache-Control"`
	Body         *IngressDomainDeletedBody
}

// IngressReservationBody is a reserved hostname.
type IngressReservationBody struct {
	DomainID  string    `json:"domainId" doc:"The domain the name is reserved under"`
	Suffix    string    `json:"suffix" doc:"The domain's suffix"`
	Hostname  string    `json:"hostname" doc:"The reserved hostname, lower-case with no trailing dot"`
	CreatedBy string    `json:"createdBy" doc:"The ID of the user who reserved it"`
	CreatedAt time.Time `json:"createdAt" doc:"When it was reserved"`
}

// IngressReservationOutput is one reservation, never cached.
type IngressReservationOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressReservationBody
}

// ListIngressDomainsInput is the path of the list route.
type ListIngressDomainsInput struct {
	EnvironmentPathInput
}

// CreateIngressDomainInput is the body of the registration.
type CreateIngressDomainInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Suffix string `json:"suffix" maxLength:"253" doc:"The domain to register, such as apps.example.com. A public suffix (co.uk, github.io) is refused with 422 ingress_domain_public_suffix before the DNS provider is asked"`
		Issuer string `json:"issuer,omitempty" enum:"staging,production" default:"production" doc:"The Let's Encrypt directory, production when left out"`
		Token  string `json:"token,omitempty" doc:"The DNS provider API token, at most 4096 bytes. It is tested first, then stored sealed; it is never returned. 422 ingress_token_missing when left out, ingress_token_too_long when over the bound"`
	}
}

// ReplaceIngressDomainTokenInput is the path and body of the token replacement.
type ReplaceIngressDomainTokenInput struct {
	EnvironmentPathInput
	DomainID string `path:"domainId" doc:"The domain's ID"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Token string `json:"token,omitempty" doc:"The new DNS provider API token, at most 4096 bytes. It is tested first and replaces the saved one only when the test passes; it is never returned"`
	}
}

// DeleteIngressDomainInput is the path of the delete route.
type DeleteIngressDomainInput struct {
	EnvironmentPathInput
	DomainID string `path:"domainId" doc:"The domain's ID"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// ReserveIngressHostnameInput is the path and body of a reservation.
type ReserveIngressHostnameInput struct {
	EnvironmentPathInput
	DomainID string `path:"domainId" doc:"The domain's ID"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Hostname string `json:"hostname" maxLength:"254" doc:"The exact hostname to hold back, strictly below the domain. It is lower-cased and a trailing dot is dropped. 422 ingress_hostname_not_under_domain for the domain itself or a name outside it, ingress_hostname_invalid for a wildcard or anything that is not a hostname"`
	}
}

// UnreserveIngressHostnameInput is the path of a release.
type UnreserveIngressHostnameInput struct {
	EnvironmentPathInput
	DomainID string `path:"domainId" doc:"The domain's ID"`
	Hostname string `path:"hostname" doc:"The reserved hostname"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// ingressDomainBody maps a domain onto the contract. reserved may be nil.
func ingressDomainBody(d ingress.Domain, reserved []string, certificates int) IngressDomainBody {
	b := IngressDomainBody{
		Certificates: certificates,
		ID:           d.ID, Suffix: d.Suffix, Issuer: string(d.Issuer), DNSProvider: d.DNSProvider, TokenSet: d.HasToken,
		CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Reserved: nonNil(reserved),
	}
	if !d.TokenUpdatedAt.IsZero() {
		at := d.TokenUpdatedAt
		b.TokenUpdatedAt = &at
	}
	return b
}

// tokenTestFailure is a token test that did not pass: the problem the caller gets and the class its audit row records.
type tokenTestFailure struct {
	problem error
	class   string
}

// dnsProblem is the problem of one class of DNS failure. suffix is the domain the test was for; nothing the provider said
// and nothing of the token is in the sentence.
func dnsProblem(class, suffix string) error {
	switch class {
	case dns.ClassTokenInvalid:
		return httpx.Problem(http.StatusUnprocessableEntity, dns.ClassTokenInvalid,
			"The DNS provider did not accept this token. Check that it is a current API token and paste it again.")
	case dns.ClassTokenScope:
		return httpx.Problem(http.StatusUnprocessableEntity, dns.ClassTokenScope,
			"The DNS provider accepted the token, but it cannot list any DNS zone. Give it permission to read zones and edit DNS records for the zone that holds “"+suffix+"”.")
	case dns.ClassZoneNotFound:
		return httpx.Problem(http.StatusUnprocessableEntity, dns.ClassZoneNotFound,
			"No DNS zone this token can see holds “"+suffix+"”. Use a token for the account that holds the zone, or register a domain inside a zone this token sees.")
	case dns.ClassRateLimited:
		return httpx.Problem(http.StatusServiceUnavailable, dns.ClassRateLimited,
			"The DNS provider asked TierPlan to slow down. Wait a minute and try again.")
	default:
		return httpx.Problem(http.StatusServiceUnavailable, dns.ClassUnavailable,
			"The DNS provider did not answer. Nothing was saved; try again shortly.")
	}
}

// printableToken reports whether every byte of a token is a printable ASCII character with no space, which is all a
// provider's bearer token holds. Anything else cannot be a token, and a control character must never reach a header.
func printableToken(token string) bool {
	for i := 0; i < len(token); i++ {
		if c := token[i]; c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// testDomainToken asks the provider, with the token, for the zone that holds the suffix. The token passes when the
// provider finds a zone that is the suffix or a parent of it. It runs before any write, so the single write connection is
// never held while the provider is slow. A token that sees the zone but lacks the right to edit DNS records passes here
// and fails at issuance (RESEARCH A10).
func testDomainToken(ctx context.Context, log *slog.Logger, factory dns.Factory, token, suffix string) *tokenTestFailure {
	if !printableToken(token) {
		return &tokenTestFailure{problem: dnsProblem(dns.ClassTokenInvalid, suffix), class: dns.ClassTokenInvalid}
	}
	ctx, cancel := context.WithTimeout(ctx, ingressTokenTestBudget)
	defer cancel()
	z, err := factory(token).Zone(ctx, suffix)
	if err != nil {
		class := dns.ClassUnavailable
		var de *dns.Error
		if errors.As(err, &de) && de.Class != "" {
			class = de.Class
		}
		switch class {
		case dns.ClassTokenInvalid, dns.ClassTokenScope, dns.ClassZoneNotFound, dns.ClassRateLimited, dns.ClassUnavailable:
		default:
			class = dns.ClassUnavailable
		}
		// The class is the whole record of a classified failure; anything else is logged with the token struck out.
		if class == dns.ClassUnavailable {
			log.Warn("ingress domain token test failed", "suffix", suffix, "class", class, "error", strings.ReplaceAll(err.Error(), token, "[redacted]"))
		}
		return &tokenTestFailure{problem: dnsProblem(class, suffix), class: class}
	}
	zone := ingress.NormalizeHost(z.Name)
	if zone != suffix && !strings.HasSuffix(suffix, "."+zone) {
		return &tokenTestFailure{problem: dnsProblem(dns.ClassZoneNotFound, suffix), class: dns.ClassZoneNotFound}
	}
	return nil
}

// checkIngressToken bounds a typed token: surrounding space is dropped, nothing is a 422 ingress_token_missing, and more
// than maxIngressTokenBytes is a 422 ingress_token_too_long. Neither detail holds the token.
func checkIngressToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	switch {
	case token == "":
		return "", httpx.Problem(http.StatusUnprocessableEntity, problemIngressTokenMissing, "Enter the DNS provider API token for this domain.")
	case len(token) > maxIngressTokenBytes:
		return "", httpx.Problem(http.StatusUnprocessableEntity, problemIngressTokenTooLong, "A DNS token is at most 4096 bytes; this one is longer.")
	}
	return token, nil
}

// ingressInternal logs a server-side failure and answers a plain 500.
func ingressInternal(log *slog.Logger, what string, err error) error {
	log.Error("ingress domain request failed", "step", what, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// holdersProblem is the 409 for a domain that stacks still serve names under. The sentence and the errors list name every
// holder: a domain is registered in one environment and its holders are stacks of that environment, which the Admin
// manages.
func holdersProblem(suffix string, holders []ingressstore.Holder) error {
	m := &huma.ErrorModel{Type: problemIngressDomainInUse, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict}
	var seen []string
	for i, h := range holders {
		name := h.StackName
		if name == "" {
			name = "TierPlan itself"
		}
		label := name + " in " + h.EnvName
		m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "holders[" + strconv.Itoa(i) + "]", Message: label})
		dup := false
		for _, s := range seen {
			if s == label {
				dup = true
				break
			}
		}
		if !dup {
			seen = append(seen, label)
		}
	}
	m.Detail = "“" + suffix + "” cannot be deleted while hostnames under it are claimed by: " + strings.Join(seen, ", ") + ". Remove those hostnames from the stacks first."
	return m
}

// RegisterIngress registers the ingress domain routes: list, register, replace the token, delete, reserve and release.
// Every one needs ingress.manage on the environment (its Admin) and every id is looked up inside that environment, so a
// domain id of another environment is a 404, exactly like one that does not exist. The routes are always registered;
// without a Store or a DNS provider they answer 503 ingress_unavailable.
func RegisterIngress(r *httpx.Router, d IngressDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Ingress domains are not available on this server.")
	}
	// factory is the provider factory every domain uses today, nil when the server has none.
	factory := func() dns.Factory { return d.DNSProviders[ingress.ProviderCloudflare] }
	notFound := func() error {
		return httpx.Problem(http.StatusNotFound, problemIngressDomainNotFound, "No such domain in this environment.")
	}
	// audited builds the entry of one change of a domain or of a name under it.
	audited := func(ctx context.Context, p *rbac.Principal, userAgent, action, envID, domainID string, details map[string]string) audit.Entry {
		return audit.Entry{
			Actor: p.AuditActor(), Action: action, TargetType: ingressDomainTarget, TargetID: domainID, EnvID: envID,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
		}
	}
	// refused records a token test that did not pass, with the DNS class as the reason, and returns the problem. A domain
	// that does not exist yet has no target id.
	refused := func(ctx context.Context, p *rbac.Principal, userAgent, action, envID, domainID string, details map[string]string, f *tokenTestFailure) error {
		e := audited(ctx, p, userAgent, action, envID, domainID, details)
		e.Result, e.Reason = audit.ResultFailure, f.class
		if err := d.Audit.Record(ctx, e); err != nil {
			return ingressInternal(d.Log, "audit the refused token test", err)
		}
		return f.problem
	}
	// failed maps a refusal of the store onto its problem.
	failed := func(what string, err error) error {
		var (
			ne      *ingress.NameError
			clm     *ingressstore.ReservedClaimedError
			pending *ingressstore.CleanupPendingError
		)
		switch {
		case errors.As(err, &pending):
			// The records are removed with the domain's own token by the sweep; start it now.
			if d.CertSweep != nil {
				d.CertSweep()
			}
			return httpx.Problem(http.StatusConflict, problemIngressDomainCleanupPending,
				"TierPlan is still removing DNS records it made with this domain's token. Try again in a minute: the domain's token cannot change or be deleted until they are gone.")
		case errors.As(err, &ne):
			return httpx.Problem(http.StatusUnprocessableEntity, ne.Code, ne.Message)
		case errors.Is(err, ingressstore.ErrDomainNotFound):
			return notFound()
		case errors.Is(err, ingressstore.ErrDomainExists):
			return httpx.Problem(http.StatusConflict, problemIngressDomainExists, "This environment already has that domain registered.")
		case errors.Is(err, ingressstore.ErrTokenEmpty):
			return httpx.Problem(http.StatusUnprocessableEntity, problemIngressTokenMissing, "Enter the DNS provider API token for this domain.")
		case errors.Is(err, ingressstore.ErrNotUnderDomain):
			return httpx.Problem(http.StatusUnprocessableEntity, problemIngressHostnameNotUnder, "A reserved name must be strictly below the domain: not the domain itself and not a name outside it.")
		case errors.As(err, &clm):
			return httpx.Problem(http.StatusConflict, problemIngressHostnameClaimed, clm.Message)
		}
		return ingressInternal(d.Log, what, err)
	}
	// domainOf reads one domain of the environment, ErrDomainNotFound when the id is another environment's or unknown.
	domainOf := func(ctx context.Context, envID, id string) (ingressstore.DomainView, error) {
		views, err := d.Store.Domains(ctx, envID)
		if err != nil {
			return ingressstore.DomainView{}, err
		}
		for _, v := range views {
			if v.ID == id {
				return v, nil
			}
		}
		return ingressstore.DomainView{}, ingressstore.ErrDomainNotFound
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listIngressDomains",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/domains",
		Summary:     "List the domains of an environment",
		Description: "The domains an environment's stacks may serve hostnames under, by suffix, each with the hostnames reserved under it and whether a DNS " +
			"token is saved and when it last changed. The token is never returned. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *ListIngressDomainsInput) (*IngressDomainsOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		views, err := d.Store.Domains(ctx, in.EnvID)
		if err != nil {
			return nil, failed("list the domains", err)
		}
		out := &IngressDomainsOutput{CacheControl: "no-store"}
		out.Body.Items = make([]IngressDomainBody, 0, len(views))
		for _, v := range views {
			out.Body.Items = append(out.Body.Items, ingressDomainBody(v.Domain, v.Reserved, v.Certificates))
		}
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "createIngressDomain",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/ingress/domains",
		DefaultStatus: http.StatusCreated,
		Summary:       "Register a domain with its DNS token",
		Description: "Registers a domain for the environment. The suffix is checked first: a public suffix is refused with 422 " + ingress.CodeDomainPublicSuffix +
			" before the DNS provider is contacted, and anything that is not a domain with 422 " + ingress.CodeDomainInvalid + ". The token is then tested by asking the provider " +
			"for the zone that holds the suffix, and the domain is stored only when a zone is found; a refused test is 422 dns_token_invalid, dns_token_scope or dns_zone_not_found, " +
			"or 503 dns_rate_limited or dns_unavailable, and stores nothing. A token that sees the zone but may not edit its DNS records passes this test and fails when a " +
			"certificate is first issued. The token is stored sealed and never returned: the answer carries tokenSet and tokenUpdatedAt only. The issuer is production unless " +
			"staging is chosen, and no route changes it afterwards. 409 " + problemIngressDomainExists + " when the environment already has the suffix (another environment may). " +
			"Audited as ingress.domain_created without the token; a refused test is audited as a failure with the DNS class as its reason. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *CreateIngressDomainInput) (*IngressDomainOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil || factory() == nil {
			return nil, unavailable()
		}
		suffix := ingress.NormalizeHost(in.Body.Suffix)
		if err := ingress.ValidateSuffix(suffix); err != nil {
			return nil, failed("validate the suffix", err)
		}
		issuer := ingress.Issuer(in.Body.Issuer)
		if in.Body.Issuer == "" {
			issuer = ingress.IssuerProduction
		}
		if !issuer.Valid() {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, "ingress_issuer_invalid", "The issuer is staging or production.")
		}
		token, err := checkIngressToken(in.Body.Token)
		if err != nil {
			return nil, err
		}
		attempt := map[string]string{"suffix": suffix, "issuer": string(issuer), "provider": ingress.ProviderCloudflare}
		if f := testDomainToken(ctx, d.Log, factory(), token, suffix); f != nil {
			return nil, refused(ctx, p, in.UserAgent, audit.ActionIngressDomainCreated, in.EnvID, "", attempt, f)
		}
		dom, err := d.Store.CreateDomain(ctx, ingressstore.CreateDomainInput{
			EnvID: in.EnvID, Suffix: suffix, Issuer: issuer, Token: []byte(token), Actor: p.ID,
		}, func(ctx context.Context, tx *sql.Tx, dom ingress.Domain) error {
			details := map[string]string{"suffix": dom.Suffix, "issuer": string(dom.Issuer), "provider": dom.DNSProvider, "credential": "set"}
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionIngressDomainCreated, in.EnvID, dom.ID, details))
		})
		if err != nil {
			return nil, failed("register the domain", err)
		}
		return &IngressDomainOutput{CacheControl: "no-store", Body: ingressDomainBody(dom, nil, 0)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "replaceIngressDomainToken",
		Method:      http.MethodPut,
		Path:        "/api/env/{envId}/ingress/domains/{domainId}/token",
		Summary:     "Replace a domain's DNS token",
		Description: "Tests the new token the way registration does, then replaces the saved one. The domain, its issuer and every hostname claimed under it stay; " +
			"tokenUpdatedAt moves. A token that fails its test is refused with the same 422 or 503 problems as registration and the saved token stays in place. The token is " +
			"never returned. 404 " + problemIngressDomainNotFound + " for a domain of another environment. Audited as ingress.domain_credential_replaced without the token. " +
			"Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *ReplaceIngressDomainTokenInput) (*IngressDomainOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil || factory() == nil {
			return nil, unavailable()
		}
		cur, err := domainOf(ctx, in.EnvID, in.DomainID)
		if err != nil {
			return nil, failed("read the domain", err)
		}
		token, err := checkIngressToken(in.Body.Token)
		if err != nil {
			return nil, err
		}
		if f := testDomainToken(ctx, d.Log, factory(), token, cur.Suffix); f != nil {
			return nil, refused(ctx, p, in.UserAgent, audit.ActionIngressDomainCredentialReplaced, in.EnvID, cur.ID, map[string]string{"suffix": cur.Suffix}, f)
		}
		dom, err := d.Store.SetDomainToken(ctx, in.EnvID, in.DomainID, []byte(token), p.ID, func(ctx context.Context, tx *sql.Tx, dom ingress.Domain) error {
			details := map[string]string{"suffix": dom.Suffix, "credential": "replaced"}
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionIngressDomainCredentialReplaced, in.EnvID, dom.ID, details))
		})
		if err != nil {
			return nil, failed("replace the token", err)
		}
		return &IngressDomainOutput{CacheControl: "no-store", Body: ingressDomainBody(dom, cur.Reserved, cur.Certificates)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteIngressDomain",
		Method:        http.MethodDelete,
		Path:          "/api/env/{envId}/ingress/domains/{domainId}",
		DefaultStatus: http.StatusOK,
		Summary:       "Delete a domain",
		Description: "Deletes a domain and the hostnames reserved under it, when no hostname claims it. While one does, 409 " + problemIngressDomainInUse + " names every holding " +
			"stack and its environment in the detail and in the errors list (locations holders[0], holders[1], ...), and nothing changes. While TierPlan still holds DNS records it made " +
			"with the domain's token, 409 " + problemIngressDomainCleanupPending + " and nothing changes: the removal is started at once, so the same request succeeds shortly. " +
			"404 " + problemIngressDomainNotFound + " for a domain that is gone or belongs to another environment, with no audit row. " +
			"Answers 200 with certificatesLeft when live certificates under the domain are left to expire (they stop renewing), and 204 with no body when none are. " +
			"Audited as ingress.domain_deleted, with the certificate count. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
		Responses: map[string]*huma.Response{
			"204": {
				Description: "Deleted, and no certificates were left behind",
				Headers:     map[string]*huma.Param{"Cache-Control": {Schema: &huma.Schema{Type: "string"}}},
			},
			// Two success answers make huma leave out the default error answer it adds for one, so it is stated here.
			"default": {
				Description: "Error",
				Content: map[string]*huma.MediaType{"application/problem+json": {
					Schema: r.API().OpenAPI().Components.Schemas.Schema(reflect.TypeFor[huma.ErrorModel](), true, "ErrorModel"),
				}},
			},
		},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *DeleteIngressDomainInput) (*IngressDomainDeletedOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		// The certificates the deletion leaves behind: they stop renewing and expire (D-28), and the Admin is told how many (D-46).
		cur, err := domainOf(ctx, in.EnvID, in.DomainID)
		if err != nil {
			return nil, failed("read the domain", err)
		}
		certs := cur.Certificates
		err = d.Store.DeleteDomain(ctx, in.EnvID, in.DomainID, func(ctx context.Context, tx *sql.Tx, dom ingress.Domain) error {
			details := map[string]string{"suffix": dom.Suffix, "issuer": string(dom.Issuer), "certificates": strconv.Itoa(certs)}
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionIngressDomainDeleted, in.EnvID, dom.ID, details))
		})
		var use *ingressstore.DomainInUseError
		if errors.As(err, &use) {
			return nil, holdersProblem(cur.Suffix, use.Holders)
		}
		if err != nil {
			return nil, failed("delete the domain", err)
		}
		if certs == 0 {
			return &IngressDomainDeletedOutput{Status: http.StatusNoContent, CacheControl: "no-store"}, nil
		}
		return &IngressDomainDeletedOutput{Status: http.StatusOK, CacheControl: "no-store", Body: &IngressDomainDeletedBody{CertificatesLeft: certs}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "reserveIngressHostname",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/ingress/domains/{domainId}/reserved",
		DefaultStatus: http.StatusCreated,
		Summary:       "Reserve a hostname under a domain",
		Description: "Holds back one exact hostname strictly below the domain, so no stack in any environment can claim it. The name is lower-cased and a trailing dot is dropped. " +
			"422 " + problemIngressHostnameNotUnder + " for the domain itself or a name outside it, 422 " + problemIngressHostnameInvalid + " for a wildcard or anything that is not a hostname. " +
			"409 " + problemIngressHostnameClaimed + " when a stack already holds the name: the detail names the stack and its environment only when the caller may see that environment, " +
			"and says only that another environment holds it otherwise. Reserving a name that is reserved already is a no-op that writes no audit row. 404 " + problemIngressDomainNotFound +
			" for a domain of another environment. Audited as ingress.host_reserved. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *ReserveIngressHostnameInput) (*IngressReservationOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		var canView func(string) bool
		if d.Checker != nil {
			acc, err := d.Checker.Access(ctx, p)
			if err != nil {
				return nil, ingressInternal(d.Log, "read the caller's access", err)
			}
			canView = func(envID string) bool { return acc.For(envID).Role != rbac.RoleNone }
		}
		res, _, err := d.Store.Reserve(ctx, ingressstore.ReserveInput{
			EnvID: in.EnvID, DomainID: in.DomainID, Hostname: in.Body.Hostname, Actor: p.ID, CanView: canView,
		}, func(ctx context.Context, tx *sql.Tx, r ingressstore.Reservation) error {
			details := map[string]string{"suffix": r.Suffix, "hostname": r.Hostname}
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionIngressHostReserved, in.EnvID, r.DomainID, details))
		})
		var ne *ingress.NameError
		if errors.As(err, &ne) {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemIngressHostnameInvalid, ne.Message)
		}
		if err != nil {
			return nil, failed("reserve the hostname", err)
		}
		return &IngressReservationOutput{CacheControl: "no-store", Body: IngressReservationBody{
			DomainID: res.DomainID, Suffix: res.Suffix, Hostname: res.Hostname, CreatedBy: res.CreatedBy, CreatedAt: res.CreatedAt,
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "unreserveIngressHostname",
		Method:        http.MethodDelete,
		Path:          "/api/env/{envId}/ingress/domains/{domainId}/reserved/{hostname}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Release a reserved hostname",
		Description: "Lets a reserved name go. Releasing a name that is not reserved is a success that writes no audit row. 404 " + problemIngressDomainNotFound +
			" for a domain of another environment. Audited as ingress.host_unreserved. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *UnreserveIngressHostnameInput) (*IngressNoContentOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		_, err := d.Store.Unreserve(ctx, in.EnvID, in.DomainID, in.Hostname, func(ctx context.Context, tx *sql.Tx, r ingressstore.Reservation) error {
			details := map[string]string{"suffix": r.Suffix, "hostname": r.Hostname}
			return d.Audit.RecordTx(ctx, tx, audited(ctx, p, in.UserAgent, audit.ActionIngressHostUnreserved, in.EnvID, r.DomainID, details))
		})
		if err != nil {
			return nil, failed("release the hostname", err)
		}
		return &IngressNoContentOutput{CacheControl: "no-store"}, nil
	})
}
