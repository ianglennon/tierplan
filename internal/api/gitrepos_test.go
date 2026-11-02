// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/gitx/gittest"
)

// Testing a git repository address from the server (plan 09-01, GIT-01, D-04, D-05). The router,
// policy, CSRF guard and rate limit are the real ones; the repository is a real git server.

type gitTestResp struct {
	Steps struct {
		Reachable struct{ State string } `json:"reachable"`
		SignedIn  struct{ State string } `json:"signedIn"`
		Refs      struct{ State string } `json:"refs"`
	} `json:"steps"`
	Branches      []string `json:"branches"`
	Tags          []string `json:"tags"`
	BranchCount   int      `json:"branchCount"`
	TagCount      int      `json:"tagCount"`
	DefaultBranch string   `json:"defaultBranch"`
	Failure       *struct {
		Code string `json:"code"`
		Host string `json:"host"`
	} `json:"failure"`
}

func decodeGitTest(t *testing.T, rec *httptest.ResponseRecorder) gitTestResp {
	t.Helper()
	var r gitTestResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return r
}

func gitTestBody(url, caPEM string) string {
	b, _ := json.Marshal(map[string]string{"url": url, "caPem": caPEM})
	return string(b)
}

// publicRepo is a repository with the branches main and dev, a lightweight tag v1 and an annotated
// tag v2, served over TLS.
func publicRepo(t *testing.T, o gittest.HTTPOptions) *gittest.HTTPServer {
	t.Helper()
	repo := gittest.NewRepo(t)
	first := repo.Commit(t, map[string]string{"compose.yaml": "services: {}\n"}, "first")
	second := repo.Commit(t, map[string]string{"compose.yaml": "services: {a: {image: x}}\n"}, "second")
	repo.Branch(t, "dev", first)
	repo.Tag(t, "v1", first, false)
	repo.Tag(t, "v2", second, true)
	o.TLS = true
	return gittest.NewHTTPServer(t, repo, o)
}

