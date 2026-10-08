// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
)

// The account lifecycle routes (plan 03-14, AUTH-04, D-22c, D-02). Negative
// cases are mandatory (D-81).

func (f *authFixture) userAction(t *testing.T, cookie, csrf, method, id, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/users/" + id
	if action != "" {
		path += "/" + action
	}
	return f.call(t, authReq{method: method, path: path, body: body, cookie: cookie, csrf: csrf})
}

func (f *authFixture) disable(t *testing.T, cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.userAction(t, cookie, csrf, http.MethodPost, id, "disable", "")
}

func (f *authFixture) enable(t *testing.T, cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.userAction(t, cookie, csrf, http.MethodPost, id, "enable", "")
}

type userDetailResp struct {
	userSummary
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func decodeDetail(t *testing.T, rec *httptest.ResponseRecorder) userDetailResp {
	t.Helper()
	var d userDetailResp
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("not a user: %v: %s", err, rec.Body)
	}
	return d
}

func TestGetUserAnswersTheDetailAndAnUnknownIDIs404UserNotFound(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)

	rec := f.userAction(t, cookie, "", http.MethodGet, id, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	d := decodeDetail(t, rec)
	if d.ID != id || d.Username != "alice" || d.Status != "active" || d.CreatedAt == "" || d.UpdatedAt == "" || d.Teams == nil {
		t.Fatalf("detail = %+v", d)
	}
	if rec := f.userAction(t, cookie, "", http.MethodGet, "no-such-user", "", ""); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("unknown id: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
}

func TestDisableUserEndsEverySessionRefusesSignInAndEnableRestoresIt(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)
	f.addTeam(t, "ops", "alice")
	f.addTeamRole(t, "team-ops", "env-a", "operator")
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
		VALUES ('ra-alice', 'user', ?, 'operator', 'env-a', 0)`, id); err != nil {
		t.Fatal(err)
	}
	c1, _ := f.signIn(t, "alice", false)
	c2, _ := f.signIn(t, "alice", true)

	rec := f.disable(t, cookie, csrf, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if d := decodeDetail(t, rec); d.Status != "disabled" {
		t.Fatalf("answer status = %q, want disabled", d.Status)
	}
	for _, c := range []string{c1, c2} {
		if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: c}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a session of the disabled user: %d %s, want 401", rec.Code, rec.Body)
		}
	}
	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", goodPassword, false)})
	if rec.Code != http.StatusUnauthorized || problemType(t, rec) != "invalid_credentials" {
		t.Fatalf("sign-in of a disabled user: %d %s, want the generic 401", rec.Code, rec.Body)
	}
	var kept int
	if err := f.st.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM role_assignments WHERE principal_id = ?) + (SELECT COUNT(*) FROM team_members WHERE user_id = ?)`, id, id).Scan(&kept); err != nil || kept != 2 {
		t.Fatalf("kept grants and memberships = %d (%v), want the grant and the membership", kept, err)
	}
	entries := f.audits(t, "user.disabled")
	if len(entries) != 1 || entries[0].TargetID != id || entries[0].ActorID != "id-admin" || entries[0].Result != "success" {
		t.Fatalf("audit = %+v", entries)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'user.disabled'`).Scan(&details); err != nil || details != `{"ended":"2","paused":"0","username":"alice"}` {
		t.Fatalf("details = %s (%v), want ended 2", details, err)
	}

	rec = f.enable(t, cookie, csrf, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	if d := decodeDetail(t, rec); d.Status != "active" {
		t.Fatalf("answer status = %q, want active", d.Status)
	}
	f.signIn(t, "alice", false) // can sign in again
	if n := len(f.audits(t, "user.enabled")); n != 1 {
		t.Fatalf("user.enabled entries = %d, want 1", n)
	}
}

func TestDisableAndEnableAreNoOpsWhenAlreadyInTheState(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)

	if rec := f.enable(t, cookie, csrf, id); rec.Code != http.StatusOK {
		t.Fatalf("enable an enabled user: %d %s", rec.Code, rec.Body)
	}
	f.disable(t, cookie, csrf, id)
	if rec := f.disable(t, cookie, csrf, id); rec.Code != http.StatusOK {
		t.Fatalf("disable a disabled user: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "user.enabled")); n != 0 {
		t.Fatalf("user.enabled entries = %d, want none for a no-op", n)
	}
	if n := len(f.audits(t, "user.disabled")); n != 1 {
		t.Fatalf("user.disabled entries = %d, want one", n)
	}
}

func TestDisablingTheOnlyActiveInstanceAdminAnswers409LastInstanceAdmin(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.disable(t, cookie, csrf, "id-admin")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "last_instance_admin" {
		t.Fatalf("disable the only admin: %d %s, want 409 last_instance_admin", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("the admin's session after a refusal: %d, want it kept", rec.Code)
	}
	if n := len(f.audits(t, "user.disabled")); n != 0 {
		t.Fatalf("a refusal wrote %d entries", n)
	}

	f.addUser(t, "second", true)
	if rec := f.disable(t, cookie, csrf, "id-admin"); rec.Code != http.StatusOK {
		t.Fatalf("an admin disabling themselves with another admin around: %d %s", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("their own session after disabling themselves: %d, want 401", rec.Code)
	}
}

func TestDisableAndEnableAnUnknownUserAnswer404(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if rec := f.disable(t, cookie, csrf, "no-such-user"); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if rec := f.enable(t, cookie, csrf, "no-such-user"); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
}

func TestUserLifecycleRoutesNeedUsersManage(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	id := f.addUser(t, "alice", false)
	f.addUser(t, "bob", false)
	bob, bobCSRF := f.signIn(t, "bob", false)

	if rec := f.userAction(t, bob, "", http.MethodGet, id, "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("get: %d, want 403", rec.Code)
	}
	if rec := f.disable(t, bob, bobCSRF, id); rec.Code != http.StatusForbidden {
		t.Fatalf("disable: %d, want 403", rec.Code)
	}
	if rec := f.enable(t, bob, bobCSRF, id); rec.Code != http.StatusForbidden {
		t.Fatalf("enable: %d, want 403", rec.Code)
	}
	if rec := f.disable(t, "", "", id); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous disable: %d, want 401", rec.Code)
	}
}
