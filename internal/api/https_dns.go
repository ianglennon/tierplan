// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The DNS-01 routes: save the validation method and token, test a token, remove the token. None of them
// ever returns the token, and every answer is no-store.
//
// The token field carries no maxLength in the schema on purpose: a schema violation is answered with the
// offending value, which would echo a secret. The manager refuses a token over 256 bytes as
// dns_token_invalid before any request is made, and the server's body limit bounds the request.

// The sections of a https.settings_updated row (Phase 17, D-13).
const (
	sectionValidation    = "validation"
	sectionDNSCredential = "dns_credential" //nolint:gosec // G101: the name of an audit section, not a credential
	sectionProfile       = "profile"
	sectionHSTS          = "hsts"
)

// reasonTokenTestFailed is the reason of a row whose token did not pass its test: not a problem type, because the
// request itself was answered 200 with the per-name test.
const reasonTokenTestFailed = "token_test_failed"

// errTokenTestFailed is the refusal a row records for a token that failed its test.
var errTokenTestFailed error = &huma.ErrorModel{Type: reasonTokenTestFailed, Status: http.StatusUnprocessableEntity}

// failedClasses are the distinct classes of the names that did not pass, sorted and comma-joined. The classes are
// the classifier's own closed vocabulary; nothing the provider said is in them.
func failedClasses(t acme.TokenTest) string {
	var classes []string
	for _, n := range t.Names {
		class := ""
		switch {
		case n.Failure != nil:
			class = n.Failure.Class
		default:
			for _, s := range n.Steps {
				if s.State == acme.TestFailed && s.Class != "" {
					class = s.Class
					break
				}
			}
		}
		if class != "" && !slices.Contains(classes, class) {
			classes = append(classes, class)
		}
	}
	slices.Sort(classes)
	return strings.Join(classes, ",")
}

// validationDetails is the details of the one row a saved validation settings form writes, nil when the save
// changed nothing. The fields are listed in the fixed order challenge, resolvers, dns_credential and only a field
// that changed has its values; the token has none, only whether it was set or replaced.
func validationDetails(c acme.ValidationChanges) map[string]string {
	var fields []string
	details := map[string]string{}
	if c.ChallengeChanged() {
		fields = append(fields, "challenge")
		details["challenge_from"], details["challenge_to"] = c.ChallengeFrom, c.ChallengeTo
	}
	if c.ResolversChanged() {
		fields = append(fields, "resolvers")
		details["resolvers_from"], details["resolvers_to"] = strings.Join(c.ResolversFrom, ","), strings.Join(c.ResolversTo, ",")
	}
	if c.Credential != "" {
		fields = append(fields, "dns_credential")
		details["dns_credential"] = c.Credential
	}
	if len(fields) == 0 {
		return nil
	}
	details["fields"] = strings.Join(fields, ",")
	details["section"] = sectionValidation
	if !c.ChallengeChanged() && !c.ResolversChanged() {
		details["section"] = sectionDNSCredential
	}
	return details
}

// dnsTestedDetails are the facts of one Test token press: the provider, how many names were tested and passed, the
// classes of any failures, and whose token it was (typed, saved or env). The token is not among them.
func dnsTestedDetails(t acme.TokenTest, dnsView acme.DNSView, typed bool) map[string]string {
	passed := 0
	for _, n := range t.Names {
		if n.Passed() {
			passed++
		}
	}
	source := "saved"
	switch {
	case typed:
		source = "typed"
	case dnsView.TokenSource == acme.TokenSourceEnv:
		source = "env"
	}
	details := map[string]string{
		"provider": dnsView.Provider, "names": strconv.Itoa(len(t.Names)), "passed": strconv.Itoa(passed), "credential_source": source,
	}
	if classes := failedClasses(t); classes != "" {
		details["failed_classes"] = classes
	}
	return details
}

