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
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	legoacme "github.com/go-acme/lego/v5/acme"

	"github.com/ianglennon/tierplan/internal/acme/dns"
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
			if !reflect.DeepEqual(got, tc.want) {
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
	for _, class := range []string{
		ClassDNSTokenInvalid, ClassDNSTokenScope, ClassDNSZoneNotFound, ClassDNSDelegationTarget,
		ClassDNSNotVisible, ClassDNSRateLimited, ClassDNSUnavailable,
	} {
		seen[Classify(&dns.Error{Class: class}, ClassifyInput{ChallengeType: ChallengeDNS01}).Class] = true
	}
	seen[Classify(&ChallengeError{Host: "a.example.com", Err: problem(legoacme.IncorrectResponseErrorType, 400)},
		ClassifyInput{ChallengeType: ChallengeDNS01}).Class] = true
	if len(seen) != 20 {
		t.Fatalf("the sample reached %d classes, want 20: %v", len(seen), seen)
	}
}

// TestClassifyDNSError checks that a DNS provider failure keeps its class through the wrapping the
// order code adds, carries what its copy needs, and shows only the class, status and code as detail.
func TestClassifyDNSError(t *testing.T) {
	wrap := func(de *dns.Error) error {
		return fmt.Errorf("present the dns-01 challenge for %s: %w", "a.example.com", de)
	}
	for _, class := range []string{
		dns.ClassTokenInvalid, dns.ClassTokenScope, dns.ClassZoneNotFound, dns.ClassDelegationTarget,
		dns.ClassNotVisible, dns.ClassRateLimited, dns.ClassUnavailable,
	} {
		t.Run(class, func(t *testing.T) {
			de := &dns.Error{
				Class: class, Host: "_acme-challenge.a.example.com", Zone: "example.com", Target: "t.example.net",
				Resolvers: []string{"ns1.example.net:53", "ns2.example.net:53"}, RetryAfter: 45 * time.Second,
				Status: 403, Code: 9109, Provider: "cloudflare", Op: "find zone",
				Err: errors.New("the remote said something private"),
			}
			got := Classify(wrap(de), ClassifyInput{ChallengeType: ChallengeDNS01})
			want := Failure{
				Class: class, Host: "_acme-challenge.a.example.com", Zone: "example.com", Target: "t.example.net",
				Resolvers: []string{"ns1.example.net:53", "ns2.example.net:53"}, RetryAfter: 45 * time.Second,
			}
			detail := got.Detail
			got.Detail = ""
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Classify = %+v, want %+v", got, want)
			}
			if !strings.Contains(detail, "cloudflare find zone: HTTP 403 code 9109 ("+class+")") {
				t.Errorf("Detail = %q, want the operation, status, code and class", detail)
			}
			if strings.Contains(detail, "private") {
				t.Errorf("Detail = %q carries the wrapped cause's text", detail)
			}
		})
	}

	t.Run("a challenge error around a DNS error keeps the DNS host", func(t *testing.T) {
		err := &ChallengeError{Host: "a.example.com", Err: &dns.Error{Class: dns.ClassZoneNotFound}}
		if got := Classify(err, ClassifyInput{}); got.Class != ClassDNSZoneNotFound || got.Host != "a.example.com" {
			t.Fatalf("Classify = %+v, want dns_zone_not_found for a.example.com", got)
		}
	})
	t.Run("an empty class is internal", func(t *testing.T) {
		got := Classify(wrap(&dns.Error{Status: 422, Code: 9999, Provider: "cloudflare", Op: "create record"}), ClassifyInput{})
		if got.Class != ClassInternal || got.Detail == "" {
			t.Fatalf("Classify = %+v, want internal with a detail", got)
		}
	})
	t.Run("an unknown class is internal", func(t *testing.T) {
		if got := Classify(wrap(&dns.Error{Class: "dns_from_the_future"}), ClassifyInput{}); got.Class != ClassInternal {
			t.Fatalf("Classify = %+v, want internal", got)
		}
	})
	t.Run("an empty-class DNS error with a network cause is still internal", func(t *testing.T) {
		de := &dns.Error{Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}}
		if got := Classify(wrap(de), ClassifyInput{}); got.Class != ClassInternal {
			t.Fatalf("Classify = %+v, want internal", got)
		}
	})
}

// TestClassifyValidationTXT checks that the CA's validation problems are about the TXT record under
// DNS-01 and keep their Phase 14 class under HTTP-01 (UI-SPEC J7).
func TestClassifyValidationTXT(t *testing.T) {
	for _, tc := range []struct {
		name, typ string
		httpClass string
	}{
		{"unauthorized", legoacme.UnauthorizedErrorType, ClassValidationUnreachable},
		{"incorrect response", legoacme.IncorrectResponseErrorType, ClassValidationUnreachable},
		{"connection", legoacme.ConnectionErrorType, ClassValidationUnreachable},
		{"dns", legoacme.DNSErrorType, ClassValidationDNS},
		{"timeout", timeoutErrorType, ClassValidationUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &ChallengeError{Host: "a.example.com", Err: problem(tc.typ, 400)}
			got := Classify(err, ClassifyInput{ChallengeType: ChallengeDNS01})
			if got.Class != ClassValidationTXT || got.Host != "a.example.com" {
				t.Fatalf("under dns-01 Classify = %+v, want validation_txt for a.example.com", got)
			}
			for _, ct := range []string{ChallengeHTTP01, ""} {
				if got := Classify(err, ClassifyInput{ChallengeType: ct}); got.Class != tc.httpClass {
					t.Fatalf("under %q Classify = %+v, want %s", ct, got, tc.httpClass)
				}
			}
		})
	}
	t.Run("a compound problem is classified by its sub-problem", func(t *testing.T) {
		pd := &legoacme.ProblemDetails{Type: legoacme.CompoundErrorType, HTTPStatus: 400, SubProblems: []legoacme.SubProblem{
			{Type: legoacme.UnauthorizedErrorType, Identifier: legoacme.Identifier{Type: "dns", Value: "b.example.com"}},
		}}
		if got := Classify(pd, ClassifyInput{ChallengeType: ChallengeDNS01}); got.Class != ClassValidationTXT || got.Host != "b.example.com" {
			t.Fatalf("Classify = %+v, want validation_txt for b.example.com", got)
		}
	})
	t.Run("other problems keep their class under dns-01", func(t *testing.T) {
		for typ, want := range map[string]string{
			legoacme.CaaErrorType:                ClassCAAForbids,
			legoacme.RateLimitedErrorType:        ClassRateLimited,
			legoacme.ServerInternalErrorType:     ClassCAUnavailable,
			legoacme.RejectedIdentifierErrorType: ClassIdentifierRejected,
		} {
			err := &ChallengeError{Host: "c.example.com", Err: problem(typ, 400)}
			if got := Classify(err, ClassifyInput{ChallengeType: ChallengeDNS01}); got.Class != want {
				t.Errorf("%s under dns-01: Classify = %+v, want %s", typ, got, want)
			}
		}
	})
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
