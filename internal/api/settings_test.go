// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	EnrolledAgents  int    `json:"enrolledAgents"`
	OfflineAgents   int    `json:"offlineAgents"`

	AgentEndpointRefused bool `json:"agentEndpointRefused"`
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Agents in the settings (plan 02-26, D-18, D-19): how many are enrolled and
// offline, and a changed address pushed to the connected ones.

// fakeSettingsAgents stands in for the hub and the agents service.
type fakeSettingsAgents struct {
	mu        sync.Mutex
	enrolled  []string
	connected map[string]bool
	pins      []string
	pushes    [][]string
	pushPins  [][]string
	err       error
}

func (f *fakeSettingsAgents) EnrolledAgentIDs(context.Context) ([]string, error) {
	return f.enrolled, f.err
}

func (f *fakeSettingsAgents) Connected(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[id]
}

func (f *fakeSettingsAgents) PushURLs(urls, pins []string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, urls)
	f.pushPins = append(f.pushPins, pins)
	return len(f.connected)
}

func (f *fakeSettingsAgents) Pins() []string { return f.pins }

func (f *fakeSettingsAgents) pushed() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.pushes...)
}

func agentsFixture(t *testing.T, cfg config.Config, fake *fakeSettingsAgents) (*authFixture, string, string) {
	t.Helper()
	f := newAuthFixture(t, cfg)
	f.settingsAgents = fake
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)
	return f, cookie, csrf
}

func TestGetSettingsCountsEnrolledAgentsAndTheOnesWithNoConnection(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{enrolled: []string{"a", "b", "c"}, connected: map[string]bool{"a": true, "c": true}}
	f, cookie, _ := agentsFixture(t, config.Config{}, fake)
	code, s := f.getSettings(t, cookie)
	if code != http.StatusOK || s.EnrolledAgents != 3 || s.OfflineAgents != 1 {
		t.Fatalf("got %d %+v, want 3 enrolled and 1 offline", code, s)
	}
	fake.connected = map[string]bool{}
	if _, s := f.getSettings(t, cookie); s.OfflineAgents != 3 {
		t.Errorf("offline = %d with nobody connected, want 3", s.OfflineAgents)
	}
}

func TestGetSettingsWithNoAgentsOrNoAgentSourceCountsZero(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	if _, s := f.getSettings(t, cookie); s.EnrolledAgents != 0 || s.OfflineAgents != 0 {
		t.Fatalf("no agent source: %+v", s)
	}
	f2, cookie2, _ := agentsFixture(t, config.Config{}, &fakeSettingsAgents{})
	if _, s := f2.getSettings(t, cookie2); s.EnrolledAgents != 0 || s.OfflineAgents != 0 {
		t.Fatalf("no agents: %+v", s)
	}
}

func TestGetSettingsStillAnswersWhenTheAgentsCannotBeCounted(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{err: context.DeadlineExceeded}
	f, cookie, _ := agentsFixture(t, config.Config{}, fake)
	code, s := f.getSettings(t, cookie)
	if code != http.StatusOK || s.EnrolledAgents != 0 || s.OfflineAgents != 0 {
		t.Fatalf("got %d %+v: a failed count must not take the settings page down", code, s)
	}
}

func TestSavingANewAgentURLPushesItFirstWithTheCurrentPinsToConnectedAgents(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{pins: []string{"sha256/x"}, connected: map[string]bool{"a": true}}
	f, cookie, csrf := agentsFixture(t, config.Config{}, fake)
	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://pub.example"})
	base := len(fake.pushed())

	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://bayplan.lan:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := fake.pushed()
	if len(got) != base+1 || len(got[base]) != 1 || got[base][0] != "wss://bayplan.lan:7480/agent/connect" {
		t.Fatalf("pushes = %v, want one more with the new endpoint", got)
	}
	if pins := fake.pushPins[base]; len(pins) != 1 || pins[0] != "sha256/x" {
		t.Errorf("pushed pins = %v, want the hub's current pins", pins)
	}
}

