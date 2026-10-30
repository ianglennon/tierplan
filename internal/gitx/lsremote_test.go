// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/internal/registry"
)

// TestMain installs the HTTPS transport once for the package, as the server does at startup.
func TestMain(m *testing.M) {
	// The one test that checks NewGoGit installs it runs in a child process that leaves it to NewGoGit.
	if os.Getenv(noInstallEnv) == "" {
		gitx.InstallTransport(registry.NewEgressPolicy(nil))
	}
	os.Exit(m.Run())
}

// fixture is a repository with main and dev, a lightweight tag v1 and an annotated tag v2, served
// over TLS.
type fixture struct {
	repo       *gittest.Repo
	srv        *gittest.HTTPServer
	first, dev string
}

func newFixture(t *testing.T, o gittest.HTTPOptions) *fixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	first := repo.Commit(t, map[string]string{"compose.yaml": "services: {}\n"}, "first")
	dev := repo.Commit(t, map[string]string{"compose.yaml": "services: {a: {image: x}}\n"}, "second")
	repo.Branch(t, "dev", dev)
	repo.Branch(t, "main", dev)
	repo.Tag(t, "v1", first, false)
	repo.Tag(t, "v2", dev, true)
	// A forge advertises these too; they are not branches or tags.
	repo.Ref(t, "refs/pull/1/head", first)
	return &fixture{repo: repo, srv: gittest.NewHTTPServer(t, repo, o), first: first, dev: dev}
}

func TestLsRemotePeeledAndDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t, gittest.HTTPOptions{TLS: true})
	g := gitx.NewGoGit(gitx.Options{})

	got, err := g.LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM})
	if err != nil {
		t.Fatalf("LsRemote: %v", err)
	}
	want := gitx.RefList{
		Branches: []gitx.Ref{
			{Name: "dev", Kind: gitx.KindBranch, Commit: f.dev},
			{Name: "main", Kind: gitx.KindBranch, Commit: f.dev},
		},
		Tags: []gitx.Ref{
			{Name: "v1", Kind: gitx.KindTag, Commit: f.first},
			{Name: "v2", Kind: gitx.KindTag, Commit: f.dev},
		},
		Default: "main",
	}
	if len(got.Branches) != 2 || len(got.Tags) != 2 {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want.Branches {
		if got.Branches[i] != want.Branches[i] {
			t.Errorf("branch %d = %+v, want %+v", i, got.Branches[i], want.Branches[i])
		}
	}
	for i := range want.Tags {
		if got.Tags[i] != want.Tags[i] {
			t.Errorf("tag %d = %+v, want %+v (an annotated tag is listed at its commit, never its tag object)", i, got.Tags[i], want.Tags[i])
		}
	}
	if got.Default != "main" {
		t.Errorf("Default = %q, want main (the branch HEAD points at)", got.Default)
	}
	if f.srv.Requests() != 1 {
		t.Errorf("the server saw %d ref advertisements, want 1", f.srv.Requests())
	}
}

// basicAuthFixture is the same repository behind a user name and a token.
func basicAuthFixture(t *testing.T) (*fixture, gitx.TokenAuth) {
	t.Helper()
	const token = "tok-0123456789-abcdef"
	return newFixture(t, gittest.HTTPOptions{TLS: true, BasicAuth: &[2]string{"ci-user", token}}), gitx.TokenAuth{Username: "ci-user", Token: token}
}

// wantCode fails the test unless err is a *gitx.Error with the code.
func wantCode(t *testing.T, err error, code gitx.Code) {
	t.Helper()
	var ge *gitx.Error
	if !errors.As(err, &ge) || ge.Code != code {
		t.Errorf("err = %v, want a gitx.Error with code %s", err, code)
	}
}

func TestLsRemoteCA(t *testing.T) {
	t.Parallel()
	f := newFixture(t, gittest.HTTPOptions{TLS: true})
	g := gitx.NewGoGit(gitx.Options{})

	_, err := g.LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL})
	wantCode(t, err, gitx.CodeCertUntrusted)
	var ge *gitx.Error
	if errors.As(err, &ge) && ge.Host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", ge.Host)
	}

	if _, err := g.LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM}); err != nil {
		t.Errorf("with the fixture's CA: %v", err)
	}
	// The CA belongs to one call: the next call without it fails again.
	_, err = g.LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL})
	wantCode(t, err, gitx.CodeCertUntrusted)
}

