// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
