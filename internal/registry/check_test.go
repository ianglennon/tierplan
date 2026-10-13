// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry is a registry over TLS that answers the way the real ones were probed to
// (RESEARCH Pattern 6): a Bearer challenge on /v2/, a token endpoint that refuses bad
// credentials with the registry's own status, and a tag list a token may or may not read.
type fakeRegistry struct {
	srv *httptest.Server
	// open answers /v2/ with 200 and no challenge.
	open bool
	// refuseStatus is the token endpoint's answer to bad credentials: 401 for Docker Hub
	// and GitLab, 403 for GHCR.
	refuseStatus int
	// tagsStatus is the answer to the tag list of a signed-in client: 200, 401, 403 or 404.
	tagsStatus int
	// tags are the repository's tag names; nil answers the one tag "1.0". pageSize, when set,
	// splits the list into pages joined by Link headers, as registries do.
	tags     []string
	pageSize int

	// scheme is https for the usual fake and http for the plain one.
	scheme string

	// extra, when set, answers any request the fake does not (manifests and blobs).
	extra http.HandlerFunc

	// signedInV2, when set, answers a /v2/ request that already carries the issued token, which
	// is the sign-in check a client makes after a call was refused (tag browsing, G-05-17a).
	signedInV2 http.HandlerFunc

	user, pass string
	mu         sync.Mutex
	paths      []string
}

func newFakeRegistry(t *testing.T, refuseStatus int) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{refuseStatus: refuseStatus, tagsStatus: http.StatusOK, user: "bot", pass: "s3cret-token-value", scheme: "https"}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// newPlainFakeRegistry is newFakeRegistry that speaks plain HTTP.
func newPlainFakeRegistry(t *testing.T, refuseStatus int) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{refuseStatus: refuseStatus, tagsStatus: http.StatusOK, user: "bot", pass: "s3cret-token-value", scheme: "http"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, f.scheme+"://") }

func (f *fakeRegistry) egress() EgressPolicy {
	if f.scheme == "http" {
		return NewEgressPolicy(nil)
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	return NewEgressPolicy(nil).WithRootCAs(pool)
}

func (f *fakeRegistry) client(user, pass string) *Client {
	return NewClient(HostOptions{Host: f.host(), Username: user, Password: pass}, f.egress(), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *fakeRegistry) challenge(w http.ResponseWriter, scope string) {
	c := `Bearer realm="` + f.scheme + `://` + f.host() + `/token",service="fake"`
	if scope != "" {
		c += `,scope="` + scope + `"`
	}
	w.Header().Set("WWW-Authenticate", c)
	w.WriteHeader(http.StatusUnauthorized)
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/v2/" && f.open:
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/v2/":
		if r.Header.Get("Authorization") == "Bearer good" {
			if f.signedInV2 != nil {
				f.signedInV2(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		f.challenge(w, "")
	case r.URL.Path == "/token":
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.pass {
			w.WriteHeader(f.refuseStatus)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"denied"}]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "good", "expires_in": 300})
	case strings.HasSuffix(r.URL.Path, "/tags/list"):
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
		if r.Header.Get("Authorization") != "Bearer good" {
			f.challenge(w, "repository:"+repo+":pull")
			return
		}
		if f.tagsStatus == http.StatusUnauthorized {
			// A real registry says what scope was missing, in a challenge.
			f.challenge(w, "repository:"+repo+":pull")
			return
		}
		if f.tagsStatus != http.StatusOK {
			w.WriteHeader(f.tagsStatus)
			return
		}
		f.serveTags(w, r, repo)
	case f.extra != nil:
		f.extra(w, r)
	default:
		http.NotFound(w, r)
	}
}

// tagRequests is how many tag lists the registry has been asked for.
func (f *fakeRegistry) tagRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if strings.HasSuffix(p, "/tags/list") {
			n++
		}
	}
	return n
}

// serveTags answers a tag list: all of f.tags, or one page of them with a Link to the next.
func (f *fakeRegistry) serveTags(w http.ResponseWriter, r *http.Request, repo string) {
	names := f.tags
	if names == nil {
		names = []string{"1.0"}
	}
	start := 0
	if last := r.URL.Query().Get("last"); last != "" {
		for i, n := range names {
			if n == last {
				start = i + 1
			}
		}
	}
	end := len(names)
	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
		w.Header().Set("Link", `</v2/`+repo+`/tags/list?last=`+names[end-1]+`&n=`+strconv.Itoa(f.pageSize)+`>; rel="next"`)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": names[start:end]})
}

