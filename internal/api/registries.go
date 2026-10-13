// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

const (
	problemRegistriesUnavailable = "registries_unavailable"
	problemCredentialNameTaken   = "credential_name_taken" //nolint:gosec // G101: a problem or target type, not a credential
	problemHostPrefixTaken       = "host_prefix_taken"
	problemHubFallbackTaken      = "hub_fallback_taken"
	problemCredentialNotFound    = "credential_not_found" //nolint:gosec // G101: a problem or target type, not a credential
	problemRepositoryRequired    = "repository_required"

	credentialTargetType = "registry_credential" //nolint:gosec // G101: a problem or target type, not a credential

	// testsPerMinute is how many credential tests one user may run in a minute, across both
	// route families: a test is a sign-in attempt against a registry of the caller's choosing,
	// and must not become a way to guess a password (T-05-39).
	testsPerMinute = 10
	// testBudget is the time one test may take in all, whatever it reaches.
	testBudget = 30 * time.Second
	// tagPickBudget is the time the server's listing of a repository's tags may take when a
	// check from an environment was sent without a tag (G-05-4c).
	tagPickBudget = 10 * time.Second
)

// RegistriesDeps are the dependencies of the registry credential routes.
type RegistriesDeps struct {
	// Store keeps the credentials. Nil answers 503 registries_unavailable, so the routes
	// are always registered.
	Store *registry.Store
	// Envs names the environment of an environment credential.
	Envs *envs.Service
	// Aggregator reaches a node's engine for a check from an environment. Nil answers 503
	// registries_unavailable on those routes.
	Aggregator *envs.Aggregator
	Audit      *audit.Writer
	// Checker decides who may see and manage which credentials.
	Checker *rbac.Checker
	// Egress is the policy every registry connection of the server dials through. Its zero
	// value refuses link-local, unspecified and multicast addresses.
	Egress registry.EgressPolicy
	// Clients keeps the registry clients. Nil makes a cache from Egress and Version, so the
	// test routes work in a deployment that does not share one.
	Clients *registry.ClientCache
	// Version is the build version, sent as the user agent bayplan/<version>.
	Version string
	// Now is the clock the credential alerts count their days by. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// CredentialRequest is the body of POST /api/registries.
type CredentialRequest struct {
	Name        string `json:"name,omitempty" maxLength:"256" doc:"What the credential is called, unique among the instance credentials. 1 to 64 characters: letters, digits, spaces and . _ ( ) -; 422 name_required or name_invalid"`
	Type        string `json:"type,omitempty" maxLength:"32" doc:"dockerhub, ghcr, gitlab or generic. The first three fill in the host; 422 type_invalid for anything else"`
	Host        string `json:"host,omitempty" maxLength:"512" doc:"The registry host, with a port if it has one, like registry.example.org:5000. Ignored for dockerhub and ghcr, registry.gitlab.com when left out for gitlab, required for generic: 422 host_required or host_invalid. No scheme and no path"`
	Prefix      string `json:"prefix,omitempty" maxLength:"512" doc:"The namespace the credential covers, like acme or acme/team: lower-case path segments with no slash at either end. Empty covers the whole registry. 422 prefix_invalid, and hub_prefix_required for docker.io with neither a prefix nor hubFallback"`
	Username    string `json:"username,omitempty" maxLength:"512" doc:"The registry user name; 422 username_required"`
	Secret      string `json:"secret,omitempty" maxLength:"4096" doc:"The password or token. It is sealed with the vault and never returned by any route; only its last 4 characters can be shown, and only when it has 12 or more. 422 secret_required"`
	HubFallback bool   `json:"hubFallback,omitempty" doc:"Use this credential for every Docker Hub pull no more specific credential matches. Docker Hub only (422 hub_fallback_not_hub), one per layer (409 hub_fallback_taken)"`
	Expires     string `json:"expires,omitempty" maxLength:"32" doc:"An optional day, YYYY-MM-DD. It lasts to the end of that day, UTC. 422 expiry_invalid, and expiry_past for a day that has gone"`
	CAPEM       string `json:"caPem,omitempty" maxLength:"262144" doc:"An optional certificate authority, in PEM, that Bayplan's own tests and tag browsing trust for this registry. At most 64 KiB and it must hold a certificate: 422 ca_invalid. Public material, returned to people who may manage the credential. It never changes the server's trust store and does nothing on a node"`
	Insecure    bool   `json:"insecure,omitempty" doc:"Let Bayplan's own tests and tag browsing reach this registry without verifying its certificate, and over plain HTTP when it answers plain HTTP. Off by default. It does nothing on a node: each node's Docker daemon needs its own insecure-registries or certs.d setup"`
}

// CreateCredentialInput is the body of the create route.
type CreateCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      CredentialRequest
}

// CreateEnvCredentialInput is the path and body of the environment create route.
type CreateEnvCredentialInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      CredentialRequest
}

// CredentialPatchRequest is the body of PATCH /api/registries/{credentialId}. Every member is
// optional: one left out keeps what is stored, and one sent as an empty string clears a prefix
// or an expiry. There is no scope: a credential never moves between layers.
type CredentialPatchRequest struct {
	Name        *string `json:"name,omitempty" maxLength:"256" doc:"A new name, unique among the instance credentials. Same rules as when adding; 422 name_required or name_invalid"`
	Type        *string `json:"type,omitempty" maxLength:"32" doc:"dockerhub, ghcr, gitlab or generic; 422 type_invalid for anything else"`
	Host        *string `json:"host,omitempty" maxLength:"512" doc:"A new registry host. Ignored for dockerhub and ghcr; 422 host_required or host_invalid"`
	Prefix      *string `json:"prefix,omitempty" maxLength:"512" doc:"A new namespace; an empty string covers the whole registry. 422 prefix_invalid, and hub_prefix_required for docker.io with neither a prefix nor hubFallback"`
	Username    *string `json:"username,omitempty" maxLength:"512" doc:"A new registry user name; 422 username_required"`
	Secret      *string `json:"secret,omitempty" maxLength:"4096" doc:"A replacement password or token. Left out or empty keeps the stored secret, which is never read back. A new one is sealed with the vault and replaces it. It is never returned by any route"`
	HubFallback *bool   `json:"hubFallback,omitempty" doc:"Whether it is used for every Docker Hub pull no more specific credential matches. Docker Hub only (422 hub_fallback_not_hub), one per layer (409 hub_fallback_taken)"`
	Expires     *string `json:"expires,omitempty" maxLength:"32" doc:"A day, YYYY-MM-DD, or an empty string for none. 422 expiry_invalid, and expiry_past for a day that has gone (the credential's own unchanged day is never refused)"`
	CAPEM       *string `json:"caPem,omitempty" maxLength:"262144" doc:"A new custom certificate authority in PEM, or an empty string for none. 422 ca_invalid for more than 64 KiB or text that holds no certificate"`
	Insecure    *bool   `json:"insecure,omitempty" doc:"Whether Bayplan's own tests and tag browsing skip certificate verification and may use plain HTTP for this registry"`
}

// UpdateCredentialInput is the request of the update route.
type UpdateCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent    string `header:"User-Agent" hidden:"true"`
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
	Body         CredentialPatchRequest
}

// DeleteCredentialInput is the request of the delete route.
type DeleteCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent    string `header:"User-Agent" hidden:"true"`
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
}

// UpdateEnvCredentialInput is the request of the environment update route.
type UpdateEnvCredentialInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent    string `header:"User-Agent" hidden:"true"`
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
	Body         CredentialPatchRequest
}

// DeleteEnvCredentialInput is the request of the environment delete route.
type DeleteEnvCredentialInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent    string `header:"User-Agent" hidden:"true"`
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
}

// CredentialScopeBody says where a credential applies.
type CredentialScopeBody struct {
	Kind    string  `json:"kind" enum:"instance,environment" doc:"instance: every environment. environment: one environment only"`
	EnvID   *string `json:"envId" nullable:"true" doc:"The environment of an environment credential; null for an instance credential"`
	EnvName *string `json:"envName" nullable:"true" doc:"The name of that environment; null for an instance credential"`
}

// CredentialTestBody is the outcome of the last test of a credential.
type CredentialTestBody struct {
	At     time.Time `json:"at" format:"date-time" doc:"When it was tested"`
	OK     bool      `json:"ok" doc:"Whether the registry accepted it"`
	Detail string    `json:"detail" doc:"A short sentence about the outcome. Never the secret"`
}