// HTTPSACMEDNSErrorBody is the facts of one failed DNS step. It carries data only; the browser writes
// the words.
type HTTPSACMEDNSErrorBody struct {
	Code      string     `json:"code" doc:"The class of the failure, one of the codes of lastError in the status"`
	Host      string     `json:"host,omitempty" doc:"The certificate name the failure is about"`
	Zone      string     `json:"zone,omitempty" doc:"The DNS zone the failure is about"`
	Target    string     `json:"target,omitempty" doc:"The name the challenge name is delegated to, when it is"`
	Resolvers []string   `json:"resolvers,omitempty" doc:"The resolvers that were asked"`
	RetryAt   *time.Time `json:"retryAt,omitempty" doc:"When to try again, for a rate limit that said when"`
}

// HTTPSACMETestStepBody is one row of a name's token test.
type HTTPSACMETestStepBody struct {
	ID    string `json:"id" enum:"zone,created,read,deleted" doc:"The row: the zone was found, a test record was created, read back and deleted"`
	State string `json:"state" enum:"done,failed,skipped" doc:"Where the row ended. skipped is a row that was not reached because an earlier one failed"`
	Code  string `json:"code,omitempty" doc:"The class of a failure, for example dns_token_invalid"`
}

// HTTPSACMETestInfoBody is the row that never changes the verdict: whether the resolvers showed the test record.
type HTTPSACMETestInfoBody struct {
	State     string   `json:"state" enum:"seen,not_seen,not_checked" doc:"Every resolver showed the record, some had not when the wait ended, or it was never looked for"`
	Resolvers []string `json:"resolvers" doc:"The resolvers that showed it (seen) or had not shown it (not_seen)"`
	Seconds   int      `json:"seconds" doc:"How long the wait took, in seconds"`
}

// HTTPSACMENameTestBody is the token test of one certificate name.
type HTTPSACMENameTestBody struct {
	Name        string                  `json:"name" doc:"The certificate name tested"`
	Record      string                  `json:"record" doc:"The TXT record name the test wrote at, or would have"`
	Zone        string                  `json:"zone" doc:"The DNS provider zone that record is in; empty when it was not found"`
	DelegatedTo string                  `json:"delegatedTo,omitempty" doc:"The name the challenge name is a CNAME to, when it is"`
	OwnZone     string                  `json:"ownZone,omitempty" doc:"Set when the name's own zone is not at the provider and the challenge name is delegated into one that is: the zone that is not there"`
	Steps       []HTTPSACMETestStepBody `json:"steps" doc:"The four rows, in order: zone, created, read, deleted"`
	Info        HTTPSACMETestInfoBody   `json:"info" doc:"Whether the resolvers showed the test record. It never changes the verdict"`
	Error       *HTTPSACMEDNSErrorBody  `json:"error,omitempty" doc:"The first row that failed, classified"`
}

// HTTPSACMETokenTestBody is the result of testing a token against the certificate names. It carries no token.
type HTTPSACMETokenTestBody struct {
	Passed bool                    `json:"passed" doc:"True when every name passed its four rows. The info row has no say"`
	Names  []HTTPSACMENameTestBody `json:"names" doc:"The names tested, in certificate-name order"`
}

// tokenTestBody maps a token test onto the contract.
func tokenTestBody(t acme.TokenTest) *HTTPSACMETokenTestBody {
	b := &HTTPSACMETokenTestBody{Passed: t.Passed, Names: make([]HTTPSACMENameTestBody, 0, len(t.Names))}
	for _, n := range t.Names {
		nb := HTTPSACMENameTestBody{
			Name: n.Name, Record: n.Record, Zone: n.Zone, DelegatedTo: n.DelegatedTo, OwnZone: n.OwnZone,
			Steps: make([]HTTPSACMETestStepBody, 0, len(n.Steps)),
			Info:  HTTPSACMETestInfoBody{State: n.Info.State, Resolvers: nonNil(n.Info.Resolvers), Seconds: n.Info.Seconds},
		}
		for _, s := range n.Steps {
			nb.Steps = append(nb.Steps, HTTPSACMETestStepBody{ID: s.ID, State: s.State, Code: s.Class})
		}
		if f := n.Failure; f != nil {
			nb.Error = &HTTPSACMEDNSErrorBody{Code: f.Class, Host: f.Host, Zone: f.Zone, Target: f.Target, Resolvers: f.Resolvers}
			if f.RetryAfter > 0 {
				at := time.Now().Add(f.RetryAfter).UTC()
				nb.Error.RetryAt = &at
			}
		}
		b.Names = append(b.Names, nb)
	}
	return b
}