func TestChangingThePublicURLPushesTheNewEndpointWhenItIsWhatAgentsDial(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{connected: map[string]bool{"a": true}}
	f, cookie, csrf := agentsFixture(t, config.Config{}, fake)
	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://old.example"})
	base := len(fake.pushed())

	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://new.example/bayplan"})
	got := fake.pushed()
	if len(got) != base+1 || got[base][0] != "wss://new.example/bayplan/agent/connect" {
		t.Fatalf("pushes = %v, want the endpoint of the new public URL", got)
	}
}

func TestClearingTheAgentURLPushesTheEndpointOfThePublicURL(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{connected: map[string]bool{"a": true}}
	f, cookie, csrf := agentsFixture(t, config.Config{}, fake)
	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://pub.example", "agentUrl": "https://lan.internal:7480"})
	base := len(fake.pushed())

	f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": ""})
	got := fake.pushed()
	if len(got) != base+1 || got[base][0] != "wss://pub.example/agent/connect" {
		t.Fatalf("pushes = %v, want the public URL's endpoint after the agent URL is cleared", got)
	}
}

func TestNothingIsPushedWhenTheEndpointAgentsDialDidNotChange(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{connected: map[string]bool{"a": true}}
	f, cookie, csrf := agentsFixture(t, config.Config{}, fake)
	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://pub.example", "agentUrl": "https://lan.internal:7480"})
	base := len(fake.pushed())

	// The public URL changes under an agent URL: agents keep dialing the agent URL.
	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://other.example"})
	// The name changes.
	f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Renamed"})
	// The same value again, and an invalid one.
	f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://lan.internal:7480"})
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "ftp://nope"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid agent URL: %d", rec.Code)
	}
	if got := fake.pushed(); len(got) != base {
		t.Fatalf("pushes = %v, want none after changes that left the endpoint alone", got[base:])
	}
}

func TestARefusedChangeOfALockedAgentURLPushesNothing(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{connected: map[string]bool{"a": true}}
	f, cookie, csrf := agentsFixture(t, config.Config{PublicURL: "https://pub.example", AgentURL: "https://agent.internal:7480"}, fake)
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://elsewhere.example"}); rec.Code != http.StatusConflict {
		t.Fatalf("locked: %d", rec.Code)
	}
	if got := fake.pushed(); len(got) != 0 {
		t.Fatalf("pushes = %v after a refused change", got)
	}
}

func TestAChangeWithNoAgentSourceStillSaves(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://bayplan.lan:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

// The agent address agents refuse (G-02-R4-3, plan 02-45): a plain address for a
// host that is not this machine, with the development switch off.

func TestGetSettingsSaysWhenAgentsCannotUseTheEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label      string
		publicURL  string
		agentURL   string
		allow      bool
		wantRefuse bool
	}{
		{"plain agent URL for another host, switch off", "", "http://10.0.0.5:7480", false, true},
		{"plain agent URL for another host, switch on", "", "http://10.0.0.5:7480", true, false},
		{"plain public URL for another host, no agent URL, switch off", "http://10.0.0.5:7480", "", false, true},
		{"plain public URL for another host, no agent URL, switch on", "http://10.0.0.5:7480", "", true, false},
		{"plain agent URL on this machine", "https://pub.example", "http://127.0.0.1:7480", false, false},
		{"https agent URL under a plain public URL", "http://10.0.0.5:7480", "https://10.0.0.5:7480", false, false},
		{"https public URL, no agent URL", "https://pub.example", "", false, false},
		{"no address at all", "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			f := newAuthFixture(t, config.Config{})
			f.agentAllowInsecure = tc.allow
			f.h = f.build(t)
			if tc.publicURL != "" {
				f.inst.Apply(f.inst.Name(), tc.publicURL)
			}
			if tc.agentURL != "" {
				f.inst.ApplyAgentURL(tc.agentURL)
			}
			cookie, _ := f.signIn(t, "admin", false)
			code, s := f.getSettings(t, cookie)
			if code != http.StatusOK {
				t.Fatalf("GET /api/settings = %d", code)
			}
			if s.AgentEndpointRefused != tc.wantRefuse {
				t.Errorf("agentEndpointRefused = %v for endpoint %q, want %v", s.AgentEndpointRefused, s.AgentEndpoint, tc.wantRefuse)
			}
		})
	}
}

