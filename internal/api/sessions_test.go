// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
)

// The session routes (plan 03-15, AUTH-04, D-23). Negative cases are mandatory
// (D-81).

type sessionResp struct {
	ID         string `json:"id"`
	UserAgent  string `json:"userAgent"`
	IP         string `json:"ip"`
	CreatedAt  string `json:"createdAt"`
	LastSeenAt string `json:"lastSeenAt"`
	Current    bool   `json:"current"`
}

func (f *authFixture) mySessions(t *testing.T, cookie string) []sessionResp {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me/sessions", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("list sessions: %d %s", rec.Code, rec.Body)
	}
	var out []sessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not a session list: %v: %s", err, rec.Body)
	}
	return out
}

// otherSession is the one session in the list that is not the caller's.
func otherSession(t *testing.T, list []sessionResp) sessionResp {
	t.Helper()
	for _, s := range list {
		if !s.Current {
			return s
		}
	}
	t.Fatalf("no other session in %+v", list)
	return sessionResp{}
}

func (f *authFixture) revokeMine(t *testing.T, cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodDelete, path: "/api/me/sessions/" + id, cookie: cookie, csrf: csrf})
}

func TestMySessionsListsTheCallersSessionsWithTheCurrentOneMarkedAndNoHash(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	f.signIn(t, "admin", true)
	f.signIn(t, "admin", false)
	f.addUser(t, "alice", false)
	f.signIn(t, "alice", false) // another user's session must not appear

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me/sessions", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "hash") {
		t.Fatalf("the body mentions a hash: %s", rec.Body)
	}
	list := f.mySessions(t, cookie)
	if len(list) != 3 {
		t.Fatalf("sessions = %+v, want the admin's three", list)
	}
	current := 0
	for _, s := range list {
		if len(s.ID) != 32 || s.UserAgent != authUA || s.IP != authIP || s.CreatedAt == "" || s.LastSeenAt == "" {
			t.Fatalf("session = %+v", s)
		}
		if s.Current {
			current++
		}
	}
	if current != 1 {
		t.Fatalf("current sessions = %d, want exactly one", current)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me/sessions"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: %d, want 401", rec.Code)
	}
}

func TestMySessionRevokeSignsTheOtherBrowserOutAtItsNextRequestAndAudits(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	other, _ := f.signIn(t, "admin", false)
	target := otherSession(t, f.mySessions(t, cookie))

	if code := f.meStatus(t, other); code != http.StatusOK {
		t.Fatalf("the other session before revoke: %d, want 200", code)
	}
	rec := f.revokeMine(t, cookie, csrf, target.ID)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s, want 204", rec.Code, rec.Body)
	}
	if code := f.meStatus(t, other); code != http.StatusUnauthorized {
		t.Fatalf("the revoked session's next request: %d, want 401", code)
	}
	if code := f.meStatus(t, cookie); code != http.StatusOK {
		t.Fatalf("the current session after revoke: %d, want 200", code)
	}
	entries := f.audits(t, "session.revoked")
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != "id-admin" || entries[0].Result != "success" {
		t.Fatalf("audit = %+v", entries)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'session.revoked'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"handle":"`+target.ID+`"`) || !strings.Contains(details, `"device":"`+authUA+`"`) || !strings.Contains(details, `"username":"admin"`) {
		t.Fatalf("details = %s", details)
	}
}

func TestMySessionRevokeOfTheCurrentSessionIs409CurrentSessionAndOfAnotherUsersIs404(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.signIn(t, "admin", false)
	f.addUser(t, "alice", false)
	aliceCookie, _ := f.signIn(t, "alice", false)

	var current sessionResp
	for _, s := range f.mySessions(t, cookie) {
		if s.Current {
			current = s
		}
	}
	rec := f.revokeMine(t, cookie, csrf, current.ID)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "current_session" {
		t.Fatalf("revoke current: %d %s, want 409 current_session", rec.Code, rec.Body)
	}
	if code := f.meStatus(t, cookie); code != http.StatusOK {
		t.Fatalf("the current session after a refused revoke: %d", code)
	}

	aliceSession := f.mySessions(t, aliceCookie)[0]
	rec = f.revokeMine(t, cookie, csrf, aliceSession.ID)
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "session_not_found" {
		t.Fatalf("revoke another user's: %d %s, want 404 session_not_found", rec.Code, rec.Body)
	}
	if code := f.meStatus(t, aliceCookie); code != http.StatusOK {
		t.Fatal("another user's session was revoked")
	}
	if rec := f.revokeMine(t, cookie, csrf, "no-such-handle"); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d, want 404", rec.Code)
	}
}

func TestMySessionRevokeNeedsAValidCSRFToken(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	f.signIn(t, "admin", false)
	target := otherSession(t, f.mySessions(t, cookie))
	if rec := f.revokeMine(t, cookie, "", target.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("revoke without a token: %d, want 403", rec.Code)
	}
	if len(f.mySessions(t, cookie)) != 2 {
		t.Fatal("a refused revoke removed a session")
	}
}

func TestMySessionRevokeOthersKeepsOnlyTheCurrentSessionAndAudits(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	o1, _ := f.signIn(t, "admin", false)
	o2, _ := f.signIn(t, "admin", true)
	f.addUser(t, "alice", false)
	aliceCookie, _ := f.signIn(t, "alice", false)

	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/me/sessions/revoke-others", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke others: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Ended int `json:"ended"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Ended != 2 {
		t.Fatalf("answer = %s (%v), want ended 2", rec.Body, err)
	}
	for _, c := range []string{o1, o2} {
		if code := f.meStatus(t, c); code != http.StatusUnauthorized {
			t.Fatalf("another session after revoke-others: %d, want 401", code)
		}
	}
	if f.meStatus(t, cookie) != http.StatusOK || f.meStatus(t, aliceCookie) != http.StatusOK {
		t.Fatal("revoke-others ended the current session or another user's")
	}
	if n := len(f.audits(t, "user.sessions_revoked")); n != 1 {
		t.Fatalf("user.sessions_revoked entries = %d, want 1", n)
	}
}