// resolverDetail is the sentence for a resolver entry that was refused.
func resolverDetail(e *dnscheck.ResolverError) string {
	if e.Reason == dnscheck.ReasonTooMany {
		return "List at most " + strconv.Itoa(dnscheck.MaxResolvers) + " resolvers."
	}
	return "A resolver is a host name or IP address, optionally followed by :port."
}

// PutACMEValidationInput is the body of the validation save.
type PutACMEValidationInput struct {
	// UserAgent is the browser's, recorded in the audit row. The hidden header leaves the contract unchanged.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		ChallengeType string   `json:"challengeType" enum:"http-01,dns-01" doc:"How Let's Encrypt is shown you control the names: http-01 over port 80, or dns-01 with a TXT record at your DNS provider"`
		Token         string   `json:"token,omitempty" doc:"A new DNS API token, at most 256 printable characters with no space; anything else fails its test as dns_token_invalid with no request made. It is tested against the certificate names first and stored sealed only when every name passes. Leave out to keep the saved token"`
		Resolvers     []string `json:"resolvers,omitempty" maxItems:"32" maxLength:"300" doc:"The resolvers the propagation check asks, as host or host:port; an empty list means the zone's own name servers. Leave out to keep the saved list"`
	}
}

// HTTPSACMEValidationOutput is the result of the validation save.
type HTTPSACMEValidationOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Saved         bool                    `json:"saved" doc:"True when the settings were stored. False when the new token failed its test: nothing was stored"`
		Test          *HTTPSACMETokenTestBody `json:"test,omitempty" doc:"The token test the save ran; left out when no new token was given"`
		ChallengeType string                  `json:"challengeType" doc:"The validation method in use now: TIERPLAN_ACME_CHALLENGE when it is set, else the saved one"`
		TokenSaved    bool                    `json:"tokenSaved" doc:"True when a DNS API token is saved now. The token is never returned"`
		Resolvers     []string                `json:"resolvers" doc:"The resolvers saved now"`
	}
}

// TestACMEDNSTokenInput is the body of the token test.
type TestACMEDNSTokenInput struct {
	// UserAgent is the browser's, recorded in the audit row. The hidden header leaves the contract unchanged.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Token     string   `json:"token,omitempty" doc:"The token to test, at most 256 printable characters with no space; anything else fails as dns_token_invalid with no request made. Leave out to test the saved token"`
		Resolvers []string `json:"resolvers,omitempty" maxItems:"32" maxLength:"300" doc:"The resolvers to ask, as host or host:port. Leave out to use the saved list"`
	}
}

// DeleteACMEDNSTokenInput is the token removal. It has no body; the hidden header is for the audit row.
type DeleteACMEDNSTokenInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// HTTPSACMETokenTestOutput is the result of a token test.
type HTTPSACMETokenTestOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         HTTPSACMETokenTestBody
}

