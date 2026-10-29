// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// problemGitUnavailable is the problem type of a server built without git support.
const problemGitUnavailable = "git_unavailable"

// namesShown is how many branch names and tag names a test answers with. The counts say how many
// there are.
const namesShown = 20

// GitDeps is what the git routes need.
type GitDeps struct {
	// Git is the client the tests list a repository with. Nil answers 503 git_unavailable.
	Git gitx.Client
	// Sync reads the files of a stack out of a repository folder. Nil answers 503 git_unavailable on the
	// create-from-repository route.
	Sync *gitsync.Service
	// Conns keeps the git credentials and repositories. Nil answers 503 git_unavailable on their
	// routes, so the routes are always registered.
	Conns *gitconn.Store
	// Envs names the environment of an environment connection.
	Envs *envs.Service
	// Audit records every change and every test that signs in with a credential.
	Audit *audit.Writer
	// Checker decides who may see and manage which connections.
	Checker *rbac.Checker
	Log     *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Access registers the owner of a webhook in the transaction that creates it. Nil answers 503 git_unavailable on the
	// create-webhook route.
	Access *access.Service
	// Instance supplies the public URL a webhook address is built on. Nil, or no public URL set, builds the address on
	// the origin the request came to.
	Instance *instance.Instance
	// OnCheck, when set, is called when the sync check a request started has ended, with the stack, what it found and
	// the error of Bayplan's own storage if there was one. Tests use it to know that a check is over.
	OnCheck func(stackID string, outcome gitsync.Outcome, err error)
}

// GitTestRequest is the body of a repository test: an address, for a server with a private
// certificate authority that authority, and optionally the saved credential to sign in with. It
// never carries a token: a credential is picked by ID.
type GitTestRequest struct {
	URL          string `json:"url" maxLength:"2048" doc:"The repository address: https://host/path.git, ssh://git@host/path.git or git@host:path.git. 422 url_unsupported for any other scheme, a path, file://, git:// or http://; 422 url_userinfo for an address with a user name and password in it"`
	CAPEM        string `json:"caPem,omitempty" maxLength:"262144" doc:"A certificate authority to trust for this test, in PEM, for a self-hosted server with its own. It is used for this one test. 422 ca_invalid when it is not a PEM certificate; 422 ca_too_large over 64 KiB. There is no option to skip certificate verification"`
	CredentialID string `json:"credentialId,omitempty" maxLength:"64" doc:"The saved git credential to sign in with. It must be one a repository of this scope may use: the instance's, or this environment's on the environment route (422 credential_scope otherwise). Nothing is ever matched by address. A test that signs in is audited as git.repo_tested"`
	HostKey      string `json:"hostKey,omitempty" maxLength:"4096" doc:"For an ssh address, the host key the person confirmed after an earlier test answered hostKey.state untrusted, in authorized_keys form (hostKey.key of that answer). It is treated as trusted for this one test only and saved nowhere; no credential is ever sent to a host whose key is not trusted. 422 host_key_invalid when it is not one public key"`
}

// TestGitAddressInput is the request of the instance test route.
type TestGitAddressInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      GitTestRequest
}

// TestGitAddressEnvInput is the request of the environment test route.
type TestGitAddressEnvInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      GitTestRequest
}

// GitTestStepBody is the outcome of one step of a repository test.
type GitTestStepBody struct {
	State string `json:"state" enum:"passed,failed,not_checked,not_needed" doc:"passed, failed, not_checked when an earlier step failed, or not_needed when the step did not apply (no sign-in was asked for)"`
}

// GitTestStepsBody is the three steps of a repository test.
type GitTestStepsBody struct {
	Reachable GitTestStepBody `json:"reachable"`
	SignedIn  GitTestStepBody `json:"signedIn"`
	Refs      GitTestStepBody `json:"refs"`
}

// GitTestFailureBody is why a test failed: a fixed code and the host it was made to, never text
// from the server or from the network.
type GitTestFailureBody struct {
	Code string `json:"code" enum:"unreachable,auth_refused,not_found,cert_untrusted,host_key_changed,host_key_untrusted,egress_blocked,ref_missing,timeout,too_large" doc:"Why the test failed"`
	Host string `json:"host" doc:"The host the test was made to"`
}

// GitTestHostKeyBody is what a test of an ssh address found out about the machine it talked to.
type GitTestHostKeyBody struct {
	State            string `json:"state" enum:"none,builtin,trusted,untrusted,changed" doc:"none for an https address, or an ssh host Bayplan holds no key for; builtin when the host is github.com, gitlab.com or bitbucket.org (or their ssh-over-443 names) and its pinned keys matched; trusted when the key a person confirmed matched; untrusted when the host showed a key nobody has trusted yet, so the test stopped before any credential was sent; changed when the host showed a different key than the trusted one, which is never accepted automatically"`
	Type             string `json:"type" doc:"The key type the host showed (ssh-ed25519); empty when it showed none"`
	Fingerprint      string `json:"fingerprint" doc:"The SHA256 fingerprint of the key the host showed (SHA256:...)"`
	Key              string `json:"key" doc:"The key the host showed in authorized_keys form, set when state is untrusted or changed: send it back as hostKey, or to trust-host-key, after comparing the fingerprint"`
	SavedFingerprint string `json:"savedFingerprint" doc:"When state is changed, the fingerprint of the trusted key the new one differs from"`
}

// GitTestResultBody is the answer to a repository test. A test that failed is still a 200: it is a
// result, not an error of the request.
type GitTestResultBody struct {
	Steps         GitTestStepsBody    `json:"steps"`
	Branches      []string            `json:"branches" doc:"The first 20 branch names, in name order"`
	Tags          []string            `json:"tags" doc:"The first 20 tag names, in name order"`
	BranchCount   int                 `json:"branchCount" doc:"How many branches the repository has"`
	TagCount      int                 `json:"tagCount" doc:"How many tags the repository has"`
	DefaultBranch string              `json:"defaultBranch" doc:"The branch HEAD points at; empty when the server does not say"`
	Failure       *GitTestFailureBody `json:"failure" doc:"Set when the test failed; null when it passed"`
	HostKey       GitTestHostKeyBody  `json:"hostKey" doc:"What the test found out about the SSH host key; state none for an https address"`
}

