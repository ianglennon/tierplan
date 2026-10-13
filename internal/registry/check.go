// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"strings"

	"github.com/regclient/regclient/types/errs"
)

// ErrNotARegistry is what a probe of /v2/ fails with when the host answered with a status a
// registry does not use there.
var ErrNotARegistry = errors.New("registry: the host did not answer like a registry")

// The states of one step of a credential test.
const (
	StepPassed     = "passed"
	StepFailed     = "failed"
	StepNotChecked = "not_checked"
)

// The codes a step reports. The SPA words each of them; no registry text is ever sent on.
const (
	CodeRegistryUnreachable = "registry_unreachable"
	CodeRegistryTLS         = "registry_tls"
	CodeRegistryHTTP        = "registry_http"
	CodeSignInRefused       = "sign_in_refused"
	CodeNoLoginAsked        = "no_login_asked"
	CodeNoPullAccess        = "no_pull_access"
	CodeRepositoryNotFound  = "repository_not_found"
	CodeRateLimited         = "rate_limited"
	CodeCheckFailed         = "check_failed"
)

// CheckInput is what one test of a credential needs.
type CheckInput struct {
	HostOptions
	// Type is the credential type, dockerhub, ghcr, gitlab or generic.
	Type string
	// Prefix is the namespace the credential covers. It is recorded with the test and does
	// not change what is checked.
	Prefix string
	// TestRepository is a repository to try to read, like acme/app. Empty skips the last
	// step.
	TestRepository string
}

// StepResult is the outcome of one step.
type StepResult struct {
	// State is passed, failed or not_checked.
	State string
	// Code says why for failed, and why nothing was checked for no_login_asked. Empty
	// otherwise.
	Code string
}

// CheckResult is the three steps of a credential test (D-12).
type CheckResult struct {
	Reachable StepResult
	SignedIn  StepResult
	CanPull   StepResult
}

// Failed reports whether any step failed.
func (r CheckResult) Failed() bool {
	return r.Reachable.State == StepFailed || r.SignedIn.State == StepFailed || r.CanPull.State == StepFailed
}

// FailureCode is the code of the first step that failed, or empty.
func (r CheckResult) FailureCode() string {
	for _, s := range []StepResult{r.Reachable, r.SignedIn, r.CanPull} {
		if s.State == StepFailed {
			return s.Code
		}
	}
	return ""
}

// Check runs the registry's own flow against it in three steps and reports each: is it
// reachable, does it accept the sign-in, and can the sign-in read the test repository. c
// carries the credentials; anon has none and is only asked whether a login is wanted at all.
// A step that cannot run because an earlier one failed is not_checked. It never returns an
// error: every failure is a code (RESEARCH Pattern 6, D-12).
func Check(ctx context.Context, c, anon *Client, in CheckInput) CheckResult {
	res := CheckResult{
		Reachable: StepResult{State: StepNotChecked},
		SignedIn:  StepResult{State: StepNotChecked},
		CanPull:   StepResult{State: StepNotChecked},
	}

	probe, err := anon.Probe(ctx)
	if err != nil {
		res.Reachable = StepResult{State: StepFailed, Code: classify(err)}
		return res
	}
	res.Reachable = StepResult{State: StepPassed}

	signedIn := true
	// A probe that went through regclient's sign-in cannot say the registry wants no login, so
	// the sign-in is tried.
	if probe.AsksLogin || probe.LoginUnknown {
		if err := c.Ping(ctx); err != nil {
			res.SignedIn = StepResult{State: StepFailed, Code: classify(err)}
			signedIn = false
		} else {
			res.SignedIn = StepResult{State: StepPassed}
		}
	} else {
		res.SignedIn = StepResult{State: StepNotChecked, Code: CodeNoLoginAsked}
	}

	repo, ok := TestRepositoryPath(in.Host, in.TestRepository)
	if repo == "" || !ok || !signedIn {
		return res
	}
	if _, err := c.TagList(ctx, repo, 1); err != nil {
		code := classify(err)
		if code == CodeSignInRefused {
			// The sign-in worked a moment ago, so a refusal here is the repository's.
			code = CodeNoPullAccess
		}
		res.CanPull = StepResult{State: StepFailed, Code: code}
		return res
	}
	res.CanPull = StepResult{State: StepPassed}
	return res
}

// testTagListMax is how many tag names PickTestTag asks for.
const testTagListMax = 1000

// testTagDefault is the tag a check reads when the registry lists no better one.
const testTagDefault = "latest"

// PickTestTag is the tag a check from an environment reads when none was asked for: latest when
// the registry lists it, otherwise the newest tag it lists (CompareTags order, the order of the
// tag browser; signature and attestation tags are skipped). listed says whether the name came
// from the registry; it is false, with latest, when the tags cannot be listed or none is usable.
// A repository pinned to version tags has no latest, and a node's engine cannot read a manifest
// without a concrete tag, so a blind latest would report a missing tag as a failed credential
// (G-05-4c, D-13). c should be a client built through the egress policy.
func PickTestTag(ctx context.Context, c *Client, repo string) (tag string, listed bool) {
	names, err := c.TagList(ctx, repo, testTagListMax)
	if err != nil {
		return testTagDefault, false
	}
	if slices.Contains(names, testTagDefault) {
		return testTagDefault, true
	}
	usable := make([]string, 0, len(names))
	for _, n := range names {
		if ValidTag(n) && !HiddenTag(n) {
			usable = append(usable, n)
		}
	}
	if len(usable) == 0 {
		return testTagDefault, false
	}
	slices.SortStableFunc(usable, CompareTags)
	return usable[0], true
}

// TestRepositoryPath is the repository to read on host: the path below the host, with the
// host dropped when it was typed, and library/ added for a bare Docker Hub name. ok is false
// for something that is not a repository path. An empty repository is "", true: nothing to try.
func TestRepositoryPath(host, repo string) (path string, ok bool) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return "", true
	}
	repo = strings.TrimPrefix(repo, host+"/")
	if host == hubHost && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if len(repo) > maxPrefixLen || !prefixPattern.MatchString(repo) {
		return "", false
	}
	return repo, true
}

// classify maps an error from a registry call to the code the SPA words. It looks at the
// error's type and never at registry text.
func classify(err error) string {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.Is(err, errs.ErrHTTPRateLimit):
		return CodeRateLimited
	case errors.Is(err, errs.ErrHTTPUnauthorized):
		return CodeSignInRefused
	case errors.Is(err, errs.ErrNotFound):
		return CodeRepositoryNotFound
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid), errors.As(err, &verify):
		return CodeRegistryTLS
	case errors.Is(err, errs.ErrHTTPRedirectRefused), errors.Is(err, ErrNotARegistry), errors.Is(err, ErrResponseTooLarge), isPlainHTTPAnswer(err):
		return CodeRegistryHTTP
	case errors.Is(err, ErrEgressBlocked), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return CodeRegistryUnreachable
	case isNetworkError(err):
		return CodeRegistryUnreachable
	}
	return CodeCheckFailed
}

// isPlainHTTPAnswer reports the error Go's client returns when it speaks TLS to a server that
// answers in plain HTTP.
func isPlainHTTPAnswer(err error) bool {
	return err != nil && strings.Contains(err.Error(), "server gave HTTP response to HTTPS client")
}

// isNetworkError reports a failure to connect, resolve or read, as opposed to an answer.
func isNetworkError(err error) bool {
	var ne net.Error
	var op *net.OpError
	var dns *net.DNSError
	return errors.As(err, &ne) || errors.As(err, &op) || errors.As(err, &dns)
}
