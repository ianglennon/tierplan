// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://x"+target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChallengesServeUntilCleanUp(t *testing.T) {
	ch := NewChallenges()
	if ch.Type() != ChallengeHTTP01 {
		t.Fatalf("Type = %q, want %q", ch.Type(), ChallengeHTTP01)
	}
	if err := ch.Present(t.Context(), "a.example.com", "tok-1_A", "tok-1_A.thumb"); err != nil {
		t.Fatal(err)
	}
	if ch.Len() != 1 {
		t.Fatalf("Len = %d, want 1", ch.Len())
	}
	h := ch.Handler()
	for i := range 3 {
		rec := serve(h, http.MethodGet, challengePrefix+"tok-1_A")
		if rec.Code != http.StatusOK || rec.Body.String() != "tok-1_A.thumb" {
			t.Fatalf("GET %d = %d %q, want 200 and the key authorization", i, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "text/plain" {
			t.Fatalf("Content-Type = %q, want text/plain", got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	}
	head := serve(h, http.MethodHead, challengePrefix+"tok-1_A")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d with %d body bytes, want 200 and none", head.Code, head.Body.Len())
	}
	if err := ch.CleanUp(t.Context(), "a.example.com", "tok-1_A", "tok-1_A.thumb"); err != nil {
		t.Fatal(err)
	}
	if ch.Len() != 0 {
		t.Fatalf("Len after CleanUp = %d, want 0", ch.Len())
	}
	if rec := serve(h, http.MethodGet, challengePrefix+"tok-1_A"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after CleanUp = %d, want 404", rec.Code)
	}
	if err := ch.CleanUp(t.Context(), "a.example.com", "never", ""); err != nil {
		t.Fatalf("CleanUp of an unknown token: %v", err)
	}
}

func TestChallengesRejectBadTokensAndMethods(t *testing.T) {
	long := strings.Repeat("a", 300)
	ch := NewChallenges()
	for _, tok := range []string{"good", long} {
		if err := ch.Present(t.Context(), "a.example.com", tok, "ka-"+tok); err != nil {
			t.Fatal(err)
		}
	}
	// A token that is not base64url must never reach the map, even if one were stored under it.
	ch.tokens["a/b"] = "slash"
	ch.tokens["../x"] = "dots"
	h := ch.Handler()

	for _, tc := range []struct {
		name, method, target string
		want                 int
	}{
		{"a known token", http.MethodGet, challengePrefix + "good", http.StatusOK},
		{"a 300 character token of allowed characters", http.MethodGet, challengePrefix + long, http.StatusOK},
		{"an unknown token", http.MethodGet, challengePrefix + "other", http.StatusNotFound},
		{"dot dot", http.MethodGet, challengePrefix + "../x", http.StatusNotFound},
		{"a further slash", http.MethodGet, challengePrefix + "a/b", http.StatusNotFound},
		{"an encoded slash", http.MethodGet, challengePrefix + "a%2Fb", http.StatusNotFound},
		{"an empty token", http.MethodGet, challengePrefix, http.StatusNotFound},
		{"a dot in the token", http.MethodGet, challengePrefix + "a.b", http.StatusNotFound},
		{"outside the prefix", http.MethodGet, "/other/good", http.StatusNotFound},
		{"POST", http.MethodPost, challengePrefix + "good", http.StatusMethodNotAllowed},
		{"PUT", http.MethodPut, challengePrefix + "good", http.StatusMethodNotAllowed},
		{"DELETE", http.MethodDelete, challengePrefix + "good", http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, tc.method, tc.target)
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.target, rec.Code, tc.want)
			}
			if tc.want == http.StatusMethodNotAllowed {
				if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
					t.Fatalf("Allow = %q, want GET, HEAD", got)
				}
			}
			if tc.want == http.StatusNotFound && strings.Contains(rec.Body.String(), "slash") {
				t.Fatalf("a rejected token leaked a stored value: %q", rec.Body.String())
			}
		})
	}
}