func TestPickTestTag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tags       []string
		status     int
		want       string
		wantListed bool
	}{
		{"latest is listed among others", []string{"2.0", "latest"}, http.StatusOK, "latest", true},
		{"only a version tag", []string{"1.36.1"}, http.StatusOK, "1.36.1", true},
		{"hidden tags are skipped and the newest version wins", []string{"1.2", "1.10", "sha256-" + strings.Repeat("a", 64) + ".sig", "x.sig"}, http.StatusOK, "1.10", true},
		{"nothing usable but hidden tags", []string{"x.sig", "buildcache"}, http.StatusOK, "latest", false},
		{"no tags", []string{}, http.StatusOK, "latest", false},
		{"a refused listing", []string{"1.0"}, http.StatusForbidden, "latest", false},
		{"a missing repository", []string{"1.0"}, http.StatusNotFound, "latest", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRegistry(t, http.StatusUnauthorized)
			f.tags, f.tagsStatus = tc.tags, tc.status
			got, listed := PickTestTag(context.Background(), f.client(f.user, f.pass), "acme/app")
			if got != tc.want || listed != tc.wantListed {
				t.Errorf("PickTestTag = %q, %v; want %q, %v", got, listed, tc.want, tc.wantListed)
			}
		})
	}
}

func TestPickTestTagFallsBackToLatestWhenTheRegistryCannotBeReached(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	c := f.client(f.user, f.pass)
	f.srv.Close()
	if got, listed := PickTestTag(context.Background(), c, "acme/app"); got != "latest" || listed {
		t.Errorf("PickTestTag = %q, %v; want latest, false", got, listed)
	}
}

func TestCheckPassesAllThreeStepsWhenTheSignInAndTheRepositoryAreGood(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	got := Check(context.Background(), f.client(f.user, f.pass), f.client("", ""), CheckInput{TestRepository: "acme/app"})
	want := CheckResult{StepResult{State: StepPassed}, StepResult{State: StepPassed}, StepResult{State: StepPassed}}
	if got != want {
		t.Fatalf("result = %+v, want all passed", got)
	}
}

func TestCheckReportsARefusedSignInWhetherTheTokenEndpointAnswers401Or403(t *testing.T) {
	for name, status := range map[string]int{"hub-and-gitlab-like 401": http.StatusUnauthorized, "ghcr-like 403": http.StatusForbidden} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRegistry(t, status)
			got := Check(context.Background(), f.client(f.user, "wrong"), f.client("", ""), CheckInput{TestRepository: "acme/app"})
			if got.Reachable.State != StepPassed {
				t.Errorf("reachable = %+v, want passed: a 401 on /v2/ is a registry that wants a login", got.Reachable)
			}
			if got.SignedIn != (StepResult{State: StepFailed, Code: CodeSignInRefused}) {
				t.Errorf("signed in = %+v, want failed sign_in_refused", got.SignedIn)
			}
			if got.CanPull.State != StepNotChecked {
				t.Errorf("can pull = %+v, want not checked after a refused sign-in", got.CanPull)
			}
		})
	}
}

func TestCheckReportsSignedInNotCheckedWhenTheRegistryAsksForNoLogin(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.open = true
	got := Check(context.Background(), f.client(f.user, f.pass), f.client("", ""), CheckInput{})
	if got.Reachable.State != StepPassed || got.SignedIn != (StepResult{State: StepNotChecked, Code: CodeNoLoginAsked}) {
		t.Fatalf("result = %+v, want reachable and signed in not checked with no_login_asked", got)
	}
	if got.CanPull.State != StepNotChecked || got.Failed() {
		t.Fatalf("result = %+v, want nothing failed", got)
	}
}

func TestCheckFailsCanPullWhenTheTokenCannotReadTheTestRepository(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f := newFakeRegistry(t, http.StatusUnauthorized)
		f.tagsStatus = status
		got := Check(context.Background(), f.client(f.user, f.pass), f.client("", ""), CheckInput{TestRepository: "acme/app"})
		if got.SignedIn.State != StepPassed || got.CanPull != (StepResult{State: StepFailed, Code: CodeNoPullAccess}) {
			t.Errorf("tags %d: result = %+v, want signed in and can pull failed no_pull_access", status, got)
		}
	}
}

