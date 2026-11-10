// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"time"
	"unicode/utf8"

	legoacme "github.com/go-acme/lego/v5/acme"

	"github.com/ianglennon/tierplan/internal/jobs"
)

// MaxDetailBytes caps the raw problem document kept for the Details disclosure.
const MaxDetailBytes = 8 << 10

// The twelve classes an ACME failure maps to. The values are stored and sent to the browser, so
// they are a contract; the English copy and the fix each one gets live in the API and the SPA.
const (
	ClassRateLimited           = "rate_limited"
	ClassValidationUnreachable = "validation_unreachable"
	ClassValidationDNS         = "validation_dns"
	ClassCAAForbids            = "caa_forbids"
	ClassIdentifierRejected    = "identifier_rejected"
	ClassContactInvalid        = "contact_invalid"
	ClassTOSActionRequired     = "tos_action_required"
	ClassAccountGone           = "account_gone"
	ClassProfileInvalid        = "profile_invalid"
	ClassCAUnavailable         = "ca_unavailable"
	ClassCAUntrusted           = "ca_untrusted"
	ClassInternal              = "internal"
)

// timeoutErrorType is the problem type some CAs return when a validation attempt timed out. lego
// has no constant for it.
const timeoutErrorType = "urn:ietf:params:acme:error:timeout"

// ChallengeError is a failed authorization for one host. The order code wraps the CA's problem in
// it so Classify can name the host the copy talks about.
type ChallengeError struct {
	Host string
	Err  error
}

func (e *ChallengeError) Error() string {
	if e.Err == nil {
		return "challenge for " + e.Host + " failed"
	}
	return "challenge for " + e.Host + ": " + e.Err.Error()
}

func (e *ChallengeError) Unwrap() error { return e.Err }

// ClassifyInput is what Classify knows about the order that failed and the CA's answer does not say.
type ClassifyInput struct {
	ChallengeType, Profile string
}

// Failure is one classified failure: the class, the values its copy needs, and the raw document
// for the Details disclosure. It carries data only.
type Failure struct {
	Class string
	// Host is the host a validation failure is about.
	Host string
	// Profile is the profile the CA refused.
	Profile string
	// TermsURL is where the CA says the terms can be read (the problem's instance).
	TermsURL string
	// RetryAfter is the CA's Retry-After for a rate limit, zero when it gave none.
	RetryAfter time.Duration
	// Detail is the raw problem document, free of control characters and at most MaxDetailBytes.
	Detail string
}

// Classify maps any error an ACME order can return to one Failure. It never returns an empty class.
func Classify(err error, in ClassifyInput) Failure {
	f := Failure{Class: ClassInternal, Detail: cleanDetail(describe(err))}
	if err == nil {
		return f
	}

	var ce *ChallengeError
	if errors.As(err, &ce) {
		f.Host = ce.Host
	}

	var rl *legoacme.RateLimitedError
	if errors.As(err, &rl) {
		f.Class = ClassRateLimited
		f.RetryAfter = rl.RetryAfter
		return f
	}

	var pd *legoacme.ProblemDetails
	if errors.As(err, &pd) && pd != nil {
		class, host := classifyProblem(pd)
		if class == "" {
			if pd.HTTPStatus >= 500 {
				class = ClassCAUnavailable
			} else {
				class = ClassInternal
			}
		}
		f.Class = class
		if f.Host == "" {
			f.Host = host
		}
		switch class {
		case ClassTOSActionRequired:
			f.TermsURL = pd.Instance
		case ClassProfileInvalid:
			f.Profile = in.Profile
		}
		return f
	}

	// Not a CA problem document: the transport or the trust store failed before the CA answered.
	// Trust is checked first because a verification error can arrive wrapped in a network error.
	var unknown *x509.UnknownAuthorityError
	var verify *tls.CertificateVerificationError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknown), errors.As(err, &verify), errors.As(err, &hostname), errors.As(err, &invalid):
		f.Class = ClassCAUntrusted
	case isNetworkFailure(err):
		f.Class = ClassCAUnavailable
	}
	return f
}