// GitTestOutput is the answer of both test routes.
type GitTestOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitTestResultBody
}

// gitTestOutcome is a finished ls-remote: the answer, and the failure that is nil when it passed.
type gitTestOutcome struct {
	out     *GitTestOutput
	failure *gitx.Error
}

// lsRemote lists the remote and words the answer. signedIn says a credential was used, so the
// sign-in step reads passed rather than not_needed. An error is a failure that is not a *gitx.Error:
// the caller answers 500.
func (d GitDeps) lsRemote(ctx context.Context, rem gitx.Remote, signedIn bool) (gitTestOutcome, error) {
	refs, err := d.Git.LsRemote(ctx, rem)
	out := &GitTestOutput{CacheControl: "no-store", Body: GitTestResultBody{Branches: []string{}, Tags: []string{}}}
	if err != nil {
		var ge *gitx.Error
		if !errors.As(err, &ge) {
			// The text of an unknown error could carry the address or a token: log its type only.
			d.Log.Error("git test failed unexpectedly", "error_type", fmt.Sprintf("%T", err))
			return gitTestOutcome{}, huma.Error500InternalServerError("internal error")
		}
		out.Body.Steps, out.Body.Failure = failedSteps(ge.Code), &GitTestFailureBody{Code: string(ge.Code), Host: ge.Host}
		out.Body.HostKey = hostKeyBody(rem, nil, ge)
		return gitTestOutcome{out: out, failure: ge}, nil
	}
	signIn := "not_needed"
	if signedIn {
		signIn = "passed"
	}
	out.Body.Steps = GitTestStepsBody{
		Reachable: GitTestStepBody{State: "passed"}, SignedIn: GitTestStepBody{State: signIn}, Refs: GitTestStepBody{State: "passed"},
	}
	for _, b := range refs.Branches[:min(len(refs.Branches), namesShown)] {
		out.Body.Branches = append(out.Body.Branches, b.Name)
	}
	for _, t := range refs.Tags[:min(len(refs.Tags), namesShown)] {
		out.Body.Tags = append(out.Body.Tags, t.Name)
	}
	out.Body.BranchCount, out.Body.TagCount, out.Body.DefaultBranch = len(refs.Branches), len(refs.Tags), refs.Default
	out.Body.HostKey = hostKeyBody(rem, refs.HostKey, nil)
	return gitTestOutcome{out: out}, nil
}

// hostKeyBody words what a test found out about the host key. verified is the key a successful test
// checked; failure is the failure of a test that did not pass.
func hostKeyBody(rem gitx.Remote, verified *gitx.PresentedKey, failure *gitx.Error) GitTestHostKeyBody {
	b := GitTestHostKeyBody{State: string(gitx.HostKeyStateOf(rem))}
	switch {
	case failure != nil && failure.Code == gitx.CodeHostKeyUntrusted && failure.Presented != nil:
		b.State, b.Type, b.Fingerprint, b.Key = string(gitx.HostKeyUntrusted), failure.Presented.Type, failure.Presented.Fingerprint, failure.Presented.Line
	case failure != nil && failure.Code == gitx.CodeHostKeyChanged && failure.Presented != nil:
		b.State, b.Type, b.Fingerprint, b.Key = string(gitx.HostKeyChanged), failure.Presented.Type, failure.Presented.Fingerprint, failure.Presented.Line
		b.SavedFingerprint = failure.SavedFingerprint
	case verified != nil:
		b.Type, b.Fingerprint = verified.Type, verified.Fingerprint
	}
	return b
}

// testGitAddress validates a submission and lists the repository's branches and tags from this
// server. It writes nothing (Phase 9 D-04), except that a test that signs in with a saved
// credential is audited: sending a credential to an address is worth a record. envID is the
// environment of the route, empty for the instance's, and decides which credentials may be picked.
func testGitAddress(ctx context.Context, d GitDeps, limiter *testLimiter, userAgent, envID string, req GitTestRequest) (*GitTestOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
	}
	if d.Git == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	if req.CredentialID != "" && d.Conns == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	// The address and the authority are checked before the limiter is spent: a refused submission
	// reaches no server.
	if err := validateGitTest(req); err != nil {
		return nil, err
	}
	rem := gitx.Remote{URL: req.URL, CAPEM: req.CAPEM}
	var cred *gitconn.Credential
	if req.CredentialID != "" {
		r, c, err := d.Conns.RemoteFor(ctx, envID, req.URL, req.CAPEM, req.CredentialID)
		if err != nil {
			return nil, d.gitFail("pick the credential to test with", err)
		}
		rem, cred = r, &c
	}
	if req.HostKey != "" {
		// Trusted for this one call: it is in the Remote and nowhere else.
		rem.HostKeys = []gitx.HostKey{{Line: strings.TrimSpace(req.HostKey)}}
	}
	if !limiter.allow(p.ID) {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many tests. Wait a minute and try again.")
	}
	res, err := d.lsRemote(ctx, rem, cred != nil)
	if err != nil {
		return nil, err
	}
	if cred != nil {
		details := map[string]string{
			"layer": layerOf(envID), "host": hostOf(req.URL), "credential": cred.Name, "credential_layer": cred.Layer(),
		}
		if err := d.auditTest(ctx, p, userAgent, envID, "", details, res.failure); err != nil {
			return nil, err
		}
	}
	return res.out, nil
}

// layerOf is the layer a route works in.
func layerOf(envID string) string {
	if envID == "" {
		return gitconn.LayerInstance
	}
	return gitconn.LayerEnvironment
}

// hostOf is the host of a repository address, empty when it does not parse.
func hostOf(url string) string {
	u, err := gitx.ValidateURL(url)
	if err != nil {
		return ""
	}
	return u.Host
}