func TestLsRemoteRefusesACAThatIsNotACertificateBeforeConnecting(t *testing.T) {
	t.Parallel()
	f := newFixture(t, gittest.HTTPOptions{TLS: true})
	g := gitx.NewGoGit(gitx.Options{})
	for name, pem := range map[string]string{
		"text":      "this is not a certificate",
		"too large": f.srv.CAPEM + strings.Repeat("#", registry.MaxCAPEMBytes),
	} {
		if _, err := g.LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL, CAPEM: pem}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := f.srv.Requests(); n != 0 {
		t.Errorf("the server saw %d requests, want 0: a bad CA is refused before any connection", n)
	}
}

func TestLsRemoteAuth(t *testing.T) {
	t.Parallel()
	f, auth := basicAuthFixture(t)
	g := gitx.NewGoGit(gitx.Options{})
	ctx := context.Background()

	_, err := g.LsRemote(ctx, gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM})
	wantCode(t, err, gitx.CodeAuthRefused)

	_, err = g.LsRemote(ctx, gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM, Auth: gitx.TokenAuth{Username: auth.Username, Token: "wrong-token"}})
	wantCode(t, err, gitx.CodeAuthRefused)

	got, err := g.LsRemote(ctx, gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM, Auth: auth})
	if err != nil || len(got.Branches) != 2 {
		t.Errorf("with the token: %+v, %v, want two branches", got, err)
	}

	// A repository that is not on the server.
	missing := strings.Replace(f.srv.URL, "repo.git", "missing.git", 1)
	_, err = g.LsRemote(ctx, gitx.Remote{URL: missing, CAPEM: f.srv.CAPEM, Auth: auth})
	wantCode(t, err, gitx.CodeNotFound)
}

func TestLsRemoteEgressBlocked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, gittest.HTTPOptions{TLS: true})
	ctx := context.Background()
	remote := gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM}

	// Loopback is allowed by default, where a self-hosted git server lives.
	if _, err := gitx.NewGoGit(gitx.Options{}).LsRemote(ctx, remote); err != nil {
		t.Fatalf("default policy: %v", err)
	}
	before := f.srv.Requests()

	blocking := registry.NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	_, err := gitx.NewGoGit(gitx.Options{Policy: blocking}).LsRemote(ctx, remote)
	wantCode(t, err, gitx.CodeEgressBlocked)
	if after := f.srv.Requests(); after != before {
		t.Errorf("the server saw %d requests after the blocked call, want %d: the connection must not be made", after, before)
	}
}

func TestLsRemoteRefusesAMetadataAddressAndARedirectToOne(t *testing.T) {
	t.Parallel()
	g := gitx.NewGoGit(gitx.Options{})
	ctx := context.Background()

	_, err := g.LsRemote(ctx, gitx.Remote{URL: "https://169.254.169.254/latest/meta-data.git"})
	wantCode(t, err, gitx.CodeEgressBlocked)

	// A server that sends the client elsewhere.
	redirectTo := func(target string) gitx.Remote {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		return gitx.Remote{URL: srv.URL + "/x.git", CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))}
	}

	_, err = g.LsRemote(ctx, redirectTo("https://169.254.169.254/x.git/info/refs?service=git-upload-pack"))
	wantCode(t, err, gitx.CodeEgressBlocked)

	// A redirect from https to plain http is never followed: the target sees no request.
	var plainHits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainHits.Add(1) }))
	t.Cleanup(plain.Close)
	if _, err := g.LsRemote(ctx, redirectTo(plain.URL+"/x.git/info/refs?service=git-upload-pack")); err == nil {
		t.Error("a redirect to plain http was followed")
	}
	if n := plainHits.Load(); n != 0 {
		t.Errorf("the plain http server saw %d requests, want 0", n)
	}
}

func TestInstallTransportRunsOnceAndASecondCallChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, gittest.HTTPOptions{TLS: true})
	// TestMain installed the permissive policy. A later call with a blocking one must not replace it.
	gitx.InstallTransport(registry.NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}))
	gitx.InstallTransport(registry.NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}))
	if _, err := gitx.NewGoGit(gitx.Options{}).LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM}); err != nil {
		t.Errorf("after a second InstallTransport: %v, want the first installation to stand", err)
	}
}