// CredentialFailureBody is the last time a pull or a browse failed to sign in with the credential.
type CredentialFailureBody struct {
	At   time.Time `json:"at" format:"date-time" doc:"When it failed"`
	Code string    `json:"code" doc:"A stable code, like unauthorized or denied"`
}

// CredentialBody is a registry credential as the browser sees it. It has no secret field:
// the secret is write-only (D-05), and last4 is all that is left of it.
type CredentialBody struct {
	ID          string                          `json:"id" doc:"The credential's ID"`
	Scope       CredentialScopeBody             `json:"scope" doc:"Where it applies"`
	Name        string                          `json:"name" doc:"Its name, unique within its scope"`
	Type        string                          `json:"type" enum:"dockerhub,ghcr,gitlab,generic" doc:"The registry preset"`
	Host        string                          `json:"host" doc:"The registry host, lower case, with a port if it has one"`
	Prefix      string                          `json:"prefix" doc:"The namespace it covers; empty for the whole registry"`
	Username    string                          `json:"username" doc:"The registry user name"`
	Last4       string                          `json:"last4" doc:"The last 4 characters of the secret, or empty when the secret is shorter than 12 characters. The page shows •••• before it"`
	HubFallback bool                            `json:"hubFallback" doc:"Whether it is used for every Docker Hub pull no more specific credential matches"`
	CAPEM       string                          `json:"caPem" doc:"The custom certificate authority in PEM, public material, for people who may manage the credential; empty for everyone else and when there is none"`
	HasCA       bool                            `json:"hasCa" doc:"Whether the credential carries a custom certificate authority, whoever is asking"`
	Insecure    bool                            `json:"insecure" doc:"Whether Bayplan's own tests and tag browsing skip certificate verification and may use plain HTTP for this registry. Nodes are not affected"`
	ExpiresAt   *string                         `json:"expiresAt" nullable:"true" format:"date-time" doc:"The end of the last day it is valid; null when it does not expire"`
	LastTest    Nullable[CredentialTestBody]    `json:"lastTest" doc:"The last test of the credential; null when it was never tested"`
	LastFailure Nullable[CredentialFailureBody] `json:"lastFailure" doc:"The last sign-in failure with it; null when there was none"`
	CanManage   bool                            `json:"canManage" doc:"Whether the caller may change or delete it"`
	CreatedAt   time.Time                       `json:"createdAt" format:"date-time" doc:"When it was added"`
	UpdatedAt   time.Time                       `json:"updatedAt" format:"date-time" doc:"When it was last changed"`
}

// CredentialOutput wraps CredentialBody for huma.
type CredentialOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CredentialBody
}

// CredentialListBody is the credentials the caller may see.
type CredentialListBody struct {
	Items []CredentialBody `json:"items" doc:"Instance credentials first, then each environment's, by host and prefix"`
}

// CredentialListOutput wraps CredentialListBody for huma.
type CredentialListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CredentialListBody
}

// CredentialAlertBody is one credential that needs its managers' attention, in the header bell.
type CredentialAlertBody struct {
	Kind         string              `json:"kind" enum:"expiring,expired,auth_failed" doc:"expiring: it stops working within 7 days. expired: its expiry day has passed. auth_failed: its registry refused it on the last use and it has not worked since"`
	CredentialID string              `json:"credentialId" doc:"The credential's ID"`
	Name         string              `json:"name" doc:"Its name"`
	Host         string              `json:"host" doc:"The registry host"`
	Prefix       string              `json:"prefix" doc:"The namespace it covers; empty for the whole registry"`
	Scope        CredentialScopeBody `json:"scope" doc:"Where it applies"`
	Days         int                 `json:"days,omitempty" doc:"For expiring: the whole days left, rounded up, at least 1"`
	At           time.Time           `json:"at" format:"date-time" doc:"For expiring and expired: the end of the last day it is valid. For auth_failed: when the registry refused it"`
}

// CredentialAlertsBody lists the credentials that need attention now.
type CredentialAlertsBody struct {
	Items []CredentialAlertBody `json:"items" doc:"Only the credentials that need attention, one item each, the most severe first; empty when there are none"`
}

// CredentialAlertsOutput wraps CredentialAlertsBody for huma.
type CredentialAlertsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CredentialAlertsBody
}

// CredentialPathInput names one credential.
type CredentialPathInput struct {
	CredentialID string `path:"credentialId" maxLength:"64" doc:"The credential's ID"`
}

// ResolveRequest is the body of POST /api/env/{envId}/registries/resolve.
type ResolveRequest struct {
	Ref string `json:"ref" minLength:"1" maxLength:"512" doc:"The image to check, like nginx:1.27 or ghcr.io/acme/app:latest. No tag means latest; a bare name is a Docker Hub library image. 422 ref_invalid when it is not an image reference"`
}

// ResolveInput is the path and body of the resolve route.
type ResolveInput struct {
	EnvironmentPathInput
	Body ResolveRequest
}

// ResolvedCredentialBody is the credential a pull would use: what it is and what it covers,
// never its secret.
type ResolvedCredentialBody struct {
	ID     string              `json:"id" doc:"The credential's ID"`
	Name   string              `json:"name" doc:"Its name"`
	Scope  CredentialScopeBody `json:"scope" doc:"Where it applies"`
	Host   string              `json:"host" doc:"The registry host it covers"`
	Prefix string              `json:"prefix" doc:"The namespace it covers; empty for the whole registry"`
}

// ConsideredCredentialBody is one credential for the image's registry and what became of it.
type ConsideredCredentialBody struct {
	CredentialID string              `json:"credentialId" doc:"The credential's ID"`
	Name         string              `json:"name" doc:"Its name"`
	Scope        CredentialScopeBody `json:"scope" doc:"Where it applies"`
	Matches      bool                `json:"matches" doc:"Whether it covers the image at all"`
	Result       string              `json:"result" enum:"used,shorter_prefix,overridden_by_environment,hub_fallback_overridden,no_match" doc:"used: the pulls use it. shorter_prefix: it covers the image with a shorter prefix than the one used. overridden_by_environment: an instance credential the environment's of the same specificity beat. hub_fallback_overridden: a Docker Hub fallback a more specific credential beat. no_match: it is for the same registry but not this namespace"`
}

// ResolveBody is the answer of the resolve route.
type ResolveBody struct {
	Input      string                           `json:"input" doc:"The reference as it was sent, trimmed"`
	Normalized string                           `json:"normalized" doc:"The reference as the engine will pull it: docker.io and library/ filled in, and latest when there is no tag or digest"`
	Credential Nullable[ResolvedCredentialBody] `json:"credential" doc:"The credential a pull would use, or null for an anonymous pull"`
	MatchedOn  string                           `json:"matchedOn" doc:"prefix, host or hub_fallback; empty for an anonymous pull"`
	Considered []ConsideredCredentialBody       `json:"considered" doc:"Every credential for the image's registry that the instance and this environment hold: the one used first, then those that cover the image from most to least specific, then those that do not"`
}

// ResolveOutput wraps ResolveBody for huma.
type ResolveOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ResolveBody
}

// CredentialTestRequest is the body of the credential test routes. The values are the sheet's
// as they stand, saved or not.
type CredentialTestRequest struct {
	CredentialID   string  `json:"credentialId,omitempty" maxLength:"64" doc:"A stored credential of this layer to test. A field left blank is taken from it, and with no secret sent the stored secret is tried. When nothing is changed from what is stored, the result is remembered on the credential. 404 credential_not_found for one of another layer or environment"`
	Type           string  `json:"type,omitempty" maxLength:"32" doc:"dockerhub, ghcr, gitlab or generic, as when adding; 422 type_invalid"`
	Host           string  `json:"host,omitempty" maxLength:"512" doc:"The registry host, as when adding; 422 host_required or host_invalid"`
	Prefix         string  `json:"prefix,omitempty" maxLength:"512" doc:"The namespace the credential covers. Recorded with the test; it does not change what is tried"`
	Username       string  `json:"username,omitempty" maxLength:"512" doc:"The registry user name; 422 username_required"`
	Secret         string  `json:"secret,omitempty" maxLength:"4096" doc:"The password or token to try. It is used for this one test, never stored by it, and never returned or recorded. 422 secret_required when there is none to try"`
	TestRepository string  `json:"testRepository,omitempty" maxLength:"512" doc:"A repository to try to read with the sign-in, like acme/app. Without one, the last step is not checked. 422 test_repository_invalid"`
	CAPEM          *string `json:"caPem,omitempty" maxLength:"262144" doc:"The custom certificate authority to trust for this test, in PEM. Left out, a stored credential's own is used; an empty string means none. 422 ca_invalid"`
	Insecure       *bool   `json:"insecure,omitempty" doc:"Whether to skip certificate verification and allow plain HTTP for this test. Left out, a stored credential's own switch is used"`
}

