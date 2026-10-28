// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/httpx"
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
	Log *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// GitTestRequest is the body of a repository test: an address and, for a server with a private
// certificate authority, that authority. It never carries a credential: a test of a public
// repository is the whole of this plan, and signing in follows with the credentials.
type GitTestRequest struct {
	URL   string `json:"url" maxLength:"2048" doc:"The repository address: https://host/path.git, ssh://git@host/path.git or git@host:path.git. 422 url_unsupported for any other scheme, a path, file://, git:// or http://; 422 url_userinfo for an address with a user name and password in it"`
	CAPEM string `json:"caPem,omitempty" maxLength:"262144" doc:"A certificate authority to trust for this test, in PEM, for a self-hosted server with its own. It is used for this one test. 422 ca_invalid when it is not a PEM certificate; 422 ca_too_large over 64 KiB. There is no option to skip certificate verification"`
}

// TestGitAddressInput is the request of the instance test route.
type TestGitAddressInput struct {
	Body GitTestRequest
}

// TestGitAddressEnvInput is the request of the environment test route.
type TestGitAddressEnvInput struct {
	EnvironmentPathInput
	Body GitTestRequest
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
}

// GitTestOutput is the answer of both test routes.
type GitTestOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitTestResultBody
}

// testGitAddress validates a submission and lists the repository's branches and tags from this
// server. It writes nothing (Phase 9 D-04).
func testGitAddress(ctx context.Context, d GitDeps, limiter *testLimiter, req GitTestRequest) (*GitTestOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
	}
	if d.Git == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	// The address and the authority are checked before the limiter is spent: a refused submission
	// reaches no server.
	if err := validateGitTest(req); err != nil {
		return nil, err
	}
	if !limiter.allow(p.ID) {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many tests. Wait a minute and try again.")
	}
	refs, err := d.Git.LsRemote(ctx, gitx.Remote{URL: req.URL, CAPEM: req.CAPEM})
	out := &GitTestOutput{CacheControl: "no-store", Body: GitTestResultBody{Branches: []string{}, Tags: []string{}}}
	if err != nil {
		var ge *gitx.Error
		if !errors.As(err, &ge) {
			// The text of an unknown error could carry the address or a token: log its type only.
			d.Log.Error("git test failed unexpectedly", "error_type", fmt.Sprintf("%T", err))
			return nil, huma.Error500InternalServerError("internal error")
		}
		out.Body.Steps, out.Body.Failure = failedSteps(ge.Code), &GitTestFailureBody{Code: string(ge.Code), Host: ge.Host}
		return out, nil
	}
	out.Body.Steps = GitTestStepsBody{
		Reachable: GitTestStepBody{State: "passed"}, SignedIn: GitTestStepBody{State: "not_needed"}, Refs: GitTestStepBody{State: "passed"},
	}
	for _, b := range refs.Branches[:min(len(refs.Branches), namesShown)] {
		out.Body.Branches = append(out.Body.Branches, b.Name)
	}
	for _, t := range refs.Tags[:min(len(refs.Tags), namesShown)] {
		out.Body.Tags = append(out.Body.Tags, t.Name)
	}
	out.Body.BranchCount, out.Body.TagCount, out.Body.DefaultBranch = len(refs.Branches), len(refs.Tags), refs.Default
	return out, nil
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
		return testGitAddress(ctx, d, limiter, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "testGitAddress",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/git/repos/test",
		Summary:     "Test a git repository address from the server, for an environment",
		Description: "As testing an address for the instance, for the repositories of one environment. Needs git.manage on the environment (its Admin, or an instance admin); an Operator or Viewer gets 403. " +
			"The 10 tests a minute per user are shared with the instance family: 429 rate_limited. Writes nothing.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermGitManage), func(ctx context.Context, in *TestGitAddressEnvInput) (*GitTestOutput, error) {
		return testGitAddress(ctx, d, limiter, in.Body)
	})
}
