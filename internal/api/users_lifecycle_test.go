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
	LastInstanceAdmin bool   `json:"lastInstanceAdmin"`
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Delete, instance admin, unlock and profile (plan 03-14 Task 2).

func (f *authFixture) deleteUser(t *testing.T, cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.userAction(t, cookie, csrf, http.MethodDelete, id, "", "")
}

func (f *authFixture) setAdmin(t *testing.T, cookie, csrf, id string, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"enabled":false}`
	if enabled {
		body = `{"enabled":true}`
	}
	return f.userAction(t, cookie, csrf, http.MethodPost, id, "instance-admin", body)
}

func (f *authFixture) unlock(t *testing.T, cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.userAction(t, cookie, csrf, http.MethodPost, id, "unlock", "")
}

func (f *authFixture) patchUser(t *testing.T, cookie, csrf, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.userAction(t, cookie, csrf, http.MethodPatch, id, "", body)
}

func TestDeleteUserAnswers204AndLeavesATombstoneWithNothingThatLetsThemIn(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)
	f.addTeam(t, "ops", "alice")
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
		VALUES ('ra-alice', 'user', ?, 'operator', 'env-a', 0)`, id); err != nil {
		t.Fatal(err)
	}
	aliceCookie, _ := f.signIn(t, "alice", false)

	if rec := f.deleteUser(t, cookie, csrf, id); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", rec.Code, rec.Body)
	}
	var disabled int
	var deletedAt *int64
	var hash, email string
	if err := f.st.Read.QueryRow(`SELECT disabled, deleted_at, password_hash, email FROM users WHERE id = ?`, id).Scan(&disabled, &deletedAt, &hash, &email); err != nil {
		t.Fatal(err)
	}
	if disabled != 1 || deletedAt == nil || hash != "" || email != "" {
		t.Fatalf("tombstone = disabled %d deleted_at %v hash %q email %q", disabled, deletedAt, hash, email)
	}
	var left int
	if err := f.st.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions WHERE user_id = ?)
		+ (SELECT COUNT(*) FROM role_assignments WHERE principal_id = ?) + (SELECT COUNT(*) FROM team_members WHERE user_id = ?)`, id, id, id).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left for the user = %d (%v), want none", left, err)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: aliceCookie}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("their session: %d, want 401", rec.Code)
	}
	if n := len(f.audits(t, "user.deleted")); n != 1 {
		t.Fatalf("user.deleted entries = %d, want 1", n)
	}

	if rec := f.userAction(t, cookie, "", http.MethodGet, id, "", ""); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("get after delete: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
	if rec := f.deleteUser(t, cookie, csrf, id); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("delete again: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
	if rec := f.createUser(t, cookie, csrf, map[string]any{"username": "alice"}); rec.Code != http.StatusConflict || problemType(t, rec) != "username_reserved" {
		t.Fatalf("create the name again: %d %s, want 409 username_reserved", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", goodPassword, false)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in of a deleted user: %d, want 401", rec.Code)
	}
}

func TestDeletingTheOnlyActiveInstanceAdminAnswers409AndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.deleteUser(t, cookie, csrf, "id-admin")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "last_instance_admin" {
		t.Fatalf("delete the only admin: %d %s, want 409 last_instance_admin", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("the admin's session after a refusal: %d, want it kept", rec.Code)
	}
	if n := len(f.audits(t, "user.deleted")); n != 0 {
		t.Fatalf("a refusal wrote %d entries", n)
	}
}

func TestSetInstanceAdminGrantsAndRevokesAndRefusesTheLastOne(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)

	rec := f.setAdmin(t, cookie, csrf, id, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	if d := decodeDetail(t, rec); !d.InstanceAdmin {
		t.Fatalf("answer = %+v, want an instance admin", d)
	}
	if n := len(f.audits(t, "user.admin_granted")); n != 1 {
		t.Fatalf("user.admin_granted entries = %d, want 1", n)
	}
	// Granting to an admin is a no-op.
	if rec := f.setAdmin(t, cookie, csrf, id, true); rec.Code != http.StatusOK {
		t.Fatalf("grant again: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "user.admin_granted")); n != 1 {
		t.Fatalf("granting twice wrote %d entries, want 1", n)
	}

	if rec := f.setAdmin(t, cookie, csrf, id, false); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	} else if d := decodeDetail(t, rec); d.InstanceAdmin {
		t.Fatalf("answer = %+v, want no longer an instance admin", d)
	}
	if n := len(f.audits(t, "user.admin_revoked")); n != 1 {
		t.Fatalf("user.admin_revoked entries = %d, want 1", n)
	}

	rec = f.setAdmin(t, cookie, csrf, "id-admin", false)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "last_instance_admin" {
		t.Fatalf("demote the only admin: %d %s, want 409 last_instance_admin", rec.Code, rec.Body)
	}
	if rec := f.setAdmin(t, cookie, csrf, "no-such-user", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user: %d, want 404", rec.Code)
	}
	if rec := f.userAction(t, cookie, csrf, http.MethodPost, id, "instance-admin", `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a body with no enabled flag: %d, want 422", rec.Code)
	}
}

