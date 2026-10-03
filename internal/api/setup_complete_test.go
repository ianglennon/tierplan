// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
)

// POST /api/setup/complete (plan 10, AUTH-01).

const goodPassword = "correct horse battery"

// completeJSON builds a completion body with valid defaults, overridden by over.
func completeJSON(t *testing.T, token string, over map[string]any) string {
	t.Helper()
	body := map[string]any{
		"token":        token,
		"username":     "Admin",
		"password":     goodPassword,
		"instanceName": "Home lab",
		"publicUrl":    "https://bayplan.example",
	}
	for k, v := range over {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *setupFixture) completeFrom(t *testing.T, remoteAddr, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/setup/complete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "setup-test/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *setupFixture) complete(t *testing.T, over map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return f.completeFrom(t, testClientAddr, completeJSON(t, f.token, over), nil)
}

type completeResponse struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
	CSRFToken       string `json:"csrfToken"`
	InstanceName    string `json:"instanceName"`
	PublicURL       string `json:"publicUrl"`
	PublicURLLocked bool   `json:"publicUrlLocked"`
}

func decodeComplete(t *testing.T, rec *httptest.ResponseRecorder) completeResponse {
	t.Helper()
	var r completeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("not a completion body: %v: %s", err, rec.Body)
	}
	return r
}

func (f *setupFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *setupFixture) userCount(t *testing.T) int { return f.count(t, `SELECT COUNT(*) FROM users`) }

func (f *setupFixture) setting(t *testing.T, key string) (string, bool) {
	t.Helper()
	v, err := f.st.RQ.GetSetting(context.Background(), key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, true
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName(auth.CookieScope{Secure: true}) || c.Name == auth.CookieName(auth.CookieScope{Secure: false}) {
			return c
		}
	}
	return nil
}