// auditTest records a test that signed in with a credential, with the code of its failure.
func (d GitDeps) auditTest(ctx context.Context, p *rbac.Principal, userAgent, envID, repoID string, details map[string]string, failure *gitx.Error) error {
	entry := audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionGitRepoTested, TargetType: gitRepoTargetType, TargetID: repoID, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}
	if failure != nil {
		entry.Result, entry.Reason = audit.ResultFailure, string(failure.Code)
		details["code"] = entry.Reason
	}
	if err := d.Audit.Record(ctx, entry); err != nil {
		d.Log.Error("could not audit a git test", "host", details["host"], "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	return nil
}

// failedSteps says which step a failure stopped at.
func failedSteps(code gitx.Code) GitTestStepsBody {
	failed, skipped := GitTestStepBody{State: "failed"}, GitTestStepBody{State: "not_checked"}
	if code == gitx.CodeAuthRefused {
		return GitTestStepsBody{Reachable: GitTestStepBody{State: "passed"}, SignedIn: failed, Refs: skipped}
	}
	if code == gitx.CodeNotFound || code == gitx.CodeTooLarge {
		return GitTestStepsBody{Reachable: GitTestStepBody{State: "passed"}, SignedIn: GitTestStepBody{State: "not_needed"}, Refs: failed}
	}
	return GitTestStepsBody{Reachable: failed, SignedIn: skipped, Refs: skipped}
}

// validateGitTest answers a 422 naming the field at fault for an address or an authority the
// server refuses to use.
func validateGitTest(req GitTestRequest) error {
	var problems []*huma.ErrorDetail
	if _, err := gitx.ValidateURL(req.URL); err != nil {
		code := "url_unsupported"
		if errors.Is(err, gitx.ErrURLUserinfo) {
			code = "url_userinfo"
		}
		problems = append(problems, &huma.ErrorDetail{Location: "body.url", Message: code})
	}
	switch err := gitx.ValidateCA(req.CAPEM); {
	case errors.Is(err, gitx.ErrCATooLarge):
		problems = append(problems, &huma.ErrorDetail{Location: "body.caPem", Message: "ca_too_large"})
	case err != nil:
		problems = append(problems, &huma.ErrorDetail{Location: "body.caPem", Message: "ca_invalid"})
	}
	if k := strings.TrimSpace(req.HostKey); k != "" {
		if _, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(k)); err != nil || strings.TrimSpace(string(rest)) != "" || strings.ContainsAny(k, "\r\n") {
			problems = append(problems, &huma.ErrorDetail{Location: "body.hostKey", Message: "host_key_invalid"})
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &huma.ErrorModel{
		Type: problemValidationFailed, Title: http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity, Detail: "validation failed", Errors: problems,
	}
}

// RegisterGit registers the git routes: for now, testing a repository address from the server.
func RegisterGit(r *httpx.Router, d GitDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	// One limit across both families: a user who may use both gets 10 tests a minute in all.
	limiter := &testLimiter{}

	httpx.Register(r, huma.Operation{
		OperationID: "testGitAddressInstance",
		Method:      http.MethodPost,
		Path:        "/api/git/repos/test",
		Summary:     "Test a git repository address from the server",
		Description: "Lists the branches and tags of a repository over HTTPS from the Bayplan server, which is where the mirrors live, and answers three steps (reachable, signedIn, refs), " +
			"the first 20 branch names and tag names with their counts, and the default branch. " +
			"It works on values that are not saved and writes nothing: saving a repository never depends on a test. A test that failed is a 200 with failure.code and failure.host, never text from the server or the network. " +
			"The server refuses link-local, unspecified, multicast and cloud-metadata addresses at dial time, redirects included, and allows private and loopback ranges. Certificates are always verified: a server with a private CA " +
			"fails with cert_untrusted unless caPem carries that CA. Needs git.instance. At most 10 tests a minute per user across both families: 429 rate_limited. " +
			"422 validation_failed names url (url_unsupported, url_userinfo) or caPem (ca_invalid, ca_too_large).",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitInstance), func(ctx context.Context, in *TestGitAddressInput) (*GitTestOutput, error) {
		return testGitAddress(ctx, d, limiter, in.UserAgent, "", in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testGitAddress",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/git/repos/test",
		Summary:     "Test a git repository address from the server, for an environment",
		Description: "As testing an address for the instance, for the repositories of one environment. Needs git.manage on the environment (its Admin, or an instance admin); an Operator or Viewer gets 403. " +
			"The 10 tests a minute per user are shared with the instance family: 429 rate_limited. credentialId may name an instance credential or one of this environment (credential_scope otherwise). " +
			"Writes nothing unless it signs in with a credential, which is audited as git.repo_tested.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *TestGitAddressEnvInput) (*GitTestOutput, error) {
		return testGitAddress(ctx, d, limiter, in.UserAgent, in.EnvID, in.Body)
	})

	registerGitCredentials(r, d)
	registerGitCredentialManagement(r, d)
	registerGitRepos(r, d, limiter)
	registerGitRepoManagement(r, d)
	registerGitBrowse(r, d)
}

// GitRepoRequest is the body of adding or changing a git repository.
type GitRepoRequest struct {
	Name         string  `json:"name,omitempty" maxLength:"256" doc:"What the repository is called, unique among the repositories of its scope. 1 to 64 characters: letters, digits, spaces and . _ ( ) -; 422 name_required or name_invalid"`
	URL          string  `json:"url,omitempty" maxLength:"2048" doc:"The repository address: https://host/path.git, ssh://git@host/path.git or git@host:path.git. 422 url_unsupported for any other scheme, a path, file://, git:// or http://; 422 url_userinfo for an address with a user name and password in it"`
	CredentialID *string `json:"credentialId,omitempty" maxLength:"64" doc:"The saved credential to sign in with, picked explicitly: nothing is matched by address. Left out or empty means a public repository when adding. It must be a credential of the repository's own scope or an instance credential for an environment repository, and only an instance credential for an instance repository: 422 credential_scope"`
	CAPEM        string  `json:"caPem,omitempty" maxLength:"262144" doc:"A certificate authority to trust for this repository, in PEM, for a self-hosted server with its own. 422 ca_invalid when it is not a PEM certificate; 422 ca_too_large over 64 KiB. There is no option to skip certificate verification"`
	HostKey      string  `json:"hostKey,omitempty" maxLength:"4096" doc:"For an ssh address, the host key the person confirmed after a test answered hostKey.state untrusted, in authorized_keys form. Saving it records who trusted it and when (audited git.host_key_trusted, or git.host_key_retrusted when it replaces another). Left out keeps the saved key, unless url moves to another host or port, which forgets it. Ignored for github.com, gitlab.com and bitbucket.org, whose keys ship with Bayplan. 422 host_key_invalid when it is not one public key; 422 host_key_not_ssh for an address that is not ssh"`
}

// GitRepoCredentialBody is the credential a repository uses.
type GitRepoCredentialBody struct {
	ID    string              `json:"id" doc:"The credential's ID"`
	Name  string              `json:"name" doc:"Its name"`
	Scope CredentialScopeBody `json:"scope" doc:"Where it can be used"`
}

// GitRepoHostKeyBody is the SSH host key a person trusted for a repository.
type GitRepoHostKeyBody struct {
	Type        string    `json:"type" doc:"The key type (ssh-ed25519)"`
	Fingerprint string    `json:"fingerprint" doc:"Its SHA256 fingerprint (SHA256:...)"`
	TrustedAt   time.Time `json:"trustedAt" format:"date-time" doc:"When a person trusted it"`
}

// GitRepoTestBody is the outcome of the last test of a saved repository.
type GitRepoTestBody struct {
	At   time.Time `json:"at" format:"date-time" doc:"When it was tested"`
	OK   bool      `json:"ok" doc:"Whether the test passed"`
	Code string    `json:"code" doc:"When it failed, the stable code it failed with; empty when it passed"`
}

// GitRepoBody is a git repository as the browser sees it.
type GitRepoBody struct {
	ID         string                          `json:"id" doc:"The repository's ID"`
	Scope      CredentialScopeBody             `json:"scope" doc:"Where it can be used: every environment, or one"`
	Name       string                          `json:"name" doc:"Its name, unique within its scope"`
	URL        string                          `json:"url" doc:"Its address"`
	Host       string                          `json:"host" doc:"The host of its address"`
	Credential Nullable[GitRepoCredentialBody] `json:"credential" doc:"The credential picked for it; null for a public repository"`
	CAPEM      string                          `json:"caPem" doc:"The custom certificate authority in PEM, public material, for people who may manage the repository; empty for everyone else and when there is none"`
	HasCA      bool                            `json:"hasCa" doc:"Whether the repository carries a custom certificate authority, whoever is asking"`
	HostKey    Nullable[GitRepoHostKeyBody]    `json:"hostKey" doc:"The SSH host key a person trusted for it; null when none was (and always for a host with built-in keys, which need none)"`
	LastTest   Nullable[GitRepoTestBody]       `json:"lastTest" doc:"The last test of the saved repository; null when it was never tested"`
	UsedBy     int                             `json:"usedBy" doc:"How many stacks use it"`
	ReadOnly   bool                            `json:"readOnly" doc:"True when the caller may see the repository but not change or delete it"`
	CreatedAt  time.Time                       `json:"createdAt" format:"date-time" doc:"When it was added"`
	UpdatedAt  time.Time                       `json:"updatedAt" format:"date-time" doc:"When it was last changed"`
}

// GitTrustHostKeyRequest is the body of trusting a host key.
type GitTrustHostKeyRequest struct {
	HostKey string `json:"hostKey" maxLength:"4096" doc:"The SSH host key to trust, in authorized_keys form (hostKey.key of a test that answered untrusted or changed), after comparing its fingerprint with the one the host's administrator publishes. 422 host_key_invalid when it is not one public key; 422 host_key_not_ssh for a repository whose address is not ssh"`
}

// TrustGitHostKeyInput is the request of the instance trust-host-key route.
type TrustGitHostKeyInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
	Body      GitTrustHostKeyRequest
}

// TrustGitEnvHostKeyInput is the request of the environment trust-host-key route.
type TrustGitEnvHostKeyInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
	Body      GitTrustHostKeyRequest
}

// GitRepoOutput wraps GitRepoBody for huma.
type GitRepoOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitRepoBody
}