func isNetworkFailure(err error) bool {
	var op *net.OpError
	var dns *net.DNSError
	return errors.As(err, &op) || errors.As(err, &dns) || errors.Is(err, context.DeadlineExceeded)
}

// classifyProblem classifies a problem by its type, and a generic one (compound, or a type with
// no class of its own) by its first sub-problem that has one. It returns "" when nothing matches.
func classifyProblem(pd *legoacme.ProblemDetails) (class, host string) {
	if c := classifyType(pd.Type); c != "" {
		return c, ""
	}
	for _, sp := range pd.SubProblems {
		if c := classifyType(sp.Type); c != "" {
			return c, sp.Identifier.Value
		}
	}
	return "", ""
}

func classifyType(typ string) string {
	switch typ {
	case legoacme.RateLimitedErrorType:
		return ClassRateLimited
	case legoacme.ConnectionErrorType, legoacme.IncorrectResponseErrorType,
		legoacme.UnauthorizedErrorType, legoacme.TLSErrorType, timeoutErrorType:
		return ClassValidationUnreachable
	case legoacme.DNSErrorType:
		return ClassValidationDNS
	case legoacme.CaaErrorType:
		return ClassCAAForbids
	case legoacme.RejectedIdentifierErrorType, legoacme.UnsupportedIdentifierErrorType:
		return ClassIdentifierRejected
	case legoacme.InvalidContactErrorType, legoacme.UnsupportedContactErrorType:
		return ClassContactInvalid
	case legoacme.UserActionRequiredErrorType:
		return ClassTOSActionRequired
	case legoacme.AccountDoesNotExistErrorType:
		return ClassAccountGone
	case legoacme.InvalidProfileErrorType:
		return ClassProfileInvalid
	case legoacme.ServerInternalErrorType:
		return ClassCAUnavailable
	case legoacme.BadCSRErrorType, legoacme.OrderNotReadyErrorType, legoacme.MalformedErrorType:
		return ClassInternal
	}
	return ""
}

// problemDoc is the part of a problem document the Details disclosure shows.
type problemDoc struct {
	Type        string       `json:"type,omitempty"`
	Detail      string       `json:"detail,omitempty"`
	Status      int          `json:"status,omitempty"`
	Instance    string       `json:"instance,omitempty"`
	SubProblems []subProblem `json:"subproblems,omitempty"`
}

type subProblem struct {
	Type       string `json:"type,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Identifier string `json:"identifier,omitempty"`
}

// describe is the raw text behind a failure: the problem document as JSON when the CA sent one,
// otherwise the error's own text. Every CA-supplied string is cleaned before it is encoded.
func describe(err error) string {
	if err == nil {
		return ""
	}
	var pd *legoacme.ProblemDetails
	if !errors.As(err, &pd) || pd == nil {
		return err.Error()
	}
	doc := problemDoc{
		Type:     jobs.Clean(pd.Type, nil),
		Detail:   jobs.Clean(pd.Detail, nil),
		Status:   pd.HTTPStatus,
		Instance: jobs.Clean(pd.Instance, nil),
	}
	for _, sp := range pd.SubProblems {
		doc.SubProblems = append(doc.SubProblems, subProblem{
			Type:       jobs.Clean(sp.Type, nil),
			Detail:     jobs.Clean(sp.Detail, nil),
			Identifier: jobs.Clean(sp.Identifier.Value, nil),
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if encErr := enc.Encode(doc); encErr != nil {
		return err.Error()
	}
	return buf.String()
}

// cleanDetail makes s safe to store and draw as text: control characters stripped, clipped to
// jobs.MaxDetailRunes characters, and then to MaxDetailBytes on a character boundary.
func cleanDetail(s string) string {
	s = jobs.Clean(s, nil)
	if len(s) <= MaxDetailBytes {
		return s
	}
	cut := MaxDetailBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