func TestPatchSettingsRefusesAPlainAgentURLForAnotherHostWithoutTheSwitch(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{connected: map[string]bool{"a": true}}
	f, cookie, csrf := agentsFixture(t, config.Config{}, fake)
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://agent.internal:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	audits, pushes := len(f.audits(t, "settings.updated")), len(fake.pushed())

	for _, bad := range []string{"http://10.0.0.5:7480", "http://bayplan.lan:7480", "http://[2001:db8::1]:7480"} {
		rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Changed", "agentUrl": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%q: status = %d: %s, want 422", bad, rec.Code, rec.Body)
		}
		p := decodeProblem(t, rec)
		if p.Type != "validation_failed" || len(p.Errors) != 1 || p.Errors[0].Location != "body.agentUrl" || p.Errors[0].Message != "insecure_agent_url" {
			t.Fatalf("%q: problem = %+v, want insecure_agent_url at body.agentUrl", bad, p)
		}
	}
	if _, s := f.getSettings(t, cookie); s.AgentURL != "https://agent.internal:7480" || s.InstanceName == "Changed" {
		t.Fatalf("a refused change stored agent URL %q name %q", s.AgentURL, s.InstanceName)
	}
	if n := len(f.audits(t, "settings.updated")); n != audits {
		t.Fatalf("%d settings.updated entries after refusals, want %d", n, audits)
	}
	if got := fake.pushed(); len(got) != pushes {
		t.Fatalf("pushes = %v after refused changes", got[pushes:])
	}
}

func TestPatchSettingsAcceptsAPlainAgentURLOnlyOnThisMachineOrWithTheSwitch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label     string
		allow     bool
		publicURL string // applied before the request, "" for none
		agentURL  string // applied before the request, "" for none
		body      map[string]string
		// wantRefused is agentEndpointRefused in the answer.
		wantRefused bool
	}{
		{"loopback address, switch off", false, "", "", map[string]string{"agentUrl": "http://127.0.0.1:7480"}, false},
		{"localhost, switch off", false, "", "", map[string]string{"agentUrl": "http://localhost:7480"}, false},
		{"another host, switch on", true, "", "", map[string]string{"agentUrl": "http://10.0.0.5:7480"}, false},
		{"https for another host, switch off", false, "", "", map[string]string{"agentUrl": "https://10.0.0.5:7480"}, false},
		{"clearing it under a plain public URL", false, "http://10.0.0.5:7480", "https://agent.example", map[string]string{"agentUrl": ""}, true},
		{"a plain public URL with no agent URL", false, "", "", map[string]string{"publicUrl": "http://10.0.0.5:7480"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			f := newAuthFixture(t, config.Config{})
			f.agentAllowInsecure = tc.allow
			f.h = f.build(t)
			if tc.publicURL != "" {
				f.inst.Apply(f.inst.Name(), tc.publicURL)
			}
			if tc.agentURL != "" {
				f.inst.ApplyAgentURL(tc.agentURL)
			}
			cookie, csrf := f.signIn(t, "admin", false)
			rec := f.patchSettings(t, cookie, csrf, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s, want 200", rec.Code, rec.Body)
			}
			var s settingsResp
			if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
				t.Fatal(err)
			}
			if s.AgentEndpointRefused != tc.wantRefused {
				t.Errorf("agentEndpointRefused = %v in the answer (endpoint %q), want %v", s.AgentEndpointRefused, s.AgentEndpoint, tc.wantRefused)
			}
		})
	}
}

// Browser-shell limits (plan 04-23, D-29, D-31).

type shellSettingsResp struct {
	IdleMinutes int `json:"idleMinutes"`
	MaxPerUser  int `json:"maxPerUser"`
}