// CreateGitRepoInput is the request of the instance create route.
type CreateGitRepoInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      GitRepoRequest
}

// CreateEnvGitRepoInput is the request of the environment create route.
type CreateEnvGitRepoInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      GitRepoRequest
}

// GitRepoPathInput names one repository.
type GitRepoPathInput struct {
	RepoID string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
}

// GitEnvRepoPathInput names one repository under an environment.
type GitEnvRepoPathInput struct {
	EnvironmentPathInput
	RepoID string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
}

// TestGitRepoInput is the request of the instance test-a-saved-repository route.
type TestGitRepoInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
}

// TestGitEnvRepoInput is the request of the environment test-a-saved-repository route.
type TestGitEnvRepoInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
}

// gitRepoBody maps a stored repository to its body. readOnly is the caller's.
func (d GitDeps) gitRepoBody(ctx context.Context, r gitconn.Repo, readOnly bool) GitRepoBody {
	b := GitRepoBody{
		ID: r.ID, Scope: d.scopeBody(ctx, r.EnvID), Name: r.Name, URL: r.URL, Host: r.Host(), HasCA: r.CAPEM != "",
		UsedBy: r.UsedBy, ReadOnly: readOnly, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if !readOnly {
		b.CAPEM = r.CAPEM
	}
	if r.CredentialID != "" {
		b.Credential = Some(GitRepoCredentialBody{ID: r.CredentialID, Name: r.CredentialName, Scope: d.scopeBody(ctx, r.CredentialEnvID)})
	}
	if r.HostKey != "" && r.HostKeyTrustedAt != nil {
		typ, _, _ := strings.Cut(r.HostKey, " ")
		b.HostKey = Some(GitRepoHostKeyBody{Type: typ, Fingerprint: r.HostKeyFP, TrustedAt: *r.HostKeyTrustedAt})
	}
	if r.LastTest != nil {
		b.LastTest = Some(GitRepoTestBody{At: r.LastTest.At, OK: r.LastTest.OK, Code: r.LastTest.Code})
	}
	return b
}

// gitRepoAuditDetails is what the audit row of a repository change records: its name, its layer, the
// host of its address (never the whole address), the credential it uses by name and layer, and
// whether it has its own CA, so an environment repository that uses an instance credential is
// visible in the log.
func gitRepoAuditDetails(r gitconn.Repo) map[string]string {
	details := map[string]string{
		"name": r.Name, "layer": r.Layer(), "host": r.Host(), "credential": r.CredentialName, "custom_ca": strconv.FormatBool(r.CAPEM != ""),
	}
	if r.CredentialID != "" {
		details["credential_layer"] = gitconn.LayerInstance
		if r.CredentialEnvID != "" {
			details["credential_layer"] = gitconn.LayerEnvironment
		}
	}
	return details
}

// hostKeyAuditDetails is what the audit row of trusting a host key records: the repository, its layer,
// its host and the fingerprint of the key (public material), and the fingerprint it replaced.
func hostKeyAuditDetails(r gitconn.Repo, replaced string) map[string]string {
	details := map[string]string{"name": r.Name, "layer": r.Layer(), "host": r.Host(), "fingerprint": r.HostKeyFP}
	if replaced != "" {
		details["fingerprint_from"] = replaced
	}
	return details
}

// createGitRepo stores a repository of the instance layer (envID empty) or of one environment and
// audits it in the same transaction.
func (d GitDeps) createGitRepo(ctx context.Context, p *rbac.Principal, envID, userAgent string, req GitRepoRequest) (*GitRepoOutput, error) {
	in := gitconn.RepoInput{Name: req.Name, URL: req.URL, CredentialID: req.CredentialID, CAPEM: req.CAPEM, HostKey: req.HostKey}
	if envID != "" {
		in.EnvID = &envID
	}
	r, err := d.Conns.CreateRepo(ctx, in, p.ID, func(ctx context.Context, tx *sql.Tx, r gitconn.Repo) error {
		if err := d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitRepoCreated, gitRepoTargetType, r.ID, envID, gitRepoAuditDetails(r)); err != nil {
			return err
		}
		if r.HostKey == "" {
			return nil
		}
		return d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitHostKeyTrusted, gitRepoTargetType, r.ID, envID, hostKeyAuditDetails(r, ""))
	})
	if err != nil {
		return nil, d.gitFail("create repository", err)
	}
	return &GitRepoOutput{CacheControl: "no-store", Body: d.gitRepoBody(ctx, r, false)}, nil
}