func TestGitAddressTest(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	env := f.add(t).ID
	srv := publicRepo(t, gittest.HTTPOptions{})
	body := gitTestBody(srv.URL, srv.CAPEM)

	for name, tc := range map[string]struct {
		path string
		user credentialUser
	}{
		"instance route as an instance admin":    {"/api/git/repos/test", f.asAdmin(t)},
		"environment route as the env admin":     {"/api/env/" + env + "/git/repos/test", f.asRoleOn(t, "envadmin", env, "admin")},
		"environment route as an instance admin": {"/api/env/" + env + "/git/repos/test", f.asAdmin(t)},
	} {
		rec := f.credCall(t, tc.user, http.MethodPost, tc.path, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", name, cc)
		}
		got := decodeGitTest(t, rec)
		if got.Steps.Reachable.State != "passed" || got.Steps.SignedIn.State != "not_needed" || got.Steps.Refs.State != "passed" || got.Failure != nil {
			t.Errorf("%s: steps = %+v failure = %+v, want reachable passed, signedIn not_needed, refs passed", name, got.Steps, got.Failure)
		}
		if !slices.Equal(got.Branches, []string{"dev", "main"}) || !slices.Equal(got.Tags, []string{"v1", "v2"}) {
			t.Errorf("%s: branches %v tags %v, want [dev main] and [v1 v2] (the annotated tag is listed too)", name, got.Branches, got.Tags)
		}
		if got.BranchCount != 2 || got.TagCount != 2 || got.DefaultBranch != "main" {
			t.Errorf("%s: counts %d/%d default %q, want 2/2 and main", name, got.BranchCount, got.TagCount, got.DefaultBranch)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// A test writes nothing of its own: the only rows are the sign-ins of the fixture's users.
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM audit_log WHERE action LIKE 'git.%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("git audit rows = %d, %v; a test records nothing (D-04)", n, err)
	}
}

func TestGitAddressTestIsForAnAdminOfTheEnvironmentOrTheInstance(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	env := f.add(t).ID
	srv := publicRepo(t, gittest.HTTPOptions{})
	body := gitTestBody(srv.URL, srv.CAPEM)
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)
	for name, u := range map[string]credentialUser{
		"viewer":   f.asRoleOn(t, "viewer", env, "viewer"),
		"operator": f.asRoleOn(t, "operator", env, "operator"),
	} {
		if rec := f.credCall(t, u, http.MethodPost, "/api/env/"+env+"/git/repos/test", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s on the environment route: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	// An environment the user holds no role in is not found to them, as for every environment route.
	nobody := credentialUser{nobodyCookie, nobodyCSRF}
	if rec := f.credCall(t, nobody, http.MethodPost, "/api/env/"+env+"/git/repos/test", body); rec.Code != http.StatusNotFound {
		t.Errorf("user with no role: %d %s, want 404", rec.Code, rec.Body)
	}
	for name, u := range map[string]credentialUser{
		"env admin": f.asRoleOn(t, "envadmin", env, "admin"),
		"viewer":    f.asRoleOn(t, "viewer2", env, "viewer"),
	} {
		if rec := f.credCall(t, u, http.MethodPost, "/api/git/repos/test", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s on the instance route: %d %s, want 403 (it needs git.instance)", name, rec.Code, rec.Body)
		}
	}
}

// countingGit answers every listing with an empty repository and counts the calls. It does not
// mirror anything: the embedded nil client would panic if a test asked it to.
type countingGit struct {
	gitx.Client
	calls atomic.Int64
}

func (c *countingGit) LsRemote(context.Context, gitx.Remote) (gitx.RefList, error) {
	c.calls.Add(1)
	return gitx.RefList{Default: "main"}, nil
}

func TestTheEleventhGitTestInAMinuteIsRateLimited(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	env := f.add(t).ID
	g := &countingGit{}
	f.gitClient = g
	f.h = f.build(t)
	admin := f.asAdmin(t)
	body := gitTestBody("https://git.example.org/acme/app.git", "")
	for i := 1; i <= 10; i++ {
		if rec := f.credCall(t, admin, http.MethodPost, "/api/git/repos/test", body); rec.Code != http.StatusOK {
			t.Fatalf("test %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := f.credCall(t, admin, http.MethodPost, "/api/git/repos/test", body)
	if p := decodeProblem(t, rec); rec.Code != http.StatusTooManyRequests || p.Type != "rate_limited" {
		t.Errorf("11th test: %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	if n := g.calls.Load(); n != 10 {
		t.Errorf("the client was called %d times, want 10: a limited test reaches no server", n)
	}
	// The limit is shared by the two families.
	if rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+env+"/git/repos/test", body); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the environment route after 10 instance tests: %d %s, want 429", rec.Code, rec.Body)
	}
}

func TestAGitTestThatFailsIsAResultThatNamesTheHostOnly(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	// Nothing listens on port 1 of the loopback address, which the policy allows.
	rec := f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/git/repos/test", gitTestBody("https://127.0.0.1:1/secret/path.git", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := decodeGitTest(t, rec)
	if got.Failure == nil || got.Failure.Code != "unreachable" || got.Failure.Host != "127.0.0.1" {
		t.Fatalf("failure = %+v, want unreachable at 127.0.0.1", got.Failure)
	}
	if got.Steps.Reachable.State != "failed" || got.Steps.SignedIn.State != "not_checked" || got.Steps.Refs.State != "not_checked" {
		t.Errorf("steps = %+v, want reachable failed and the rest not_checked", got.Steps)
	}
	if strings.Contains(rec.Body.String(), "secret/path") || strings.Contains(rec.Body.String(), ":1/") {
		t.Errorf("the answer carries the address or raw error text: %s", rec.Body)
	}
}

func TestGitAddressTestRefusals(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	srv := publicRepo(t, gittest.HTTPOptions{})

	// An address or an authority the server will not use is a 422 that names the field and a code.
	for _, tc := range []struct{ name, url, ca, location, code string }{
		{"file scheme", "file:///etc/passwd", "", "body.url", "url_unsupported"},
		{"a bare path", "/etc/passwd", "", "body.url", "url_unsupported"},
		{"git scheme", "git://git.example.org/x.git", "", "body.url", "url_unsupported"},
		{"plain http", "http://git.example.org/x.git", "", "body.url", "url_unsupported"},
		{"an option", "-oProxyCommand=touch /tmp/x", "", "body.url", "url_unsupported"},
		{"no host", "https:///x.git", "", "body.url", "url_unsupported"},
		{"no address", "", "", "body.url", "url_unsupported"},
		{"a control character", "https://git.example.org/x\x00.git", "", "body.url", "url_unsupported"},
		{"a password in the address", "https://ci:hunter2-secret@git.example.org/x.git", "", "body.url", "url_userinfo"},
		{"a CA that is not a certificate", srv.URL, "this is not a certificate", "body.caPem", "ca_invalid"},
		{"a CA over the size limit", srv.URL, srv.CAPEM + strings.Repeat("#", 64<<10), "body.caPem", "ca_too_large"},
	} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/git/repos/test", gitTestBody(tc.url, tc.ca))
		p := decodeProblem(t, rec)
		if rec.Code != http.StatusUnprocessableEntity || p.Type != "validation_failed" || len(p.Errors) != 1 || p.Errors[0].Location != tc.location || p.Errors[0].Message != tc.code {
			t.Errorf("%s: %d %s, want 422 validation_failed %s %s", tc.name, rec.Code, rec.Body, tc.location, tc.code)
		}
		if strings.Contains(rec.Body.String(), "hunter2-secret") {
			t.Errorf("%s: the answer carries the password from the address: %s", tc.name, rec.Body)
		}
	}
	// The refusal text is the screens' wording.
	rec := f.credCall(t, admin, http.MethodPost, "/api/git/repos/test", gitTestBody("file:///etc", ""))
	if !strings.Contains(rec.Body.String(), "url_unsupported") {
		t.Errorf("the body does not carry the code: %s", rec.Body)
	}

	// A refused submission reaches no server and spends none of the 10 tests a minute.
	for range 12 {
		f.credCall(t, admin, http.MethodPost, "/api/git/repos/test", gitTestBody("file:///etc", ""))
	}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/git/repos/test", gitTestBody(srv.URL, srv.CAPEM)); rec.Code != http.StatusOK {
		t.Errorf("a valid test after 13 refused ones: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestGitAddressTestFailuresAreFixedCodesAndNeverRawText(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	const password = "basic-auth-password-0123456789" //nolint:gosec // G101: a made-up password for the test fixture
	locked := publicRepo(t, gittest.HTTPOptions{BasicAuth: &[2]string{"ci-user", password}})
	open := publicRepo(t, gittest.HTTPOptions{})

	for _, tc := range []struct {
		name, url, ca, code, host string
		reachable, signedIn, refs string
	}{
		{"a private CA that was not supplied", open.URL, "", "cert_untrusted", "127.0.0.1", "failed", "not_checked", "not_checked"},
		{"a server that asks for a sign-in", locked.URL, locked.CAPEM, "auth_refused", "127.0.0.1", "passed", "failed", "not_checked"},
		{"a repository that is not there", strings.Replace(open.URL, "repo.git", "missing.git", 1), open.CAPEM, "not_found", "127.0.0.1", "passed", "not_needed", "failed"},
		{"a metadata address", "https://169.254.169.254/latest.git", "", "egress_blocked", "169.254.169.254", "failed", "not_checked", "not_checked"},
	} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/git/repos/test", gitTestBody(tc.url, tc.ca))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s, want 200 (a failed test is a result)", tc.name, rec.Code, rec.Body)
			continue
		}
		got := decodeGitTest(t, rec)
		if got.Failure == nil || got.Failure.Code != tc.code || got.Failure.Host != tc.host {
			t.Errorf("%s: failure = %+v, want %s at %s", tc.name, got.Failure, tc.code, tc.host)
		}
		if got.Steps.Reachable.State != tc.reachable || got.Steps.SignedIn.State != tc.signedIn || got.Steps.Refs.State != tc.refs {
			t.Errorf("%s: steps = %+v, want %s / %s / %s", tc.name, got.Steps, tc.reachable, tc.signedIn, tc.refs)
		}
		if len(got.Branches) != 0 || len(got.Tags) != 0 {
			t.Errorf("%s: a failed test lists %v %v", tc.name, got.Branches, got.Tags)
		}
		for _, leak := range []string{password, "x509", "dial tcp", "certificate", "lookup", "unknown authority", "repo.git", "info/refs"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("%s: the answer carries %q: %s", tc.name, leak, rec.Body)
			}
		}
	}
	// Nothing about the failures reached the server's log either.
	logs := f.logs.String()
	for _, leak := range []string{password, "x509", "dial tcp"} {
		if strings.Contains(logs, leak) {
			t.Errorf("the log carries %q", leak)
		}
	}
}