func TestPlainHTTPAndTheFileAndGitProtocolsAreNeverSpoken(t *testing.T) {
	t.Parallel()
	g := gitx.NewGoGit(gitx.Options{})
	for _, u := range []string{"http://127.0.0.1:1/x.git", "git://127.0.0.1:1/x.git", "file:///etc"} {
		_, err := g.LsRemote(context.Background(), gitx.Remote{URL: u})
		if !errors.Is(err, gitx.ErrURLUnsupported) {
			t.Errorf("%s: %v, want ErrURLUnsupported", u, err)
		}
	}
}

// Error text is a code and a host, never what go-git or the network said (RESEARCH Pitfall 16).
func TestClassifyNeverCopiesTheErrorText(t *testing.T) {
	t.Parallel()
	secret := errors.New(`Get "https://ci-user:tok-SECRET@git.example.org/acme/app.git/info/refs": dial tcp 10.1.2.3:443: connection refused`)
	err := gitx.Classify(secret, "git.example.org")
	var ge *gitx.Error
	if !errors.As(err, &ge) || ge.Code != gitx.CodeUnreachable || ge.Host != "git.example.org" {
		t.Fatalf("Classify = %v, want unreachable at git.example.org", err)
	}
	if got, want := err.Error(), "git: unreachable (git.example.org)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if errors.Is(err, secret) || errors.Unwrap(err) != nil {
		t.Error("the classified error wraps the original, which carries the address and the token")
	}
}

func TestClassifyMapsEachKindOfFailureToItsCode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want gitx.Code
	}{
		"unknown authority":  {x509.UnknownAuthorityError{}, gitx.CodeCertUntrusted},
		"wrapped by tls":     {&url.Error{Op: "Get", URL: "https://h/x", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, gitx.CodeCertUntrusted},
		"wrong host in cert": {x509.HostnameError{Host: "h"}, gitx.CodeCertUntrusted},
		"expired cert":       {x509.CertificateInvalidError{Reason: x509.Expired}, gitx.CodeCertUntrusted},
		"authentication":     {transport.ErrAuthenticationRequired, gitx.CodeAuthRefused},
		"authorization":      {fmt.Errorf("wrapped: %w", transport.ErrAuthorizationFailed), gitx.CodeAuthRefused},
		"not found":          {transport.ErrRepositoryNotFound, gitx.CodeNotFound},
		"egress":             {&net.OpError{Op: "dial", Err: registry.ErrEgressBlocked}, gitx.CodeEgressBlocked},
		"refused protocol":   {gitx.ErrProtocolRefused, gitx.CodeEgressBlocked},
		"too large":          {fmt.Errorf("read: %w", registry.ErrResponseTooLarge), gitx.CodeTooLarge},
		"deadline":           {context.DeadlineExceeded, gitx.CodeTimeout},
		"network timeout":    {&net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, gitx.CodeTimeout},
		"missing reference":  {plumbing.ErrReferenceNotFound, gitx.CodeRefMissing},
		"connection refused": {&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, gitx.CodeUnreachable},
		"anything else":      {errors.New("boom"), gitx.CodeUnreachable},
		"canceled":           {context.Canceled, gitx.CodeUnreachable},
	} {
		var ge *gitx.Error
		err := gitx.Classify(tc.err, "h")
		if !errors.As(err, &ge) || ge.Code != tc.want {
			t.Errorf("%s: Classify = %v, want code %s", name, err, tc.want)
		}
	}
	if err := gitx.Classify(nil, "h"); err != nil {
		t.Errorf("Classify(nil) = %v, want nil", err)
	}
}

// D-05: there is no option to skip certificate verification, anywhere in the code that talks to a
// git host. The setting's names are spelled in two halves so this file does not trip the gate that
// keeps them out of the tree (scripts/insecure-tls-check.sh).
func TestNoOptionSkipsCertificateVerification(t *testing.T) {
	t.Parallel()
	banned := []string{"Insecure" + "SkipVerify", "Insecure" + "SkipTLS"}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v, %d files", err, len(files))
	}
	files = append(files, "../registry/egress.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // G304: the names are this package's own files
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range banned {
			if strings.Contains(string(src), b) {
				t.Errorf("%s names %s: git over HTTPS always verifies certificates", name, b)
			}
		}
	}
}
