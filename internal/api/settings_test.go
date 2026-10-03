// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
)

// Settings: instance name and public URL (plan 26, D-03, D-04, AUTH-08).

type settingsResp struct {
	InstanceName    string `json:"instanceName"`
	PublicURL       string `json:"publicUrl"`
	PublicURLLocked bool   `json:"publicUrlLocked"`
}

func (f *authFixture) getSettings(t *testing.T, cookie string) (int, settingsResp) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/settings", cookie: cookie})
	var s settingsResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("not a settings body: %v: %s", err, rec.Body)
		}
	}
	return rec.Code, s
}

func (f *authFixture) patchSettings(t *testing.T, cookie, csrf string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	return f.call(t, authReq{method: http.MethodPatch, path: "/api/settings", body: string(b), cookie: cookie, csrf: csrf})
}

// auditDetails returns the details of every settings.updated entry, oldest first.
func (f *authFixture) auditDetails(t *testing.T, action string) []map[string]string {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGetSettingsReportsTheNameTheURLAndTheLock(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Home", "publicUrl": "https://bay.example"}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	code, s := f.getSettings(t, cookie)
	if code != http.StatusOK || s.InstanceName != "Home" || s.PublicURL != "https://bay.example" || s.PublicURLLocked {
		t.Fatalf("got %d %+v", code, s)
	}

	locked := newAuthFixture(t, config.Config{PublicURL: "https://fixed.example"})
	lcookie, _ := locked.signIn(t, "admin", false)
	if code, s := locked.getSettings(t, lcookie); code != http.StatusOK || !s.PublicURLLocked || s.PublicURL != "https://fixed.example" {
		t.Fatalf("locked: got %d %+v", code, s)
	}
}

func TestPatchSettingsRenamesTheInstanceAndAuditsOldAndNew(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Home"})

	rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "  Lab  "})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got settingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.InstanceName != "Lab" {
		t.Fatalf("body = %s (%v), want the trimmed new name", rec.Body, err)
	}
	if f.inst.Name() != "Lab" {
		t.Fatalf("the cached name = %q, want Lab at once", f.inst.Name())
	}
	var stored string
	if err := f.st.Read.QueryRow(`SELECT value FROM settings WHERE key = 'instance_name'`).Scan(&stored); err != nil || stored != "Lab" {
		t.Fatalf("stored name = %q, %v", stored, err)
	}

	entries := f.audits(t, "settings.updated")
	if len(entries) != 2 || entries[1].ActorID != "id-admin" || entries[1].Result != "success" || entries[1].IP != authIP {
		t.Fatalf("audit = %+v, want two success entries by id-admin", entries)
	}
	d := f.auditDetails(t, "settings.updated")[1]
	if d["instance_name_from"] != "Home" || d["instance_name_to"] != "Lab" {
		t.Fatalf("details = %v, want instance_name_from Home and instance_name_to Lab", d)
	}
	if _, ok := d["public_url_from"]; ok {
		t.Fatalf("details = %v: an unchanged field must not be listed", d)
	}
}