// TestCredentialInput is the request of the instance test route.
type TestCredentialInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      CredentialTestRequest
}

// TestEnvCredentialInput is the request of the environment test route.
type TestEnvCredentialInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      CredentialTestRequest
}

// TestStepBody is the outcome of one step of a credential test.
type TestStepBody struct {
	State string `json:"state" enum:"passed,failed,not_checked" doc:"passed, failed, or not_checked when the step did not apply or an earlier step failed"`
	Code  string `json:"code" doc:"Why, as a stable code: registry_unreachable, registry_tls, registry_http, sign_in_refused, no_login_asked, no_pull_access, repository_not_found, rate_limited or check_failed. Empty when there is nothing to say. No text from the registry is ever returned"`
}

// CredentialTestResultBody is the three steps of a credential test.
type CredentialTestResultBody struct {
	Reachable TestStepBody `json:"reachable" doc:"Whether the registry answered"`
	SignedIn  TestStepBody `json:"signedIn" doc:"Whether it accepted the sign-in. not_checked with no_login_asked when the registry asks for no login"`
	CanPull   TestStepBody `json:"canPull" doc:"Whether the sign-in can read the test repository. not_checked without one"`
	TestedAt  time.Time    `json:"testedAt" format:"date-time" doc:"When the test ran"`
}

// CredentialTestOutput wraps CredentialTestResultBody for huma.
type CredentialTestOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CredentialTestResultBody
}

// CredentialTestFromEnvRequest is the body of POST /api/env/{envId}/registries/test-from-env: the
// values of a credential test, as the sheet has them, and the image to try to read.
type CredentialTestFromEnvRequest struct {
	CredentialTestRequest
	Repository string `json:"repository,omitempty" maxLength:"512" doc:"The repository to read through the environment's node, like acme/app. Required: the engine needs a concrete image, so 422 repository_required without one (testRepository is taken in its place). 422 test_repository_invalid"`
	Tag        string `json:"tag,omitempty" maxLength:"128" doc:"The tag to read. Left out: latest when the registry lists it, else the newest listed tag, else latest. 422 tag_invalid"`
}

// InstanceCredentialTestFromEnvRequest is the body of POST /api/registries/test-from-env.
type InstanceCredentialTestFromEnvRequest struct {
	CredentialTestFromEnvRequest
	EnvID string `json:"envId" minLength:"1" maxLength:"64" doc:"The environment whose node makes the check. 404 environment_not_found when there is none"`
}

// TestInstanceCredentialFromEnvInput is the request of the instance test-from-environment route.
type TestInstanceCredentialFromEnvInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      InstanceCredentialTestFromEnvRequest
}

// TestEnvCredentialFromEnvInput is the request of the environment test-from-environment route.
type TestEnvCredentialFromEnvInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      CredentialTestFromEnvRequest
}

// CredentialTestFromEnvBody is what one node made of a credential.
type CredentialTestFromEnvBody struct {
	NodeID     string    `json:"nodeId" doc:"The node that made the check, by the ID the node lists use"`
	Node       string    `json:"node" doc:"The node's name, for the sentence that names it"`
	State      string    `json:"state" enum:"passed,failed" doc:"passed: the node read the manifest with the credential. failed: it could not; code says why"`
	Code       string    `json:"code" doc:"Why it failed, as a stable code: pull_denied, pull_unauthorized, image_not_found, invalid_reference, rate_limited, registry_unreachable, registry_tls, registry_http, registry_http_fallback (the engine retried over plain HTTP and the registry spoke TLS, which hides the real answer), cancelled, pull_failed, agent_policy_denied (the node's agent is too old for this check) or node_unavailable (the node did not answer). Empty when it passed. No text from the registry or the engine is ever returned"`
	Digest     string    `json:"digest" doc:"The digest of the manifest the node read; empty when it failed"`
	Platforms  []string  `json:"platforms" doc:"The platforms of the image, like linux/amd64; empty when it failed"`
	Repository string    `json:"repository" doc:"The repository that was tried, with its registry host, like ghcr.io/acme/app"`
	Tag        string    `json:"tag" doc:"The tag that was read: the one asked for, else latest when the registry lists it, else the newest tag it lists, else latest"`
	Ref        string    `json:"ref" doc:"The image reference that was read, host/repository:tag, like ghcr.io/acme/app:1.4"`
	TestedAt   time.Time `json:"testedAt" format:"date-time" doc:"When the check ran"`
}

// CredentialTestFromEnvOutput wraps CredentialTestFromEnvBody for huma.
type CredentialTestFromEnvOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CredentialTestFromEnvBody
}

// tagPattern is what an image tag may be (the reference grammar's).
var tagPattern = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)

// nonNil is s, or an empty slice for nil, so a list is [] in JSON and never null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// checkNode is the node of an environment that makes a credential check: a connected manager
// on a swarm, else the first connected node by name (a standalone host has one).
func checkNode(agg envs.Aggregate) (envs.NodeRef, bool) {
	nodes := slices.Clone(agg.Nodes)
	slices.SortFunc(nodes, func(a, b envs.NodeRef) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	for _, n := range nodes {
		if n.Role == "manager" {
			return n, true
		}
	}
	if len(nodes) > 0 {
		return nodes[0], true
	}
	return envs.NodeRef{}, false
}

// checkErrorCode maps the error of a node's distribution check to the code the SPA words.
func checkErrorCode(err error) string {
	var denied *envs.AgentPolicyDeniedError
	switch {
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound), errors.Is(err, envs.ErrOffline),
		errors.Is(err, envs.ErrUnsupported), errors.Is(err, tunnel.ErrAgentGone):
		return problemNodeUnavailable
	case errors.As(envs.ClassifyDockerError(err), &denied):
		return problemAgentPolicyDenied
	}
	return dockerx.PullFailure(err)
}

// testLimiter is the per-user limit on credential tests, shared by both route families.
type testLimiter struct {
	mu sync.Mutex
	by map[string]*rate.Limiter
}

func (l *testLimiter) allow(user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string]*rate.Limiter{}
	}
	lim, ok := l.by[user]
	if !ok {
		lim = rate.NewLimiter(rate.Every(time.Minute/testsPerMinute), testsPerMinute)
		l.by[user] = lim
	}
	return lim.Allow()
}

// testFields are the fields of a submission a test looks at.
var testFields = []string{"type", "host", "prefix", "username", "secret", "caPem"}

// validateTest checks the values of a test as adding a credential would, for the fields a
// test uses, and returns them cleaned together with the repository to read. keepSecret says
// the stored secret will be used, so none has to be typed.
func validateTest(req CredentialTestRequest, keepSecret bool, now time.Time) (registry.CreateInput, string, *registry.ValidationError) {
	in := registry.CreateInput{
		Name: "test", Type: req.Type, Host: req.Host, Prefix: req.Prefix, Username: req.Username, Secret: req.Secret,
		HubFallback: req.Type == registry.TypeDockerHub, CAPEM: deref(req.CAPEM), Insecure: deref(req.Insecure),
	}
	if keepSecret && strings.TrimSpace(in.Secret) == "" {
		in.Secret = "placeholder"
	}
	clean, fields := registry.Validate(in, now)
	if keepSecret && strings.TrimSpace(req.Secret) == "" {
		clean.Secret = ""
	}
	var bad []registry.FieldError
	for _, f := range fields {
		if slices.Contains(testFields, f.Field) {
			bad = append(bad, f)
		}
	}
	repo, ok := registry.TestRepositoryPath(clean.Host, req.TestRepository)
	if !ok {
		bad = append(bad, registry.FieldError{Field: "testRepository", Code: "test_repository_invalid"})
	}
	if len(bad) > 0 {
		return clean, "", &registry.ValidationError{Fields: bad}
	}
	return clean, repo, nil
}

// deref is the value a pointer holds, or the zero value when it is nil.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func stepBody(s registry.StepResult) TestStepBody { return TestStepBody{State: s.State, Code: s.Code} }

// credentialViewer is what one caller may see and manage.
type credentialViewer struct {
	global    []rbac.Perm
	perEnv    map[string][]rbac.Perm
	anyImages bool
}