// registerHTTPSACMEDNS wires the DNS-01 routes. All three need settings.write at global scope.
func registerHTTPSACMEDNS(r *httpx.Router, d HTTPSDeps) {
	write := rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}

	httpx.Register(r, huma.Operation{
		OperationID: "putACMEValidation",
		Method:      http.MethodPut,
		Path:        "/api/https/acme/validation",
		Summary:     "Choose how the names are validated",
		Description: "Saves the validation method (http-01 or dns-01), the DNS resolvers and, when one is given, the DNS API token. A new token is " +
			"tested against the certificate names first, with no order placed and the CA not contacted; when any name fails, nothing at all is " +
			"stored and the answer is 200 with saved false and the per-name test. The token is stored sealed and never returned. 422 " +
			problemACMEDNSTokenRequired + " (field body.token) when DNS-01 is chosen and no token is given or saved; 422 " +
			problemACMEDNSResolverBad + " (field body.resolvers, the entry as the value) for a resolver that is not a host or host:port or a list " +
			"of more than five; 422 " + problemACMEChallengeUnknown + "; 422 acme_refused when no certificate could be ordered for the names; " +
			"409 " + problemACMERunActive + " while a run or a token test holds the DNS slot; 409 " + problemACMEPaused + " after a restore; 409 " +
			problemACMECertFiles + " while certificate files are set. While TIERPLAN_ACME_CHALLENGE is set it decides: its own method is accepted and the " +
			"save keeps the saved method, and any other method is refused with 409 " + problemACMEChallengeLocked + ", storing nothing. A typed token while " +
			"TIERPLAN_DNS_PROVIDER or a Cloudflare token variable is set is refused with 409 " + problemACMEDNSTokenLocked + ": the provider and its token lock " +
			"together (locks.dnsProvider and locks.dnsToken), and a change of provider would be refused with 409 " + problemACMEDNSProviderLocked +
			", though no route offers one. With the environment's token alone DNS-01 needs no saved one. While " +
			"TIERPLAN_DNS_RESOLVERS is set the same list in the same order is accepted and the save keeps the saved list, and any other list is refused with 409 " +
			problemACMEResolversLocked + "; every lock is checked before anything is stored, so a refused save stores nothing. Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *PutACMEValidationInput) (*HTTPSACMEValidationOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		var resolvers *[]string
		if in.Body.Resolvers != nil {
			resolvers = &in.Body.Resolvers
		}
		res, err := d.ACME.SaveValidation(ctx, acme.ValidationInput{
			ChallengeType: in.Body.ChallengeType, Token: in.Body.Token, Resolvers: resolvers,
		})
		if err != nil {
			// A refusal records the problem type and nothing else: a 422 can carry the typed resolver entry as its value.
			problem := acmeProblem(d.Log, "save the validation settings", err)
			if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSSettingsUpdated, problem, nil); aerr != nil {
				return nil, auditFailed(d, aerr)
			}
			return nil, problem
		}
		var row error
		var details map[string]string
		switch {
		case !res.Saved && res.Test != nil:
			row = errTokenTestFailed
			// No dns_credential value: nothing was set, replaced or removed, and the audit list labels a
			// dns_credential row without one as a failed test rather than a change (review 17 IN-03).
			details = map[string]string{"section": sectionDNSCredential}
			if classes := failedClasses(*res.Test); classes != "" {
				details["failed_classes"] = classes
			}
		case res.Saved:
			details = validationDetails(res.Changes)
		}
		if details != nil {
			if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSSettingsUpdated, row, details); aerr != nil {
				return nil, auditFailed(d, aerr)
			}
		}
		st, err := d.ACME.Status(ctx)
		if err != nil {
			return nil, acmeInternal(d.Log, "read the Let's Encrypt status", err)
		}
		out := &HTTPSACMEValidationOutput{CacheControl: "no-store"}
		out.Body.Saved = res.Saved
		if res.Test != nil {
			out.Body.Test = tokenTestBody(*res.Test)
		}
		out.Body.ChallengeType = st.ChallengeType
		out.Body.TokenSaved = st.DNS.TokenSaved
		out.Body.Resolvers = nonNil(st.DNS.Resolvers)
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testACMEDNSToken",
		Method:      http.MethodPost,
		Path:        "/api/https/acme/dns/test",
		Summary:     "Test a DNS API token",
		Description: "Tests the typed token, or the saved one when none is typed, against the names a certificate would carry now: for each name it " +
			"finds the zone, creates a test TXT record, reads it back, notes whether the resolvers show it and deletes it. No order is placed " +
			"and the CA is not contacted. Nothing is stored. A failing token is 200 with passed false and the classified error of each failing " +
			"name. 422 " + problemACMEDNSTokenRequired + " (field body.token) when none is typed or saved; 422 " + problemACMEDNSResolverBad +
			" (field body.resolvers, the entry as the value); 422 acme_refused when no certificate could be ordered for the names; 409 " +
			problemACMERunActive + " while a run or another test holds the DNS slot; 409 " + problemACMEPaused + " after a restore; 409 " +
			problemACMECertFiles + " while certificate files are set. With no token typed it tests the token DNS-01 uses: the environment's when a " +
			"variable supplies one (it is held in memory, never stored or returned), else the saved one. A typed token while a variable supplies the token is " +
			"refused with 409 " + problemACMEDNSTokenLocked + ", and so is a list other than TIERPLAN_DNS_RESOLVERS's with 409 " + problemACMEResolversLocked +
			" (the variable's list answers the test). A token the provider refuses is a 200 with the classified error of each name, never a failure of the page. Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *TestACMEDNSTokenInput) (*HTTPSACMETokenTestOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		var resolvers *[]string
		if in.Body.Resolvers != nil {
			resolvers = &in.Body.Resolvers
		}
		res, err := d.ACME.TestToken(ctx, acme.TestRequest{Token: in.Body.Token, Resolvers: resolvers})
		if err != nil {
			// A refused test (a lock, a bad resolver, a run in progress, a pause) records the problem type alone.
			problem := acmeProblem(d.Log, "test the DNS token", err)
			if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSDNSTested, problem, nil); aerr != nil {
				return nil, auditFailed(d, aerr)
			}
			return nil, problem
		}
		st, err := d.ACME.Status(ctx)
		if err != nil {
			return nil, acmeInternal(d.Log, "read the Let's Encrypt status", err)
		}
		var row error
		if !res.Passed {
			row = errTokenTestFailed
		}
		if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSDNSTested, row, dnsTestedDetails(res, st.DNS, strings.TrimSpace(in.Body.Token) != "")); aerr != nil {
			return nil, auditFailed(d, aerr)
		}
		return &HTTPSACMETokenTestOutput{CacheControl: "no-store", Body: *tokenTestBody(res)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteACMEDNSToken",
		Method:        http.MethodDelete,
		Path:          "/api/https/acme/dns/token",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Remove the saved DNS API token",
		Description: "Deletes the saved token. 409 " + problemACMEDNSTokenInUse + " while DNS-01 is the saved validation method, because renewals " +
			"would stop; 409 " + problemACMEDNSTokenLocked + " while an environment variable supplies the token; 409 " + problemACMERunActive +
			" while a run or a token test holds the DNS slot; 409 " + problemACMEPaused + " after a restore; 409 " + problemACMECertFiles +
			" while certificate files are set. Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *DeleteACMEDNSTokenInput) (*HTTPSACMENoContentOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		// Whether a token was saved is read first: removing none is not a change and writes no row.
		before, err := d.ACME.Status(ctx)
		if err != nil {
			return nil, acmeInternal(d.Log, "read the Let's Encrypt status", err)
		}
		if err := d.ACME.RemoveToken(ctx); err != nil {
			problem := acmeProblem(d.Log, "remove the DNS token", err)
			if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSSettingsUpdated, problem, nil); aerr != nil {
				return nil, auditFailed(d, aerr)
			}
			return nil, problem
		}
		if before.DNS.TokenSaved {
			details := map[string]string{"section": sectionDNSCredential, "fields": "dns_credential", "dns_credential": "removed"}
			if aerr := d.audit(ctx, in.UserAgent, audit.ActionHTTPSSettingsUpdated, nil, details); aerr != nil {
				return nil, auditFailed(d, aerr)
			}
		}
		return &HTTPSACMENoContentOutput{CacheControl: "no-store"}, nil
	})
}

// HTTPSACMENoContentOutput is an answer with no body that must still not be cached.
type HTTPSACMENoContentOutput struct {
	CacheControl string `header:"Cache-Control"`
}