// lookupRepo reads a repository the route may name: one of the layer of envID (the instance's when
// it is empty). A repository of any other layer is answered exactly like one that does not exist.
func (d GitDeps) lookupRepo(ctx context.Context, envID, id string) (gitconn.Repo, error) {
	r, err := d.Conns.GetRepo(ctx, id)
	if err != nil {
		return gitconn.Repo{}, err
	}
	if r.EnvID != envID {
		return gitconn.Repo{}, gitconn.ErrNotFound
	}
	return r, nil
}

// repoFail words an error of the store for a repository route.
func (d GitDeps) repoFail(what string, err error) error {
	if errors.Is(err, gitconn.ErrNotFound) {
		return httpx.Problem(http.StatusNotFound, problemRepositoryNotFound, "repository not found")
	}
	return d.gitFail(what, err)
}

// testSavedRepo tests a saved repository with its credential, from this server: it lists its
// branches and tags, records the outcome on the repository and audits it. A test that failed is
// still a 200. The token is opened in memory for this one call.
func (d GitDeps) testSavedRepo(ctx context.Context, limiter *testLimiter, userAgent, envID, repoID string) (*GitTestOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
	}
	if d.Git == nil || d.Conns == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	repo, err := d.lookupRepo(ctx, envID, repoID)
	if err != nil {
		return nil, d.repoFail("get repository to test", err)
	}
	if !limiter.allow(p.ID) {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many tests. Wait a minute and try again.")
	}
	rem, err := d.Conns.Remote(ctx, repo)
	if err != nil {
		return nil, d.repoFail("open the credential to test", err)
	}
	res, err := d.lsRemote(ctx, rem, repo.CredentialID != "")
	if err != nil {
		return nil, err
	}
	var code gitx.Code
	if res.failure != nil {
		code = res.failure.Code
	}
	if err := d.Conns.RecordTest(ctx, repo.ID, res.failure == nil, code); err != nil {
		d.Log.Error("could not store the result of a repository test", "repository", repo.ID, "error", err)
	}
	details := gitRepoAuditDetails(repo)
	delete(details, "custom_ca")
	if err := d.auditTest(ctx, p, userAgent, envID, repo.ID, details, res.failure); err != nil {
		return nil, err
	}
	return res.out, nil
}