func (v credentialViewer) holds(envID string, p rbac.Perm) bool {
	return slices.Contains(v.perEnv[envID], p) || slices.Contains(v.perEnv[rbac.AllEnvironments], p)
}

// admin reports an instance admin, who holds every global permission.
func (v credentialViewer) admin() bool { return slices.Contains(v.global, rbac.PermRegistriesInstance) }

// sees is D-09: an instance admin sees everything; anyone who can read images on an
// environment sees the instance credentials, and an environment's own credentials when they
// can read that environment. A caller who reads no environment sees none.
func (v credentialViewer) sees(c registry.Credential) bool {
	if v.admin() {
		return true
	}
	if c.EnvID == "" {
		return v.anyImages
	}
	return v.holds(c.EnvID, rbac.PermImagesRead)
}

// canManage is D-30: an instance admin manages every credential, an environment's Admin
// (registries.manage on it) manages that environment's credentials, and nobody else manages
// anything: the instance layer is for instance admins alone.
func (v credentialViewer) canManage(c registry.Credential) bool {
	if c.EnvID == "" {
		return v.admin()
	}
	return v.admin() || v.holds(c.EnvID, rbac.PermRegistriesManage)
}

// credentialVisibility answers, for one caller, whether each credential is visible to them and
// whether they may manage it (D-09, D-30), from the same effective permissions the policy
// enforces with. A credential that is not visible is answered exactly like one that does not
// exist, and one that is visible but not manageable is read-only to them.
func credentialVisibility(ctx context.Context, d RegistriesDeps, p *rbac.Principal) (func(registry.Credential) (visible, canManage bool), error) {
	v, err := viewerOf(ctx, d.Checker, p)
	if err != nil {
		return nil, err
	}
	return func(c registry.Credential) (bool, bool) { return v.sees(c), v.canManage(c) }, nil
}

// viewerOf reads what one caller may do, from the same effective permissions the policy
// enforces with.
func viewerOf(ctx context.Context, checker *rbac.Checker, p *rbac.Principal) (credentialViewer, error) {
	global, perEnv, err := checker.EffectivePermissions(ctx, p)
	if err != nil {
		return credentialViewer{}, err
	}
	v := credentialViewer{global: global, perEnv: perEnv}
	for _, perms := range perEnv {
		if slices.Contains(perms, rbac.PermImagesRead) {
			v.anyImages = true
			break
		}
	}
	return v, nil
}

