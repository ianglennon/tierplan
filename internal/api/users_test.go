// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestCreateUserAnswers409ForATakenOrReservedUsernameAndNothingIsCreated(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.invite(t, cookie, csrf, "alice")

	taken := f.createUser(t, cookie, csrf, map[string]any{"username": "Alice"})
	if taken.Code != http.StatusConflict || problemType(t, taken) != "username_taken" {
		t.Fatalf("a live username: %d %s, want 409 username_taken", taken.Code, taken.Body)
	}

	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = ? WHERE username = 'alice'`, f.clk.now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	reserved := f.createUser(t, cookie, csrf, map[string]any{"username": "alice"})
	if reserved.Code != http.StatusConflict || problemType(t, reserved) != "username_reserved" {
		t.Fatalf("a deleted user's username: %d %s, want 409 username_reserved", reserved.Code, reserved.Body)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'alice'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users named alice = %d, %v, want the original only", n, err)
	}
	if got := f.audits(t, "user.created"); len(got) != 1 {
		t.Fatalf("user.created rows = %d, want one: a refused creation is not a creation", len(got))
	}
}

func TestCreateUserAnswers422ForEveryBadFieldAtOnce(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	cases := []struct {
		name string
		body map[string]any
		want []string // "location code"
	}{
		{"username", map[string]any{"username": "a!"}, []string{"body.username invalid_username"}},
		{"email", map[string]any{"username": "alice", "email": "not-an-email"}, []string{"body.email invalid_email"}},
		{"display name", map[string]any{"username": "alice", "displayName": strings.Repeat("x", 65)}, []string{"body.displayName display_name_too_long"}},
		{"all", map[string]any{"username": "!", "email": "x", "displayName": strings.Repeat("x", 65)},
			[]string{"body.username invalid_username", "body.displayName display_name_too_long", "body.email invalid_email"}},
	}
	for _, c := range cases {
		rec := f.createUser(t, cookie, csrf, c.body)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "validation_failed" {
			t.Fatalf("%s: %d %s, want 422 validation_failed", c.name, rec.Code, rec.Body)
		}
		var body struct {
			Errors []struct{ Location, Message string } `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range body.Errors {
			got = append(got, e.Location+" "+e.Message)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s: errors %v, want %v", c.name, got, c.want)
		}
		if strings.Contains(rec.Body.String(), "not-an-email") {
			t.Fatalf("%s: the submitted value is echoed: %s", c.name, rec.Body)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users = %d, %v, want only the admin", n, err)
	}
}

func TestAnInvitedUserCannotSignInAndTheAnswerIsByteIdenticalToAWrongPassword(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.invite(t, cookie, csrf, "alice")

	invited := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", goodPassword, false)})
	wrong := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", "wrong wrong wrong", false)})
	unknown := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("nobody", goodPassword, false)})
	for name, rec := range map[string]*httptest.ResponseRecorder{"invited": invited, "wrong": wrong, "unknown": unknown} {
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d, want 401", name, rec.Code)
		}
	}
	if invited.Body.String() != wrong.Body.String() || invited.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ:\ninvited: %s\nwrong: %s\nunknown: %s", invited.Body, wrong.Body, unknown.Body)
	}
	if got := f.audits(t, "auth.login_failed"); len(got) != 3 || got[0].Reason != "invite_pending" {
		t.Fatalf("auth.login_failed rows = %+v, want the first to be invite_pending", got)
	}
	if invited.Header().Get("Set-Cookie") != "" {
		t.Fatal("a refused sign-in set a cookie")
	}
}

func TestSigningInSetsTheLastSignInTimeShownForTheUser(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	token := f.invite(t, cookie, csrf, "alice")
	if rec := f.acceptInvite(t, token, "a long chosen passphrase"); rec.Code != http.StatusNoContent {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body)
	}
	if u, err := f.st.RQ.GetUserByUsername(context.Background(), "alice"); err != nil || u.LastLoginAt.Valid {
		t.Fatalf("before any sign-in: %+v, %v", u.LastLoginAt, err)
	}
	f.clk.t = f.clk.t.Add(time.Hour)
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", "a long chosen passphrase", false)}); rec.Code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", rec.Code, rec.Body)
	}
	u, err := f.st.RQ.GetUserByUsername(context.Background(), "alice")
	if err != nil || !u.LastLoginAt.Valid || u.LastLoginAt.Int64 != f.clk.now().UnixNano() {
		t.Fatalf("last_login_at = %+v, %v, want the sign-in time", u.LastLoginAt, err)
	}
}

