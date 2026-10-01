// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Password change and instance warnings (plan 14, AUTH-02, AUTH-08, PLAT-01).

const newGoodPassword = "another long passphrase 42"

func changeBody(current, next string) string {
	b, _ := json.Marshal(map[string]string{"currentPassword": current, "newPassword": next})
	return string(b)
}

type problemBody struct {
	Type   string `json:"type"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %v: %s", err, rec.Body)
	}
	return p
}

func (f *authFixture) changePassword(t *testing.T, cookie, csrf, current, next string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPost, path: "/api/me/password", body: changeBody(current, next), cookie: cookie, csrf: csrf})
}

func (f *authFixture) meStatus(t *testing.T, cookie string) int {
	t.Helper()
	return f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie}).Code
}

func TestChangePasswordRotatesTheSessionEndsTheOthersAndAudits(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookieA, csrfA := f.signIn(t, "admin", false)
	cookieB, _ := f.signIn(t, "admin", false)

	rec := f.changePassword(t, cookieA, csrfA, goodPassword, newGoodPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	c := sessionCookie(rec)
	if c == nil || c.Value == "" || c.Value == cookieA || c.Value == cookieB {
		t.Fatalf("want a brand new session cookie, got %+v", c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie flags = %+v", c)
	}
	if want := f.sess.CSRFToken(auth.HashSessionID(c.Value)); body.CSRFToken != want {
		t.Fatalf("csrfToken = %q, want the new session's token %q", body.CSRFToken, want)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}

	if got := f.meStatus(t, cookieA); got != http.StatusUnauthorized {
		t.Fatalf("the old session still works: %d", got)
	}
	if got := f.meStatus(t, cookieB); got != http.StatusUnauthorized {
		t.Fatalf("the user's other session still works: %d", got)
	}
	if got := f.meStatus(t, c.Value); got != http.StatusOK {
		t.Fatalf("the new session does not work: %d", got)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("%d sessions remain, want exactly the new one", n)
	}

	// The new password signs in and the old one no longer does.
	ok := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", newGoodPassword, false)})
	if ok.Code != http.StatusOK {
		t.Fatalf("sign-in with the new password: %d %s", ok.Code, ok.Body)
	}
	old := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)})
	if old.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in with the old password: %d", old.Code)
	}

	entries := f.audits(t, "auth.password_changed")
	if len(entries) != 1 || entries[0].Result != "success" || entries[0].ActorID != "id-admin" || entries[0].TargetID != "id-admin" || entries[0].IP != authIP || entries[0].UserAgent != authUA {
		t.Fatalf("audit = %+v, want one success entry by and about id-admin", entries)
	}
}

func TestChangePasswordKeepsTheRememberChoiceOfTheSession(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", true)
	rec := f.changePassword(t, cookie, csrf, goodPassword, newGoodPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.MaxAge != 2592000 {
		t.Fatalf("cookie = %+v, want Max-Age 2592000", c)
	}
	row, err := f.st.RQ.GetSessionByHash(context.Background(), auth.HashSessionID(c.Value))
	if err != nil || row.Remember != 1 || !row.AbsoluteExpiresAt.Valid {
		t.Fatalf("session row = %+v, %v; want remembered with an absolute expiry", row, err)
	}
}

func TestChangePasswordWithAWrongCurrentPasswordChangesNothing(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	other, _ := f.signIn(t, "admin", false)

	rec := f.changePassword(t, cookie, csrf, "not the password", newGoodPassword)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	p := decodeProblem(t, rec)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.currentPassword" || p.Errors[0].Message != "current_password_incorrect" {
		t.Fatalf("errors = %+v, want current_password_incorrect at body.currentPassword", p.Errors)
	}
	if f.meStatus(t, cookie) != http.StatusOK || f.meStatus(t, other) != http.StatusOK {
		t.Fatal("a refused change ended a session")
	}
	ok := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)})
	if ok.Code != http.StatusOK {
		t.Fatalf("the old password stopped working: %d", ok.Code)
	}
	entries := f.audits(t, "auth.password_changed")
	if len(entries) != 1 || entries[0].Result != "failure" || entries[0].Reason != "current_password_incorrect" {
		t.Fatalf("audit = %+v, want one failure entry so guessing is visible", entries)
	}
}

func TestChangePasswordRejectsAShortOrCommonNewPassword(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	cases := map[string]struct{ next, code string }{
		"short":             {"short pw", "password_too_short"},
		"common":            {"password1234", "password_common"},
		"contains the name": {"my admin passphrase", "password_common"},
	}
	for name, tc := range cases {
		rec := f.changePassword(t, cookie, csrf, goodPassword, tc.next)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d: %s", name, rec.Code, rec.Body)
		}
		p := decodeProblem(t, rec)
		if len(p.Errors) != 1 || p.Errors[0].Location != "body.newPassword" || p.Errors[0].Message != tc.code {
			t.Fatalf("%s: errors = %+v, want %s at body.newPassword", name, p.Errors, tc.code)
		}
	}
	if f.meStatus(t, cookie) != http.StatusOK {
		t.Fatal("a refused change ended the session")
	}
	if got := f.audits(t, "auth.password_changed"); len(got) != 0 {
		t.Fatalf("audit = %+v, want nothing for a policy rejection", got)
	}
}

func TestChangePasswordNeedsASessionAndACSRFToken(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	if rec := f.changePassword(t, cookie, "", goodPassword, newGoodPassword); rec.Code != http.StatusForbidden {
		t.Fatalf("without X-CSRF-Token: %d, want 403", rec.Code)
	}
	if rec := f.changePassword(t, cookie, csrf+"x", goodPassword, newGoodPassword); rec.Code != http.StatusForbidden {
		t.Fatalf("with a wrong X-CSRF-Token: %d, want 403", rec.Code)
	}
	if rec := f.changePassword(t, "", "", goodPassword, newGoodPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: %d, want 401", rec.Code)
	}
	if ok := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)}); ok.Code != http.StatusOK {
		t.Fatalf("a refused request changed the password: %d", ok.Code)
	}
}

func TestChangePasswordCapsTheLengthOfBothFields(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	long := make([]byte, 1025)
	for i := range long {
		long[i] = 'a'
	}
	if rec := f.changePassword(t, cookie, csrf, string(long), newGoodPassword); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 1025 byte current password: %d, want 422", rec.Code)
	}
	if rec := f.changePassword(t, cookie, csrf, goodPassword, string(long)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 1025 byte new password: %d, want 422", rec.Code)
	}
}

// --- GET /api/warnings and the key-backup acknowledgement ------------------

type warningsResp struct {
	Items []struct {
		ID      string `json:"id"`
		Reason  string `json:"reason"`
		KeyPath string `json:"keyPath"`
	} `json:"items"`
}

func (f *authFixture) warnings(t *testing.T, cookie string) (int, warningsResp) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/warnings", cookie: cookie})
	var w warningsResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil {
			t.Fatal(err)
		}
		if w.Items == nil {
			t.Fatalf("items must be [] not null: %s", rec.Body)
		}
	}
	return rec.Code, w
}

func TestWarningsAreEmptyForAPersistedDirectoryAndAnEnvironmentKey(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	code, w := f.warnings(t, cookie)
	if code != http.StatusOK || len(w.Items) != 0 {
		t.Fatalf("got %d %+v, want 200 and no items", code, w)
	}
}

func TestWarningsReportADataDirectoryThatIsNotPersisted(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.dataInfo = datadir.Info{Path: "/data", Persisted: false, Reason: "not a mount point"}
	f.h = f.build(t)
	cookie, _ := f.signIn(t, "admin", false)
	code, w := f.warnings(t, cookie)
	if code != http.StatusOK || len(w.Items) != 1 || w.Items[0].ID != "data_not_persisted" || w.Items[0].Reason != "not a mount point" {
		t.Fatalf("got %d %+v, want data_not_persisted with its reason", code, w)
	}
}

func fileKeyVault(t *testing.T) (*vault.Vault, string) {
	t.Helper()
	dir := t.TempDir()
	v, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if v.KeySource() != "file" {
		t.Fatalf("key source = %q, want file", v.KeySource())
	}
	return v, filepath.Join(dir, vault.KeyFile)
}

func TestWarningsRemindToBackUpAGeneratedKeyUntilItIsAcknowledged(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	v, keyPath := fileKeyVault(t)
	f.acctVault = v
	f.dataInfo = datadir.Info{Path: filepath.Dir(keyPath), Persisted: true}
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)

	code, w := f.warnings(t, cookie)
	if code != http.StatusOK || len(w.Items) != 1 || w.Items[0].ID != "key_backup" || w.Items[0].KeyPath != keyPath {
		t.Fatalf("got %d %+v, want key_backup naming %s", code, w, keyPath)
	}

	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/warnings/key-backup/ack", cookie: cookie}); rec.Code != http.StatusForbidden {
		t.Fatalf("ack without X-CSRF-Token: %d, want 403", rec.Code)
	}
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/warnings/key-backup/ack", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body)
	}

	if code, w := f.warnings(t, cookie); code != http.StatusOK || len(w.Items) != 0 {
		t.Fatalf("after the ack: %d %+v, want no items", code, w)
	}
	var stored string
	if err := f.st.Read.QueryRow(`SELECT value FROM settings WHERE key = 'key_backup_ack'`).Scan(&stored); err != nil || stored == "" {
		t.Fatalf("settings key_backup_ack = %q, %v", stored, err)
	}
	entries := f.audits(t, "warning.acknowledged")
	if len(entries) != 1 || entries[0].TargetID != "key_backup" || entries[0].ActorID != "id-admin" || entries[0].Result != "success" {
		t.Fatalf("audit = %+v, want one entry by id-admin for key_backup", entries)
	}

	// It stays acknowledged across a restart.
	f.h = f.build(t)
	if code, w := f.warnings(t, cookie); code != http.StatusOK || len(w.Items) != 0 {
		t.Fatalf("after a restart: %d %+v, want no items", code, w)
	}
}

func TestWarningsAndTheirAcknowledgementNeedSettingsPermissions(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	v, keyPath := fileKeyVault(t)
	f.acctVault = v
	f.dataInfo = datadir.Info{Path: filepath.Dir(keyPath), Persisted: false, Reason: "tmpfs"}
	f.addUser(t, "viewer", false) // no role at all, so no settings.read
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "viewer", false)

	if code, _ := f.warnings(t, cookie); code != http.StatusForbidden {
		t.Fatalf("GET /api/warnings without settings.read: %d, want 403", code)
	}
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/warnings/key-backup/ack", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ack without settings.write: %d, want 403", rec.Code)
	}
	if got := f.audits(t, "warning.acknowledged"); len(got) != 0 {
		t.Fatalf("a refused ack was recorded as an acknowledgement: %+v", got)
	}
	if code, _ := f.warnings(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/warnings anonymously: %d, want 401", code)
	}
}
