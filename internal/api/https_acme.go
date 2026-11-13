// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The problem types the Let's Encrypt routes answer with. The browser maps each to its own copy.
const (
	problemACMETOSRequired    = "acme_tos_required"
	problemACMEContactInvalid = "acme_contact_invalid"
	problemACMEAccountExists  = "acme_account_exists"
	problemACMEPaused         = "acme_paused"
	problemACMECertFiles      = "acme_cert_files"
	problemACMEProfileUnknown = "acme_profile_unknown"
	problemACMERunActive      = "acme_run_active"
	problemACMECAError        = "acme_ca_error"

	// The 409 an environment variable's lock answers with (D-17). They equal the Problem of the
	// acme.ErrLocked the manager returns; the route descriptions name them.
	problemACMELocked            = "acme_locked"
	problemACMEEmailLocked       = "acme_email_locked"
	problemACMETOSLocked         = "acme_tos_locked"
	problemACMEProfileLocked     = "acme_profile_locked"
	problemACMEChallengeLocked   = "acme_challenge_locked"
	problemACMEDNSProviderLocked = "acme_dns_provider_locked"
	problemACMEDNSTokenLocked    = "acme_dns_token_locked" //nolint:gosec // G101: a problem type name, not a credential
	problemACMEResolversLocked   = "acme_resolvers_locked"

	problemACMEDNSTokenRequired = "acme_dns_token_required" //nolint:gosec // G101: a problem type name, not a credential
	problemACMEDNSResolverBad   = "acme_dns_resolver_invalid"
	problemACMEDNSTokenInUse    = "acme_dns_token_in_use" //nolint:gosec // G101: a problem type name, not a credential
	problemACMEChallengeUnknown = "acme_challenge_unknown"
)

// HTTPSACME is the Let's Encrypt manager as the HTTPS routes use it. *acme.Manager is the one
// implementation; the run actions are used by the run routes.
type HTTPSACME interface {
	Status(ctx context.Context) (acme.Status, error)
	Register(ctx context.Context, email string, agreeTOS bool) (acme.Account, error)
	Directory(ctx context.Context) (acme.DirectoryMeta, error)
	SetProfile(ctx context.Context, profile string) error
	SaveValidation(ctx context.Context, in acme.ValidationInput) (acme.SaveResult, error)
	TestToken(ctx context.Context, req acme.TestRequest) (acme.TokenTest, error)
	RemoveToken(ctx context.Context) error
	Issue(ctx context.Context, actor string) (acme.RunStart, error)
	RenewNow(ctx context.Context, actor string) (acme.RunStart, error)
	Resume(ctx context.Context, actor string) error
	Stop(ctx context.Context, actor string) error
}

var _ HTTPSACME = (*acme.Manager)(nil)

// HTTPSACMEAccountBody is the registered account. The account key is never part of it.
type HTTPSACMEAccountBody struct {
	RegisteredAt      time.Time `json:"registeredAt" doc:"When the account was registered"`
	Contact           string    `json:"contact" doc:"The contact email address, empty when none was given"`
	TermsOfServiceURL string    `json:"termsOfServiceUrl" doc:"Where the terms of service the account agreed to can be read, empty when the CA names none"`
}

// HTTPSACMENameBody is one name the certificate would carry.
type HTTPSACMENameBody struct {
	Name    string `json:"name" doc:"The host name or address, in its ASCII form"`
	Refused bool   `json:"refused" doc:"True when this name cannot be ordered, for example an IP address"`
}

// HTTPSACMERefusalBody is one reason an order cannot start. It carries data only; the browser writes the words.
type HTTPSACMERefusalBody struct {
	Code  string   `json:"code" enum:"names_ip,agent_url_ip,names_wildcard,names_not_public,base_path,agent_dial_host" doc:"Why the order is refused"`
	Host  string   `json:"host,omitempty" doc:"The host the refusal is about"`
	Path  string   `json:"path,omitempty" doc:"The path the public address is served under (base_path)"`
	Count int      `json:"count,omitempty" doc:"How many connected agents dialed a host the certificate would not cover (agent_dial_host)"`
	Hosts []string `json:"hosts,omitempty" doc:"The hosts those agents dialed (agent_dial_host)"`
}

// HTTPSACMECertificateBody is the stored Let's Encrypt certificate, whether or not it is being served.
type HTTPSACMECertificateBody struct {
	Names     []string  `json:"names" doc:"The names it covers"`
	NotBefore time.Time `json:"notBefore" doc:"When it became valid"`
	NotAfter  time.Time `json:"notAfter" doc:"When it expires"`
	IssuedAt  time.Time `json:"issuedAt" doc:"When it was issued"`
	Renewed   bool      `json:"renewed" doc:"True when the newest issue was a renewal"`
}

