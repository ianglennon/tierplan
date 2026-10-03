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
	AgentURL        string `json:"agentUrl"`
	AgentURLLocked  bool   `json:"agentUrlLocked"`
	AgentEndpoint   string `json:"agentEndpoint"`
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
		{"url under /api", map[string]string{"publicUrl": "https://host.example/api"}, "body.publicUrl", "invalid_public_url"},
		{"url under /assets", map[string]string{"publicUrl": "https://host.example/assets/x"}, "body.publicUrl", "invalid_public_url"},
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

// The agent URL (plan 02-09, D-18): where agents dial when it differs from the
// address people browse to.

func TestGetSettingsReportsTheAgentURLTheLockAndTheEndpoint(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if _, s := f.getSettings(t, cookie); s.AgentURL != "" || s.AgentURLLocked || s.AgentEndpoint != "" {
		t.Fatalf("nothing set: %+v, want an empty agent URL, unlocked, and no endpoint", s)
	}
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://example.com/bayplan"}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if _, s := f.getSettings(t, cookie); s.AgentURL != "" || s.AgentEndpoint != "wss://example.com/bayplan/agent/connect" {
		t.Fatalf("public URL only: %+v, want the endpoint from the public URL", s)
	}

	locked := newAuthFixture(t, config.Config{PublicURL: "https://pub.example", AgentURL: "https://agent.internal:7480"})
	lcookie, _ := locked.signIn(t, "admin", false)
	_, s := locked.getSettings(t, lcookie)
	if !s.AgentURLLocked || s.AgentURL != "https://agent.internal:7480" || s.AgentEndpoint != "wss://agent.internal:7480/agent/connect" {
		t.Fatalf("locked: %+v", s)
	}
}

func TestPatchSettingsStoresTheAgentURLAndAuditsIt(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://example.com"})

	rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://bayplan.lan:7480/"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got settingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.AgentURL != "https://bayplan.lan:7480" || got.AgentEndpoint != "wss://bayplan.lan:7480/agent/connect" {
		t.Fatalf("body = %s (%v), want the normalised agent URL and its endpoint", rec.Body, err)
	}
	if f.inst.AgentURL() != "https://bayplan.lan:7480" || f.inst.AgentEndpoint() != "wss://bayplan.lan:7480/agent/connect" {
		t.Fatalf("the cached agent URL = %q, endpoint %q, want them at once", f.inst.AgentURL(), f.inst.AgentEndpoint())
	}
	var stored string
	if err := f.st.Read.QueryRow(`SELECT value FROM settings WHERE key = 'agent_url'`).Scan(&stored); err != nil || stored != "https://bayplan.lan:7480" {
		t.Fatalf("stored = %q, %v", stored, err)
	}
	d := f.auditDetails(t, "settings.updated")
	last := d[len(d)-1]
	if last["agent_url_from"] != "" || last["agent_url_to"] != "https://bayplan.lan:7480" {
		t.Fatalf("details = %v, want the agent URL field with its old and new value", last)
	}
	if _, ok := last["public_url_from"]; ok {
		t.Fatalf("details = %v: an unchanged field must not be listed", last)
	}

	// An empty agent URL clears it, and agents are told the public address again.
	rec = f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if f.inst.AgentURL() != "" || f.inst.AgentEndpoint() != "wss://example.com/agent/connect" {
		t.Fatalf("after clearing: agent URL %q endpoint %q", f.inst.AgentURL(), f.inst.AgentEndpoint())
	}
	d = f.auditDetails(t, "settings.updated")
	if last := d[len(d)-1]; last["agent_url_from"] != "https://bayplan.lan:7480" || last["agent_url_to"] != "" {
		t.Fatalf("details = %v, want the cleared agent URL audited", last)
	}
}

func TestPatchSettingsRefusesAnInvalidAgentURL(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	for _, bad := range []string{"/agent", "bayplan.lan", "ftp://bayplan.lan", "https://user@bayplan.lan", "https://bayplan.lan/api"} {
		rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%q: status = %d: %s, want 422", bad, rec.Code, rec.Body)
		}
		p := decodeProblem(t, rec)
		if p.Type != "validation_failed" || len(p.Errors) != 1 || p.Errors[0].Location != "body.agentUrl" || p.Errors[0].Message != "invalid_agent_url" {
			t.Fatalf("%q: problem = %+v, want invalid_agent_url at body.agentUrl", bad, p)
		}
	}
	if f.inst.AgentURL() != "" {
		t.Fatalf("a refused change stored %q", f.inst.AgentURL())
	}
	if n := len(f.audits(t, "settings.updated")); n != 0 {
		t.Fatalf("%d settings.updated entries after only invalid requests", n)
	}
}

func TestPatchSettingsRefusesAnEnvironmentLockedAgentURL(t *testing.T) {
	f := newAuthFixture(t, config.Config{AgentURL: "https://fixed.internal:7480"})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Changed", "agentUrl": "https://other.example"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s, want 409", rec.Code, rec.Body)
	}
	if p := decodeProblem(t, rec); p.Type != "agent_url_locked" {
		t.Fatalf("problem type = %q, want agent_url_locked", p.Type)
	}
	if f.inst.AgentURL() != "https://fixed.internal:7480" || f.inst.Name() == "Changed" {
		t.Fatalf("agent URL %q name %q: a refused change must change nothing", f.inst.AgentURL(), f.inst.Name())
	}
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": ""}); rec.Code != http.StatusConflict {
		t.Fatalf("clearing a locked agent URL: %d, want 409", rec.Code)
	}

	// Naming the fixed value again is not a change, so the name still goes through.
	rec = f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Lab", "agentUrl": "https://fixed.internal:7480"})
	if rec.Code != http.StatusOK || f.inst.Name() != "Lab" {
		t.Fatalf("renaming with the fixed agent URL echoed back: %d %s", rec.Code, rec.Body)
	}
	// The public URL is a different lock.
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://pub.example"}); rec.Code != http.StatusOK {
		t.Fatalf("the public URL stays editable while only the agent URL is locked: %d %s", rec.Code, rec.Body)
	}
}

func TestPatchSettingsWithTheSameAgentURLIsANoOp(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://bayplan.lan:7480"})
	before := len(f.audits(t, "settings.updated"))
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://BAYPLAN.lan:7480/"}); rec.Code != http.StatusOK {
		t.Fatalf("no-op: %d %s", rec.Code, rec.Body)
	}
	if after := len(f.audits(t, "settings.updated")); after != before {
		t.Fatalf("%d entries, want %d: a no-op must not be audited", after, before)
	}
}

func TestAgentURLNeedsSettingsPermissions(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH agentUrl without settings.write: %d, want 403", rec.Code)
	}
	if f.inst.AgentURL() != "" {
		t.Fatal("a refused PATCH changed the agent URL")
	}
}