// trustHostKey stores the SSH host key a person confirmed as the one the repository's host is
// trusted by, replacing any earlier one, and audits it in the same transaction: git.host_key_retrusted
// with both fingerprints when a key was saved before, git.host_key_trusted for the first. It is the
// only way a changed key is accepted; no test and no fetch ever does it.
func (d GitDeps) trustHostKey(ctx context.Context, p *rbac.Principal, envID, repoID, userAgent, line string) (*GitRepoOutput, error) {
	repo, err := d.lookupRepo(ctx, envID, repoID)
	if err != nil {
		return nil, d.repoFail("get repository to trust a host key", err)
	}
	r, err := d.Conns.TrustHostKey(ctx, repo.ID, line, p.ID, func(ctx context.Context, tx *sql.Tx, before, after gitconn.Repo) error {
		if before.HostKey != "" {
			return d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitHostKeyRetrusted, gitRepoTargetType, after.ID, envID, hostKeyAuditDetails(after, before.HostKeyFP))
		}
		return d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitHostKeyTrusted, gitRepoTargetType, after.ID, envID, hostKeyAuditDetails(after, ""))
	})
	if err != nil {
		return nil, d.repoFail("trust a host key", err)
	}
	return &GitRepoOutput{CacheControl: "no-store", Body: d.gitRepoBody(ctx, r, false)}, nil
}

// registerGitRepos registers the repository routes.
func registerGitRepos(r *httpx.Router, d GitDeps, limiter *testLimiter) {
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	const createDoc = "A repository is an address, an optional credential picked by ID and an optional custom certificate authority. Saving never runs or waits for a test. " +
		"422 validation_failed names every field at fault with a stable code (name_required, name_invalid, url_unsupported, url_userinfo, ca_invalid, ca_too_large, credential_scope); " +
		"409 name_taken when another repository of the scope has the name. "

	httpx.Register(r, huma.Operation{
		OperationID:   "createGitRepoInstance",
		Method:        http.MethodPost,
		Path:          "/api/git/repos",
		Summary:       "Add an instance git repository",
		DefaultStatus: http.StatusCreated,
		Description: "Stores a repository that stacks of every environment can use. " + createDoc +
			"An instance repository may use only an instance credential (credential_scope otherwise). Needs git.instance. Audited as git.repo_created with the host of its address and the credential's name and scope.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitInstance), func(ctx context.Context, in *CreateGitRepoInput) (*GitRepoOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Conns == nil {
			return nil, unavailable()
		}
		return d.createGitRepo(ctx, p, "", in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "createGitRepo",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/git/repos",
		Summary:       "Add a git repository to an environment",
		DefaultStatus: http.StatusCreated,
		Description: "Stores a repository that stacks of this environment can use. " + createDoc +
			"An environment repository may use a credential of its own environment or an instance credential, and no other environment's (credential_scope). " +
			"Needs git.manage on the environment (its Admin, or an instance admin). Audited as git.repo_created with the environment, the host of its address and the credential's name and scope.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *CreateEnvGitRepoInput) (*GitRepoOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Conns == nil {
			return nil, unavailable()
		}
		return d.createGitRepo(ctx, p, in.EnvID, in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getGitRepo",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/git/repos/{repoId}",
		Summary:     "Read one git repository of an environment",
		Description: "A repository of this environment, or an instance repository that applies to it (readOnly unless the caller holds git.instance). Needs git.read on the environment. " +
			"A repository of another environment answers 404 repository_not_found, exactly like one that does not exist. No secret is ever included.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitRead), func(ctx context.Context, in *GitEnvRepoPathInput) (*GitRepoOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Conns == nil {
			return nil, unavailable()
		}
		repo, err := d.Conns.GetRepo(ctx, in.RepoID)
		if err == nil && repo.EnvID != "" && repo.EnvID != in.EnvID {
			err = gitconn.ErrNotFound
		}
		if err != nil {
			return nil, d.repoFail("get repository", err)
		}
		v, err := gitViewerOf(ctx, d.Checker, p)
		if err != nil {
			return nil, d.gitFail("effective permissions", err)
		}
		return &GitRepoOutput{CacheControl: "no-store", Body: d.gitRepoBody(ctx, repo, !v.manages(repo.EnvID))}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testGitRepoInstance",
		Method:      http.MethodPost,
		Path:        "/api/git/repos/{repoId}/test",
		Summary:     "Test a saved instance git repository from the server",
		Description: "Runs the same test as testing an address, on the saved repository with its saved credential, and records when it ran, whether it passed and the code it failed with. " +
			"A test that failed is a 200 with failure.code and failure.host. Needs git.instance. A repository of an environment answers 404 repository_not_found here. " +
			"At most 10 tests a minute per user across all the test routes: 429 rate_limited. Audited as git.repo_tested with the host, the credential's name and scope and the code of a failure.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitInstance), func(ctx context.Context, in *TestGitRepoInput) (*GitTestOutput, error) {
		return d.testSavedRepo(ctx, limiter, in.UserAgent, "", in.RepoID)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testGitRepo",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/git/repos/{repoId}/test",
		Summary:     "Test a saved git repository of an environment from the server",
		Description: "As testing an instance repository, for a repository of this environment. Needs git.manage on the environment (its Admin, or an instance admin). " +
			"A repository of another environment, or of the instance, answers 404 repository_not_found. The 10 tests a minute per user are shared with the other test routes: 429 rate_limited. " +
			"Audited as git.repo_tested with the environment.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *TestGitEnvRepoInput) (*GitTestOutput, error) {
		return d.testSavedRepo(ctx, limiter, in.UserAgent, in.EnvID, in.RepoID)
	})

	const trustDoc = "Stores the SSH host key a person confirmed as the one the repository's host is trusted by, replacing any earlier one, and records who trusted it and when. " +
		"This is the only way a changed key is accepted: a test or fetch that sees a different key fails with host_key_changed and sends no credential until an administrator does this. " +
		"The last test is forgotten. 422 validation_failed names hostKey (host_key_invalid, host_key_not_ssh). " +
		"Audited as git.host_key_retrusted with the fingerprint of the new key and of the one it replaced, or git.host_key_trusted when the repository had no key. "
	trustGuard := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Conns == nil {
			return nil, unavailable()
		}
		return p, nil
	}

	httpx.Register(r, huma.Operation{
		OperationID: "trustGitHostKeyInstance",
		Method:      http.MethodPost,
		Path:        "/api/git/repos/{repoId}/trust-host-key",
		Summary:     "Trust an SSH host key for an instance git repository",
		Description: trustDoc + "Needs git.instance. A repository of an environment answers 404 repository_not_found here.",
		Tags:        []string{"git"},
	}, rbac.Require(rbac.PermGitInstance), func(ctx context.Context, in *TrustGitHostKeyInput) (*GitRepoOutput, error) {
		p, err := trustGuard(ctx)
		if err != nil {
			return nil, err
		}
		return d.trustHostKey(ctx, p, "", in.RepoID, in.UserAgent, in.Body.HostKey)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "trustGitHostKey",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/git/repos/{repoId}/trust-host-key",
		Summary:     "Trust an SSH host key for an environment's git repository",
		Description: trustDoc + "Needs git.manage on the environment (its Admin, or an instance admin); an Operator or Viewer gets 403. A repository of another environment, and an instance repository, answer 404 repository_not_found here.",
		Tags:        []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *TrustGitEnvHostKeyInput) (*GitRepoOutput, error) {
		p, err := trustGuard(ctx)
		if err != nil {
			return nil, err
		}
		return d.trustHostKey(ctx, p, in.EnvID, in.RepoID, in.UserAgent, in.Body.HostKey)
	})
}