func (f *authFixture) getShellSettings(t *testing.T, cookie string) (int, shellSettingsResp) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/settings/shells", cookie: cookie})
	var s shellSettingsResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("not a shell settings body: %v: %s", err, rec.Body)
		}
	}
	return rec.Code, s
}

func (f *authFixture) patchShellSettings(t *testing.T, cookie, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPatch, path: "/api/settings/shells", body: body, cookie: cookie, csrf: csrf})
}

func TestShellSettingsStartAtThirtyMinutesAndTenShells(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	if code, s := f.getShellSettings(t, cookie); code != http.StatusOK || s.IdleMinutes != 30 || s.MaxPerUser != 10 {
		t.Fatalf("got %d %+v, want 200 with 30 and 10", code, s)
	}
}

func TestPatchShellSettingsChangesTheLimitsAtOnceAndAuditsThem(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	rec := f.patchShellSettings(t, cookie, csrf, `{"idleMinutes":45,"maxPerUser":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got shellSettingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.IdleMinutes != 45 || got.MaxPerUser != 3 {
		t.Fatalf("body = %s (%v), want 45 and 3", rec.Body, err)
	}
	if f.inst.ShellIdle() != 45*time.Minute || f.inst.ShellCap() != 3 {
		t.Fatalf("cached = %v and %d, want 45m and 3 at once", f.inst.ShellIdle(), f.inst.ShellCap())
	}
	entries := f.audits(t, "settings.updated")
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].Result != "success" {
		t.Fatalf("audit = %+v, want one success entry by id-admin", entries)
	}
	d := f.auditDetails(t, "settings.updated")[0]
	if d["shell_idle_minutes_from"] != "30" || d["shell_idle_minutes_to"] != "45" || d["shell_max_per_user_from"] != "10" || d["shell_max_per_user_to"] != "3" {
		t.Fatalf("details = %v, want both limits with their old and new value", d)
	}
}

func TestPatchShellSettingsChangesOnlyTheFieldSentAndANoOpIsNotAudited(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if rec := f.patchShellSettings(t, cookie, csrf, `{"maxPerUser":4}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if f.inst.ShellIdle() != 30*time.Minute || f.inst.ShellCap() != 4 {
		t.Fatalf("cached = %v and %d, want 30m and 4", f.inst.ShellIdle(), f.inst.ShellCap())
	}
	if rec := f.patchShellSettings(t, cookie, csrf, `{"maxPerUser":4,"idleMinutes":30}`); rec.Code != http.StatusOK {
		t.Fatalf("no-op status = %d: %s", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "settings.updated")); n != 1 {
		t.Fatalf("%d settings.updated entries, want 1: a PATCH that changes nothing is not audited", n)
	}
}

func TestPatchShellSettingsRefusesValuesOutsideTheirRangeAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	for _, body := range []string{`{"idleMinutes":4}`, `{"idleMinutes":241}`, `{"maxPerUser":0}`, `{"maxPerUser":51}`, `{"idleMinutes":60,"maxPerUser":51}`} {
		if rec := f.patchShellSettings(t, cookie, csrf, body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", body, rec.Code, rec.Body)
		}
	}
	if f.inst.ShellIdle() != 30*time.Minute || f.inst.ShellCap() != 10 {
		t.Fatalf("cached = %v and %d after refusals, want the defaults", f.inst.ShellIdle(), f.inst.ShellCap())
	}
	if n := len(f.audits(t, "settings.updated")); n != 0 {
		t.Fatalf("%d settings.updated entries after refusals, want none", n)
	}
}

func TestShellSettingsAreForTheInstanceAdministratorOnly(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false) // no role at all
	cookie, csrf := f.signIn(t, "viewer", false)
	if code, _ := f.getShellSettings(t, cookie); code != http.StatusForbidden {
		t.Fatalf("GET without settings.read: %d, want 403", code)
	}
	if rec := f.patchShellSettings(t, cookie, csrf, `{"maxPerUser":50}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH without settings.write: %d, want 403", rec.Code)
	}
	if f.inst.ShellCap() != 10 {
		t.Fatalf("cap = %d after a refused PATCH, want 10", f.inst.ShellCap())
	}
}
