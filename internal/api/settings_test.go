// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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

	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
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

	// The session cookie is always the __Host- one, so the change of origin
	// leaves the session valid; only the Origin guard moves.
	ack := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/warnings/key-backup/ack", strings.NewReader(""))
		req.Header.Set("Origin", origin)
		req.Header.Set("X-CSRF-Token", csrf)
		req.RemoteAddr = authIP + ":40000"
		req.AddCookie(&http.Cookie{Name: auth.CookieName(auth.CookieScope{}), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := ack("https://old.example"); code != http.StatusForbidden {
		t.Fatalf("a POST from the old origin: %d, want 403", code)
	}
	if code := ack("https://new.example"); code != http.StatusNoContent {
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
		{"relative url", map[string]string{"publicUrl": "/tierplan"}, "body.publicUrl", "invalid_public_url"},
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
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://example.com/tierplan"}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if _, s := f.getSettings(t, cookie); s.AgentURL != "" || s.AgentEndpoint != "wss://example.com/tierplan/agent/connect" {
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

	rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://tierplan.lan:7480/"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got settingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.AgentURL != "https://tierplan.lan:7480" || got.AgentEndpoint != "wss://tierplan.lan:7480/agent/connect" {
		t.Fatalf("body = %s (%v), want the normalised agent URL and its endpoint", rec.Body, err)
	}
	if f.inst.AgentURL() != "https://tierplan.lan:7480" || f.inst.AgentEndpoint() != "wss://tierplan.lan:7480/agent/connect" {
		t.Fatalf("the cached agent URL = %q, endpoint %q, want them at once", f.inst.AgentURL(), f.inst.AgentEndpoint())
	}
	var stored string
	if err := f.st.Read.QueryRow(`SELECT value FROM settings WHERE key = 'agent_url'`).Scan(&stored); err != nil || stored != "https://tierplan.lan:7480" {
		t.Fatalf("stored = %q, %v", stored, err)
	}
	d := f.auditDetails(t, "settings.updated")
	last := d[len(d)-1]
	if last["agent_url_from"] != "" || last["agent_url_to"] != "https://tierplan.lan:7480" {
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
	if last := d[len(d)-1]; last["agent_url_from"] != "https://tierplan.lan:7480" || last["agent_url_to"] != "" {
		t.Fatalf("details = %v, want the cleared agent URL audited", last)
	}
}

func TestPatchSettingsRefusesAnInvalidAgentURL(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	for _, bad := range []string{"/agent", "tierplan.lan", "ftp://tierplan.lan", "https://user@tierplan.lan", "https://tierplan.lan/api"} {
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
	f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://tierplan.lan:7480"})
	before := len(f.audits(t, "settings.updated"))
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://TIERPLAN.lan:7480/"}); rec.Code != http.StatusOK {
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

	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://tierplan.lan:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := fake.pushed()
	if len(got) != base+1 || len(got[base]) != 1 || got[base][0] != "wss://tierplan.lan:7480/agent/connect" {
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

	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://new.example/tierplan"})
	got := fake.pushed()
	if len(got) != base+1 || got[base][0] != "wss://new.example/tierplan/agent/connect" {
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
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://tierplan.lan:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

// assertHTTPSOnlyReason checks a 422 says why an http URL was refused and does
// not repeat the submitted value (D-07, T-13.1-43).
func assertHTTPSOnlyReason(t *testing.T, rec *httptest.ResponseRecorder, submitted string) {
	t.Helper()
	body := rec.Body.String()
	if !strings.Contains(body, "HTTPS only") {
		t.Errorf("%q: body = %s, want the reason: TierPlan serves HTTPS only", submitted, body)
	}
	if strings.Contains(body, submitted) {
		t.Errorf("%q: body = %s echoes the submitted URL", submitted, body)
	}
}

func TestPatchSettingsRefusesAnHTTPAgentURLAndPublicURLForEveryHost(t *testing.T) {
	t.Parallel()
	fake := &fakeSettingsAgents{connected: map[string]bool{"a": true}}
	f, cookie, csrf := agentsFixture(t, config.Config{}, fake)
	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://agent.internal:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	audits, pushes := len(f.audits(t, "settings.updated")), len(fake.pushed())

	for _, bad := range []string{"http://10.0.0.5:7480", "http://tierplan.lan:7480", "http://[2001:db8::1]:7480", "http://127.0.0.1:7480", "http://localhost:7480", "http://[::1]:7480"} {
		rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Changed", "agentUrl": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%q: status = %d: %s, want 422", bad, rec.Code, rec.Body)
		}
		p := decodeProblem(t, rec)
		if p.Type != "validation_failed" || len(p.Errors) != 1 || p.Errors[0].Location != "body.agentUrl" || p.Errors[0].Message != "invalid_agent_url" {
			t.Fatalf("%q: problem = %+v, want invalid_agent_url at body.agentUrl", bad, p)
		}
		assertHTTPSOnlyReason(t, rec, bad)
		// The public URL runs the same rule, so an http one is refused too.
		rec = f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Changed", "publicUrl": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("public URL %q: status = %d: %s, want 422", bad, rec.Code, rec.Body)
		}
		p = decodeProblem(t, rec)
		if p.Type != "validation_failed" || len(p.Errors) != 1 || p.Errors[0].Location != "body.publicUrl" || p.Errors[0].Message != "invalid_public_url" {
			t.Fatalf("public URL %q: problem = %+v, want invalid_public_url at body.publicUrl", bad, p)
		}
		assertHTTPSOnlyReason(t, rec, bad)
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

func TestPatchSettingsAcceptsAnHTTPSAgentURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label     string
		publicURL string // applied before the request, "" for none
		agentURL  string // applied before the request, "" for none
		body      map[string]string
	}{
		{"https for another host", "", "", map[string]string{"agentUrl": "https://10.0.0.5:7480"}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			f := newAuthFixture(t, config.Config{})
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
			if want := "wss://10.0.0.5:7480/agent/connect"; s.AgentEndpoint != want {
				t.Errorf("agentEndpoint = %q in the answer, want %q", s.AgentEndpoint, want)
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

// Stack history retention (plan 08-06, STK-08, D-01): how many revisions of each stack are kept.

type stackHistorySettingsResp struct {
	Kept   int `json:"kept"`
	Min    int `json:"min"`
	Max    int `json:"max"`
	Pruned int `json:"pruned"`
}

type stackHistoryPreviewResp struct {
	Revisions int `json:"revisions"`
	Stacks    int `json:"stacks"`
}

func (f *authFixture) getStackHistorySettings(t *testing.T, cookie string) (int, stackHistorySettingsResp) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/settings/stack-history", cookie: cookie})
	var s stackHistorySettingsResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("not a stack history settings body: %v: %s", err, rec.Body)
		}
	}
	return rec.Code, s
}

func (f *authFixture) patchStackHistorySettings(t *testing.T, cookie, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPatch, path: "/api/settings/stack-history", body: body, cookie: cookie, csrf: csrf})
}

func (f *authFixture) previewStackHistory(t *testing.T, cookie, keep string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodGet, path: "/api/settings/stack-history/preview?keep=" + keep, cookie: cookie})
}

