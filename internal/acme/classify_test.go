// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	legoacme "github.com/go-acme/lego/v5/acme"
)

func problem(typ string, status int) *legoacme.ProblemDetails {
	return &legoacme.ProblemDetails{Type: typ, Detail: "detail of " + typ, HTTPStatus: status}
}

func TestClassify(t *testing.T) {
	const ns = "urn:ietf:params:acme:error:"
	tosProblem := problem(legoacme.UserActionRequiredErrorType, 403)
	tosProblem.Instance = "https://ca.example/terms/v2"
	compound := &legoacme.ProblemDetails{
		Type:       legoacme.CompoundErrorType,
		HTTPStatus: 400,
		SubProblems: []legoacme.SubProblem{
			{Type: legoacme.CaaErrorType, Detail: "caa", Identifier: legoacme.Identifier{Type: "dns", Value: "caa.example.com"}},
		},
	}
	tests := []struct {
		name string
		err  error
		in   ClassifyInput
		want Failure // Detail is checked separately
	}{
		{"rate limited carries Retry-After", &legoacme.RateLimitedError{ProblemDetails: problem(legoacme.RateLimitedErrorType, 429), RetryAfter: 2 * time.Hour},
			ClassifyInput{}, Failure{Class: ClassRateLimited, RetryAfter: 2 * time.Hour}},
		{"a rate limited problem without the wrapper", problem(legoacme.RateLimitedErrorType, 429),
			ClassifyInput{}, Failure{Class: ClassRateLimited}},
		{"a connection problem names the host", &ChallengeError{Host: "a.example.com", Err: problem(legoacme.ConnectionErrorType, 400)},
			ClassifyInput{ChallengeType: ChallengeHTTP01}, Failure{Class: ClassValidationUnreachable, Host: "a.example.com"}},
		{"an incorrect response is unreachable", &ChallengeError{Host: "a.example.com", Err: problem(legoacme.IncorrectResponseErrorType, 400)},
			ClassifyInput{}, Failure{Class: ClassValidationUnreachable, Host: "a.example.com"}},
		{"unauthorized is unreachable", &ChallengeError{Host: "b.example.com", Err: problem(legoacme.UnauthorizedErrorType, 403)},
			ClassifyInput{}, Failure{Class: ClassValidationUnreachable, Host: "b.example.com"}},
		{"a timeout is unreachable", &ChallengeError{Host: "c.example.com", Err: problem(ns+"timeout", 400)},
			ClassifyInput{}, Failure{Class: ClassValidationUnreachable, Host: "c.example.com"}},
		{"a dns problem", &ChallengeError{Host: "d.example.com", Err: problem(legoacme.DNSErrorType, 400)},
			ClassifyInput{}, Failure{Class: ClassValidationDNS, Host: "d.example.com"}},
		{"a caa problem", &ChallengeError{Host: "e.example.com", Err: problem(legoacme.CaaErrorType, 403)},
			ClassifyInput{}, Failure{Class: ClassCAAForbids, Host: "e.example.com"}},
		{"a challenge error wrapped again", fmt.Errorf("order: %w", &ChallengeError{Host: "f.example.com", Err: problem(legoacme.DNSErrorType, 400)}),
			ClassifyInput{}, Failure{Class: ClassValidationDNS, Host: "f.example.com"}},
		{"a compound problem is classified by its sub-problem", compound,
			ClassifyInput{}, Failure{Class: ClassCAAForbids, Host: "caa.example.com"}},
		{"rejected identifier", problem(legoacme.RejectedIdentifierErrorType, 400),
			ClassifyInput{}, Failure{Class: ClassIdentifierRejected}},
		{"unsupported identifier", problem(legoacme.UnsupportedIdentifierErrorType, 400),
			ClassifyInput{}, Failure{Class: ClassIdentifierRejected}},
		{"invalid contact", problem(legoacme.InvalidContactErrorType, 400),
			ClassifyInput{}, Failure{Class: ClassContactInvalid}},
		{"unsupported contact", problem(legoacme.UnsupportedContactErrorType, 400),
			ClassifyInput{}, Failure{Class: ClassContactInvalid}},
		{"terms of service changed", tosProblem,
			ClassifyInput{}, Failure{Class: ClassTOSActionRequired, TermsURL: "https://ca.example/terms/v2"}},
		{"account does not exist", problem(legoacme.AccountDoesNotExistErrorType, 400),
			ClassifyInput{}, Failure{Class: ClassAccountGone}},
		{"invalid profile carries the profile asked for", problem(legoacme.InvalidProfileErrorType, 400),
			ClassifyInput{Profile: "shortlived"}, Failure{Class: ClassProfileInvalid, Profile: "shortlived"}},
		{"server internal", problem(legoacme.ServerInternalErrorType, 500),
			ClassifyInput{}, Failure{Class: ClassCAUnavailable}},
		{"an http 5xx problem of an unknown type", problem(ns+"somethingNew", 503),
			ClassifyInput{}, Failure{Class: ClassCAUnavailable}},
		{"a network error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			ClassifyInput{}, Failure{Class: ClassCAUnavailable}},
		{"a dns error for the CA host", &net.DNSError{Err: "no such host", Name: "ca.example"},
			ClassifyInput{}, Failure{Class: ClassCAUnavailable}},
		{"a deadline", fmt.Errorf("directory: %w", context.DeadlineExceeded),
			ClassifyInput{}, Failure{Class: ClassCAUnavailable}},
		{"an unknown authority", &x509.UnknownAuthorityError{},
			ClassifyInput{}, Failure{Class: ClassCAUntrusted}},
		{"a certificate verification error", &tls.CertificateVerificationError{Err: errors.New("bad")},
			ClassifyInput{}, Failure{Class: ClassCAUntrusted}},
		{"an untrusted CA behind a network wrapper", &net.OpError{Op: "remote error", Err: &x509.UnknownAuthorityError{}},
			ClassifyInput{}, Failure{Class: ClassCAUntrusted}},
		{"bad CSR", problem(legoacme.BadCSRErrorType, 400),
			ClassifyInput{}, Failure{Class: ClassInternal}},
		{"order not ready", problem(legoacme.OrderNotReadyErrorType, 403),
			ClassifyInput{}, Failure{Class: ClassInternal}},
		{"malformed", problem(legoacme.MalformedErrorType, 400),
			ClassifyInput{}, Failure{Class: ClassInternal}},
		{"a plain error", errors.New("something odd"),
			ClassifyInput{}, Failure{Class: ClassInternal}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err, tc.in)
			detail := got.Detail
			got.Detail = ""
			if got != tc.want {
				t.Fatalf("Classify = %+v, want %+v", got, tc.want)
			}
			if detail == "" {
				t.Fatal("Detail is empty, want the raw document or error text")
			}
		})
	}
}

