// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package httpx

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func hstsFor(t *testing.T, opts Options, req *http.Request) string {
	t.Helper()
	r := New(opts)
	r.Exempt(http.MethodGet, "/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec.Header().Get("Strict-Transport-Security")
}

func TestHSTSIsSentWhenTheHookSaysSo(t *testing.T) {
	opts := Options{HSTS: func(r *http.Request) bool { return r.TLS != nil }}

	plain := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if got := hstsFor(t, opts, plain); got != "" {
		t.Errorf("plain HTTP request: Strict-Transport-Security = %q, want none", got)
	}

	secure := httptest.NewRequest(http.MethodGet, "https://bayplan.example/healthz", nil)
	secure.TLS = &tls.ConnectionState{}
	if got := hstsFor(t, opts, secure); got != "max-age=31536000" {
		t.Errorf("TLS request: Strict-Transport-Security = %q, want max-age=31536000", got)
	}
}

func TestHSTSIsAbsentWithoutAHook(t *testing.T) {
	secure := httptest.NewRequest(http.MethodGet, "https://bayplan.example/healthz", nil)
	secure.TLS = &tls.ConnectionState{}
	if got := hstsFor(t, Options{}, secure); got != "" {
		t.Errorf("no hook: Strict-Transport-Security = %q, want none", got)
	}
}

func TestHSTSIsAlsoSentOnErrorsAndNotFound(t *testing.T) {
	opts := Options{HSTS: func(*http.Request) bool { return true }}
	r := New(opts)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Strict-Transport-Security") == "" {
		t.Errorf("status %d, HSTS %q: a 404 must carry the header too", rec.Code, rec.Header().Get("Strict-Transport-Security"))
	}
}