func TestPatchSettingsSwitchesTheOriginGuardImmediately(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://old.example/"}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://NEW.example"}); rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	if f.inst.PublicURL() != "https://new.example" || !f.inst.Secure() {
		t.Fatalf("public URL = %q secure = %v, want the normalised new origin and a Secure cookie", f.inst.PublicURL(), f.inst.Secure())
	}

	// The instance is https now, so the browser's bare cookie is no longer
	// believed (WR-01): the switch signs sessions out once. The same session ID
	// presented as the __Host- cookie a re-sign-in would set is still valid.
	ack := func(origin string, secure bool) int {
		req := httptest.NewRequest(http.MethodPost, "/api/warnings/key-backup/ack", strings.NewReader(""))
		req.Header.Set("Origin", origin)
		req.Header.Set("X-CSRF-Token", csrf)
		req.RemoteAddr = authIP + ":40000"
		req.AddCookie(&http.Cookie{Name: auth.CookieName(auth.CookieScope{Secure: secure}), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := ack("https://old.example", true); code != http.StatusForbidden {
		t.Fatalf("a POST from the old origin: %d, want 403", code)
	}
	if code := ack("https://new.example", false); code != http.StatusUnauthorized {
		t.Fatalf("a POST from the new origin with the bare cookie: %d, want 401 (the scheme switch signs browsers out once)", code)
	}
	if code := ack("https://new.example", true); code != http.StatusNoContent {
		t.Fatalf("a POST from the new origin: %d, want 204", code)
	}

	d := f.auditDetails(t, "settings.updated")
	last := d[len(d)-1]
	if last["public_url_from"] != "https://old.example" || last["public_url_to"] != "https://new.example" {
		t.Fatalf("details = %v, want the old and new public URL", last)
	}
}

func TestPatchSettingsRefusesAnEnvironmentLockedPublicURL(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://fixed.example"})
	cookie, csrf := f.signIn(t, "admin", false)
	f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Home"})

	rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Changed", "publicUrl": "https://other.example"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s, want 409", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Type != "public_url_locked" {
		t.Fatalf("problem type = %q, want public_url_locked", p.Type)
	}
	if f.inst.PublicURL() != "https://fixed.example" || f.inst.Name() != "Home" {
		t.Fatalf("name %q url %q: a refused change must change nothing", f.inst.Name(), f.inst.PublicURL())
	}
	if n := len(f.audits(t, "settings.updated")); n != 1 {
		t.Fatalf("%d settings.updated entries, want only the seed", n)
	}

	// Naming the fixed value again is not a change, so the name still goes through.
	rec = f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Lab", "publicUrl": "https://fixed.example"})
	if rec.Code != http.StatusOK || f.inst.Name() != "Lab" {
		t.Fatalf("renaming with the fixed URL echoed back: %d %s", rec.Code, rec.Body)
	}
}

func TestPatchSettingsValidatesEveryField(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	for _, c := range []struct {
		name     string
		body     map[string]string
		location string
		code     string
	}{
		{"empty name", map[string]string{"instanceName": "   "}, "body.instanceName", "instance_name_required"},
		{"65 characters", map[string]string{"instanceName": strings.Repeat("é", 65)}, "body.instanceName", "instance_name_too_long"},
		{"relative url", map[string]string{"publicUrl": "/bayplan"}, "body.publicUrl", "invalid_public_url"},
		{"empty url", map[string]string{"publicUrl": ""}, "body.publicUrl", "invalid_public_url"},
		{"url with credentials", map[string]string{"publicUrl": "https://user@host.example"}, "body.publicUrl", "invalid_public_url"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := f.patchSettings(t, cookie, csrf, c.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d: %s, want 422", rec.Code, rec.Body)
			}
			p := decodeProblem(t, rec)
			if p.Type != "validation_failed" || len(p.Errors) != 1 || p.Errors[0].Location != c.location || p.Errors[0].Message != c.code {
				t.Fatalf("problem = %+v, want %s at %s", p, c.code, c.location)
			}
		})
	}
	if n := len(f.audits(t, "settings.updated")); n != 0 {
		t.Fatalf("%d settings.updated entries after only invalid requests", n)
	}
	// A 64 character name is the longest allowed.
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": strings.Repeat("é", 64)}); rec.Code != http.StatusOK {
		t.Fatalf("64 characters: %d %s", rec.Code, rec.Body)
	}
}

func TestPatchSettingsWithNothingNewIsANoOpAndIsNotAudited(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Home"})
	before := len(f.audits(t, "settings.updated"))
	for _, body := range []map[string]string{{}, {"instanceName": "Home"}} {
		if rec := f.patchSettings(t, cookie, csrf, body); rec.Code != http.StatusOK {
			t.Fatalf("%v: %d %s", body, rec.Code, rec.Body)
		}
	}
	if after := len(f.audits(t, "settings.updated")); after != before {
		t.Fatalf("%d entries, want %d: a no-op must not be audited", after, before)
	}
}

func TestSettingsNeedSettingsPermissions(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false) // no role at all
	cookie, csrf := f.signIn(t, "viewer", false)

	if code, _ := f.getSettings(t, cookie); code != http.StatusForbidden {
		t.Fatalf("GET without settings.read: %d, want 403", code)
	}
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Mine"}); rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH without settings.write: %d, want 403", rec.Code)
	}
	if f.inst.Name() == "Mine" {
		t.Fatal("a refused PATCH changed the name")
	}
	if code, _ := f.getSettings(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("GET anonymously: %d, want 401", code)
	}
	admin, _ := f.signIn(t, "admin", false)
	if rec := f.patchSettings(t, admin, "", map[string]string{"instanceName": "Mine"}); rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH without X-CSRF-Token: %d, want 403", rec.Code)
	}
}