// HTTPSACMERenewalBody is when the certificate is next renewed and why then.
type HTTPSACMERenewalBody struct {
	NextAt time.Time `json:"nextAt" doc:"When the next renewal is due"`
	Basis  string    `json:"basis" enum:"ari,two_thirds,half" doc:"What the time was worked out from: the CA's renewal information, two thirds of the certificate's life, or half of it"`
}

// HTTPSACMEFallbackBody says the Let's Encrypt certificate has expired and the self-signed one is served.
type HTTPSACMEFallbackBody struct {
	ExpiredAt time.Time `json:"expiredAt" doc:"When the Let's Encrypt certificate expired"`
}

// HTTPSACMEErrorBody is the last failure. Detail is the CA's own text, cleaned and cut to a fixed size.
type HTTPSACMEErrorBody struct {
	Code     string    `json:"code" enum:"rate_limited,validation_unreachable,validation_dns,caa_forbids,identifier_rejected,contact_invalid,tos_action_required,account_gone,profile_invalid,ca_unavailable,ca_untrusted,internal,dns_token_invalid,dns_token_scope,dns_zone_not_found,dns_delegation_target,dns_not_visible,dns_rate_limited,dns_unavailable,validation_txt" doc:"The class of the failure"`
	At       time.Time `json:"at" doc:"When it happened"`
	Host     string    `json:"host,omitempty" doc:"The host a validation failure is about"`
	Profile  string    `json:"profile,omitempty" doc:"The profile the CA refused"`
	TermsURL string    `json:"termsUrl,omitempty" doc:"Where to read the terms the CA wants accepted"`
	Detail   string    `json:"detail,omitempty" doc:"The CA's problem document as text, for a Details disclosure. Untrusted text: show it as text only"`
	// The facts a DNS failure is about.
	Zone      string   `json:"zone,omitempty" doc:"The DNS zone a DNS failure is about"`
	Target    string   `json:"target,omitempty" doc:"The name the challenge name is delegated to, when a DNS failure is about a delegated name"`
	Resolvers []string `json:"resolvers,omitempty" doc:"The resolvers a propagation failure asked"`
}

// HTTPSACMEBlockBody says a manual attempt is refused for now.
type HTTPSACMEBlockBody struct {
	Until  time.Time `json:"until" doc:"When an attempt is allowed again"`
	Reason string    `json:"reason" enum:"told_to_wait,failure_guard" doc:"The CA asked us to wait, or three runs failed within the hour"`
}

// HTTPSACMERunBody is one certificate run.
type HTTPSACMERunBody struct {
	JobID      string     `json:"jobId" doc:"The run's job; follow its steps on GET /api/https/acme/runs/{jobId}/stream while the server still holds the run"`
	Kind       string     `json:"kind" enum:"issue,renew" doc:"A first issue or a renewal"`
	Trigger    string     `json:"trigger" enum:"admin,schedule,resume" doc:"What started it"`
	Outcome    string     `json:"outcome" enum:"running,succeeded,failed,cancelled,reused" doc:"How it went"`
	Challenge  string     `json:"challenge" enum:"http-01,dns-01" doc:"The challenge the run used to prove the names"`
	StartedAt  time.Time  `json:"startedAt" doc:"When it started"`
	FinishedAt *time.Time `json:"finishedAt,omitempty" doc:"When it ended; left out while it runs"`
}

