// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// gateServer builds a server from env with a data directory of its own, closed with the test.
func gateServer(t *testing.T, env map[string]string) *Server {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
	srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// plainFrom serves one request through the server's handler as a peer at remote, over TLS or not.
func plainFrom(srv *Server, method, target, remote string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remote
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

const nonLoopbackPeer = "203.0.113.9:40000"

func TestGateRedirectsAPlainRequestToThePublicURL(t *testing.T) {
	srv := gateServer(t, map[string]string{
		"TIERPLAN_TLS_SELF_SIGNED": "true",
		"TIERPLAN_PUBLIC_URL":      "https://tierplan.example.com:8443/tp",
	})
	if !srv.https.Live() {
		t.Fatal("TLS is not live")
	}
	host := func(r *http.Request) { r.Host = "evil.test" }

	rec := plainFrom(srv, http.MethodGet, "/tp/api/version?x=1&y=%2F", nonLoopbackPeer, host)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "https://tierplan.example.com:8443/tp/api/version?x=1&y=%2F"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want none", got)
	}

	// The unprefixed form lands on the same address below the base path.
	rec = plainFrom(srv, http.MethodGet, "/api/version?x=1", nonLoopbackPeer, host)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("unprefixed status = %d, want 307", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "https://tierplan.example.com:8443/tp/api/version?x=1"; got != want {
		t.Errorf("unprefixed Location = %q, want %q", got, want)
	}

	// A POST is redirected with 307 too, which keeps the method and the body.
	rec = plainFrom(srv, http.MethodPost, "/tp/api/auth/login", nonLoopbackPeer, nil)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("POST status = %d, want 307", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "https://tierplan.example.com:8443/tp/api/auth/login"; got != want {
		t.Errorf("POST Location = %q, want %q", got, want)
	}

	// A request that arrived over TLS is served: following the Location is not redirected again.
	rec = plainFrom(srv, http.MethodGet, "/tp/api/version", nonLoopbackPeer, func(r *http.Request) { r.TLS = &tls.ConnectionState{} })
	if rec.Code == http.StatusTemporaryRedirect || rec.Header().Get("Location") != "" {
		t.Errorf("over TLS: status = %d, Location = %q, want it served", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Body.String(), "version") {
		t.Errorf("over TLS: body = %q, want the version document", rec.Body.String())
	}
}

// installSuppliedCert makes TLS live on srv with a certificate of its own, as a later plan's
// certificate flow will (D-12): no environment variable and no setting is involved.
func installSuppliedCert(t *testing.T, srv *Server) {
	t.Helper()
	cert, _ := genCert(t, "")
	srv.https.Install(tlsKindSupplied, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil })
}

// gatePassed reports whether the gate let a request through to the router: it answered neither with its
// redirect nor with its refusal.
func gatePassed(rec *httptest.ResponseRecorder) bool {
	return rec.Code != http.StatusTemporaryRedirect && rec.Code != http.StatusBadRequest && rec.Header().Get("Location") == ""
}

// gateOracle is the decision table written out a second way, as nested rules, so the table test
// checks decideGate against the stated rules and not against itself.
func gateOracle(f gateFacts) gateAction {
	probe := f.path == "/healthz" || f.path == "/readyz"
	https := strings.HasPrefix(f.publicURL, "https://")
	switch {
	case f.overTLS, probe, !f.live:
		return gateServe
	case !https && f.envTLS:
		return gateRefuse
	case !https:
		return gateServe
	case strings.HasPrefix(f.path, "/.well-known/acme-challenge/"), f.loopback, f.forwardedHTTPS:
		return gateServe
	}
	return gateRedirect
}

func TestGateDecisionTable(t *testing.T) {
	bools := []bool{false, true}
	paths := []string{"/healthz", "/readyz", "/.well-known/acme-challenge/tok", "/api/version", "/", "/healthz/x", "/.well-known/acme-challenge"}
	urls := []string{"", "http://h:7480", "https://h:8443", "https://h:8443/tp"}
	var seen = map[gateAction]int{}
	n := 0
	for _, overTLS := range bools {
		for _, path := range paths {
			for _, loopback := range bools {
				for _, fwd := range bools {
					for _, live := range bools {
						for _, pub := range urls {
							for _, env := range bools {
								f := gateFacts{overTLS: overTLS, path: path, loopback: loopback, forwardedHTTPS: fwd, live: live, publicURL: pub, envTLS: env}
								got, want := decideGate(f), gateOracle(f)
								if got != want {
									t.Errorf("decideGate(%+v) = %d, want %d", f, got, want)
								}
								seen[got]++
								n++
							}
						}
					}
				}
			}
		}
	}
	for _, a := range []gateAction{gateServe, gateRedirect, gateRefuse} {
		if seen[a] == 0 {
			t.Errorf("the table never produces action %d: it checks nothing about it", a)
		}
	}
	// The rules the plan states, spelled out for a reader.
	base := gateFacts{path: "/api/version", live: true, publicURL: "https://h:8443"}
	if decideGate(base) != gateRedirect {
		t.Error("live + https + non-exempt + non-loopback + not forwarded must redirect")
	}
	if f := (gateFacts{path: "/api/version", live: true, publicURL: "http://h:7480", envTLS: true}); decideGate(f) != gateRefuse {
		t.Error("live + http public URL + env TLS must refuse")
	}
	if f := (gateFacts{path: "/api/version", live: true, envTLS: true, loopback: true}); decideGate(f) != gateRefuse {
		t.Error("the D-14 refusal exempts only the probes, not a loopback peer")
	}
	if f := (gateFacts{path: "/api/version", live: true, publicURL: "http://h:7480"}); decideGate(f) != gateServe {
		t.Error("live + http public URL + no env TLS must serve")
	}
	if f := (gateFacts{path: "/api/version", publicURL: "https://h:8443"}); decideGate(f) != gateServe {
		t.Error("TLS not live must serve")
	}
	t.Logf("%d rows checked", n)
}

func TestRedirectTargetIgnoresHostileTargets(t *testing.T) {
	const pub = "https://h:8443"
	req := func(method, target string, mutate func(*http.Request)) *http.Request {
		r := httptest.NewRequest(method, target, nil)
		if mutate != nil {
			mutate(r)
		}
		return r
	}
	empty := &http.Request{Method: http.MethodGet, URL: &url.URL{}}
	cases := []struct {
		name string
		pub  string
		r    *http.Request
		want string
	}{
		{"plain path and query", pub, req("GET", "/api/version?x=1", nil), pub + "/api/version?x=1"},
		{"Host header is ignored", pub, req("GET", "/x", func(r *http.Request) { r.Host = "evil.test" }), pub + "/x"},
		{"absolute-form target is ignored", pub, req("GET", "http://evil.test/x", nil), pub + "/x"},
		{"double slash stays a path", pub, req("GET", "//evil.test/x", nil), pub + "//evil.test/x"},
		{"backslash is escaped", pub, req("GET", "/\\evil.test", nil), pub + "/%5Cevil.test"},
		{"encoded slashes keep their encoding", pub, req("GET", "/%2f%2fevil.test", nil), pub + "/%2f%2fevil.test"},
		{"query with ampersand and percent is kept raw", pub, req("GET", "/x?a=1&b=%25&c", nil), pub + "/x?a=1&b=%25&c"},
		{"OPTIONS * goes to the root", pub, req("OPTIONS", "*", nil), pub + "/"},
		{"an empty path goes to the root", pub, empty, pub + "/"},
		{"a base path is kept", pub + "/tp", req("GET", "/api/version", nil), pub + "/tp/api/version"},
		{"a trailing slash on the public URL is not doubled", pub + "/", req("GET", "/x", nil), pub + "/x"},
		{"a port", "https://h.example:9443", req("GET", "/x", nil), "https://h.example:9443/x"},
		{"an IPv6 literal host", "https://[2001:db8::1]:8443", req("GET", "/x?y=1", nil), "https://[2001:db8::1]:8443/x?y=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redirectTarget(tc.pub, tc.r)
			if got != tc.want {
				t.Errorf("redirectTarget = %q, want %q", got, tc.want)
			}
			if !strings.HasPrefix(got, strings.TrimRight(tc.pub, "/")) {
				t.Errorf("Location %q does not start with the public URL %q", got, tc.pub)
			}
			if strings.Contains(got, "evil.test") && !strings.Contains(tc.want, "evil.test") {
				t.Errorf("Location %q carries the hostile authority", got)
			}
		})
	}
}

func TestGateRefusesPlainHTTPOnAnEnvTLSInstallWithoutAnHTTPSPublicURL(t *testing.T) {
	for _, env := range []map[string]string{
		{"TIERPLAN_TLS_SELF_SIGNED": "true"},
	} {
		srv := gateServer(t, env)
		for _, peer := range []string{nonLoopbackPeer, "127.0.0.1:40000"} {
			rec := plainFrom(srv, http.MethodGet, "/api/version", peer, func(r *http.Request) { r.Host = "evil.test" })
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%v from %s: status = %d, want 400", env, peer, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
			if !strings.Contains(rec.Body.String(), "https://") {
				t.Errorf("body = %q, want it to say to use https://", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "evil.test") || strings.Contains(rec.Body.String(), "/api/version") {
				t.Errorf("body %q echoes the request", rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("Location = %q, want none", loc)
			}
			if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
				t.Errorf("Strict-Transport-Security = %q, want none", got)
			}
		}
		// Sign-in and the setup wizard are not reachable in clear either.
		for _, target := range []string{"/", "/api/auth/login", "/api/setup/status", "/setup"} {
			if rec := plainFrom(srv, http.MethodGet, target, nonLoopbackPeer, nil); rec.Code != http.StatusBadRequest {
				t.Errorf("%v GET %s: status = %d, want 400", env, target, rec.Code)
			}
		}
		for _, probe := range []string{"/healthz", "/readyz"} {
			if rec := plainFrom(srv, http.MethodGet, probe, nonLoopbackPeer, nil); rec.Code != http.StatusOK {
				t.Errorf("%v %s: status = %d, want 200", env, probe, rec.Code)
			}
		}
	}
}

func TestGateServesPlainHTTPWhenTLSIsLiveButThePublicURLIsHTTP(t *testing.T) {
	srv := gateServer(t, nil)
	installSuppliedCert(t, srv)
	if !srv.https.Live() {
		t.Fatal("TLS is not live")
	}
	for _, pub := range []string{"http://h:7480", ""} {
		srv.Instance().Apply("TierPlan", pub)
		rec := plainFrom(srv, http.MethodGet, "/api/version", nonLoopbackPeer, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("public URL %q: status = %d, want 200 (TierPlan never redirects to a URL it cannot name)", pub, rec.Code)
		}
		if rec.Header().Get("Location") != "" {
			t.Errorf("public URL %q: Location = %q, want none", pub, rec.Header().Get("Location"))
		}
	}
}

func TestGateServesWhenTLSIsNotLive(t *testing.T) {
	srv := gateServer(t, map[string]string{"TIERPLAN_PUBLIC_URL": "https://h:8443"})
	// A fresh install is live now; take the certificate away to reach the gate's not-live rule.
	srv.https.Remove()
	if srv.https.Live() {
		t.Fatal("TLS is live")
	}
	if rec := plainFrom(srv, http.MethodGet, "/api/version", nonLoopbackPeer, nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: with no TLS there is nothing to redirect to", rec.Code)
	}
}

func TestGateExemptions(t *testing.T) {
	srv := gateServer(t, map[string]string{
		"TIERPLAN_TLS_SELF_SIGNED": "true",
		"TIERPLAN_PUBLIC_URL":      "https://h:8443",
		"TIERPLAN_TRUSTED_PROXIES": "203.0.113.0/24",
	})
	for _, target := range []string{"/healthz", "/readyz", "/.well-known/acme-challenge/tok"} {
		if rec := plainFrom(srv, http.MethodGet, target, nonLoopbackPeer, nil); !gatePassed(rec) {
			t.Errorf("%s: status = %d, Location = %q, want it served", target, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, peer := range []string{"127.0.0.1:40000", "[::1]:40000", "[::ffff:127.0.0.1]:40000"} {
		if rec := plainFrom(srv, http.MethodGet, "/api/version", peer, nil); !gatePassed(rec) {
			t.Errorf("loopback peer %s: status = %d, want it served", peer, rec.Code)
		}
	}
	// A loopback claim in a header is worth nothing.
	forged := func(r *http.Request) {
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r.Header.Set("X-Real-IP", "127.0.0.1")
	}
	if rec := plainFrom(srv, http.MethodGet, "/api/version", "198.51.100.7:40000", forged); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("forged loopback header: status = %d, want 307", rec.Code)
	}
	https := func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }
	if rec := plainFrom(srv, http.MethodGet, "/api/version", nonLoopbackPeer, https); !gatePassed(rec) {
		t.Errorf("trusted proxy marking https: status = %d, want it served (a proxied install never loops)", rec.Code)
	}
	if rec := plainFrom(srv, http.MethodGet, "/api/version", "198.51.100.7:40000", https); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("untrusted peer marking https: status = %d, want 307 (the header is ignored)", rec.Code)
	}
	// A trusted proxy that says http still gets the redirect.
	plainProto := func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }
	if rec := plainFrom(srv, http.MethodGet, "/api/version", nonLoopbackPeer, plainProto); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("trusted proxy marking http: status = %d, want 307", rec.Code)
	}
}

func TestGateRedirectAlwaysPointsAtHTTPS(t *testing.T) {
	srv := gateServer(t, nil)
	installSuppliedCert(t, srv)
	srv.Instance().Apply("TierPlan", "https://a:8443")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for flip := false; ; flip = !flip {
			select {
			case <-stop:
				return
			default:
			}
			if flip {
				srv.Instance().Apply("TierPlan", "http://a:8443")
			} else {
				srv.Instance().Apply("TierPlan", "https://a:8443")
			}
		}
	}()
	redirects := 0
	for range 300 {
		rec := plainFrom(srv, http.MethodGet, "/api/version", nonLoopbackPeer, nil)
		if rec.Code == http.StatusTemporaryRedirect {
			redirects++
			if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://a:8443") {
				t.Errorf("Location = %q, want it to start https://a:8443 while the URL is being changed", loc)
			}
		}
	}
	close(stop)
	<-done
	t.Logf("%d of 300 requests were redirected", redirects)
}
