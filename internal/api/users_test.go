// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
)

// Creating a user (plan 03-11, AUTH-04, D-22a, D-24). Negative cases are
// mandatory (D-81).

type userSummary struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	DisplayName   string `json:"displayName"`
	Email         string `json:"email"`
	InstanceAdmin bool   `json:"instanceAdmin"`
	Status        string `json:"status"`
	LastLoginAt   string `json:"lastLoginAt"`
	Teams         []any  `json:"teams"`
}

type createUserResp struct {
	User userSummary `json:"user"`
	Link struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"link"`
}

// tokenInLink is the 43 base64url characters of a 256-bit token.
var tokenInLink = regexp.MustCompile(`/invite#token=([A-Za-z0-9_-]{43})$`)

func (f *authFixture) createUser(t *testing.T, cookie, csrf string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return f.call(t, authReq{method: http.MethodPost, path: "/api/users", body: string(b), cookie: cookie, csrf: csrf})
}

// invite creates a user as the admin and returns the token of the invite link.
func (f *authFixture) invite(t *testing.T, cookie, csrf, username string) string {
	t.Helper()
	rec := f.createUser(t, cookie, csrf, map[string]any{"username": username})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", username, rec.Code, rec.Body)
	}
	var got createUserResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := tokenInLink.FindStringSubmatch(got.Link.URL)
	if m == nil {
		t.Fatalf("link %q does not end in /invite#token=<43 characters>", got.Link.URL)
	}
	return m[1]
}

func TestCreateUserAnswers201WithTheUserAndAnInviteLinkInTheFragment(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.createUser(t, cookie, csrf, map[string]any{"username": "Alice"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var got createUserResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	u := got.User
	if u.ID == "" || u.Username != "alice" || u.DisplayName != "" || u.Email != "" || u.InstanceAdmin || u.Status != "invite_pending" || u.Teams == nil {
		t.Fatalf("user = %+v", u)
	}
	// No public URL is set: the link is relative to this Bayplan.
	if !strings.HasPrefix(got.Link.URL, "/invite#token=") || !tokenInLink.MatchString(got.Link.URL) {
		t.Fatalf("link url = %q", got.Link.URL)
	}
	if got.Link.ExpiresAt != "2026-01-04T00:00:00Z" {
		t.Fatalf("expiresAt = %q, want 72 hours after the fixture clock", got.Link.ExpiresAt)
	}
}

func TestCreateUserLinkStartsWithThePublicURLWhenOneIsSet(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://bayplan.example/sub/"})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.createUser(t, cookie, csrf, map[string]any{"username": "alice"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got createUserResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Link.URL, "https://bayplan.example/sub/invite#token=") {
		t.Fatalf("link url = %q, want it under the public URL", got.Link.URL)
	}
}

func TestCreateUserKeepsTheDisplayNameAndEmailAndTrimsThem(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.createUser(t, cookie, csrf, map[string]any{"username": "alice", "displayName": "  Alice Liddell ", "email": " alice@example.test "})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got createUserResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.User.DisplayName != "Alice Liddell" || got.User.Email != "alice@example.test" {
		t.Fatalf("user = %+v", got.User)
	}
}

func TestCreateUserNeedsUsersManage(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "bob", false)
	bobCookie, bobCSRF := f.signIn(t, "bob", false)

	if rec := f.createUser(t, bobCookie, bobCSRF, map[string]any{"username": "alice"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a user who is not an instance admin: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.createUser(t, "", "", map[string]any{"username": "alice"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s, want 401", rec.Code, rec.Body)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'alice'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("users named alice = %d, %v; a refused request must create nothing", n, err)
	}
}

// The tracer: an admin invites alice, alice opens the link, chooses a password
// and signs in with it.
func TestCreateUserThenAcceptTheInviteThenSignIn(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	adminCookie, adminCSRF := f.signIn(t, "admin", false)
	token := f.invite(t, adminCookie, adminCSRF, "alice")

	lookup := f.call(t, authReq{method: http.MethodPost, path: "/api/invite/lookup", body: `{"token":"` + token + `"}`})
	if lookup.Code != http.StatusOK {
		t.Fatalf("lookup = %d %s", lookup.Code, lookup.Body)
	}
	var info struct{ Kind, Username, InstanceName string }
	if err := json.Unmarshal(lookup.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Kind != "invite" || info.Username != "alice" {
		t.Fatalf("lookup = %+v", info)
	}

	// Before a password exists alice cannot sign in.
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", "anything at all", false)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in before the invite is accepted = %d, want 401", rec.Code)
	}

	const chosen = "a long chosen passphrase"
	accept := f.call(t, authReq{method: http.MethodPost, path: "/api/invite/accept", body: `{"token":"` + token + `","password":"` + chosen + `"}`})
	if accept.Code != http.StatusNoContent {
		t.Fatalf("accept = %d %s", accept.Code, accept.Body)
	}
	if c := sessionCookie(accept); c != nil {
		t.Fatalf("accepting a link set the cookie %+v; it must not sign anyone in", c)
	}
	if n := f.sessionCount(t); n != 1 { // the admin's
		t.Fatalf("sessions = %d, want only the admin's", n)
	}

	signIn := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", chosen, false)})
	if signIn.Code != http.StatusOK {
		t.Fatalf("sign-in with the chosen password = %d %s", signIn.Code, signIn.Body)
	}

	for _, action := range []string{"user.created", "user.invite_created", "user.invite_accepted"} {
		if got := f.audits(t, action); len(got) != 1 {
			t.Fatalf("%s rows = %+v, want one", action, got)
		}
	}
}