// HTTPSACMEBody is everything the Let's Encrypt page shows. It is read from local state and makes no
// request to the CA. It never holds a key or a key authorization.
type HTTPSACMEBody struct {
	DirectoryURL        string                    `json:"directoryUrl" doc:"The CA directory in use"`
	DirectoryOverridden bool                      `json:"directoryOverridden" doc:"True when the test-only directory override is set; its certificates are not trusted by browsers"`
	Mode                string                    `json:"mode" enum:"off,active" doc:"active when Let's Encrypt is in use"`
	PausedReason        string                    `json:"pausedReason" enum:",restore,cert_files" doc:"Why renewals are paused: a backup was restored, or certificate files are set. Empty when they run"`
	HasState            bool                      `json:"hasState" doc:"True when a Let's Encrypt certificate or an open order is stored"`
	ChallengeType       string                    `json:"challengeType" doc:"The challenge used to prove the names"`
	ListenPort          int                       `json:"listenPort" doc:"The port TierPlan listens on"`
	PublicHost          string                    `json:"publicHost" doc:"The host of the public address, empty when none is set"`
	Account             *HTTPSACMEAccountBody     `json:"account,omitempty" doc:"The registered account; left out before registration"`
	Names               []HTTPSACMENameBody       `json:"names" doc:"The names a certificate would carry now"`
	Refusals            []HTTPSACMERefusalBody    `json:"refusals" doc:"Every reason an order cannot start, empty when one could"`
	Certificate         *HTTPSACMECertificateBody `json:"certificate,omitempty" doc:"The stored Let's Encrypt certificate"`
	NamesMismatch       bool                      `json:"namesMismatch" doc:"True when the stored certificate's names are not the names wanted now"`
	Profile             string                    `json:"profile" doc:"The profile orders ask for: the one TIERPLAN_ACME_PROFILE sets when it is set, otherwise the saved one; empty is the CA's default"`
	Renewal             *HTTPSACMERenewalBody     `json:"renewal,omitempty" doc:"The next renewal; left out unless Let's Encrypt is in use"`
	Fallback            *HTTPSACMEFallbackBody    `json:"fallback,omitempty" doc:"Set when the certificate has expired and the self-signed one is served"`
	LastError           *HTTPSACMEErrorBody       `json:"lastError,omitempty" doc:"The last failure, until a run succeeds"`
	Failures            int                       `json:"failures" doc:"Consecutive failed runs"`
	NextAttemptAt       *time.Time                `json:"nextAttemptAt,omitempty" doc:"When the next automatic attempt is due after a failure"`
	Blocked             *HTTPSACMEBlockBody       `json:"blocked,omitempty" doc:"Set while a manual attempt is refused"`
	ActiveRun           *HTTPSACMERunBody         `json:"activeRun,omitempty" doc:"The run in progress"`
	LastRun             *HTTPSACMERunBody         `json:"lastRun,omitempty" doc:"The run that ended last"`
	ValidationUntested  bool                      `json:"validationUntested" doc:"True when the saved validation method has not been proved by a run yet; false while a run is in progress"`
	SuggestDNS01        *HTTPSACMESuggestionBody  `json:"suggestDns01,omitempty" doc:"Set when HTTP-01 is saved and the public host resolves to a private address, so DNS-01 may suit this install better"`
	DNS                 HTTPSACMEDNSBody          `json:"dns" doc:"The DNS-01 settings and the DNS records TierPlan could not delete yet. The token itself is never returned"`
	Locks               HTTPSLocksBody            `json:"locks" doc:"The settings an environment variable fixes, each with the variable's name. A setting no variable fixes has no entry, and a locked setting cannot be changed through the API"`
}

// HTTPSLockBody says that an environment variable fixes a setting. The variable's name always comes from
// the server, so the page never spells one itself.
type HTTPSLockBody struct {
	Var    string `json:"var" doc:"The environment variable that fixes the setting, for example TIERPLAN_ACME_PROFILE"`
	Value  string `json:"value,omitempty" doc:"What the variable sets. Never set for a secret"`
	Stored string `json:"stored,omitempty" doc:"The value kept in the database, used again when the variable is removed. Never set for a secret, and left out when nothing is stored"`
}

// HTTPSLocksBody is one entry for each setting the environment fixes; a setting no variable fixes is left out.
type HTTPSLocksBody struct {
	ACME          *HTTPSLockBody `json:"acme,omitempty" doc:"The Let's Encrypt switch, fixed by TIERPLAN_ACME. Value and stored are on or off"`
	Email         *HTTPSLockBody `json:"email,omitempty" doc:"The account contact, fixed by TIERPLAN_ACME_EMAIL"`
	AcceptTOS     *HTTPSLockBody `json:"acceptTos,omitempty" doc:"The agreement to the CA's terms, fixed by TIERPLAN_ACME_ACCEPT_TOS. It has no stored value"`
	Profile       *HTTPSLockBody `json:"profile,omitempty" doc:"The certificate profile, fixed by TIERPLAN_ACME_PROFILE"`
	ChallengeType *HTTPSLockBody `json:"challengeType,omitempty" doc:"The validation method, fixed by TIERPLAN_ACME_CHALLENGE. Value is http-01 or dns-01 and stored is the saved method"`
	DNSProvider   *HTTPSLockBody `json:"dnsProvider,omitempty" doc:"The DNS provider, fixed by TIERPLAN_DNS_PROVIDER or by a Cloudflare token variable: the provider and its token lock together. Value is the provider id and stored the saved one"`
	DNSToken      *HTTPSLockBody `json:"dnsToken,omitempty" doc:"The DNS API token, supplied by TIERPLAN_CLOUDFLARE_API_TOKEN or TIERPLAN_CLOUDFLARE_API_TOKEN_FILE, whichever var names. It is a secret: it has no value and no stored value, and a saved token is not removed or replaced while it is locked"`
	Resolvers     *HTTPSLockBody `json:"resolvers,omitempty" doc:"The propagation-check resolvers, fixed by TIERPLAN_DNS_RESOLVERS. Value and stored are the lists as host:port joined by commas; value is empty when the variable fixes the zone's own name servers"`
}

