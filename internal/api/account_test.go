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
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/httpx"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestChangePasswordIsThrottledPerUserEvenForTheRightPassword(t *testing.T) {
	t.Parallel()
	// A stolen session must not be able to guess the current password at the
	// speed of the hash alone (01-14 hand-off, T-01-60). The budget is per user,
	// so it is spent by right and wrong attempts alike, and it refills with time.
	f := newAuthFixture(t, config.Config{})
	f.pwLimiter = auth.NewIPLimiter(time.Minute, 3, 10*time.Minute, f.clk.now)
	f.h = f.build(t)
	f.addUser(t, "bob", false)
	cookie, csrf := f.signIn(t, "admin", false)

	for i := range 3 {
		rec := f.changePassword(t, cookie, csrf, "not the password", newGoodPassword)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d = %d, want 422 while the budget lasts: %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := f.changePassword(t, cookie, csrf, goodPassword, newGoodPassword)
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
		t.Fatalf("4th attempt = %d %s, want 429 rate_limited even with the right password", rec.Code, rec.Body)
	}
	if f.meStatus(t, cookie) != http.StatusOK {
		t.Fatal("a throttled attempt ended the session")
	}
	if ok := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)}); ok.Code != http.StatusOK {
		t.Fatalf("the password changed although the attempt was refused: %d", ok.Code)
	}

	entries := f.audits(t, "auth.password_changed")
	if len(entries) != 4 {
		t.Fatalf("audit has %d entries, want the 3 failures and the refusal", len(entries))
	}
	if last := entries[len(entries)-1]; last.Result != "denied" || last.Reason != "rate_limited" {
		t.Errorf("last audit entry = %+v, want denied / rate_limited", last)
	}

	// Another user's budget is separate.
	bobCookie, bobCSRF := f.signIn(t, "bob", false)
	if rec := f.changePassword(t, bobCookie, bobCSRF, "not the password", newGoodPassword); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("another user's attempt = %d, want 422: %s", rec.Code, rec.Body)
	}

	// A minute later one attempt is back, and the right password goes through.
	f.clk.t = f.clk.t.Add(time.Minute)
	if rec := f.changePassword(t, cookie, csrf, goodPassword, newGoodPassword); rec.Code != http.StatusOK {
		t.Fatalf("after the refill = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestChangePasswordRejectsAShortOrCommonNewPassword(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		IP      string `json:"ip"`
		Host    string `json:"host"`
		Scheme  string `json:"scheme"`
		URL     string `json:"url"`
		Subject string `json:"subject"`
		Count   int    `json:"count"`
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
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	code, w := f.warnings(t, cookie)
	if code != http.StatusOK || len(w.Items) != 0 {
		t.Fatalf("got %d %+v, want 200 and no items", code, w)
	}
}

func TestWarningsReportADataDirectoryThatIsNotPersisted(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// --- proxy items in GET /api/warnings (plan 02-25, D-53) -------------------

const proxyPublicURL = "https://bayplan.example.com"

// withProxyObserver gives the fixture a proxy observer on the fixture's clock and a
// public URL, and rebuilds the router.
func (f *authFixture) withProxyObserver(t *testing.T) {
	t.Helper()
	f.proxy = httpx.NewProxyObserver(f.inst.PublicURL, f.clk.now)
	f.inst.Apply("Bayplan", proxyPublicURL)
	f.h = f.build(t)
}

func TestWarningsListEachProxyProblemWithItsParametersWhileItIsFresh(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.withProxyObserver(t)
	cookie, _ := f.signIn(t, "admin", false)

	// A request through an untrusted peer with forwarded headers, one for another host over
	// plain http, and one failed upgrade.
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := httpx.ClientIPMiddleware(nil)(f.proxy.Observe(ok))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.7:4000"
	req.Host = "other.example.com"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	f.proxy.RecordUpgradeFailure("203.0.113.9")

	code, w := f.warnings(t, cookie)
	if code != http.StatusOK || len(w.Items) != 3 {
		t.Fatalf("got %d %+v, want the three proxy warnings", code, w)
	}
	byID := map[string]int{}
	for i, it := range w.Items {
		byID[it.ID] = i
	}
	if i, ok := byID["proxy_untrusted_forwarded"]; !ok || w.Items[i].IP != "198.51.100.7" {
		t.Errorf("proxy_untrusted_forwarded = %+v, want ip 198.51.100.7", w.Items)
	}
	if i, ok := byID["proxy_url_mismatch"]; !ok || w.Items[i].Host != "other.example.com" || w.Items[i].Scheme != "http" || w.Items[i].URL != proxyPublicURL {
		t.Errorf("proxy_url_mismatch = %+v, want host other.example.com, scheme http and the public URL", w.Items)
	}
	if _, ok := byID["proxy_websocket_failing"]; !ok {
		t.Errorf("no proxy_websocket_failing in %+v", w.Items)
	}

	f.clk.t = f.clk.t.Add(59 * time.Minute)
	if _, w := f.warnings(t, cookie); len(w.Items) != 3 {
		t.Errorf("59 minutes later: %+v, want the three still", w.Items)
	}
	f.clk.t = f.clk.t.Add(2 * time.Minute)
	if _, w := f.warnings(t, cookie); len(w.Items) != 0 {
		t.Errorf("an hour after the last sighting: %+v, want none", w.Items)
	}
}

func TestWarningsShowTheNewestSignalOfAKindOnly(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.withProxyObserver(t)
	cookie, _ := f.signIn(t, "admin", false)
	f.proxy.RecordUpgradeFailure("203.0.113.1")
	f.clk.t = f.clk.t.Add(time.Second)
	f.proxy.RecordUpgradeFailure("203.0.113.2")
	_, w := f.warnings(t, cookie)
	if len(w.Items) != 1 || w.Items[0].ID != "proxy_websocket_failing" || w.Items[0].IP != "203.0.113.2" {
		t.Fatalf("got %+v, want one proxy_websocket_failing naming the newest address", w.Items)
	}
}

func TestProxyWarningsNeedSettingsRead(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.withProxyObserver(t)
	f.proxy.RecordUpgradeFailure("203.0.113.1")
	f.addUser(t, "viewer", false)
	cookie, _ := f.signIn(t, "viewer", false)
	if code, _ := f.warnings(t, cookie); code != http.StatusForbidden {
		t.Fatalf("GET /api/warnings without settings.read: %d, want 403", code)
	}
}

// --- refusal bursts in GET /api/warnings (plan 03-10, D-20) -----------------

// burstedCoalescer is a coalescer in which user "mallory" has just been refused
// n times inside one window.
func burstedCoalescer(t *testing.T, f *authFixture, n int) *auth.RefusalCoalescer {
	t.Helper()
	c := auth.NewRefusalCoalescer(f.aud, slog.New(slog.DiscardHandler), f.clk.now)
	e := audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: "id-mallory", Name: "mallory"},
		Action: audit.ActionPermissionDenied, Result: audit.ResultDenied, Reason: "forbidden",
		TargetType: "route", TargetID: "GET /api/audit",
	}
	for range n {
		if err := c.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	f.clk.t = f.clk.t.Add(2 * time.Minute)
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAnInstanceAdminSeesARefusalBurstForOneHour(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.refusals = burstedCoalescer(t, f, 12)
	f.h = f.build(t)
	cookie, _ := f.signIn(t, "admin", false)

	_, w := f.warnings(t, cookie)
	if len(w.Items) != 1 || w.Items[0].ID != "refusal_burst" || w.Items[0].Subject != "mallory" || w.Items[0].Count != 12 {
		t.Fatalf("got %+v, want one refusal_burst naming mallory and 12", w.Items)
	}
	// The session idles out long before an hour, so the admin signs in again.
	f.clk.t = f.clk.t.Add(57 * time.Minute) // 59 minutes after the burst
	cookie, _ = f.signIn(t, "admin", false)
	if _, w := f.warnings(t, cookie); len(w.Items) != 1 {
		t.Errorf("59 minutes later: %+v, want the burst still", w.Items)
	}
	f.clk.t = f.clk.t.Add(2 * time.Minute)
	cookie, _ = f.signIn(t, "admin", false)
	if _, w := f.warnings(t, cookie); len(w.Items) != 0 {
		t.Errorf("an hour later: %+v, want none", w.Items)
	}
}

func TestAWarningListedOnceForAUserWhoBurstsTwice(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	c := burstedCoalescer(t, f, 12)
	e := audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: "id-mallory", Name: "mallory"},
		Action: audit.ActionPermissionDenied, Result: audit.ResultDenied, Reason: "forbidden",
		TargetType: "route", TargetID: "GET /api/other",
	}
	for range 20 {
		if err := c.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	f.clk.t = f.clk.t.Add(2 * time.Minute)
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.refusals = c
	f.h = f.build(t)
	cookie, _ := f.signIn(t, "admin", false)
	if _, w := f.warnings(t, cookie); len(w.Items) != 1 || w.Items[0].Count != 20 {
		t.Fatalf("got %+v, want one item with the newest burst's 20", w.Items)
	}
}

func TestARefusalBurstIsNotShownWithoutSettingsRead(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.refusals = burstedCoalescer(t, f, 12)
	f.h = f.build(t)
	f.addUser(t, "viewer", false)
	cookie, _ := f.signIn(t, "viewer", false)
	if code, w := f.warnings(t, cookie); code != http.StatusForbidden || len(w.Items) != 0 {
		t.Fatalf("GET /api/warnings as a non-admin: %d %+v, want 403", code, w.Items)
	}
}

// Agents Bayplan does not know after a restore (D-31): one warning, with how many,
// for an instance admin while the registry holds any.
func TestUnknownAgentsRaisesAWarningWithTheCountForAnInstanceAdminWhileTheRegistryHoldsAny(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	f.addAgentEnv(t, "env-a", "Production")
	if _, w := f.warnings(t, cookie); len(w.Items) != 0 {
		t.Fatalf("with an empty registry: %+v", w.Items)
	}

	now := f.clk.now()
	f.unknown.Record(agents.UnknownAgent{EnvID: "env-a", Node: "agent-1", Key: "k1", LastAttempt: now})
	f.unknown.Record(agents.UnknownAgent{EnvID: "env-a", Node: "agent-2", Key: "k2", LastAttempt: now.Add(-time.Minute)})
	_, w := f.warnings(t, cookie)
	if len(w.Items) != 1 || w.Items[0].ID != "unknown_agents" || w.Items[0].Count != 2 {
		t.Fatalf("got %+v, want one unknown_agents with count 2", w.Items)
	}

	f.unknown.Forget("env-a", "k1")
	f.unknown.Forget("env-a", "k2")
	if _, w := f.warnings(t, cookie); len(w.Items) != 0 {
		t.Fatalf("after the agents enrolled: %+v, want none", w.Items)
	}
}

func TestTheUnknownAgentsWarningEmptiesWhenTheAgentsStopTryingAndIsNeverShownToAnyoneButAnInstanceAdmin(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addAgentEnv(t, "env-a", "Production")
	f.unknown.Record(agents.UnknownAgent{EnvID: "env-a", Node: "agent-1", Key: "k1", LastAttempt: f.clk.now()})

	f.addUser(t, "viewer", false)
	viewer, _ := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/warnings", cookie: viewer})
	if strings.Contains(rec.Body.String(), "unknown_agents") {
		t.Fatalf("a non-admin was told: %d %s", rec.Code, rec.Body)
	}

	f.clk.t = f.clk.t.Add(25 * time.Hour)
	cookie, _ := f.signIn(t, "admin", false)
	if _, w := f.warnings(t, cookie); len(w.Items) != 0 {
		t.Fatalf("a day after the last attempt: %+v, want none", w.Items)
	}
}