// seedRevisions gives a stack of its own n good revisions, each with a sealed variable, and returns how many
// revisions and variables the store holds for it.
func (f *authFixture) seedRevisions(t *testing.T, name string, n int) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT OR IGNORE INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'e', '', 1, 1, 1)`,
		`INSERT INTO stacks (id, env_id, name, target, source_kind, source_ref, created_by, created_at) VALUES ('s-` + name + `', 'env-1', '` + name + `', 'swarm', 'inline', '', 'ada', 1)`,
	} {
		if _, err := f.st.Write.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i := 1; i <= n; i++ {
		id := "r-" + name + "-" + itoaT(int64(i))
		if _, err := f.st.Write.ExecContext(ctx, `INSERT INTO stack_snapshots (id, stack_id, kind, seq, outcome, created_by, created_at) VALUES (?, ?, 'revision', ?, 'good', 'ada', ?)`, id, "s-"+name, i, i); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.Write.ExecContext(ctx, `INSERT INTO stack_snapshot_vars (snapshot_id, name, value_sealed, secret) VALUES (?, 'K', 'sealed', 1)`, id); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *authFixture) countStored(t *testing.T) (revisions, vars int) {
	t.Helper()
	row := f.st.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM stack_snapshots WHERE kind = 'revision'), (SELECT COUNT(*) FROM stack_snapshot_vars)`)
	if err := row.Scan(&revisions, &vars); err != nil {
		t.Fatal(err)
	}
	return revisions, vars
}

func TestStackHistorySettingsStartAtTwentyWithTheirBounds(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	code, s := f.getStackHistorySettings(t, cookie)
	if code != http.StatusOK || s.Kept != 20 || s.Min != 5 || s.Max != 200 {
		t.Fatalf("got %d %+v, want 200 with kept 20, min 5, max 200", code, s)
	}
}