// httpsLockBody maps a lock onto the contract; nil stays nil.
func httpsLockBody(l *acme.Lock) *HTTPSLockBody {
	if l == nil {
		return nil
	}
	return &HTTPSLockBody{Var: l.Var, Value: l.Value, Stored: l.Stored}
}

// HTTPSACMEDNSBody is the DNS-01 side of the status.
type HTTPSACMEDNSBody struct {
	Provider    string                  `json:"provider" doc:"The DNS provider id the saved token is for, for example cloudflare"`
	TokenSaved  bool                    `json:"tokenSaved" doc:"True when a DNS API token is saved in the database. A token an environment variable supplies is not a saved one. The token is never returned"`
	TokenSource string                  `json:"tokenSource" enum:"env,stored,none" doc:"Which token DNS-01 uses: env when an environment variable supplies it (it is never stored), stored for the saved one, none when there is no token. The token is never returned"`
	Resolvers   []string                `json:"resolvers" doc:"The resolvers the propagation check asks, as host:port; empty means the zone's own name servers"`
	LeftOver    []HTTPSACMELeftOverBody `json:"leftOver" doc:"TXT records TierPlan created at the DNS provider and could not delete yet, oldest first; empty when none"`
}

// HTTPSACMESuggestionBody is the DNS-01 suggestion: the host and the private address it resolves to.
type HTTPSACMESuggestionBody struct {
	Host    string `json:"host" doc:"The public host"`
	Address string `json:"address" doc:"The private address it resolves to, as this server sees it"`
}

// HTTPSACMELeftOverBody is one TXT record still at the DNS provider.
type HTTPSACMELeftOverBody struct {
	Name         string     `json:"name" doc:"The record name, for example _acme-challenge.example.com"`
	RecordID     string     `json:"recordId" doc:"The DNS provider's ID for the record"`
	Code         string     `json:"code,omitempty" doc:"The class of the last delete error, for example dns_unavailable"`
	CreatedAt    time.Time  `json:"createdAt" doc:"When the record was created"`
	LastTryAt    *time.Time `json:"lastTryAt,omitempty" doc:"When a delete was last tried; left out when none was"`
	NextTryAt    *time.Time `json:"nextTryAt,omitempty" doc:"When the next delete is tried; left out while retries are paused"`
	RetryPaused  bool       `json:"retryPaused" doc:"True while no delete is tried"`
	PausedReason string     `json:"pausedReason" enum:",restore,cert_files" doc:"Why retries are paused: a backup was restored, or certificate files are set. Empty when they run"`
}