func TestSetupCompleteCreatesTheAdminSignsInAndStoresTheSettings(t *testing.T) {
	f := newSetupFixture(t, config.Config{SessionRememberLifetime: 720 * time.Hour})
	rec := f.complete(t, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := decodeComplete(t, rec)
	if got.User.ID == "" || got.User.Username != "admin" || got.User.Role != "admin" || got.CSRFToken == "" {
		t.Fatalf("body = %+v", got)
	}

	// The user, with an argon2id hash and not the password.
	u, err := f.st.RQ.GetUserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("admin not stored under the lower-cased name: %v", err)
	}
	if u.ID != got.User.ID || !strings.HasPrefix(u.PasswordHash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(u.PasswordHash, goodPassword) {
		t.Fatalf("user row %+v", u)
	}
	// The global admin role.
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_type = 'user' AND principal_id = ? AND role_id = 'admin' AND env_id IS NULL`, u.ID); n != 1 {
		t.Fatalf("admin role assignments = %d, want 1", n)
	}
	// The settings.
	if v, ok := f.setting(t, "instance_name"); !ok || v != "Home lab" {
		t.Fatalf("instance_name = %q, %v", v, ok)
	}
	if v, ok := f.setting(t, "public_url"); !ok || v != "https://bayplan.example" {
		t.Fatalf("public_url = %q, %v", v, ok)
	}
	// The session: a hardened cookie, stored only as a hash, for this user, and the matching CSRF token.
	c := sessionCookie(rec)
	if c == nil {
		t.Fatalf("no session cookie in %v", rec.Header()["Set-Cookie"])
	}
	if c.Name != "__Host-bayplan_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 0 {
		t.Fatalf("cookie = %+v", c)
	}
	sum := sha256.Sum256([]byte(c.Value))
	row, err := f.st.RQ.GetSessionByHash(context.Background(), sum[:])
	if err != nil {
		t.Fatalf("no stored session for the cookie: %v", err)
	}
	if row.UserID != u.ID || row.Remember != 0 || row.AbsoluteExpiresAt.Valid || row.Ip != "203.0.113.9" || row.UserAgent != "setup-test/1.0" {
		t.Fatalf("session row %+v", row)
	}
	if got.CSRFToken != f.sessions.CSRFToken(sum[:]) {
		t.Fatal("the returned CSRF token is not the session's")
	}
	// The audit entry, in the chain, with the new user as actor.
	rows := f.auditRows(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	a := rows[0]
	if a.Action != audit.ActionSetupCompleted || a.ActorType != string(audit.ActorUser) || a.ActorID != u.ID || a.ActorName != "admin" ||
		a.Result != string(audit.ResultSuccess) || a.Ip != "203.0.113.9" {
		t.Fatalf("audit row %+v", a)
	}
	var details map[string]string
	if err := json.Unmarshal([]byte(a.Details), &details); err != nil || details["username"] != "admin" || details["instance_name"] != "Home lab" {
		t.Fatalf("audit details %q (%v)", a.Details, err)
	}
	// The token is spent: file gone, gate shut.
	if _, err := auth.ReadTokenFile(f.dataDir); err == nil {
		t.Fatal("the setup-token file survived completion")
	}
	if f.gate.NeedsSetup() || f.gate.Verify(f.token) {
		t.Fatal("the gate is still open after completion")
	}
}

func TestSetupCompleteNeverEchoesOrLogsThePasswordAndIsNotCacheable(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	rec := f.complete(t, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store (the body carries a CSRF token)", cc)
	}
	rows := f.auditRows(t)
	for name, hay := range map[string]string{"response": rec.Body.String(), "log": f.logs.String(), "audit details": rows[0].Details, "audit reason": rows[0].Reason} {
		if strings.Contains(hay, goodPassword) {
			t.Errorf("the password leaked into the %s", name)
		}
	}
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Value, goodPassword) {
			t.Error("the password leaked into a cookie")
		}
	}
}

func TestSetupCompleteMakesTheInstanceAndTheOriginGuardUseTheNewSettingsAtOnce(t *testing.T) {
	f := newSetupFixture(t, config.Config{SessionRememberLifetime: 24 * time.Hour})
	if rec := f.complete(t, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var inst struct {
		InstanceName string `json:"instanceName"`
		NeedsSetup   bool   `json:"needsSetup"`
	}
	if err := json.Unmarshal(f.get(t, "/api/instance").Body.Bytes(), &inst); err != nil {
		t.Fatal(err)
	}
	if inst.InstanceName != "Home lab" || inst.NeedsSetup {
		t.Fatalf("GET /api/instance = %+v, want {Home lab false}", inst)
	}
	// A mutation from the new public origin passes the Origin guard (and then meets the closed gate);
	// one from the old, bare-address origin is refused by the guard itself.
	allowed := f.completeFrom(t, testClientAddr, `{"token":"x"}`, map[string]string{"Origin": "https://bayplan.example"})
	if allowed.Code == http.StatusForbidden {
		t.Fatalf("Origin https://bayplan.example was refused after setup: %s", allowed.Body)
	}
	refused := f.completeFrom(t, testClientAddr, `{"token":"x"}`, map[string]string{"Origin": "http://127.0.0.1:7480"})
	if refused.Code != http.StatusForbidden {
		t.Fatalf("Origin http://127.0.0.1:7480 = %d, want 403 after the public URL was set", refused.Code)
	}
}

func TestSetupCompleteUsesAnUnprefixedCookieWhenThePublicURLIsHTTP(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	rec := f.complete(t, map[string]any{"publicUrl": "http://10.0.0.5:7480"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Name != "bayplan_session" || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %+v", c)
	}
}

func TestSetupCompleteStoresTheNormalisedPublicURL(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	if rec := f.complete(t, map[string]any{"publicUrl": "HTTPS://Bayplan.Example/ui/"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if v, _ := f.setting(t, "public_url"); v != "https://bayplan.example/ui" {
		t.Fatalf("public_url = %q, want the normalised form", v)
	}
}

func TestSetupCompleteIgnoresASubmittedPublicURLWhenTheEnvironmentFixesIt(t *testing.T) {
	f := newSetupFixture(t, config.Config{PublicURL: "https://env.example"})
	rec := f.complete(t, map[string]any{"publicUrl": "https://attacker.example"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decodeComplete(t, rec)
	if got.PublicURL != "https://env.example" || !got.PublicURLLocked {
		t.Fatalf("reported public URL = %q locked %v, want the env value, locked", got.PublicURL, got.PublicURLLocked)
	}
	if v, ok := f.setting(t, "public_url"); ok {
		t.Fatalf("public_url %q was stored although the environment fixes it", v)
	}
	if f.inst.PublicOrigin() != "https://env.example" {
		t.Fatalf("origin = %q", f.inst.PublicOrigin())
	}
	if c := sessionCookie(rec); c == nil || c.Name != "__Host-bayplan_session" {
		t.Fatalf("cookie = %+v, want the https name from the env URL", c)
	}
}

func TestSetupCompleteIgnoresAnInvalidSubmittedURLWhenTheEnvironmentFixesIt(t *testing.T) {
	f := newSetupFixture(t, config.Config{PublicURL: "https://env.example"})
	if rec := f.complete(t, map[string]any{"publicUrl": "not a url"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the submitted URL is irrelevant when locked: %s", rec.Code, rec.Body)
	}
}

type fieldProblem struct {
	Status int `json:"status"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func TestSetupCompleteRejectsEachInvalidInputWithAFieldCodeAndCreatesNothing(t *testing.T) {
	tooLong := strings.Repeat("x", 65)
	for name, tc := range map[string]struct {
		over     map[string]any
		location string
		code     string
	}{
		"username too short":         {map[string]any{"username": "ab"}, "body.username", "invalid_username"},
		"username too long":          {map[string]any{"username": strings.Repeat("a", 33)}, "body.username", "invalid_username"},
		"username with a space":      {map[string]any{"username": "bad name"}, "body.username", "invalid_username"},
		"username with unicode":      {map[string]any{"username": "ünïcode"}, "body.username", "invalid_username"},
		"password too short":         {map[string]any{"password": "abcdefghijk"}, "body.password", "password_too_short"},
		"password empty":             {map[string]any{"password": ""}, "body.password", "password_too_short"},
		"password on the list":       {map[string]any{"password": "passwordpassword"}, "body.password", "password_common"},
		"password on the list cased": {map[string]any{"password": "PasswordPassword"}, "body.password", "password_common"},
		"password has the username":  {map[string]any{"username": "gandalf", "password": "xx-gandalf-the-grey"}, "body.password", "password_common"},
		"instance name empty":        {map[string]any{"instanceName": ""}, "body.instanceName", "instance_name_required"},
		"instance name blank":        {map[string]any{"instanceName": "   "}, "body.instanceName", "instance_name_required"},
		"instance name 65 chars":     {map[string]any{"instanceName": tooLong}, "body.instanceName", "instance_name_too_long"},
		"public url relative":        {map[string]any{"publicUrl": "/bayplan"}, "body.publicUrl", "invalid_public_url"},
		"public url no scheme":       {map[string]any{"publicUrl": "bayplan.example"}, "body.publicUrl", "invalid_public_url"},
		"public url wrong scheme":    {map[string]any{"publicUrl": "ftp://bayplan.example"}, "body.publicUrl", "invalid_public_url"},
		"public url with userinfo":   {map[string]any{"publicUrl": "https://user:" + "secret@bayplan.example"}, "body.publicUrl", "invalid_public_url"},
		"public url with a query":    {map[string]any{"publicUrl": "https://bayplan.example/?a=b"}, "body.publicUrl", "invalid_public_url"},
		"public url empty":           {map[string]any{"publicUrl": ""}, "body.publicUrl", "invalid_public_url"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetupFixture(t, config.Config{})
			rec := f.complete(t, tc.over)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
			}
			var p fieldProblem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range p.Errors {
				if e.Location == tc.location && e.Message == tc.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors = %+v, want location %q message %q", p.Errors, tc.location, tc.code)
			}
			if n := f.userCount(t); n != 0 {
				t.Fatalf("a rejected input created %d users", n)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM sessions`) + f.count(t, `SELECT COUNT(*) FROM settings`) + f.count(t, `SELECT COUNT(*) FROM role_assignments`); n != 0 {
				t.Fatalf("a rejected input left %d rows behind", n)
			}
			if sessionCookie(rec) != nil {
				t.Fatal("a rejected input set a session cookie")
			}
			if !f.gate.NeedsSetup() {
				t.Fatal("a rejected input closed the gate; the token holder must be able to fix the form and retry")
			}
			if n := len(f.auditRows(t)); n != 0 {
				t.Fatalf("a validation failure wrote %d audit entries, want 0", n)
			}
			if strings.Contains(rec.Body.String(), goodPassword) {
				t.Fatal("the response echoes the password")
			}
		})
	}
}

func TestSetupCompleteReportsEveryInvalidFieldAtOnce(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	rec := f.complete(t, map[string]any{"username": "x", "password": "short", "instanceName": "", "publicUrl": "nope"})
	var p fieldProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d (%v): %s", rec.Code, err, rec.Body)
	}
	have := map[string]string{}
	for _, e := range p.Errors {
		have[e.Location] = e.Message
	}
	want := map[string]string{"body.username": "invalid_username", "body.password": "password_too_short", "body.instanceName": "instance_name_required", "body.publicUrl": "invalid_public_url"}
	for k, v := range want {
		if have[k] != v {
			t.Errorf("errors[%s] = %q, want %q (all: %v)", k, have[k], v, have)
		}
	}
}

func TestSetupCompleteAcceptsA64CharacterInstanceNameCountedInCharacters(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	name := strings.Repeat("é", 64) // 128 bytes, 64 characters
	if rec := f.complete(t, map[string]any{"instanceName": name}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if v, _ := f.setting(t, "instance_name"); v != name {
		t.Fatalf("instance_name = %q", v)
	}
}

func TestSetupCompleteTrimsTheInstanceName(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	if rec := f.complete(t, map[string]any{"instanceName": "  Home lab \t"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if v, _ := f.setting(t, "instance_name"); v != "Home lab" {
		t.Fatalf("instance_name = %q, want the trimmed name", v)
	}
}

func TestSetupCompleteCapsTheBodyFields(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	if rec := f.complete(t, map[string]any{"password": strings.Repeat("a", 1025)}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 1025 character password = %d, want 422", rec.Code)
	}
	if rec := f.complete(t, map[string]any{"instanceName": strings.Repeat("a", 5000)}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 5000 character instance name = %d, want 422", rec.Code)
	}
	if rec := f.completeFrom(t, testClientAddr, `{"token":"x"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a body with missing fields = %d, want 422", rec.Code)
	}
	if n := f.userCount(t); n != 0 {
		t.Fatalf("users = %d, want 0", n)
	}
}

func TestSetupCompleteWrongTokenIs401AuditedAndCreatesNothing(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	const attempt = "deadbeef-deadbeef-deadbeef-deadbeef"
	// Garbage fields too: the token is checked first, so a stranger learns nothing about validation.
	rec := f.completeFrom(t, testClientAddr, completeJSON(t, attempt, map[string]any{"username": "x"}), nil)
	if rec.Code != http.StatusUnauthorized || problemType(t, rec) != "invalid_setup_token" {
		t.Fatalf("got %d %s, want 401 invalid_setup_token", rec.Code, rec.Body)
	}
	rows := f.auditRows(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionSetupTokenFailed || rows[0].Reason != "invalid_token" ||
		rows[0].ActorType != string(audit.ActorAnonymous) || rows[0].Ip != "203.0.113.9" {
		t.Fatalf("audit rows %+v, want one setup.token_failed / invalid_token / anonymous / the client IP", rows)
	}
	for _, hay := range []string{f.logs.String(), rec.Body.String(), rows[0].Details} {
		if strings.Contains(hay, attempt) || strings.Contains(hay, goodPassword) {
			t.Fatal("the attempted token or the password leaked")
		}
	}
	if f.userCount(t) != 0 || sessionCookie(rec) != nil || !f.gate.NeedsSetup() {
		t.Fatal("a wrong token changed state")
	}
}

func TestSetupCompleteSharesTheRateLimitWithTheTokenCheck(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	for i := 1; i <= 5; i++ {
		if rec := f.completeFrom(t, testClientAddr, completeJSON(t, "wrong", nil), nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	rec := f.complete(t, nil)
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
		t.Fatalf("6th attempt = %d %s, want 429 rate_limited even with the right token", rec.Code, rec.Body)
	}
	rows := f.auditRows(t)
	if len(rows) != 6 || rows[5].Action != audit.ActionSetupTokenFailed || rows[5].Reason != "rate_limited" {
		t.Fatalf("audit rows = %d, last %+v; want 6 with a rate_limited entry last", len(rows), rows[len(rows)-1])
	}
	if f.userCount(t) != 0 {
		t.Fatal("a rate-limited request created a user")
	}
}

func TestSetupCompleteReplayAfterCompletionIs409AuditedAndChangesNothing(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	first := f.complete(t, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first completion = %d: %s", first.Code, first.Body)
	}
	before, err := f.st.RQ.GetUserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}

	second := f.complete(t, map[string]any{"username": "mallory", "password": "an entirely different passphrase", "instanceName": "Pwned"})
	if second.Code != http.StatusConflict || problemType(t, second) != "setup_complete" {
		t.Fatalf("replay = %d %s, want 409 setup_complete", second.Code, second.Body)
	}
	if sessionCookie(second) != nil {
		t.Fatal("a replay was given a session")
	}
	if n := f.userCount(t); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
	after, _ := f.st.RQ.GetUserByUsername(context.Background(), "admin")
	if after.PasswordHash != before.PasswordHash {
		t.Fatal("a replay changed the admin's password")
	}
	if v, _ := f.setting(t, "instance_name"); v != "Home lab" {
		t.Fatalf("instance_name = %q after a replay", v)
	}
	rows := f.auditRows(t)
	if len(rows) != 2 || rows[1].Action != audit.ActionSetupAttemptDenied || rows[1].Ip != "203.0.113.9" {
		t.Fatalf("audit rows %+v, want setup.completed then setup.attempt_denied", rows)
	}
}

func TestSetupCompleteConcurrentCompletionsHaveExactlyOneWinner(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	const n = 6
	codes := make([]int, n)
	types := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Distinct client addresses, so the per-IP limiter does not decide the race.
			body := completeJSON(t, f.token, map[string]any{"username": fmt.Sprintf("admin%d", i)})
			rec := f.completeFrom(t, fmt.Sprintf("203.0.113.%d:4000", 10+i), body, nil)
			codes[i] = rec.Code
			if rec.Code == http.StatusConflict {
				var p struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &p)
				types[i] = p.Type
			}
		}()
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
			if types[i] != "setup_complete" {
				t.Errorf("loser %d got problem type %q, want setup_complete", i, types[i])
			}
		default:
			t.Errorf("completion %d returned %d", i, c)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("%d winners and %d conflicts, want 1 and %d (codes %v)", ok, conflict, n-1, codes)
	}
	if got := f.userCount(t); got != 1 {
		t.Fatalf("users = %d, want exactly 1", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE role_id = 'admin'`); got != 1 {
		t.Fatalf("admin role assignments = %d, want 1", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM sessions`); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
	completed := f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'setup.completed'`)
	denied := f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'setup.attempt_denied'`)
	if completed != 1 || denied != n-1 {
		t.Fatalf("audit: %d setup.completed and %d setup.attempt_denied, want 1 and %d", completed, denied, n-1)
	}
}

// failingTxAudit makes the completion's audit write fail.
type failingTxAudit struct{ *audit.Writer }

func (a failingTxAudit) RecordTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error {
	if e.Action == audit.ActionSetupCompleted {
		return errors.New("injected audit failure")
	}
	return a.Writer.RecordTx(ctx, tx, e)
}

func TestSetupCompleteRollsEverythingBackWhenTheAuditWriteFails(t *testing.T) {
	f := newSetupFixtureWith(t, config.Config{}, func(w *audit.Writer) api.SetupAuditor { return failingTxAudit{w} })
	rec := f.complete(t, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a failed completion set a session cookie")
	}
	if f.userCount(t) != 0 {
		t.Fatal("the user survived a failed completion")
	}
	for _, table := range []string{"role_assignments", "sessions", "settings", "audit_log"} {
		if n := f.count(t, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("%s has %d rows after a failed completion, want 0", table, n)
		}
	}
	if !f.gate.NeedsSetup() {
		t.Fatal("the gate closed although nothing was created")
	}
	if tok, err := auth.ReadTokenFile(f.dataDir); err != nil || tok != f.token {
		t.Fatalf("the token file was consumed by a failed completion: %q, %v", tok, err)
	}
	if f.inst.Name() != "" || f.inst.PublicURL() != "" {
		t.Fatal("the instance cache changed although the transaction rolled back")
	}
}
