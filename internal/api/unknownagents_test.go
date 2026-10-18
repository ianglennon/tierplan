// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/config"
)

type unknownAgentsResp struct {
	Items []struct {
		EnvID       string `json:"envId"`
		EnvName     string `json:"envName"`
		Node        string `json:"node"`
		IP          string `json:"ip"`
		LastAttempt string `json:"lastAttempt"`
	} `json:"items"`
}

func (f *authFixture) addAgentEnv(t *testing.T, id, name string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES (?, 'agent', ?, '', 1, 1, 1)`, id, name); err != nil {
		t.Fatal(err)
	}
}

func (f *authFixture) unknownAgents(t *testing.T, cookie string) (int, unknownAgentsResp) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments/unknown-agents", cookie: cookie})
	var got unknownAgentsResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, got
}

func TestAnInstanceAdminListsTheUnknownAgentsWithTheirEnvironmentNewestFirst(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	f.addAgentEnv(t, "env-a", "Production")
	f.addAgentEnv(t, "env-b", "Staging")
	now := f.clk.now()
	f.unknown.Record(agents.UnknownAgent{EnvID: "env-a", Node: "agent-1", IP: "198.51.100.1", Key: "k1", LastAttempt: now.Add(-2 * time.Hour)})
	f.unknown.Record(agents.UnknownAgent{EnvID: "env-b", Node: "agent-2", IP: "198.51.100.2", Key: "k2", LastAttempt: now.Add(-time.Hour)})
	// Its environment is gone: not shown.
	f.unknown.Record(agents.UnknownAgent{EnvID: "env-gone", Node: "agent-3", IP: "198.51.100.3", Key: "k3", LastAttempt: now})
	// Older than a day: not shown.
	f.unknown.Record(agents.UnknownAgent{EnvID: "env-a", Node: "agent-4", IP: "198.51.100.4", Key: "k4", LastAttempt: now.Add(-25 * time.Hour)})

	code, got := f.unknownAgents(t, cookie)
	if code != http.StatusOK || len(got.Items) != 2 {
		t.Fatalf("status %d, items %+v; want two", code, got.Items)
	}
	first, second := got.Items[0], got.Items[1]
	if first.EnvID != "env-b" || first.EnvName != "Staging" || first.Node != "agent-2" || first.IP != "198.51.100.2" ||
		first.LastAttempt != now.Add(-time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("first = %+v", first)
	}
	if second.EnvID != "env-a" || second.EnvName != "Production" || second.Node != "agent-1" {
		t.Errorf("second = %+v", second)
	}
}

func TestTheUnknownAgentsListIsEmptyWithNoneAndRefusedToAnyoneButAnInstanceAdmin(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	adminCookie, _ := f.signIn(t, "admin", false)
	f.addAgentEnv(t, "env-a", "Production")
	if code, got := f.unknownAgents(t, adminCookie); code != http.StatusOK || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("with none: %d %+v, want 200 and an empty list", code, got)
	}

	f.unknown.Record(agents.UnknownAgent{EnvID: "env-a", Node: "agent-1", Key: "k", LastAttempt: f.clk.now()})
	f.addUser(t, "envadmin", false)
	f.addTeamRole(t, f.addTeam(t, "ops", "envadmin"), "env-a", "admin")
	cookie, _ := f.signIn(t, "envadmin", false)
	if code, _ := f.unknownAgents(t, cookie); code != http.StatusForbidden {
		t.Errorf("an environment admin: %d, want 403", code)
	}
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments/unknown-agents"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}