// acmeStatusBody maps the manager's status onto the contract.
func acmeStatusBody(st acme.Status, listenPort int) *HTTPSACMEBody {
	b := &HTTPSACMEBody{
		DirectoryURL: st.DirectoryURL, DirectoryOverridden: st.DirectoryOverridden, Mode: st.Mode,
		PausedReason: st.PausedReason, HasState: st.HasState, ChallengeType: st.ChallengeType, ListenPort: listenPort,
		NamesMismatch: st.NamesMismatch, Profile: st.Profile, Failures: st.Failures,
		Names:    make([]HTTPSACMENameBody, 0, len(st.Names)),
		Refusals: make([]HTTPSACMERefusalBody, 0, len(st.Refusals)),
	}
	if b.Mode == "" {
		b.Mode = acme.ModeOff
	}
	b.Locks = HTTPSLocksBody{
		ACME: httpsLockBody(st.Locks.ACME), Email: httpsLockBody(st.Locks.Email),
		AcceptTOS: httpsLockBody(st.Locks.AcceptTOS), Profile: httpsLockBody(st.Locks.Profile),
		ChallengeType: httpsLockBody(st.Locks.ChallengeType), DNSProvider: httpsLockBody(st.Locks.DNSProvider),
		DNSToken: httpsLockBody(st.Locks.DNSToken), Resolvers: httpsLockBody(st.Locks.Resolvers),
	}
	refusedHost := map[string]bool{}
	noPublicURL := false
	for _, r := range st.Refusals {
		b.Refusals = append(b.Refusals, HTTPSACMERefusalBody{Code: r.Code, Host: r.Host, Path: r.Path, Count: r.Count, Hosts: slices.Clone(r.Hosts)})
		switch r.Code {
		case acme.RefuseNamesIP, acme.RefuseAgentURLIP, acme.RefuseNamesWildcard, acme.RefuseNamesNotPublic:
			if r.Host == "" {
				noPublicURL = true
			} else {
				refusedHost[r.Host] = true
			}
		}
	}
	for _, n := range st.Names {
		b.Names = append(b.Names, HTTPSACMENameBody{Name: n, Refused: refusedHost[n]})
	}
	// The public address' host is the first name; with no public address the first name is the agent's.
	if len(st.Names) > 0 && !noPublicURL {
		b.PublicHost = st.Names[0]
	}
	if a := st.Account; a != nil {
		b.Account = &HTTPSACMEAccountBody{
			RegisteredAt: a.RegisteredAt.UTC(), Contact: strings.TrimPrefix(a.Contact, "mailto:"), TermsOfServiceURL: a.TermsOfService,
		}
	}
	if c := st.Certificate; c != nil {
		b.Certificate = &HTTPSACMECertificateBody{
			Names: append([]string{}, c.Names...), NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC(),
			IssuedAt: c.IssuedAt.UTC(), Renewed: c.Renewed,
		}
	}
	if !st.RenewAt.IsZero() {
		b.Renewal = &HTTPSACMERenewalBody{NextAt: st.RenewAt.UTC(), Basis: string(st.RenewBasis)}
	}
	if st.Fallback {
		b.Fallback = &HTTPSACMEFallbackBody{ExpiredAt: st.ExpiredAt.UTC()}
	}
	if e := st.LastError; e != nil {
		b.LastError = &HTTPSACMEErrorBody{
			Code: e.Class, At: e.At.UTC(), Host: e.Host, Profile: e.Profile, TermsURL: e.TermsURL, Detail: e.Detail,
			Zone: e.Zone, Target: e.Target, Resolvers: slices.Clone(e.Resolvers),
		}
	}
	if !st.NextAttemptAt.IsZero() {
		t := st.NextAttemptAt.UTC()
		b.NextAttemptAt = &t
	}
	if bl := st.Block; bl != nil {
		b.Blocked = &HTTPSACMEBlockBody{Until: bl.Until.UTC(), Reason: bl.Reason}
	}
	b.ActiveRun = acmeRunBody(st.ActiveRun)
	b.LastRun = acmeRunBody(st.LastRun)
	b.DNS = HTTPSACMEDNSBody{
		Provider: st.DNS.Provider, TokenSaved: st.DNS.TokenSaved, TokenSource: st.DNS.TokenSource, Resolvers: nonNil(slices.Clone(st.DNS.Resolvers)),
		LeftOver: make([]HTTPSACMELeftOverBody, 0, len(st.DNS.LeftOver)),
	}
	for _, l := range st.DNS.LeftOver {
		b.DNS.LeftOver = append(b.DNS.LeftOver, HTTPSACMELeftOverBody{
			Name: l.Name, RecordID: l.RecordID, Code: l.Class, CreatedAt: l.CreatedAt.UTC(),
			LastTryAt: utcOrNil(l.LastTryAt), NextTryAt: utcOrNil(l.NextTryAt),
			RetryPaused: l.RetryPaused, PausedReason: l.PausedReason,
		})
	}
	b.ValidationUntested = st.ValidationUntested
	if sg := st.SuggestDNS01; sg != nil {
		b.SuggestDNS01 = &HTTPSACMESuggestionBody{Host: sg.Host, Address: sg.Address}
	}
	return b
}

// challengeOrDefault is c, or HTTP-01 for a run recorded before the challenge was.
func challengeOrDefault(c string) string {
	if c == "" {
		return acme.ChallengeHTTP01
	}
	return c
}