// Listing users (plan 03-12, AUTH-04, D-25).

func (f *authFixture) listUsers(t *testing.T, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodGet, path: "/api/users", cookie: cookie})
}

func TestListUsersShowsEveryLiveUserSortedWithStatusAndTeamCount(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.invite(t, cookie, csrf, "carol")
	f.addUser(t, "bob", false)
	f.addTeam(t, "ops", "bob")
	f.addTeam(t, "dev", "bob")
	f.addUser(t, "dave", false)
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = ? WHERE username = 'dave'`, f.clk.now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	rec := f.listUsers(t, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var got []struct {
		userSummary
		TeamCount int `json:"teamCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]int{}
	for i, u := range got {
		names = append(names, u.Username)
		byName[u.Username] = i
	}
	if strings.Join(names, ",") != "admin,bob,carol" {
		t.Fatalf("users = %v, want admin, bob, carol sorted and the deleted dave left out", names)
	}
	if u := got[byName["carol"]]; u.Status != "invite_pending" || u.TeamCount != 0 || u.LastLoginAt != "" {
		t.Fatalf("carol = %+v, want invite_pending with no teams and no sign-in", u)
	}
	if u := got[byName["bob"]]; u.Status != "active" || u.TeamCount != 2 {
		t.Fatalf("bob = %+v, want active in two teams", u)
	}
	if u := got[byName["admin"]]; !u.InstanceAdmin || u.Status != "active" || u.LastLoginAt == "" {
		t.Fatalf("admin = %+v, want an active instance admin with a last sign-in", u)
	}
}

