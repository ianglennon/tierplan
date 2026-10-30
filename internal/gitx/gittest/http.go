// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gittest

import (
	"crypto/subtle"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: the httpoxy warning is for Go before 1.6.3; the CGI here is a test fixture serving git-http-backend
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// HTTPOptions are the choices of NewHTTPServer.
type HTTPOptions struct {
	// TLS serves https with a certificate the test must trust through HTTPServer.CAPEM.
	TLS bool
	// BasicAuth, when set, is the user name and password a request must carry.
	BasicAuth *[2]string
	// AllowAnySHA1 lets a client ask for a commit by its ID even when no branch or tag reaches it
	// (uploadpack.allowAnySHA1InWant), as GitHub does. Without it such a request is refused, as
	// most servers do.
	AllowAnySHA1 bool
}

// HTTPServer is a git smart-HTTP server for one repository.
type HTTPServer struct {
	// URL is the address of the repository: http(s)://127.0.0.1:port/repo.git.
	URL string
	// CAPEM is the certificate to trust for a TLS server, in PEM; empty for plain http.
	CAPEM string

	requests atomic.Int64
	srv      *httptest.Server
}

// Requests is how many requests for the ref advertisement (info/refs) the server has received,
// whether or not they were allowed in. A test of deduplication counts them.
func (s *HTTPServer) Requests() int { return int(s.requests.Load()) }

// Close stops the server. The test's cleanup does it too.
func (s *HTTPServer) Close() { s.srv.Close() }

// backendPath is where git keeps git-http-backend, from git's own answer, never a fixed path.
func backendPath(tb testing.TB) string {
	tb.Helper()
	requireGit(tb)
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		tb.Fatalf("git --exec-path: %v", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		tb.Fatal("git-http-backend is required for the git fixtures; see scripts/ci-preflight.sh")
	}
	return p
}

// NewHTTPServer serves r over git's smart HTTP protocol with git-http-backend, behind an
// httptest server, and stops it when the test ends.
func NewHTTPServer(tb testing.TB, r *Repo, o HTTPOptions) *HTTPServer {
	tb.Helper()
	backend := cgi.Handler{
		Path:   backendPath(tb),
		Stderr: io.Discard,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(r.bare),
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"HOME=" + filepath.Dir(r.bare),
		},
	}
	if o.AllowAnySHA1 {
		backend.Env = append(backend.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=uploadpack.allowAnySHA1InWant", "GIT_CONFIG_VALUE_0=true")
	}
	s := &HTTPServer{}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/info/refs") {
			s.requests.Add(1)
		}
		if o.BasicAuth != nil {
			user, pass, ok := req.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(o.BasicAuth[0])) != 1 ||
				subtle.ConstantTimeCompare([]byte(pass), []byte(o.BasicAuth[1])) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		backend.ServeHTTP(w, req)
	})
	s.srv = httptest.NewUnstartedServer(h)
	// A client that refuses the certificate is the point of some tests, not a server error to log.
	s.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if o.TLS {
		s.srv.StartTLS()
		s.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}))
	} else {
		s.srv.Start()
	}
	tb.Cleanup(s.srv.Close)
	s.URL = s.srv.URL + "/" + filepath.Base(r.bare)
	return s
}