// utcOrNil is t in UTC, or nil for the zero time.
func utcOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func acmeRunBody(r *acme.RunView) *HTTPSACMERunBody {
	if r == nil {
		return nil
	}
	b := &HTTPSACMERunBody{
		JobID: r.JobID, Kind: r.Kind, Trigger: r.Trigger, Outcome: r.Outcome, Challenge: challengeOrDefault(r.Challenge),
		StartedAt: r.StartedAt.UTC(),
	}
	if !r.FinishedAt.IsZero() {
		t := r.FinishedAt.UTC()
		b.FinishedAt = &t
	}
	return b
}

// RegisterACMEAccountInput is the body of the registration.
type RegisterACMEAccountInput struct {
	// UserAgent is the browser's, recorded in the audit row. The hidden header leaves the contract unchanged.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Email    string `json:"email,omitempty" maxLength:"254" doc:"Where the CA may send expiry and account notices. Optional: without one no contact is sent"`
		AgreeTOS bool   `json:"agreeTos,omitempty" doc:"Must be true: the person registering agrees to the CA's terms of service"`
	}
}

// RegisterACMEAccountOutput is the registered account.
type RegisterACMEAccountOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         HTTPSACMEAccountBody
}

// HTTPSACMEProfileBody is one certificate profile the CA offers.
type HTTPSACMEProfileBody struct {
	Name        string `json:"name" doc:"The profile name"`
	Description string `json:"description" doc:"What the CA says about it. Untrusted text: show it as text only"`
}

// HTTPSACMEDirectoryBody is what the CA says about itself.
type HTTPSACMEDirectoryBody struct {
	URL               string                 `json:"url" doc:"The CA directory in use"`
	Overridden        bool                   `json:"overridden" doc:"True when the test-only directory override is set"`
	TermsOfServiceURL string                 `json:"termsOfServiceUrl" doc:"Where the terms of service can be read, empty when the CA names none"`
	Profiles          []HTTPSACMEProfileBody `json:"profiles" doc:"The certificate profiles the CA offers, sorted by name; empty when it offers none"`
}

// HTTPSACMEDirectoryOutput wraps HTTPSACMEDirectoryBody for huma.
type HTTPSACMEDirectoryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         HTTPSACMEDirectoryBody
}

// PutACMEProfileInput is the body of the profile save.
type PutACMEProfileInput struct {
	// UserAgent is the browser's, recorded in the audit row. The hidden header leaves the contract unchanged.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Profile string `json:"profile" maxLength:"64" pattern:"^[a-z0-9._-]*$" doc:"The profile to order with; empty is the CA's default"`
	}
}

// HTTPSACMEProfileOutput is the saved profile.
type HTTPSACMEProfileOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Profile string `json:"profile" doc:"The profile orders ask for now; empty is the CA's default"`
	}
}

