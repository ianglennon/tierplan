// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package httpx

import (
	"net/http"
	"net/url"
	"strings"
)

// StripBasePath removes the base path from a request path that equals it or
// starts with base + "/", and leaves every other path unchanged. It is the
// outermost handler, so it runs before the router and every middleware.
//
// Coaming is served under the path of its public URL (D-50). A proxy that
// strips the prefix sends "/api/version" and one that does not sends
// "/coaming/api/version"; both reach the same route. Probes such as /healthz
// arrive unprefixed and pass through untouched. base is read on every request,
// so a public URL changed at runtime applies at once, and "" disables the
// middleware. The base is never taken from a request header (D-51, T-02-11).
func StripBasePath(base func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := ""
			if base != nil {
				b = base()
			}
			if b == "" {
				next.ServeHTTP(w, r)
				return
			}
			// base is the escaped path of the public URL, so it is compared
			// escaped against RawPath and decoded against Path.
			decoded, err := url.PathUnescape(b)
			if err != nil {
				decoded = b
			}
			stripped, ok := trimBase(r.URL.Path, decoded)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			// Clone rather than mutate: the caller's request stays as received.
			r2 := r.Clone(r.Context())
			r2.URL.Path = stripped
			if r.URL.RawPath != "" {
				// If RawPath does not carry the escaped base, drop it and let
				// Path be encoded canonically rather than serve a mismatch.
				r2.URL.RawPath, _ = trimBase(r.URL.RawPath, b)
			}
			next.ServeHTTP(w, r2)
		})
	}
}

// trimBase returns p without the leading base when p equals base or starts
// with base + "/"; the remainder always starts with "/".
func trimBase(p, base string) (string, bool) {
	if p == base {
		return "/", true
	}
	if rest, ok := strings.CutPrefix(p, base+"/"); ok {
		return "/" + rest, true
	}
	return "", false
}