func TestClassifyCoversEveryClass(t *testing.T) {
	seen := map[string]bool{}
	for _, err := range []error{
		&legoacme.RateLimitedError{ProblemDetails: problem(legoacme.RateLimitedErrorType, 429)},
		problem(legoacme.ConnectionErrorType, 400),
		problem(legoacme.DNSErrorType, 400),
		problem(legoacme.CaaErrorType, 403),
		problem(legoacme.RejectedIdentifierErrorType, 400),
		problem(legoacme.InvalidContactErrorType, 400),
		problem(legoacme.UserActionRequiredErrorType, 403),
		problem(legoacme.AccountDoesNotExistErrorType, 400),
		problem(legoacme.InvalidProfileErrorType, 400),
		problem(legoacme.ServerInternalErrorType, 500),
		&x509.UnknownAuthorityError{},
		errors.New("odd"),
	} {
		seen[Classify(err, ClassifyInput{}).Class] = true
	}
	if len(seen) != 12 {
		t.Fatalf("the sample reached %d classes, want 12: %v", len(seen), seen)
	}
}

func TestClassifyCapsAndCleansDetail(t *testing.T) {
	p := problem(legoacme.MalformedErrorType, 400)
	p.Detail = "before\x07\x1b[31mmid\x00" + strings.Repeat("é", 64<<10)
	got := Classify(p, ClassifyInput{})
	if len(got.Detail) > MaxDetailBytes {
		t.Fatalf("Detail is %d bytes, want at most %d", len(got.Detail), MaxDetailBytes)
	}
	if !utf8.ValidString(got.Detail) {
		t.Fatal("Detail is not valid UTF-8 after the cut")
	}
	for _, r := range got.Detail {
		if r != '\t' && unicode.IsControl(r) {
			t.Fatalf("Detail holds control character %q", r)
		}
	}
	if !strings.Contains(got.Detail, "beforemid") && !strings.Contains(got.Detail, "before[31mmid") {
		t.Fatalf("Detail %q lost the readable text around the control characters", got.Detail[:80])
	}

	short := problem(legoacme.RateLimitedErrorType, 429)
	short.Instance = "https://ca.example/limits"
	d := Classify(short, ClassifyInput{}).Detail
	for _, want := range []string{legoacme.RateLimitedErrorType, "detail of", "429", "https://ca.example/limits"} {
		if !strings.Contains(d, want) {
			t.Fatalf("Detail %q is missing %q", d, want)
		}
	}

	plain := Classify(errors.New("bad\x00 thing\n"), ClassifyInput{}).Detail
	if plain != "bad thing" {
		t.Fatalf("a plain error's Detail = %q, want %q", plain, "bad thing")
	}
}

func TestChallengeErrorUnwraps(t *testing.T) {
	inner := errors.New("inner")
	err := &ChallengeError{Host: "a.example.com", Err: inner}
	if !errors.Is(err, inner) {
		t.Fatal("ChallengeError does not unwrap to its cause")
	}
	if !strings.Contains(err.Error(), "a.example.com") || !strings.Contains(err.Error(), "inner") {
		t.Fatalf("Error() = %q, want the host and the cause", err.Error())
	}
}