// GitRepoListBody is the repositories the caller may see.
type GitRepoListBody struct {
	Items []GitRepoBody `json:"items" doc:"Instance repositories first, then each environment's, by name"`
}

// GitRepoListOutput wraps GitRepoListBody for huma.
type GitRepoListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitRepoListBody
}

// UpdateGitRepoInput is the request of the instance update route.
type UpdateGitRepoInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
	Body      GitRepoRequest
}

// UpdateEnvGitRepoInput is the request of the environment update route.
type UpdateEnvGitRepoInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
	Body      GitRepoRequest
}

// DeleteGitRepoInput is the request of the instance delete route.
type DeleteGitRepoInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
}

// DeleteEnvGitRepoInput is the request of the environment delete route.
type DeleteEnvGitRepoInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	RepoID    string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
}

// listGitRepos lists the repositories of every layer the caller sees. envID, when set, limits the
// list to the instance's repositories and that environment's own.
func (d GitDeps) listGitRepos(ctx context.Context, p *rbac.Principal, envID string) (*GitRepoListOutput, error) {
	v, err := gitViewerOf(ctx, d.Checker, p)
	if err != nil {
		return nil, d.gitFail("effective permissions", err)
	}
	all, err := d.Conns.ListRepos(ctx)
	if err != nil {
		return nil, d.gitFail("list repositories", err)
	}
	items := make([]GitRepoBody, 0, len(all))
	for _, r := range all {
		if (envID == "" || r.EnvID == "" || r.EnvID == envID) && v.sees(r.EnvID) {
			items = append(items, d.gitRepoBody(ctx, r, !v.manages(r.EnvID)))
		}
	}
	return &GitRepoListOutput{CacheControl: "no-store", Body: GitRepoListBody{Items: items}}, nil
}

// updateGitRepo changes a repository of one layer (envID empty for the instance's) and audits it in
// the same transaction. A change of host also records the host it pointed at before.
func (d GitDeps) updateGitRepo(ctx context.Context, p *rbac.Principal, envID, id, userAgent string, req GitRepoRequest) (*GitRepoOutput, error) {
	cur, err := d.lookupRepo(ctx, envID, id)
	if err != nil {
		return nil, d.repoFail("get repository to update", err)
	}
	r, _, err := d.Conns.UpdateRepo(ctx, envID, id,
		gitconn.RepoUpdate{Name: req.Name, URL: req.URL, CredentialID: req.CredentialID, CAPEM: req.CAPEM, HostKey: req.HostKey, TrustedBy: p.ID},
		func(ctx context.Context, tx *sql.Tx, r gitconn.Repo, changed []string) error {
			details := gitRepoAuditDetails(r)
			details["changed"] = strings.Join(changed, ",")
			if r.Host() != cur.Host() {
				// Where the repository pointed before, so a redirect of it is on record (T-09-14).
				details["host_from"] = cur.Host()
			}
			if err := d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitRepoUpdated, gitRepoTargetType, r.ID, envID, details); err != nil {
				return err
			}
			if !slices.Contains(changed, "host_key") || r.HostKey == "" {
				return nil
			}
			// A key that replaces another is a re-trust; a first key, or one for a moved address, is a trust.
			if cur.HostKey != "" && r.Host() == cur.Host() {
				return d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitHostKeyRetrusted, gitRepoTargetType, r.ID, envID, hostKeyAuditDetails(r, cur.HostKeyFP))
			}
			return d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitHostKeyTrusted, gitRepoTargetType, r.ID, envID, hostKeyAuditDetails(r, ""))
		})
	if err != nil {
		return nil, d.repoFail("update repository", err)
	}
	return &GitRepoOutput{CacheControl: "no-store", Body: d.gitRepoBody(ctx, r, false)}, nil
}

// deleteGitRepo removes a repository of one layer and audits it.
func (d GitDeps) deleteGitRepo(ctx context.Context, p *rbac.Principal, envID, id, userAgent string) error {
	_, err := d.Conns.DeleteRepo(ctx, envID, id, func(ctx context.Context, tx *sql.Tx, r gitconn.Repo) error {
		return d.gitAudit(ctx, tx, p, userAgent, audit.ActionGitRepoDeleted, gitRepoTargetType, r.ID, envID, gitRepoAuditDetails(r))
	})
	if err != nil {
		return d.repoFail("delete repository", err)
	}
	// The mirror is a cache of the repository that is gone. A failure to remove it is not the caller's: the daily
	// sweep removes a mirror whose repository does not exist.
	if m, ok := d.Git.(gitx.Mirrors); ok {
		if err := m.RemoveMirror(gitx.MirrorID(id)); err != nil && d.Log != nil {
			d.Log.Warn("could not remove the mirror of a deleted repository", "repository", id, "error", err)
		}
	}
	return nil
}