// RegisterRegistries registers the registry credential routes that are not in an environment's
// path: create an instance credential, list and read the credentials the caller may see.
// The routes are always registered; without a Store they answer 503 registries_unavailable.
func RegisterRegistries(r *httpx.Router, d RegistriesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	// failIn words a refusal for the layer it happened in: the instance's or one environment's.
	failIn := func(inEnv bool, what string, err error) error {
		var ve *registry.ValidationError
		holder := "instance credential"
		if inEnv {
			holder = "credential of this environment"
		}
		switch {
		case errors.As(err, &ve):
			return credentialFieldProblems(ve)
		case errors.Is(err, registry.ErrNameTaken):
			return conflictProblem(err, problemCredentialNameTaken, "Another "+holder+" already has that name.")
		case errors.Is(err, registry.ErrHostPrefixTaken):
			return conflictProblem(err, problemHostPrefixTaken, "Another "+holder+" already covers that registry and namespace.")
		case errors.Is(err, registry.ErrHubFallbackTaken):
			return conflictProblem(err, problemHubFallbackTaken, "Another "+holder+" is already used for all Docker Hub pulls.")
		case errors.Is(err, registry.ErrNotFound):
			return httpx.Problem(http.StatusNotFound, problemCredentialNotFound, "credential not found")
		}
		d.Log.Error("registry credential request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	fail := func(what string, err error) error { return failIn(false, what, err) }
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemRegistriesUnavailable, "Registry credentials are not available on this server.")
	}
	// create stores a credential of the instance layer (envID empty) or of one environment, and
	// writes its audit row in the same transaction, carrying the environment.
	create := func(ctx context.Context, p *rbac.Principal, envID, userAgent string, req CredentialRequest) (registry.Credential, error) {
		return d.Store.Create(ctx, registry.CreateInput{
			EnvID: envID, Name: req.Name, Type: req.Type, Host: req.Host, Prefix: req.Prefix, Username: req.Username,
			Secret: req.Secret, HubFallback: req.HubFallback, Expires: req.Expires, CAPEM: req.CAPEM, Insecure: req.Insecure,
		}, p.ID, func(ctx context.Context, tx *sql.Tx, c registry.Credential) error {
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialCreated, TargetType: credentialTargetType, TargetID: c.ID,
				EnvID: envID, Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: credentialAuditDetails(c),
			})
		})
	}
	// body is a credential as the browser sees it, with the caller's rights over it.
	body := func(ctx context.Context, vis func(registry.Credential) (bool, bool), c registry.Credential) CredentialBody {
		_, manage := vis(c)
		return credentialBody(c, manage, d.envName(ctx, c.EnvID))
	}
	// lookup reads a credential of the instance layer (envID empty) or of one environment. A
	// credential of any other layer is ErrNotFound, exactly like one that does not exist.
	lookup := func(ctx context.Context, envID, id string) (registry.Credential, error) {
		if envID != "" {
			return d.Store.GetInEnv(ctx, envID, id)
		}
		c, err := d.Store.Get(ctx, id)
		if err == nil && c.EnvID != "" {
			return registry.Credential{}, registry.ErrNotFound
		}
		return c, err
	}
	limiter := &testLimiter{}
	clients := d.Clients
	if clients == nil {
		clients = registry.NewClientCache(d.Egress, d.Version, d.Log)
	}
	// preparedTest is a submission once its stored credential has lent what was left blank.
	type preparedTest struct {
		stored *registry.Credential
		in     registry.CreateInput
		repo   string
		// secret is what the registry is shown: the typed one, else the stored one opened.
		secret string
	}
	// prepare reads the stored credential a submission names, if any, validates the values as
	// adding a credential would and opens the secret to try. The credential must belong to the
	// layer of envID (the instance's when it is empty). The secret goes to the caller and
	// nowhere else.
	prepare := func(ctx context.Context, envID string, req CredentialTestRequest) (*preparedTest, error) {
		var stored *registry.Credential
		if req.CredentialID != "" {
			c, err := lookup(ctx, envID, req.CredentialID)
			if err != nil {
				return nil, failIn(envID != "", "get credential to test", err)
			}
			stored = &c
			req.Type = cmp.Or(req.Type, c.Type)
			req.Host = cmp.Or(req.Host, c.Host)
			req.Username = cmp.Or(req.Username, c.Username)
			// The CA and the switch are lent by the stored credential only when the test did not
			// send them, so a person can try a different CA before saving it.
			if req.CAPEM == nil {
				req.CAPEM = &c.CAPEM
			}
			if req.Insecure == nil {
				req.Insecure = &c.Insecure
			}
		}
		in, repo, ve := validateTest(req, stored != nil, time.Now())
		if ve != nil {
			return nil, credentialFieldProblems(ve)
		}
		secret := in.Secret
		if secret == "" {
			// validateTest only lets this through for a stored credential. The stored secret is
			// lent only to the destination it was stored for: a different host, type, username, CA
			// or insecure switch needs a secret typed now, or a manager could have the server send
			// the stored one to a registry they run (CR-A01, CR-B01).
			if registry.SecretDestinationChanged(*stored, in.Host, in.Type, in.Username, in.CAPEM, in.Insecure) {
				return nil, credentialFieldProblems(&registry.ValidationError{
					Fields: []registry.FieldError{{Field: "secret", Code: "secret_required"}}})
			}
			var err error
			if secret, err = d.Store.Secret(ctx, stored.ID); err != nil {
				return nil, failIn(envID != "", "open the stored secret to test", err)
			}
		}
		return &preparedTest{stored: stored, in: in, repo: repo, secret: secret}, nil
	}
	// runTest tries the sign-in of a submission against its registry, from this server, and
	// audits it. The submission names a stored credential of this layer (credentialId), or it
	// does not. A stored credential fills in what was left blank and, with no secret typed,
	// lends its stored secret, which goes to the registry client and nowhere else. The outcome
	// is stored only when the test ran on the saved values.
	runTest := func(ctx context.Context, p *rbac.Principal, envID, userAgent string, req CredentialTestRequest) (*CredentialTestOutput, error) {
		pt, err := prepare(ctx, envID, req)
		if err != nil {
			return nil, err
		}
		stored, in, repo, secret := pt.stored, pt.in, pt.repo, pt.secret
		if !limiter.allow(p.ID) {
			return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many tests. Wait a minute and try again.")
		}
		// The test is of the saved values when nothing it uses was changed from them.
		saved := stored != nil && req.Secret == "" && in.Type == stored.Type && in.Host == stored.Host && in.Username == stored.Username &&
			in.CAPEM == stored.CAPEM && in.Insecure == stored.Insecure
		opts := registry.HostOptions{Host: in.Host, Username: in.Username, Password: secret, CAPEM: in.CAPEM, Insecure: in.Insecure}
		tctx, cancel := context.WithTimeout(ctx, testBudget)
		defer cancel()
		anon := registry.HostOptions{Host: in.Host, CAPEM: in.CAPEM, Insecure: in.Insecure}
		res := registry.Check(tctx, clients.New(opts), clients.New(anon), registry.CheckInput{
			HostOptions: opts, Type: in.Type, Prefix: in.Prefix, TestRepository: repo,
		})
		layer := registry.LayerInstance
		if envID != "" {
			layer = registry.LayerEnvironment
		}
		details := map[string]string{
			"layer": layer, "host": in.Host, "prefix": in.Prefix, "type": in.Type, "via": "server",
			"reachable": res.Reachable.State, "signed_in": res.SignedIn.State, "can_pull": res.CanPull.State,
			"insecure": strconv.FormatBool(in.Insecure), "custom_ca": strconv.FormatBool(in.CAPEM != ""),
		}
		entry := audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialTested, TargetType: credentialTargetType,
			EnvID: envID, Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
		}
		if stored != nil {
			entry.TargetID = stored.ID
			details["name"] = stored.Name
		}
		if repo != "" {
			details["repository"] = repo
		}
		if res.Failed() {
			entry.Result, entry.Reason = audit.ResultFailure, res.FailureCode()
			details["code"] = entry.Reason
		}
		if err := d.Audit.Record(ctx, entry); err != nil {
			d.Log.Error("could not audit a credential test", "host", in.Host, "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		if saved {
			if err := d.Store.RecordTest(ctx, stored.ID, !res.Failed(), res.FailureCode()); err != nil {
				d.Log.Error("could not store the result of a credential test", "credential", stored.ID, "error", err)
			}
		}
		return &CredentialTestOutput{CacheControl: "no-store", Body: CredentialTestResultBody{
			Reachable: stepBody(res.Reachable), SignedIn: stepBody(res.SignedIn), CanPull: stepBody(res.CanPull), TestedAt: time.Now().UTC(),
		}}, nil
	}
	// runFromEnv tries to read an image's manifest through one node of the environment runEnvID
	// with the submitted credential, which belongs to the layer of credEnvID, and audits it. The
	// node's engine makes the registry call, so a firewall, a DNS name or a certificate authority
	// only the node has is what gets tested (D-13). The answer names the node; the credential
	// travels as the engine's registry header on this one call and is neither kept nor returned.
	runFromEnv := func(ctx context.Context, p *rbac.Principal, credEnvID, runEnvID, userAgent string, req CredentialTestFromEnvRequest) (*CredentialTestFromEnvOutput, error) {
		if d.Aggregator == nil {
			return nil, unavailable()
		}
		req.TestRepository = cmp.Or(strings.TrimSpace(req.Repository), req.TestRepository)
		if strings.TrimSpace(req.TestRepository) == "" {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRepositoryRequired, "Enter a test repository.")
		}
		pt, err := prepare(ctx, credEnvID, req.CredentialTestRequest)
		if err != nil {
			return nil, err
		}
		askedTag := strings.TrimSpace(req.Tag)
		tag := cmp.Or(askedTag, "latest")
		if !tagPattern.MatchString(tag) {
			return nil, credentialFieldProblems(&registry.ValidationError{Fields: []registry.FieldError{{Field: "tag", Code: "tag_invalid"}}})
		}
		ref, perr := registry.ParseRef(pt.in.Host + "/" + pt.repo + ":" + tag)
		if perr != nil || ref.Domain != pt.in.Host || ref.Tag != tag || ref.Digest != "" {
			return nil, credentialFieldProblems(&registry.ValidationError{Fields: []registry.FieldError{{Field: "testRepository", Code: "test_repository_invalid"}}})
		}
		if !limiter.allow(p.ID) {
			return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many tests. Wait a minute and try again.")
		}
		agg, _, err := d.Aggregator.Snapshot(ctx, runEnvID)
		if err != nil {
			return nil, snapshotProblem(d.Log, runEnvID, err)
		}
		node, ok := checkNode(agg)
		if !ok {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "No node of this environment is connected to Bayplan.")
		}
		header := registry.EncodeAuth(pt.in.Username, pt.secret, pt.in.Host)
		nctx, cancel := context.WithTimeout(ctx, testBudget)
		defer cancel()
		var res dockerx.DistributionResult
		code := ""
		refused := d.Egress.RefusesHost(nctx, pt.in.Host)
		if !refused && askedTag == "" {
			// No tag was asked for: read one the registry has. A repository pinned to version tags
			// has no latest, and the engine cannot read a manifest without a concrete tag. The list
			// is made by this server through the egress-guarded client, only for a host the policy
			// allows, and any failure to list keeps latest (G-05-4c, T-05-78).
			opts := registry.HostOptions{Host: pt.in.Host, Username: pt.in.Username, Password: pt.secret, CAPEM: pt.in.CAPEM, Insecure: pt.in.Insecure}
			pctx, pcancel := context.WithTimeout(ctx, tagPickBudget)
			picked, _ := registry.PickTestTag(pctx, clients.New(opts), pt.repo)
			pcancel()
			// A name the registry supplied must parse back to the same host, repository and tag
			// (T-05-79); otherwise latest stays.
			if pref, perr := registry.ParseRef(pt.in.Host + "/" + pt.repo + ":" + picked); perr == nil &&
				pref.Domain == pt.in.Host && pref.Path == ref.Path && pref.Tag == picked && pref.Digest == "" {
				tag, ref = picked, pref
			}
		}
		if refused {
			// The node's engine makes this call, outside the server's egress policy, so a host the
			// policy refuses (cloud metadata, the operator's BAYPLAN_REGISTRY_BLOCK_CIDRS) is not
			// sent to it: it reads as unreachable, as it does from the server (WR-B02).
			code = dockerx.PullCodeRegistryUnreachable
		} else if err = d.Aggregator.OnNode(nctx, runEnvID, node.ID, func(c *dockerx.Client) error {
			var e error
			res, e = c.DistributionInspect(nctx, ref.String(), header)
			return e
		}); err != nil {
			code = checkErrorCode(err)
			if code == dockerx.PullCodeFailed || code == dockerx.PullCodeRegistryHTTPFallback || code == problemNodeUnavailable {
				// The text stays in the server log; the answer carries the code only.
				d.Log.Warn("a credential check from an environment failed", "environment", runEnvID, "node", node.ID, "repository", ref.Repo(), "code", code, "error", err)
			}
		}
		layer := registry.LayerInstance
		if credEnvID != "" {
			layer = registry.LayerEnvironment
		}
		details := map[string]string{
			"layer": layer, "host": pt.in.Host, "prefix": pt.in.Prefix, "type": pt.in.Type, "via": "env",
			"node": node.ID, "node_name": node.Name, "repository": ref.Repo(), "tag": tag,
		}
		entry := audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialTested, TargetType: credentialTargetType,
			EnvID: runEnvID, Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
		}
		if pt.stored != nil {
			entry.TargetID = pt.stored.ID
			details["name"] = pt.stored.Name
		}
		state := "passed"
		if code != "" {
			state = "failed"
			entry.Result, entry.Reason = audit.ResultFailure, code
			details["code"] = code
		}
		if err := d.Audit.Record(ctx, entry); err != nil {
			d.Log.Error("could not audit a credential check from an environment", "host", pt.in.Host, "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &CredentialTestFromEnvOutput{CacheControl: "no-store", Body: CredentialTestFromEnvBody{
			NodeID: node.ID, Node: node.Name, State: state, Code: code, Digest: res.Digest, Platforms: nonNil(res.Platforms),
			Repository: ref.Repo(), Tag: tag, Ref: ref.String(), TestedAt: time.Now().UTC(),
		}}, nil
	}
	// update changes a credential of one layer and audits it, carrying the environment.
	update := func(ctx context.Context, p *rbac.Principal, envID, id, userAgent string, patch CredentialPatchRequest) (registry.Credential, error) {
		cur, err := lookup(ctx, envID, id)
		if err != nil {
			return registry.Credential{}, err
		}
		hook := func(ctx context.Context, tx *sql.Tx, c registry.Credential, changed []string) error {
			details := credentialAuditDetails(c)
			details["changed"] = strings.Join(changed, ",")
			if slices.Contains(changed, "host") {
				// Where the credential pointed before, so a redirect of it is on record (CR-B01).
				details["host_from"], details["host_to"] = cur.Host, c.Host
			}
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialUpdated, TargetType: credentialTargetType, TargetID: c.ID,
				EnvID: envID, Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
			})
		}
		merged := mergeCredentialPatch(cur, patch)
		var c registry.Credential
		if envID != "" {
			c, _, err = d.Store.UpdateInEnv(ctx, envID, id, merged, hook)
		} else {
			c, _, err = d.Store.Update(ctx, id, merged, hook)
		}
		return c, err
	}
	// remove deletes a credential of one layer and audits it, carrying the environment.
	remove := func(ctx context.Context, p *rbac.Principal, envID, id, userAgent string) error {
		if _, err := lookup(ctx, envID, id); err != nil {
			return err
		}
		hook := func(ctx context.Context, tx *sql.Tx, c registry.Credential) error {
			return d.Audit.RecordTx(ctx, tx, audit.Entry{
				Actor: p.AuditActor(), Action: audit.ActionRegistryCredentialDeleted, TargetType: credentialTargetType, TargetID: c.ID,
				EnvID: envID, Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: credentialAuditDetails(c),
			})
		}
		var err error
		if envID != "" {
			_, err = d.Store.DeleteInEnv(ctx, envID, id, hook)
		} else {
			_, err = d.Store.Delete(ctx, id, hook)
		}
		return err
	}

	httpx.Register(r, huma.Operation{
		OperationID:   "createInstanceCredential",
		Method:        http.MethodPost,
		Path:          "/api/registries",
		Summary:       "Add an instance registry credential",
		DefaultStatus: http.StatusCreated,
		Description: "Stores a sign-in for a registry host and an optional namespace, used for pulls and tag browsing on every environment. The secret is sealed with the vault, " +
			"included in backups and re-wrapped by key rotation, and never returned by any route afterwards: the answer carries only its last 4 characters, and only for a secret of 12 or more characters. " +
			"Needs registries.instance, which only instance admins hold. 422 validation_failed names every field at fault with a stable code (name_required, name_invalid, type_invalid, host_required, host_invalid, " +
			"prefix_invalid, username_required, secret_required, hub_prefix_required, hub_fallback_not_hub, expiry_invalid, expiry_past, ca_invalid); 409 credential_name_taken, host_prefix_taken or hub_fallback_taken " +
			"when another instance credential clashes; the 409 names that credential in its errors list (location conflict.credential). Audited as registry.credential_created without the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *CreateCredentialInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		c, err := create(ctx, p, "", in.UserAgent, in.Body)
		if err != nil {
			return nil, fail("create credential", err)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: credentialBody(c, true, nil)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "createEnvCredential",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/registries",
		Summary:       "Add a registry credential to an environment",
		DefaultStatus: http.StatusCreated,
		Description: "Stores a sign-in that applies to this environment's pulls and tag browsing only, and wins over an instance credential for the same registry host and namespace (D-02). " +
			"Everything else is as adding an instance credential: the secret is sealed with the vault and never returned, only its last 4 characters (12 or more characters). " +
			"Needs registries.manage on the environment (its Admin, or an instance admin); an Operator or Viewer gets 403 and an environment the caller cannot see 404 environment_not_found. " +
			"422 validation_failed names every field at fault with a stable code, as for the instance family; 409 credential_name_taken, host_prefix_taken or hub_fallback_taken when another credential " +
			"of this environment clashes (an instance credential never does). Audited as registry.credential_created with the environment, without the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesManage), func(ctx context.Context, in *CreateEnvCredentialInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		c, err := create(ctx, p, in.EnvID, in.UserAgent, in.Body)
		if err != nil {
			return nil, failIn(true, "create environment credential", err)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: credentialBody(c, true, d.envName(ctx, c.EnvID))}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testInstanceCredential",
		Method:      http.MethodPost,
		Path:        "/api/registries/test",
		Summary:     "Test a registry sign-in from the server",
		Description: "Runs the registry's own flow from the Bayplan server in three steps and reports each one: whether the registry answers, whether it accepts the sign-in, " +
			"and, when a test repository is given, whether the sign-in can read it. Works on values that are not saved. A 401 to the unauthenticated ping counts as reachable; a refused sign-in is " +
			"reported as refused whether the registry says 401 or 403; a registry that asks for no login reports signed in as not_checked. Each step is a state and a code, never text from the registry. " +
			"The server refuses link-local, unspecified and multicast addresses (cloud metadata included) after DNS, allows private and loopback ranges, and refuses redirects to plain HTTP or local URLs. " +
			"Needs registries.instance. At most 10 tests a minute per user across both families: 429 rate_limited. 422 validation_failed names the fields at fault with the codes of adding a credential, " +
			"ca_invalid and test_repository_invalid. caPem and insecure apply the credential's own CA and switch to this test only; the operator's BAYPLAN_REGISTRY_BLOCK_CIDRS ranges stay refused even with the switch on. " +
			"A stored credential's secret is lent only to the host, type, username, CA and insecure switch it was stored with: a test that changes any of them needs a secret typed now (422 secret_required). " +
			"Audited as registry.credential_tested with each step's result, never the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *TestCredentialInput) (*CredentialTestOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		return runTest(ctx, p, "", in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testEnvCredential",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/registries/test",
		Summary:     "Test an environment registry sign-in from the server",
		Description: "As testing an instance sign-in, for the credentials of one environment: the same three steps from the Bayplan server, on unsaved values or on a stored credential of this environment " +
			"(credentialId; 404 credential_not_found for the instance's or another environment's). Needs registries.manage on the environment (its Admin, or an instance admin); an Operator or Viewer gets 403. " +
			"The 10 tests a minute per user are shared with the instance family: 429 rate_limited. The stored secret is lent only to the host, type, username, CA and insecure switch it was stored with (422 secret_required otherwise). " +
			"Audited as registry.credential_tested with the environment, never the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesManage), func(ctx context.Context, in *TestEnvCredentialInput) (*CredentialTestOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		return runTest(ctx, p, in.EnvID, in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testInstanceCredentialFromEnv",
		Method:      http.MethodPost,
		Path:        "/api/registries/test-from-env",
		Summary:     "Test a registry sign-in from an environment's node",
		Description: "Asks one node of the environment envId to read the manifest of repository:tag (latest when left out) from the credential's registry with the credential, through its agent and its " +
			"own engine. A firewall, an internal DNS name or a certificate authority that only the node has shows up here and cannot show up in the server's own test. The engine reads the manifest " +
			"with a HEAD, which Docker Hub does not count as a pull. On a swarm the check runs on one connected manager, else on the one node; the answer names the node. Works on values that are not saved, " +
			"or on a stored instance credential (credentialId). The answer is 200 whether the node could pull or not: state is passed or failed, and a failed one carries a code (pull_denied, pull_unauthorized, " +
			"image_not_found, registry_unreachable, registry_tls, registry_http, rate_limited, pull_failed, agent_policy_denied when the node's agent is older than this server, node_unavailable when the node " +
			"did not answer), never any text from the registry. Needs registries.instance. 422 repository_required without a repository; 422 validation_failed names the other fields at fault (tag_invalid, " +
			"test_repository_invalid and those of adding a credential); 404 environment_not_found; 503 node_unavailable when no node of the environment is connected; 502 agent_policy_denied when the " +
			"environment cannot be read. The 10 tests a minute per user are shared with the server tests: 429 rate_limited. The stored secret is lent only to the host, type, username, CA and insecure switch it was stored with (422 secret_required otherwise). A host that is, or resolves on the server to, an address the server's egress policy refuses (cloud metadata, link-local, BAYPLAN_REGISTRY_BLOCK_CIDRS) is not sent to the node: the answer is failed with registry_unreachable. Audited as registry.credential_tested with the environment, the node and via env, never the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *TestInstanceCredentialFromEnvInput) (*CredentialTestFromEnvOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil || d.Envs == nil {
			return nil, unavailable()
		}
		if _, err := d.Envs.Get(ctx, in.Body.EnvID); err != nil {
			if errors.Is(err, envs.ErrNotFound) {
				return nil, httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
			}
			d.Log.Error("could not read the environment of a credential check", "environment", in.Body.EnvID, "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return runFromEnv(ctx, p, "", in.Body.EnvID, in.UserAgent, in.Body.CredentialTestFromEnvRequest)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testEnvCredentialFromEnv",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/registries/test-from-env",
		Summary:     "Test an environment registry sign-in from its own node",
		Description: "As testing an instance sign-in from an environment, for the credentials of one environment and run on that environment's own node: a connected manager on a swarm, else its one node. " +
			"A stored credential (credentialId) must be this environment's own: 404 credential_not_found for the instance's or another environment's. Needs registries.manage on the environment " +
			"(its Admin, or an instance admin); an Operator or Viewer gets 403. The 10 tests a minute per user are shared with the other test routes: 429 rate_limited. Audited as " +
			"registry.credential_tested with the environment, the node and via env, never the secret. A host that the server's egress policy refuses is not sent to the node: failed with registry_unreachable.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesManage), func(ctx context.Context, in *TestEnvCredentialFromEnvInput) (*CredentialTestFromEnvOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		return runFromEnv(ctx, p, in.EnvID, in.EnvID, in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateInstanceCredential",
		Method:      http.MethodPatch,
		Path:        "/api/registries/{credentialId}",
		Summary:     "Change an instance registry credential",
		Description: "Changes the members that are sent and keeps the rest. The secret is write-only: leave it out or send it empty to keep the stored one, which is neither read nor returned; " +
			"send a new one and it is sealed again and replaces it, and the answer carries only its last 4 characters (12 or more characters). Leaving it out keeps the stored one only while the host, type, username, " +
			"CA and insecure switch stay as stored: changing any of them needs a secret typed now (422 validation_failed, secret_required on body.secret), so the stored secret never goes to a destination it was not stored for. There is no scope: a credential never moves between layers. " +
			"Needs registries.instance. A credential of an environment answers 404 credential_not_found here, exactly like one that does not exist. 422 validation_failed names every field at fault " +
			"with the same codes as adding; 409 credential_name_taken, host_prefix_taken or hub_fallback_taken when another instance credential clashes, naming it in its errors list (location conflict.credential). " +
			"Audited as registry.credential_updated with a changed detail listing the names of the fields that changed, the secret only as the word secret, and host_from and host_to when the host changed.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *UpdateCredentialInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		// This family manages the instance layer only. An environment's credential is not
		// reachable through it, whatever the caller holds.
		c, err := update(ctx, p, "", in.CredentialID, in.UserAgent, in.Body)
		if err != nil {
			return nil, fail("update credential", err)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: credentialBody(c, true, nil)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateEnvCredential",
		Method:      http.MethodPatch,
		Path:        "/api/env/{envId}/registries/{credentialId}",
		Summary:     "Change an environment's registry credential",
		Description: "Changes the members that are sent and keeps the rest, as for an instance credential: the secret is write-only (left out or empty keeps the stored one only while the host, type, username, CA and insecure switch stay as stored; changing any of them is 422 secret_required), and there is no scope. " +
			"Needs registries.manage on the environment (its Admin, or an instance admin). A credential of another environment, and one of the instance layer, answer 404 credential_not_found here, " +
			"exactly like one that does not exist, and nothing changes. 422 validation_failed names every field at fault; 409 credential_name_taken, host_prefix_taken or hub_fallback_taken when " +
			"another credential of this environment clashes, naming it in its errors list (location conflict.credential). Audited as registry.credential_updated with the environment and a changed detail " +
			"listing the names of the fields that changed, the secret only as the word secret, and host_from and host_to when the host changed.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesManage), func(ctx context.Context, in *UpdateEnvCredentialInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		c, err := update(ctx, p, in.EnvID, in.CredentialID, in.UserAgent, in.Body)
		if err != nil {
			return nil, failIn(true, "update environment credential", err)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: credentialBody(c, true, d.envName(ctx, c.EnvID))}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteInstanceCredential",
		Method:        http.MethodDelete,
		Path:          "/api/registries/{credentialId}",
		Summary:       "Delete an instance registry credential",
		DefaultStatus: http.StatusNoContent,
		Description: "Removes the credential and its sealed secret. Pulls it would have matched fall back to the next matching credential, or to anonymous; nothing else refers to it. " +
			"Needs registries.instance. A credential that is already gone, and one of an environment, answer 404 credential_not_found, so a repeated delete is safe and writes nothing. " +
			"Audited as registry.credential_deleted without the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesInstance), func(ctx context.Context, in *DeleteCredentialInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		if err := remove(ctx, p, "", in.CredentialID, in.UserAgent); err != nil {
			return nil, fail("delete credential", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteEnvCredential",
		Method:        http.MethodDelete,
		Path:          "/api/env/{envId}/registries/{credentialId}",
		Summary:       "Delete an environment's registry credential",
		DefaultStatus: http.StatusNoContent,
		Description: "Removes the credential and its sealed secret. Pulls of this environment it would have matched fall back to the next matching credential, the instance's included, or to anonymous. " +
			"Needs registries.manage on the environment (its Admin, or an instance admin). A credential of another environment, one of the instance layer, and one that is already gone answer " +
			"404 credential_not_found, so a repeated delete is safe and writes nothing. Audited as registry.credential_deleted with the environment, without the secret.",
		Tags: []string{"registries"},
	}, rbac.Require(rbac.PermRegistriesManage), func(ctx context.Context, in *DeleteEnvCredentialInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		if err := remove(ctx, p, in.EnvID, in.CredentialID, in.UserAgent); err != nil {
			return nil, failIn(true, "delete environment credential", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listCredentials",
		Method:      http.MethodGet,
		Path:        "/api/registries",
		Summary:     "List the registry credentials the signed-in user may see",
		Description: "An instance admin sees every credential. Anyone who holds images.read on at least one environment sees the instance credentials, and an environment's own credentials " +
			"when they can read that environment; canManage says whether they may change a credential. A caller with no images.read anywhere gets an empty list. No secret is ever included.",
		Tags: []string{"registries"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*CredentialListOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		vis, err := credentialVisibility(ctx, d, p)
		if err != nil {
			return nil, fail("effective permissions", err)
		}
		all, err := d.Store.List(ctx)
		if err != nil {
			return nil, fail("list credentials", err)
		}
		items := make([]CredentialBody, 0, len(all))
		for _, c := range all {
			if visible, _ := vis(c); visible {
				items = append(items, body(ctx, vis, c))
			}
		}
		return &CredentialListOutput{CacheControl: "no-store", Body: CredentialListBody{Items: items}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listCredentialAlerts",
		Method:      http.MethodGet,
		Path:        "/api/registries/alerts",
		Summary:     "List the registry credentials that need attention",
		Description: "Derived from live state, never stored, so an item is there exactly while its cause is: a credential that expires within 7 days (expiring), " +
			"whose expiry day has passed (expired) or that its registry refused on the last use and that has not worked since (auth_failed). An expiry day counts " +
			"as the end of that day in UTC, and the 7-day warning and expired use that same instant. Only credentials the caller may manage are listed: an " +
			"instance admin gets instance and environment credentials, an Env Admin the credentials of the environments they administer, and everyone else an " +
			"empty list. A credential with several problems has one item, the most severe. Nothing is renewed automatically.",
		Tags: []string{"registries"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*CredentialAlertsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		items, err := credentialAlerts(ctx, d, p, now())
		if err != nil {
			return nil, fail("credential alerts", err)
		}
		return &CredentialAlertsOutput{CacheControl: "no-store", Body: CredentialAlertsBody{Items: items}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "resolveCredential",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/registries/resolve",
		Summary:     "Which registry credential a pull of an image would use",
		Description: "Answers with the same function pulls choose a credential with: the exact registry host, the longest whole-segment namespace prefix across the instance credentials and this " +
			"environment's, the environment's credential winning a tie, and a Docker Hub fallback last. The answer names the credential and why, and lists the other credentials for the registry " +
			"with what became of each. Only the instance layer and this environment's layer are consulted, and no secret is ever returned. An unparsable reference is 422 ref_invalid. Needs images.read " +
			"on the environment. Not audited: it reads nothing but the choice.",
		Tags: []string{"registries"},
	}, rbac.Requirement{Perm: rbac.PermImagesRead, Scope: rbac.ScopeEnvironment}, func(ctx context.Context, in *ResolveInput) (*ResolveOutput, error) {
		if d.Store == nil {
			return nil, unavailable()
		}
		ref, err := registry.ParseRef(in.Body.Ref)
		if err != nil {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRefInvalid, "Enter an image like nginx:1.27 or ghcr.io/acme/app:latest.")
		}
		cands, err := d.Store.Candidates(ctx, in.EnvID)
		if err != nil {
			return nil, fail("list candidates", err)
		}
		return &ResolveOutput{CacheControl: "no-store", Body: resolveBody(strings.TrimSpace(in.Body.Ref), ref, registry.Select(cands, ref))}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getCredential",
		Method:      http.MethodGet,
		Path:        "/api/registries/{credentialId}",
		Summary:     "Read one registry credential",
		Description: "The credential as the list shows it. One the caller may not see answers exactly like one that does not exist: 404 credential_not_found.",
		Tags:        []string{"registries"},
	}, rbac.SelfService, func(ctx context.Context, in *CredentialPathInput) (*CredentialOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, unavailable()
		}
		vis, err := credentialVisibility(ctx, d, p)
		if err != nil {
			return nil, fail("effective permissions", err)
		}
		c, err := d.Store.Get(ctx, in.CredentialID)
		if err != nil {
			return nil, fail("get credential", err)
		}
		if visible, _ := vis(c); !visible {
			return nil, fail("get credential", registry.ErrNotFound)
		}
		return &CredentialOutput{CacheControl: "no-store", Body: body(ctx, vis, c)}, nil
	})
}

// credentialAlerts is the bell's registry items for one caller: the alerts of the credentials
// they can manage (D-07, D-30), so nobody hears about a credential they cannot fix (T-05-69).
func credentialAlerts(ctx context.Context, d RegistriesDeps, p *rbac.Principal, now time.Time) ([]CredentialAlertBody, error) {
	vis, err := credentialVisibility(ctx, d, p)
	if err != nil {
		return nil, err
	}
	all, err := d.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	manageable := make([]registry.Credential, 0, len(all))
	for _, c := range all {
		if visible, canManage := vis(c); visible && canManage {
			manageable = append(manageable, c)
		}
	}
	items := make([]CredentialAlertBody, 0)
	for _, a := range registry.Alerts(now, manageable) {
		scope := CredentialScopeBody{Kind: a.Layer}
		if a.EnvID != "" {
			id := a.EnvID
			scope.EnvID, scope.EnvName = &id, d.envName(ctx, a.EnvID)
		}
		items = append(items, CredentialAlertBody{
			Kind: string(a.Kind), CredentialID: a.CredentialID, Name: a.Name, Host: a.Host, Prefix: a.Prefix,
			Scope: scope, Days: a.Days, At: a.At.UTC(),
		})
	}
	return items, nil
}

// resolveBody maps the resolver's answer to the body of the resolve route.
func resolveBody(input string, ref registry.Ref, m registry.Match) ResolveBody {
	b := ResolveBody{Input: input, Normalized: ref.String(), MatchedOn: m.On, Considered: make([]ConsideredCredentialBody, 0, len(m.Considered))}
	if m.Cred != nil {
		b.Credential = Some(ResolvedCredentialBody{
			ID: m.Cred.ID, Name: m.Cred.Name, Scope: candidateScope(*m.Cred), Host: m.Cred.Host, Prefix: m.Cred.Prefix,
		})
	}
	for _, c := range m.Considered {
		b.Considered = append(b.Considered, ConsideredCredentialBody{
			CredentialID: c.Candidate.ID, Name: c.Candidate.Name, Scope: candidateScope(c.Candidate), Matches: c.Matches, Result: c.Result,
		})
	}
	return b
}

// candidateScope says where a candidate applies.
func candidateScope(c registry.Candidate) CredentialScopeBody {
	s := CredentialScopeBody{Kind: c.Layer}
	if c.EnvID != "" {
		id, name := c.EnvID, c.EnvName
		s.EnvID, s.EnvName = &id, &name
	}
	return s
}

// mergeCredentialPatch lays the members of a patch over the stored credential. The secret is
// not part of the stored credential at all: it is the replacement, or empty to keep the one
// that is stored.
func mergeCredentialPatch(cur registry.Credential, patch CredentialPatchRequest) registry.UpdateInput {
	in := registry.UpdateInput{
		Name: cur.Name, Type: cur.Type, Host: cur.Host, Prefix: cur.Prefix, Username: cur.Username, HubFallback: cur.HubFallback,
		CAPEM: cur.CAPEM, Insecure: cur.Insecure,
	}
	if cur.ExpiresAt != nil {
		in.Expires = cur.ExpiresAt.UTC().Format(time.DateOnly)
	}
	for dst, src := range map[*string]*string{
		&in.Name: patch.Name, &in.Type: patch.Type, &in.Host: patch.Host, &in.Prefix: patch.Prefix,
		&in.Username: patch.Username, &in.Secret: patch.Secret, &in.Expires: patch.Expires, &in.CAPEM: patch.CAPEM,
	} {
		if src != nil {
			*dst = *src
		}
	}
	if patch.HubFallback != nil {
		in.HubFallback = *patch.HubFallback
	}
	if patch.Insecure != nil {
		in.Insecure = *patch.Insecure
	}
	return in
}

// conflictProblem answers a clash within the layer as a 409 that names the other credential
// in its errors list, so the page can say which one.
func conflictProblem(err error, ptype, detail string) error {
	m := &huma.ErrorModel{Type: ptype, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict, Detail: detail}
	var ce *registry.ConflictError
	if errors.As(err, &ce) && ce.With != "" {
		m.Errors = []*huma.ErrorDetail{{Location: "conflict.credential", Message: ce.With}}
	}
	return m
}

// envName is the name of an environment, or nil for an instance credential or one that is gone.
func (d RegistriesDeps) envName(ctx context.Context, envID string) *string {
	if envID == "" || d.Envs == nil {
		return nil
	}
	e, err := d.Envs.Get(ctx, envID)
	if err != nil {
		return nil
	}
	return &e.Name
}

// credentialBody maps a stored credential to its body. canManage is the caller's.
func credentialBody(c registry.Credential, canManage bool, envName *string) CredentialBody {
	b := CredentialBody{
		ID: c.ID, Scope: CredentialScopeBody{Kind: c.Layer(), EnvName: envName}, Name: c.Name, Type: c.Type, Host: c.Host,
		Prefix: c.Prefix, Username: c.Username, Last4: c.Last4, HubFallback: c.HubFallback, HasCA: c.CAPEM != "", Insecure: c.Insecure,
		CanManage: canManage, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	if canManage {
		b.CAPEM = c.CAPEM
	}
	if c.EnvID != "" {
		id := c.EnvID
		b.Scope.EnvID = &id
	}
	if c.ExpiresAt != nil {
		s := c.ExpiresAt.UTC().Format(time.RFC3339)
		b.ExpiresAt = &s
	}
	if c.LastTest != nil {
		b.LastTest = Some(CredentialTestBody{At: c.LastTest.At, OK: c.LastTest.OK, Detail: c.LastTest.Detail})
	}
	if c.LastFailure != nil {
		b.LastFailure = Some(CredentialFailureBody{At: c.LastFailure.At, Code: c.LastFailure.Code})
	}
	return b
}

// credentialAuditDetails is what the audit row of a credential change records: what was stored,
// never the secret or any part of it (T-05-23).
func credentialAuditDetails(c registry.Credential) map[string]string {
	expires := ""
	if c.ExpiresAt != nil {
		expires = c.ExpiresAt.UTC().Format(time.DateOnly)
	}
	return map[string]string{
		"name": c.Name, "layer": c.Layer(), "host": c.Host, "prefix": c.Prefix, "type": c.Type,
		"username": c.Username, "hub_fallback": strconv.FormatBool(c.HubFallback), "expires": expires,
		"insecure": strconv.FormatBool(c.Insecure), "custom_ca": strconv.FormatBool(c.CAPEM != ""),
	}
}

// credentialFieldProblems answers a refused submission as one 422 validation_failed that names
// every field at fault, with its code as the message (the setup form's convention).
func credentialFieldProblems(ve *registry.ValidationError) error {
	problems := make([]*huma.ErrorDetail, len(ve.Fields))
	for i, f := range ve.Fields {
		problems[i] = &huma.ErrorDetail{Location: "body." + f.Field, Message: f.Code}
	}
	return &huma.ErrorModel{
		Type: problemValidationFailed, Title: http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity, Detail: "validation failed", Errors: problems,
	}
}