func TestListUsersStatusPrecedenceIsLockedDisabledInviteExpiredInvitePendingActive(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.invite(t, cookie, csrf, "pending")
	f.invite(t, cookie, csrf, "expired")
	f.invite(t, cookie, csrf, "lockedinvite")
	f.addUser(t, "off", false)
	f.addUser(t, "lockedoff", false)
	future := f.clk.now().Add(time.Hour).UnixNano()
	for _, q := range []string{
		`UPDATE user_links SET expires_at = 1 WHERE user_id = (SELECT id FROM users WHERE username = 'expired')`,
		`UPDATE users SET disabled = 1 WHERE username IN ('off', 'lockedoff')`,
	} {
		if _, err := f.st.Write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET locked_until = ? WHERE username IN ('lockedoff', 'lockedinvite')`, future); err != nil {
		t.Fatal(err)
	}

	var got []userSummary
	if err := json.Unmarshal(f.listUsers(t, cookie).Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, u := range got {
		status[u.Username] = u.Status
	}
	want := map[string]string{
		"admin": "active", "pending": "invite_pending", "expired": "invite_expired",
		"lockedinvite": "locked", "off": "disabled", "lockedoff": "locked",
	}
	for name, w := range want {
		if status[name] != w {
			t.Errorf("%s = %q, want %q (all: %v)", name, status[name], w, status)
		}
	}

	// A lock that has ended no longer counts.
	if _, err := f.st.Write.Exec(`UPDATE users SET locked_until = 1 WHERE username = 'lockedoff'`); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := json.Unmarshal(f.listUsers(t, cookie).Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, u := range got {
		if u.Username == "lockedoff" && u.Status != "disabled" {
			t.Errorf("lockedoff after its lock ended = %q, want disabled", u.Status)
		}
	}
}

func TestListUsersNeedsUsersManage(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "bob", false)
	bobCookie, _ := f.signIn(t, "bob", false)

	if rec := f.listUsers(t, bobCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("a user who is not an instance admin: %d %s, want 403", rec.Code, rec.Body)
	} else if strings.Contains(rec.Body.String(), "bob") && strings.Contains(rec.Body.String(), "admin") {
		t.Fatalf("a refusal must not list users: %s", rec.Body)
	}
	if rec := f.listUsers(t, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s, want 401", rec.Code, rec.Body)
	}
}

// Regenerating an invite (plan 03-12, D-22a).

func (f *authFixture) regenerate(t *testing.T, cookie, csrf, userID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPost, path: "/api/users/" + userID + "/invite", cookie: cookie, csrf: csrf})
}

func (f *authFixture) lookup(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPost, path: "/api/invite/lookup", body: `{"token":"` + token + `"}`})
}

func TestRegenerateInviteAnswersANewLinkAndTheOldTokenIsInvalidLink(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	old := f.invite(t, cookie, csrf, "alice")
	var id string
	if err := f.st.Read.QueryRow(`SELECT id FROM users WHERE username = 'alice'`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	rec := f.regenerate(t, cookie, csrf, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var got struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := tokenInLink.FindStringSubmatch(got.URL)
	if m == nil || m[1] == old {
		t.Fatalf("link %q, want a new /invite#token=<43 characters>", got.URL)
	}
	if got.ExpiresAt != "2026-01-04T00:00:00Z" {
		t.Fatalf("expiresAt = %q, want 72 hours after the fixture clock", got.ExpiresAt)
	}

	if rec := f.lookup(t, old); rec.Code != http.StatusNotFound || problemType(t, rec) != "invalid_link" {
		t.Fatalf("old link: %d %s, want 404 invalid_link", rec.Code, rec.Body)
	}
	if rec := f.lookup(t, m[1]); rec.Code != http.StatusOK {
		t.Fatalf("new link: %d %s, want 200", rec.Code, rec.Body)
	}

	entries := f.audits(t, "user.invite_regenerated")
	if len(entries) != 1 || entries[0].TargetID != id || entries[0].ActorID != "id-admin" || entries[0].Result != "success" {
		t.Fatalf("audit = %+v, want one success by the admin on the user", entries)
	}
	if strings.Contains(rec.Body.String(), old) {
		t.Fatalf("the answer repeats the old token")
	}
}

func TestRegenerateInviteAnswers409NotInvitedForAUserWhoSetAPassword(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "bob", false)

	rec := f.regenerate(t, cookie, csrf, id)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "not_invited" {
		t.Fatalf("a user with a password: %d %s, want 409 not_invited", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "user.invite_regenerated")); n != 0 {
		t.Fatalf("audit rows = %d, want a refusal to record none", n)
	}
}

func TestRegenerateInviteAnswers404UserNotFoundForAnUnknownOrDeletedID(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	if rec := f.regenerate(t, cookie, csrf, "no-such-user"); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("unknown id: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
	f.invite(t, cookie, csrf, "gone")
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = ? WHERE username = 'gone'`, f.clk.now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.st.Read.QueryRow(`SELECT id FROM users WHERE username = 'gone'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if rec := f.regenerate(t, cookie, csrf, id); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("deleted user: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
}

func TestRegenerateInviteNeedsUsersManage(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	old := f.invite(t, cookie, csrf, "alice")
	var id string
	if err := f.st.Read.QueryRow(`SELECT id FROM users WHERE username = 'alice'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	f.addUser(t, "bob", false)
	bobCookie, bobCSRF := f.signIn(t, "bob", false)

	if rec := f.regenerate(t, bobCookie, bobCSRF, id); rec.Code != http.StatusForbidden {
		t.Fatalf("a user who is not an instance admin: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.regenerate(t, "", "", id); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s, want 401", rec.Code, rec.Body)
	}
	if rec := f.lookup(t, old); rec.Code != http.StatusOK {
		t.Fatalf("a refused request must leave the link alone: %d %s", rec.Code, rec.Body)
	}
}

// --- POST /api/users/{id}/reset-password (plan 03-15, AUTH-04, D-22b) -----------

func (f *authFixture) resetPassword(t *testing.T, cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPost, path: "/api/users/" + id + "/reset-password", cookie: cookie, csrf: csrf})
}

