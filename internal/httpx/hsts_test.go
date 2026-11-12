// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package httpx

import (
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

// The router sends whatever the hook returns and nothing of its own: the lifetime is the hook's
// decision (D-10), so a value other than the old one-year constant passes through unchanged.
func TestHSTSIsSentWithTheValueTheHookReturns(t *testing.T) {
	for _, value := range []string{"max-age=300", "max-age=0", "max-age=31536000"} {
		opts := Options{HSTS: func(*http.Request) string { return value }}
		req := httptest.NewRequest(http.MethodGet, "https://tierplan.example/healthz", nil)
		if got := hstsFor(t, opts, req); got != value {
			t.Errorf("hook returned %q: Strict-Transport-Security = %q", value, got)
		}
	}
}

func TestHSTSIsNotSentWhenTheHookReturnsNothing(t *testing.T) {
	opts := Options{HSTS: func(*http.Request) string { return "" }}
	req := httptest.NewRequest(http.MethodGet, "https://tierplan.example/healthz", nil)
	if got := hstsFor(t, opts, req); got != "" {
		t.Errorf("empty value: Strict-Transport-Security = %q, want none", got)
	}
}

func TestHSTSIsAbsentWithoutAHook(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://tierplan.example/healthz", nil)
	if got := hstsFor(t, Options{}, req); got != "" {
		t.Errorf("no hook: Strict-Transport-Security = %q, want none", got)
	}
}

func TestHSTSIsAlsoSentOnErrorsAndNotFound(t *testing.T) {
	opts := Options{HSTS: func(*http.Request) string { return "max-age=300" }}
	r := New(opts)
	// chi runs its middleware chain only once a route exists.
	r.Exempt(http.MethodGet, "/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Strict-Transport-Security") != "max-age=300" {
		t.Errorf("status %d, HSTS %q: a 404 must carry the header too", rec.Code, rec.Header().Get("Strict-Transport-Security"))
	}
}