func TestUnlockClearsALockAuditedAsTheAdminAndIsANoOpOnAnUnlockedUser(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)
	for range 5 {
		f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", "wrong wrong wrong", false)})
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", goodPassword, false)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a locked account signed in: %d", rec.Code)
	}

	rec := f.unlock(t, cookie, csrf, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock: %d %s", rec.Code, rec.Body)
	}
	if d := decodeDetail(t, rec); d.Status != "active" {
		t.Fatalf("answer status = %q, want active", d.Status)
	}
	entries := f.audits(t, "auth.unlocked")
	if len(entries) != 1 || entries[0].ActorType != "user" || entries[0].ActorID != "id-admin" || entries[0].TargetID != id {
		t.Fatalf("audit = %+v, want one entry with the admin as actor", entries)
	}
	f.signIn(t, "alice", false)

	if rec := f.unlock(t, cookie, csrf, id); rec.Code != http.StatusOK {
		t.Fatalf("unlock an unlocked user: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "auth.unlocked")); n != 1 {
		t.Fatalf("auth.unlocked entries = %d, want no second one", n)
	}
	if rec := f.unlock(t, cookie, csrf, "no-such-user"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user: %d, want 404", rec.Code)
	}
}

func TestPatchUserSavesTheProfileAndRefusesAStaleVersionABadEmailAndAUsername(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)
	first := decodeDetail(t, f.userAction(t, cookie, "", http.MethodGet, id, "", ""))

	body := func(name, email, updatedAt string) string {
		b, _ := json.Marshal(map[string]any{"displayName": name, "email": email, "updatedAt": updatedAt})
		return string(b)
	}
	rec := f.patchUser(t, cookie, csrf, id, body("Alice L", "alice@example.test", first.UpdatedAt))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	saved := decodeDetail(t, rec)
	if saved.DisplayName != "Alice L" || saved.Email != "alice@example.test" || saved.UpdatedAt == first.UpdatedAt || saved.Username != "alice" {
		t.Fatalf("saved = %+v", saved)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'user.updated'`).Scan(&details); err != nil || details != `{"fields":"displayName,email","username":"alice"}` {
		t.Fatalf("details = %s (%v)", details, err)
	}

	// A second admin still holds the first version.
	rec = f.patchUser(t, cookie, csrf, id, body("Other", "alice@example.test", first.UpdatedAt))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stale_change" {
		t.Fatalf("stale save: %d %s, want 409 stale_change", rec.Code, rec.Body)
	}

	rec = f.patchUser(t, cookie, csrf, id, body("Alice L", "not an email", saved.UpdatedAt))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "body.email") {
		t.Fatalf("bad email: %d %s, want 422 on body.email", rec.Code, rec.Body)
	}

	rec = f.patchUser(t, cookie, csrf, id, `{"username":"mallory","displayName":"x","updatedAt":"`+saved.UpdatedAt+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a body with a username: %d %s, want 422", rec.Code, rec.Body)
	}
	if after := decodeDetail(t, f.userAction(t, cookie, "", http.MethodGet, id, "", "")); after.Username != "alice" {
		t.Fatalf("username = %q, want it unchanged", after.Username)
	}
	if rec := f.patchUser(t, cookie, csrf, "no-such-user", body("x", "", saved.UpdatedAt)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user: %d, want 404", rec.Code)
	}
}

func TestTheUserWriteRoutesNeedUsersManage(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	id := f.addUser(t, "alice", false)
	f.addUser(t, "bob", false)
	bob, bobCSRF := f.signIn(t, "bob", false)

	if rec := f.deleteUser(t, bob, bobCSRF, id); rec.Code != http.StatusForbidden {
		t.Fatalf("delete: %d, want 403", rec.Code)
	}
	if rec := f.setAdmin(t, bob, bobCSRF, id, true); rec.Code != http.StatusForbidden {
		t.Fatalf("instance-admin: %d, want 403", rec.Code)
	}
	if rec := f.unlock(t, bob, bobCSRF, id); rec.Code != http.StatusForbidden {
		t.Fatalf("unlock: %d, want 403", rec.Code)
	}
	if rec := f.patchUser(t, bob, bobCSRF, id, `{"displayName":"x","updatedAt":"2026-01-01T00:00:00Z"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("patch: %d, want 403", rec.Code)
	}
}

func TestTheDetailAndTheListSayWhoIsTheLastActiveInstanceAdmin(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)

	d := decodeDetail(t, f.userAction(t, cookie, "", http.MethodGet, "id-admin", "", ""))
	if !d.LastInstanceAdmin {
		t.Fatalf("detail = %+v, want the only admin marked", d)
	}
	var list []struct {
		ID                string `json:"id"`
		LastInstanceAdmin bool   `json:"lastInstanceAdmin"`
	}
	if err := json.Unmarshal(f.listUsers(t, cookie).Body.Bytes(), &list); err != nil || len(list) != 1 || !list[0].LastInstanceAdmin {
		t.Fatalf("list = %+v (%v), want the only admin marked", list, err)
	}

	f.addUser(t, "second", true)
	if d := decodeDetail(t, f.userAction(t, cookie, "", http.MethodGet, "id-admin", "", "")); d.LastInstanceAdmin {
		t.Fatalf("detail = %+v, want nobody marked with two admins", d)
	}
}