// registerHTTPSACME wires the Let's Encrypt setup routes. Reads need settings.read and every change needs
// settings.write (D-10); no new permission exists.
func registerHTTPSACME(r *httpx.Router, d HTTPSDeps) {
	read := rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}
	write := rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}

	httpx.Register(r, huma.Operation{
		OperationID:   "registerACMEAccount",
		Method:        http.MethodPost,
		Path:          "/api/https/acme/account",
		DefaultStatus: http.StatusCreated,
		Summary:       "Register the Let's Encrypt account",
		Description: "Registers TierPlan with the certificate authority, agreeing to its terms of service. The email is optional. " +
			"The account key is created and sealed on the server and never returned. 422 " + problemACMETOSRequired + " when agreeTos is not true; " +
			"422 " + problemACMEContactInvalid + " (field body.email) for an address the CA or TierPlan refuses; 409 " + problemACMEAccountExists +
			" when one is registered already; 409 " + problemACMEPaused + " after a restore, until it is resumed; 409 " + problemACMECertFiles +
			" while certificate files are set; 502 " + problemACMECAError + " when the CA fails, with its class in the first error's message. " +
			"While TIERPLAN_ACME_EMAIL is set it names the address: none typed uses it, the same address is accepted, and any other is refused with 409 " + problemACMEEmailLocked +
			". While TIERPLAN_ACME_ACCEPT_TOS is set a request that does not agree to the terms is refused with 409 " + problemACMETOSLocked +
			". A refused registration creates no account, and an existing account keeps its contact. Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *RegisterACMEAccountInput) (*RegisterACMEAccountOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		acct, err := d.ACME.Register(ctx, strings.TrimSpace(in.Body.Email), in.Body.AgreeTOS)
		var problem error
		var details map[string]string
		if err != nil {
			// A refused registration records no details at all: the address typed, which the CA or TierPlan may
			// have refused, is never stored (T-17-10).
			problem = acmeProblem(d.Log, "register the Let's Encrypt account", err)
		} else {
			details = d.registeredDetails(ctx, strings.TrimPrefix(acct.Contact, "mailto:"))
		}
		if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSAccountRegistered, problem, details); aerr != nil {
			return nil, auditFailed(d, aerr)
		}
		if problem != nil {
			return nil, problem
		}
		return &RegisterACMEAccountOutput{CacheControl: "no-store", Body: HTTPSACMEAccountBody{
			RegisteredAt: acct.CreatedAt.UTC(), Contact: strings.TrimPrefix(acct.Contact, "mailto:"), TermsOfServiceURL: acct.TermsOfService,
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getACMEDirectory",
		Method:      http.MethodGet,
		Path:        "/api/https/acme/directory",
		Summary:     "Read the CA's terms and profiles",
		Description: "The certificate authority's terms of service URL and the certificate profiles it offers, read from its directory " +
			"(kept for a day). 409 " + problemACMEPaused + " after a restore, 409 " + problemACMECertFiles + " while certificate files are set, " +
			"502 " + problemACMECAError + " when the directory cannot be read. Needs settings.read.",
		Tags: []string{"https"},
	}, read, func(ctx context.Context, _ *struct{}) (*HTTPSACMEDirectoryOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		st, err := d.ACME.Status(ctx)
		if err != nil {
			return nil, acmeInternal(d.Log, "read the Let's Encrypt status", err)
		}
		if st.CertFiles {
			return nil, acmeProblem(d.Log, "read the directory", acme.ErrCertFiles)
		}
		meta, err := d.ACME.Directory(ctx)
		if err != nil {
			return nil, acmeProblem(d.Log, "read the directory", err)
		}
		out := &HTTPSACMEDirectoryOutput{CacheControl: "no-store", Body: HTTPSACMEDirectoryBody{
			URL: st.DirectoryURL, Overridden: st.DirectoryOverridden, TermsOfServiceURL: meta.TermsOfService,
			Profiles: make([]HTTPSACMEProfileBody, 0, len(meta.Profiles)),
		}}
		for name, desc := range meta.Profiles {
			out.Body.Profiles = append(out.Body.Profiles, HTTPSACMEProfileBody{Name: name, Description: desc})
		}
		slices.SortFunc(out.Body.Profiles, func(a, b HTTPSACMEProfileBody) int { return strings.Compare(a.Name, b.Name) })
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "putACMEProfile",
		Method:      http.MethodPut,
		Path:        "/api/https/acme/profile",
		Summary:     "Choose the certificate profile",
		Description: "Saves the profile later orders ask for; an empty name is the CA's default. A name the CA does not offer is refused with 422 " +
			problemACMEProfileUnknown + " when its directory can be read, and kept as given when it cannot. 409 " + problemACMERunActive +
			" while a run is in progress, 409 " + problemACMEPaused + " after a restore, 409 " + problemACMECertFiles + " while certificate files are set. " +
			"While TIERPLAN_ACME_PROFILE is set it decides: its own value is accepted and stores nothing, and any other value is refused with 409 " +
			problemACMEProfileLocked + ". Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *PutACMEProfileInput) (*HTTPSACMEProfileOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		// The profile in use now is read first: a save of the same value, or an echo of TIERPLAN_ACME_PROFILE (which
		// Status reports as the profile), is not a change and writes no row (D-14).
		before, err := d.ACME.Status(ctx)
		if err != nil {
			return nil, acmeInternal(d.Log, "read the Let's Encrypt status", err)
		}
		if err := d.ACME.SetProfile(ctx, in.Body.Profile); err != nil {
			problem := acmeProblem(d.Log, "save the Let's Encrypt profile", err)
			if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSSettingsUpdated, problem, nil); aerr != nil {
				return nil, auditFailed(d, aerr)
			}
			return nil, problem
		}
		if before.Profile != in.Body.Profile {
			details := map[string]string{
				"section": sectionProfile, "fields": "profile", "profile_from": before.Profile, "profile_to": in.Body.Profile,
			}
			if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSSettingsUpdated, nil, details); aerr != nil {
				return nil, auditFailed(d, aerr)
			}
		}
		out := &HTTPSACMEProfileOutput{CacheControl: "no-store"}
		out.Body.Profile = in.Body.Profile
		return out, nil
	})
}

// acmeUnavailable answers a Let's Encrypt route on a server built without the manager.
func acmeUnavailable() error {
	return httpx.Problem(http.StatusServiceUnavailable, problemHTTPSUnavailable, "Let's Encrypt is not available on this server.")
}