// registerGitRepoManagement registers listing, reading, changing and deleting repositories.
func registerGitRepoManagement(r *httpx.Router, d GitDeps) {
	guard := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Conns == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
		}
		return p, nil
	}
	const putDoc = "The members replace the repository's: name and url. credentialId left out keeps the saved credential, an empty string picks none, an ID picks that credential (422 credential_scope when the layer rule forbids it). " +
		"When url moves to another host or port, a saved credential is kept only if credentialId names a credential again, else 422 credential_reselect: a token picked for one server is never aimed at another by an edit that did not mention it. " +
		"The last test is forgotten when the address, the credential or the CA changes. 422 validation_failed names every field at fault with the codes of adding; 409 name_taken when another repository of the scope has the name. " +
		"Audited as git.repo_updated with a changed detail listing the names of the fields that changed and host_from when the host changed. "
	const deleteDoc = "Removes the repository. 409 repo_in_use, with the number of stacks in errors (conflict.count), when stacks still use it: nothing is removed. One that is already gone answers 404 repository_not_found, so a repeated delete is safe and writes nothing. Audited as git.repo_deleted. "

	httpx.Register(r, huma.Operation{
		OperationID: "listGitRepos",
		Method:      http.MethodGet,
		Path:        "/api/git/repos",
		Summary:     "List the git repositories the signed-in user may see",
		Description: "An instance admin sees every repository. Anyone who holds git.read on at least one environment sees the instance repositories (readOnly) and the repositories of the environments " +
			"they can read; readOnly is false for the ones they may change. A caller with no git.read anywhere gets an empty list. No secret is ever included.",
		Tags: []string{"git"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*GitRepoListOutput, error) {
		p, err := guard(ctx)
		if err != nil {
			return nil, err
		}
		return d.listGitRepos(ctx, p, "")
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listEnvGitRepos",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/git/repos",
		Summary:     "List the git repositories that apply to an environment",
		Description: "This environment's own repositories and the instance repositories, which are readOnly unless the caller holds git.instance. Needs git.read on the environment.",
		Tags:        []string{"git"},
	}, rbac.Require(rbac.PermGitRead), func(ctx context.Context, in *EnvironmentPathInput) (*GitRepoListOutput, error) {
		p, err := guard(ctx)
		if err != nil {
			return nil, err
		}
		return d.listGitRepos(ctx, p, in.EnvID)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getGitRepoInstance",
		Method:      http.MethodGet,
		Path:        "/api/git/repos/{repoId}",
		Summary:     "Read one git repository",
		Description: "The repository as the list shows it. One the caller may not see answers exactly like one that does not exist: 404 repository_not_found.",
		Tags:        []string{"git"},
	}, rbac.SelfService, func(ctx context.Context, in *GitRepoPathInput) (*GitRepoOutput, error) {
		p, err := guard(ctx)
		if err != nil {
			return nil, err
		}
		v, err := gitViewerOf(ctx, d.Checker, p)
		if err != nil {
			return nil, d.gitFail("effective permissions", err)
		}
		repo, err := d.Conns.GetRepo(ctx, in.RepoID)
		if err == nil && !v.sees(repo.EnvID) {
			err = gitconn.ErrNotFound
		}
		if err != nil {
			return nil, d.repoFail("get repository", err)
		}
		return &GitRepoOutput{CacheControl: "no-store", Body: d.gitRepoBody(ctx, repo, !v.manages(repo.EnvID))}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateGitRepoInstance",
		Method:      http.MethodPut,
		Path:        "/api/git/repos/{repoId}",
		Summary:     "Change an instance git repository",
		Description: putDoc + "Needs git.instance. A repository of an environment answers 404 repository_not_found here.",
		Tags:        []string{"git"},
	}, rbac.Require(rbac.PermGitInstance), func(ctx context.Context, in *UpdateGitRepoInput) (*GitRepoOutput, error) {
		p, err := guard(ctx)
		if err != nil {
			return nil, err
		}
		return d.updateGitRepo(ctx, p, "", in.RepoID, in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateGitRepo",
		Method:      http.MethodPut,
		Path:        "/api/env/{envId}/git/repos/{repoId}",
		Summary:     "Change an environment's git repository",
		Description: putDoc + "Needs git.manage on the environment (its Admin, or an instance admin). A repository of another environment, and an instance repository, answer 404 repository_not_found here.",
		Tags:        []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *UpdateEnvGitRepoInput) (*GitRepoOutput, error) {
		p, err := guard(ctx)
		if err != nil {
			return nil, err
		}
		return d.updateGitRepo(ctx, p, in.EnvID, in.RepoID, in.UserAgent, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteGitRepoInstance",
		Method:        http.MethodDelete,
		Path:          "/api/git/repos/{repoId}",
		Summary:       "Delete an instance git repository",
		DefaultStatus: http.StatusNoContent,
		Description:   deleteDoc + "Needs git.instance. A repository of an environment answers 404 repository_not_found here.",
		Tags:          []string{"git"},
	}, rbac.Require(rbac.PermGitInstance), func(ctx context.Context, in *DeleteGitRepoInput) (*struct{}, error) {
		p, err := guard(ctx)
		if err != nil {
			return nil, err
		}
		return nil, d.deleteGitRepo(ctx, p, "", in.RepoID, in.UserAgent)
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "deleteGitRepo",
		Method:        http.MethodDelete,
		Path:          "/api/env/{envId}/git/repos/{repoId}",
		Summary:       "Delete an environment's git repository",
		DefaultStatus: http.StatusNoContent,
		Description:   deleteDoc + "Needs git.manage on the environment (its Admin, or an instance admin). A repository of another environment, and an instance repository, answer 404 repository_not_found here.",
		Tags:          []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *DeleteEnvGitRepoInput) (*struct{}, error) {
		p, err := guard(ctx)
		if err != nil {
			return nil, err
		}
		return nil, d.deleteGitRepo(ctx, p, in.EnvID, in.RepoID, in.UserAgent)
	})
}
