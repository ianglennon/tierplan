// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/ianglennon/tierplan/internal/httpx"
)

// acmeChallengePrefix is where an ACME HTTP-01 challenge is fetched. It is served,
// never redirected, so a certificate authority that starts on plain HTTP is not
// sent to an address it cannot validate.
const acmeChallengePrefix = "/.well-known/acme-challenge/"

// gateAction is what the gate does with one request.
type gateAction uint8

const (
	// gateServe hands the request to the router.
	gateServe gateAction = iota
	// gateRedirect answers a temporary redirect to the public URL (D-03).
	gateRedirect
	// gateRefuse answers 400: this install serves HTTPS only and has no https
	// address to send the caller to (D-14).
	gateRefuse
)

// gateFacts is everything decideGate needs, so the decision is a pure function.
type gateFacts struct {
	// overTLS is true when the request arrived on a TLS connection.
	overTLS bool
	// path is the request path with the base path already removed.
	path string
	// loopback is true when the TCP peer is a loopback address.
	loopback bool
	// forwardedHTTPS is true when a trusted proxy reports the caller used https.
	forwardedHTTPS bool
	// live is true when a certificate is installed.
	live bool
	// publicURL is the public URL as read once for this request.
	publicURL string
	// envTLS is true when TIERPLAN_TLS_SELF_SIGNED or a certificate file is set.
	envTLS bool
}

// decideGate says what to do with a request. The order is the contract:
//
//  1. A request that arrived over TLS is served, so a followed redirect never loops.
//  2. /healthz and /readyz are served whatever the mode, so probes work everywhere.
//  3. With TLS not live there is no https to send anyone to: serve (D-02).
//  4. With a public URL that is not https, TierPlan cannot name a place to send
//     the caller. An install that was told to serve TLS refuses every other plain
//     request (D-14, v0.1.0's property: sign-in is never offered in clear); any
//     other install serves (D-02).
//  5. With an https public URL, the ACME challenge, a loopback peer and a request a
//     trusted proxy marks https are served; the rest is redirected (D-03).
func decideGate(f gateFacts) gateAction {
	if f.overTLS {
		return gateServe
	}
	if f.path == "/healthz" || f.path == "/readyz" {
		return gateServe
	}
	if !f.live {
		return gateServe
	}
	if !strings.HasPrefix(httpx.OriginOf(f.publicURL), "https://") {
		if f.envTLS {
			return gateRefuse
		}
		return gateServe
	}
	if strings.HasPrefix(f.path, acmeChallengePrefix) {
		return gateServe
	}
	if f.loopback || f.forwardedHTTPS {
		return gateServe
	}
	return gateRedirect
}

// redirectTarget is where a plain request is sent: the stored public URL, then the
// path and query exactly as the request carried them. Nothing the caller chose for
// the authority is read: not Host, and not the host of an absolute-form target
// (open redirect, T-13-19).
func redirectTarget(publicURL string, r *http.Request) string {
	p := r.URL.EscapedPath()
	if !strings.HasPrefix(p, "/") {
		// OPTIONS * and an empty path.
		p = "/"
	}
	target := strings.TrimRight(publicURL, "/") + p
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	return target
}

// isLoopbackPeer reports whether remoteAddr, the TCP peer, is a loopback address.
// It is judged from the connection, never from a header.
func isLoopbackPeer(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	return ap.Addr().Unmap().IsLoopback()
}

// refusePlainHTTP answers a plain request on an install that serves HTTPS only and
// has no https public URL. The body is fixed text: nothing from the request is
// echoed, and there is no Location.
func refusePlainHTTP(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte("This TierPlan serves HTTPS on this port. Use an https:// address.\n"))
}

// newHTTPSGate is the middleware that judges every request before the router sees
// it. It sits inside the base-path stripper, so the probes and the redirect target
// are judged on the path below the base. It answers before the router's security
// headers run, so nothing it writes carries Strict-Transport-Security (D-04), and
// it only ever redirects with 307, which no browser caches as a rule.
//
// publicURL is read once per request and feeds both the decision and the Location,
// so a redirect always names an https address even while the URL is being changed.
func newHTTPSGate(rt *httpsRuntime, publicURL func() string, envTLS bool, trusted []netip.Prefix) func(http.Handler) http.Handler {
	clientIP := httpx.ClientIPMiddleware(trusted)
	return func(next http.Handler) http.Handler {
		return clientIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pub := publicURL()
			switch decideGate(gateFacts{
				overTLS:        r.TLS != nil,
				path:           r.URL.Path,
				loopback:       isLoopbackPeer(r.RemoteAddr),
				forwardedHTTPS: httpx.EffectiveScheme(r.Context()) == "https",
				live:           rt.Live(),
				publicURL:      pub,
				envTLS:         envTLS,
			}) {
			case gateRedirect:
				h := w.Header()
				h.Set("Cache-Control", "no-store")
				h.Set("Location", redirectTarget(pub, r))
				w.WriteHeader(http.StatusTemporaryRedirect)
			case gateRefuse:
				refusePlainHTTP(w)
			default:
				next.ServeHTTP(w, r)
			}
		}))
	}
}