// acmeInternal logs an error nobody can act on from the page and answers a plain 500.
func acmeInternal(log *slog.Logger, what string, err error) error {
	if log != nil {
		log.Error("a Let's Encrypt request failed", "step", what, "error", err)
	}
	return huma.Error500InternalServerError("internal error")
}

// fieldProblem is a 422 whose type is the stable code and whose first error names the field.
func fieldProblem(code, location, detail string) error {
	return fieldValueProblem(code, location, detail, nil)
}

// fieldValueProblem is fieldProblem whose first error also carries the value that is wrong. Never pass
// a secret as the value.
func fieldValueProblem(code, location, detail string, value any) error {
	return &huma.ErrorModel{
		Type: code, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity, Detail: detail,
		Errors: []*huma.ErrorDetail{{Location: location, Message: code, Value: value}},
	}
}

// acmeProblem maps an error from the Let's Encrypt manager to the problem the page reads. Anything the
// manager does not name is classified: a CA failure is 502 acme_ca_error with its class in the first
// error, and an internal failure is logged and answered as 500.
func acmeProblem(log *slog.Logger, what string, err error) error {
	if p := runProblem(err); p != nil {
		return p
	}
	var resolverErr *dnscheck.ResolverError
	var locked *acme.ErrLocked
	switch {
	case errors.As(err, &locked):
		// The variable's name, never its value (a token's least of all).
		return httpx.Problem(http.StatusConflict, locked.Problem, "Set by the "+locked.Var+" environment variable.")
	case errors.Is(err, acme.ErrPaused):
		return httpx.Problem(http.StatusConflict, problemACMEPaused, "Let's Encrypt is paused after a restore. Resume it first.")
	case errors.Is(err, acme.ErrHSTSOn):
		return httpx.Problem(http.StatusConflict, problemHSTSOn, "HSTS is on. Turn it off first.")
	case errors.Is(err, acme.ErrCertFiles):
		return httpx.Problem(http.StatusConflict, problemACMECertFiles, "Certificate files are set, so Let's Encrypt is turned off.")
	case errors.Is(err, acme.ErrAccountExists):
		return httpx.Problem(http.StatusConflict, problemACMEAccountExists, "A Let's Encrypt account is already registered.")
	case errors.Is(err, acme.ErrRunActive):
		return httpx.Problem(http.StatusConflict, problemACMERunActive, "A certificate run is in progress. Wait for it to end.")
	case errors.Is(err, acme.ErrTOSNotAccepted):
		return fieldProblem(problemACMETOSRequired, "body.agreeTos", "Agree to the certificate authority's terms of service to register.")
	case errors.Is(err, acme.ErrContactInvalid):
		return fieldProblem(problemACMEContactInvalid, "body.email", "Enter an email address such as you@example.com, or leave it empty.")
	case errors.Is(err, acme.ErrProfileUnknown):
		return fieldProblem(problemACMEProfileUnknown, "body.profile", "The certificate authority does not offer that profile.")
	case errors.Is(err, acme.ErrDNSTokenRequired):
		return fieldProblem(problemACMEDNSTokenRequired, "body.token", "Enter a DNS API token. DNS-01 cannot be used without one.")
	case errors.Is(err, acme.ErrDNSTokenInUse):
		return httpx.Problem(http.StatusConflict, problemACMEDNSTokenInUse, "DNS-01 is the saved validation method, so its token cannot be removed. Switch to HTTP-01 first.")
	case errors.Is(err, acme.ErrChallengeUnknown):
		return fieldProblem(problemACMEChallengeUnknown, "body.challengeType", "The validation method must be http-01 or dns-01.")
	case errors.As(err, &resolverErr):
		return fieldValueProblem(problemACMEDNSResolverBad, "body.resolvers", resolverDetail(resolverErr), resolverErr.Entry)
	}
	f := acme.Classify(err, acme.ClassifyInput{})
	switch f.Class {
	case acme.ClassContactInvalid:
		return fieldProblem(problemACMEContactInvalid, "body.email", "The certificate authority did not accept that email address.")
	case acme.ClassInternal:
		return acmeInternal(log, what, err)
	}
	detail := "The certificate authority could not be used: " + f.Class + "."
	if f.Detail != "" {
		detail = f.Detail
	}
	return &huma.ErrorModel{
		Type: problemACMECAError, Title: http.StatusText(http.StatusBadGateway), Status: http.StatusBadGateway, Detail: detail,
		Errors: []*huma.ErrorDetail{{Location: "class", Message: f.Class}},
	}
}