func TestUserSessionsListedAndEndedByAnInstanceAdminButNotByAnotherUser(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.addUser(t, "alice", false)
	f.addUser(t, "mallory", false)
	a1, _ := f.signIn(t, "alice", false)
	a2, _ := f.signIn(t, "alice", false)
	malloryCookie, malloryCSRF := f.signIn(t, "mallory", false)

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/users/id-alice/sessions", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list []sessionResp
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 2 {
		t.Fatalf("list = %s (%v), want alice's two", rec.Body, err)
	}
	for _, s := range list {
		if s.Current {
			t.Fatalf("an admin's view of someone else's sessions marks one current: %+v", s)
		}
	}

	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/users/id-alice/sessions", cookie: malloryCookie}); rec.Code != http.StatusForbidden {
		t.Fatalf("a user listing another's sessions: %d, want 403", rec.Code)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/users/id-alice/sessions/revoke-all", cookie: malloryCookie, csrf: malloryCSRF}); rec.Code != http.StatusForbidden {
		t.Fatalf("a user ending another's sessions: %d, want 403", rec.Code)
	}
	if f.meStatus(t, a1) != http.StatusOK {
		t.Fatal("a refused request ended a session")
	}

	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/users/id-alice/sessions/revoke-all", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke all: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Ended int `json:"ended"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Ended != 2 {
		t.Fatalf("answer = %s, want ended 2", rec.Body)
	}
	for _, c := range []string{a1, a2} {
		if code := f.meStatus(t, c); code != http.StatusUnauthorized {
			t.Fatalf("an ended session: %d, want 401", code)
		}
	}
	entries := f.audits(t, "user.sessions_revoked")
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != "id-alice" {
		t.Fatalf("audit = %+v", entries)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/users/no-such-user/sessions/revoke-all", cookie: cookie, csrf: csrf}); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("unknown user: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
}

func TestMySessionSchemaCarriesNoSessionHashField(t *testing.T) {
	t.Parallel()
	spec, err := os.ReadFile("../../openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/me/sessions", "/api/me/sessions/{publicId}", "/api/me/sessions/revoke-others", "/api/users/{userId}/sessions", "/api/users/{userId}/sessions/revoke-all"} {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("openapi.json has no path %s", p)
		}
	}
	if strings.Contains(string(spec), `"idHash"`) {
		t.Error("openapi.json carries an idHash field")
	}
}