func TestCheckFailsCanPullWithRepositoryNotFoundForAnUnknownRepository(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.tagsStatus = http.StatusNotFound
	got := Check(context.Background(), f.client(f.user, f.pass), f.client("", ""), CheckInput{TestRepository: "acme/nope"})
	if got.CanPull != (StepResult{State: StepFailed, Code: CodeRepositoryNotFound}) {
		t.Fatalf("can pull = %+v, want failed repository_not_found", got.CanPull)
	}
}

func TestCheckSkipsCanPullWithoutATestRepository(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	got := Check(context.Background(), f.client(f.user, f.pass), f.client("", ""), CheckInput{})
	if got.SignedIn.State != StepPassed || got.CanPull.State != StepNotChecked || got.CanPull.Code != "" {
		t.Fatalf("result = %+v, want can pull not checked", got)
	}
	for _, p := range f.paths {
		if strings.HasSuffix(p, "/tags/list") {
			t.Errorf("the tag list was asked for with no test repository: %v", f.paths)
		}
	}
}

func TestCheckAgainstAClosedPortIsUnreachableAndTheLaterStepsAreNotChecked(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := l.Addr().String()
	_ = l.Close()
	c := NewClient(HostOptions{Host: host, Username: "bot", Password: "x"}, NewEgressPolicy(nil), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	got := Check(context.Background(), c, c, CheckInput{TestRepository: "acme/app"})
	if got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryUnreachable}) || got.SignedIn.State != StepNotChecked || got.CanPull.State != StepNotChecked {
		t.Fatalf("result = %+v, want unreachable and the rest not checked", got)
	}
}

func TestCheckReportsACertificateTheServerDoesNotTrustAsRegistryTLS(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	// The default policy trusts the system roots, which do not include the test server's.
	c := NewClient(HostOptions{Host: f.host()}, NewEgressPolicy(nil), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	got := Check(context.Background(), c, c, CheckInput{})
	if got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryTLS}) {
		t.Fatalf("reachable = %+v, want failed registry_tls", got.Reachable)
	}
}

func TestCheckReportsAHostThatIsNotARegistryAsRegistryHTTP(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.srv.Config.Handler = http.NotFoundHandler()
	got := Check(context.Background(), f.client("bot", "x"), f.client("", ""), CheckInput{})
	if got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryHTTP}) {
		t.Fatalf("reachable = %+v, want failed registry_http", got.Reachable)
	}
}

func TestCheckAgainstABlockedAddressIsUnreachableAndNeverConnects(t *testing.T) {
	c := NewClient(HostOptions{Host: "169.254.169.254"}, NewEgressPolicy(nil), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	got := Check(context.Background(), c, c, CheckInput{})
	if got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryUnreachable}) {
		t.Fatalf("reachable = %+v, want failed registry_unreachable", got.Reachable)
	}
}

func TestTheTestRepositoryAcceptsABareHubNameAndATypedHostAndRefusesJunk(t *testing.T) {
	for _, c := range []struct{ host, in, want string }{
		{"docker.io", "nginx", "library/nginx"},
		{"docker.io", "acme/app", "acme/app"},
		{"docker.io", " /acme/app/ ", "acme/app"},
		{"docker.io", "", ""},
		{"docker.io", "docker.io/a/b", "a/b"},
		{"docker.io", "docker.io/nginx", "library/nginx"},
		{"ghcr.io", "ghcr.io/acme/app", "acme/app"},
	} {
		if got, ok := TestRepositoryPath(c.host, c.in); got != c.want || !ok {
			t.Errorf("%s %q = %q, %v, want %q", c.host, c.in, got, ok, c.want)
		}
	}
	for _, bad := range []string{"Acme/App", "a b", "acme/app?x=1", "../etc", "acme//app", "ac%2Fme", "a\nb"} {
		if got, ok := TestRepositoryPath("ghcr.io", bad); ok {
			t.Errorf("%q was accepted as %q", bad, got)
		}
	}
}

func pemOf(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}