func TestResetPasswordEndsEverySessionIssuesAResetLinkAndTheLinkChoosesANewPassword(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)
	a1, _ := f.signIn(t, "alice", false)
	a2, _ := f.signIn(t, "alice", true)

	rec := f.resetPassword(t, cookie, csrf, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var link struct{ URL, ExpiresAt string }
	if err := json.Unmarshal(rec.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	m := tokenInLink.FindStringSubmatch(link.URL)
	if m == nil || link.ExpiresAt == "" {
		t.Fatalf("link = %+v, want an /invite#token= address and an expiry", link)
	}
	for _, c := range []string{a1, a2} {
		if code := f.meStatus(t, c); code != http.StatusUnauthorized {
			t.Fatalf("a session of the user after the reset: %d, want 401", code)
		}
	}
	if f.meStatus(t, cookie) != http.StatusOK {
		t.Fatal("the reset ended the admin's own session")
	}

	look := f.lookupInvite(t, m[1])
	var got struct{ Kind, Username string }
	if look.Code != http.StatusOK || json.Unmarshal(look.Body.Bytes(), &got) != nil || got.Kind != "reset" || got.Username != "alice" {
		t.Fatalf("lookup = %d %s, want a reset link for alice", look.Code, look.Body)
	}
	const next = "a brand new passphrase 99"
	if rec := f.acceptInvite(t, m[1], next); rec.Code != http.StatusNoContent {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", goodPassword, false)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in with the old password: %d, want 401", rec.Code)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("alice", next, false)}); rec.Code != http.StatusOK {
		t.Fatalf("sign-in with the new password: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "user.password_reset_completed")); n != 1 {
		t.Fatalf("user.password_reset_completed entries = %d, want 1", n)
	}
	entries := f.audits(t, "user.password_reset_issued")
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != id || entries[0].Result != "success" || entries[0].IP != authIP || entries[0].UserAgent != authUA {
		t.Fatalf("audit = %+v", entries)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'user.password_reset_issued'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"ended":"2"`) || !strings.Contains(details, `"username":"alice"`) || strings.Contains(details, m[1]) {
		t.Fatalf("details = %s", details)
	}
}

func TestResetPasswordBuildsTheLinkOnThePublicURLAndBasePath(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://bayplan.example.test/base"})
	cookie, csrf := f.signIn(t, "admin", false)
	id := f.addUser(t, "alice", false)
	rec := f.resetPassword(t, cookie, csrf, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body)
	}
	var link struct{ URL string }
	if err := json.Unmarshal(rec.Body.Bytes(), &link); err != nil || !strings.HasPrefix(link.URL, "https://bayplan.example.test/base/invite#token=") {
		t.Fatalf("link = %q (%v), want it on the public URL and base path", link.URL, err)
	}
}

func TestResetPasswordRefusesNonAdminsUnknownDeletedAndDisabledUsersWithoutEndingAnything(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	alice := f.addUser(t, "alice", false)
	f.addUser(t, "mallory", false)
	malloryCookie, malloryCSRF := f.signIn(t, "mallory", false)
	a1, _ := f.signIn(t, "alice", false)

	if rec := f.resetPassword(t, malloryCookie, malloryCSRF, alice); rec.Code != http.StatusForbidden {
		t.Fatalf("a user resetting another's password: %d, want 403", rec.Code)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/users/" + alice + "/reset-password"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous reset: %d, want 401", rec.Code)
	}
	if rec := f.resetPassword(t, cookie, "", alice); rec.Code != http.StatusForbidden {
		t.Fatalf("reset without a CSRF token: %d, want 403", rec.Code)
	}
	if f.meStatus(t, a1) != http.StatusOK {
		t.Fatal("a refused reset ended a session")
	}
	if rec := f.resetPassword(t, cookie, csrf, "no-such-user"); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("unknown user: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}

	if rec := f.disable(t, cookie, csrf, alice); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d", rec.Code)
	}
	if rec := f.resetPassword(t, cookie, csrf, alice); rec.Code != http.StatusConflict || problemType(t, rec) != "user_disabled" {
		t.Fatalf("disabled user: %d %s, want 409 user_disabled", rec.Code, rec.Body)
	}
	if rec := f.userAction(t, cookie, csrf, http.MethodDelete, alice, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := f.resetPassword(t, cookie, csrf, alice); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("deleted user: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "user.password_reset_issued")); n != 0 {
		t.Fatalf("user.password_reset_issued entries = %d, want none for refusals", n)
	}
}
