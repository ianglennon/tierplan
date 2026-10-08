// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
)

// The public invite routes (plan 03-11, AUTH-04, D-22a). Negative cases are
// mandatory (D-81).

func (f *authFixture) lookupInvite(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPost, path: "/api/invite/lookup", body: `{"token":"` + token + `"}`})
}

func (f *authFixture) acceptInvite(t *testing.T, token, password string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"token": token, "password": password})
	return f.call(t, authReq{method: http.MethodPost, path: "/api/invite/accept", body: string(b)})
}

// unknownToken is well formed (43 base64url characters) and was never issued.
const unknownToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestLookupInviteNamesTheUserAndTheInstance(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	token := f.invite(t, cookie, csrf, "alice")

	rec := f.lookupInvite(t, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got struct{ Kind, Username, InstanceName string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "invite" || got.Username != "alice" || got.InstanceName != f.inst.Name() {
		t.Fatalf("body = %+v", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestAnUnknownTokenAnswersTheOneInvalidLinkProblem(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"lookup": f.lookupInvite(t, unknownToken),
		"accept": f.acceptInvite(t, unknownToken, "a long chosen passphrase"),
	} {
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "invalid_link" {
			t.Fatalf("%s: %d %s, want 404 invalid_link", name, rec.Code, rec.Body)
		}
	}
}

func TestAcceptRefusesAPasswordThePolicyRefusesAndKeepsTheLink(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	token := f.invite(t, cookie, csrf, "alice")

	short := f.acceptInvite(t, token, "too short")
	if short.Code != http.StatusUnprocessableEntity || !strings.Contains(short.Body.String(), `"body.password"`) || !strings.Contains(short.Body.String(), "password_too_short") {
		t.Fatalf("short password: %d %s, want 422 on body.password password_too_short", short.Code, short.Body)
	}
	common := f.acceptInvite(t, token, "alice and a bit more")
	if common.Code != http.StatusUnprocessableEntity || !strings.Contains(common.Body.String(), "password_common") {
		t.Fatalf("password containing the username: %d %s, want 422 password_common", common.Code, common.Body)
	}
	// Neither spent the link.
	if rec := f.acceptInvite(t, token, "a long chosen passphrase"); rec.Code != http.StatusNoContent {
		t.Fatalf("accept after two refused passwords = %d %s, want 204", rec.Code, rec.Body)
	}
}

func TestEveryUnusableLinkAnswersTheSameBytes(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	expired := f.invite(t, cookie, csrf, "expired")
	spent := f.invite(t, cookie, csrf, "spent")
	disabled := f.invite(t, cookie, csrf, "disabled")
	replaced := f.invite(t, cookie, csrf, "replaced")

	if rec := f.acceptInvite(t, spent, "a long chosen passphrase"); rec.Code != http.StatusNoContent {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body)
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE username = 'disabled'`); err != nil {
		t.Fatal(err)
	}
	// "replaced" gets a new link; the one handed out first no longer exists.
	if _, err := f.st.Write.Exec(`DELETE FROM user_links WHERE user_id = (SELECT id FROM users WHERE username = 'replaced')`); err != nil {
		t.Fatal(err)
	}
	// "expired" ends before the clock reaches it; the others are not affected by it.
	if _, err := f.st.Write.Exec(`UPDATE user_links SET expires_at = ? WHERE user_id = (SELECT id FROM users WHERE username = 'expired')`, f.clk.now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	tokens := map[string]string{
		"expired": expired, "spent": spent, "disabled": disabled, "replaced": replaced,
		"unknown": unknownToken, "malformed": "not a token", "empty": "",
	}
	var first string
	for name, token := range tokens {
		for path, send := range map[string]func() *httptest.ResponseRecorder{
			"lookup": func() *httptest.ResponseRecorder { return f.lookupInvite(t, token) },
			"accept": func() *httptest.ResponseRecorder { return f.acceptInvite(t, token, "a long chosen passphrase") },
		} {
			rec := send()
			if rec.Code != http.StatusNotFound || problemType(t, rec) != "invalid_link" {
				t.Fatalf("%s %s: %d %s, want 404 invalid_link", name, path, rec.Code, rec.Body)
			}
			if first == "" {
				first = rec.Body.String()
			}
			if rec.Body.String() != first {
				t.Fatalf("%s %s: body %s differs from %s", name, path, rec.Body, first)
			}
		}
	}
	if !strings.Contains(first, "This link doesn't work.") {
		t.Fatalf("body = %s, want the one documented message", first)
	}
}

func TestInviteRoutesAreRateLimitedPerAddressAndTheRefusalIsRecordedOnce(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	// 5 attempts at once, then one every 12 seconds, as in the server.
	f.inviteLimiter = auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, f.clk.now)
	f.inviteAudit = auth.NewAuditThrottle(f.aud, auth.NewIPLimiter(time.Second, 1000, time.Hour, f.clk.now), slog.New(slog.DiscardHandler), f.clk.now)
	f.h = f.build(t)

	for i := 1; i <= 5; i++ {
		if rec := f.lookupInvite(t, unknownToken); rec.Code != http.StatusNotFound {
			t.Fatalf("attempt %d: %d %s, want 404 while within the budget", i, rec.Code, rec.Body)
		}
	}
	rec := f.lookupInvite(t, unknownToken)
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
		t.Fatalf("6th attempt: %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	// The accept route shares the address's budget.
	if rec := f.acceptInvite(t, unknownToken, "a long chosen passphrase"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("accept from the same address: %d, want 429", rec.Code)
	}
	// Another address has its own.
	other := f.call(t, authReq{method: http.MethodPost, path: "/api/invite/lookup", body: `{"token":"` + unknownToken + `"}`, ip: "198.51.100.7"})
	if other.Code != http.StatusNotFound {
		t.Fatalf("another address: %d, want 404", other.Code)
	}

	limited, other2 := 0, 0
	for _, e := range f.audits(t, "auth.link_refused") {
		switch e.Reason {
		case "rate_limited":
			limited++
		case "invalid_link":
			other2++
		}
		if e.ActorType != "anonymous" || e.TargetID != "" {
			t.Fatalf("refusal entry %+v, want an anonymous actor and no target id (the link is never recorded)", e)
		}
	}
	if limited != 2 || other2 != 6 {
		t.Fatalf("auth.link_refused rate_limited=%d invalid_link=%d, want 2 and 6", limited, other2)
	}
}