func TestStackHistorySettingsAreForTheInstanceAdministratorOnly(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false) // no role at all
	cookie, csrf := f.signIn(t, "viewer", false)
	if code, _ := f.getStackHistorySettings(t, cookie); code != http.StatusForbidden {
		t.Fatalf("GET without settings.read: %d, want 403", code)
	}
	if rec := f.patchStackHistorySettings(t, cookie, csrf, `{"kept":50}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH without settings.write: %d, want 403", rec.Code)
	}
	if rec := f.previewStackHistory(t, cookie, "5"); rec.Code != http.StatusForbidden {
		t.Fatalf("preview without settings.write: %d, want 403", rec.Code)
	}
	if f.inst.StackRevisionsKept() != 20 {
		t.Fatalf("kept = %d after a refused PATCH, want 20", f.inst.StackRevisionsKept())
	}
}

func TestPatchStackHistorySettingsRefusesValuesOutsideTheRangeAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.seedRevisions(t, "web", 8)
	for _, body := range []string{`{"kept":3}`, `{"kept":4}`, `{"kept":201}`, `{"kept":0}`, `{"kept":-5}`, `{}`} {
		if rec := f.patchStackHistorySettings(t, cookie, csrf, body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", body, rec.Code, rec.Body)
		}
	}
	if f.inst.StackRevisionsKept() != 20 {
		t.Fatalf("kept = %d after refusals, want 20", f.inst.StackRevisionsKept())
	}
	if n, _ := f.countStored(t); n != 8 {
		t.Fatalf("%d revisions after refusals, want all 8", n)
	}
	if n := len(f.audits(t, "settings.updated")); n != 0 {
		t.Fatalf("%d settings.updated entries after refusals, want none", n)
	}
}

func TestPatchStackHistorySettingsToTheCurrentValueWritesNoAuditRow(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	rec := f.patchStackHistorySettings(t, cookie, csrf, `{"kept":20}`)
	var got stackHistorySettingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.Kept != 20 || got.Pruned != 0 {
		t.Fatalf("status %d body %s (%v), want 200 with kept 20 and nothing pruned", rec.Code, rec.Body, err)
	}
	if n := len(f.audits(t, "settings.updated")); n != 0 {
		t.Fatalf("%d settings.updated entries, want none: a PATCH that changes nothing is not audited", n)
	}
}

func TestPatchStackHistorySettingsLoweringAuditsAndPrunesEveryStack(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.seedRevisions(t, "web", 8) // 3 beyond 5
	f.seedRevisions(t, "db", 6)  // 1 beyond 5
	f.seedRevisions(t, "few", 3) // none

	rec := f.patchStackHistorySettings(t, cookie, csrf, `{"kept":5}`)
	var got stackHistorySettingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.Kept != 5 || got.Pruned != 4 {
		t.Fatalf("status %d body %s (%v), want 200 with kept 5 and 4 pruned", rec.Code, rec.Body, err)
	}
	if f.inst.StackRevisionsKept() != 5 {
		t.Fatalf("cached = %d, want 5 at once", f.inst.StackRevisionsKept())
	}
	// The sealed variables of the pruned revisions went with them.
	if revs, vars := f.countStored(t); revs != 13 || vars != 13 {
		t.Fatalf("%d revisions and %d variables left, want 13 and 13 (5 + 5 + 3)", revs, vars)
	}
	entries := f.audits(t, "settings.updated")
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].Result != "success" {
		t.Fatalf("audit = %+v, want one success entry by id-admin", entries)
	}
	d := f.auditDetails(t, "settings.updated")[0]
	if d["stack_revisions_kept_from"] != "20" || d["stack_revisions_kept_to"] != "5" || d["pruned"] != "4" {
		t.Fatalf("details = %v, want from 20, to 5 and 4 pruned", d)
	}
	// It is stored: the next start reads 5.
	if again, err := instance.Load(context.Background(), f.st, config.Config{}); err != nil || again.StackRevisionsKept() != 5 {
		t.Fatalf("reloaded = %v, %v, want 5", again, err)
	}
}

func TestPatchStackHistorySettingsRaisingTheLimitPrunesNothing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.seedRevisions(t, "web", 8)
	rec := f.patchStackHistorySettings(t, cookie, csrf, `{"kept":200}`)
	var got stackHistorySettingsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.Kept != 200 || got.Pruned != 0 {
		t.Fatalf("status %d body %s (%v), want 200 with kept 200 and nothing pruned", rec.Code, rec.Body, err)
	}
	if n, _ := f.countStored(t); n != 8 {
		t.Fatalf("%d revisions after raising the limit, want all 8", n)
	}
	d := f.auditDetails(t, "settings.updated")[0]
	if d["stack_revisions_kept_from"] != "20" || d["stack_revisions_kept_to"] != "200" || d["pruned"] != "0" {
		t.Fatalf("details = %v, want from 20, to 200 and 0 pruned", d)
	}
}

func TestStackHistorySettingsPreviewCountsWhatALowerLimitWouldDelete(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	f.seedRevisions(t, "web", 8)
	f.seedRevisions(t, "db", 6)

	rec := f.previewStackHistory(t, cookie, "5")
	var got stackHistoryPreviewResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.Revisions != 4 || got.Stacks != 2 {
		t.Fatalf("status %d body %s (%v), want 200 with 4 revisions in 2 stacks", rec.Code, rec.Body, err)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	rec = f.previewStackHistory(t, cookie, "20")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil || got.Revisions != 0 || got.Stacks != 0 {
		t.Fatalf("status %d body %s (%v), want nothing to delete at the current limit", rec.Code, rec.Body, err)
	}
	for _, keep := range []string{"4", "201", "x", ""} {
		if rec := f.previewStackHistory(t, cookie, keep); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("keep=%q: status = %d, want 422", keep, rec.Code)
		}
	}
	// A preview only reads.
	if n, _ := f.countStored(t); n != 14 {
		t.Fatalf("%d revisions after previews, want all 14", n)
	}
	if n := len(f.audits(t, "settings.updated")); n != 0 {
		t.Fatalf("%d settings.updated entries after previews, want none", n)
	}
}
