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

// The team routes (plan 03-17, AUTH-05, D-11, D-25). Negative cases are
// mandatory (D-81).

type teamResp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MemberCount int    `json:"memberCount"`
	GrantCount  int    `json:"grantCount"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

type teamMemberResp struct {
	UserID        string `json:"userId"`
	Username      string `json:"username"`
	InstanceAdmin bool   `json:"instanceAdmin"`
	Status        string `json:"status"`
}

type teamDetailResp struct {
	teamResp
	Members []teamMemberResp `json:"members"`
}

func (f *authFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *authFixture) teamCall(t *testing.T, cookie, csrf, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: method, path: path, body: body, cookie: cookie, csrf: csrf})
}

func (f *authFixture) createTeamAPI(t *testing.T, cookie, csrf, name string) teamResp {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"name": name})
	rec := f.teamCall(t, cookie, csrf, http.MethodPost, "/api/teams", string(b))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create team %q: %d %s", name, rec.Code, rec.Body)
	}
	var out teamResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *authFixture) addMembersAPI(t *testing.T, cookie, csrf, teamID string, userIDs ...string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string][]string{"userIds": userIDs})
	return f.teamCall(t, cookie, csrf, http.MethodPost, "/api/teams/"+teamID+"/members", string(b))
}

func TestAnInstanceAdminCreatesATeamAddsAUserAndBothPagesShowIt(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	alice := f.addUser(t, "alice", false)

	team := f.createTeamAPI(t, cookie, csrf, "ops")
	if team.Name != "ops" || team.MemberCount != 0 || team.GrantCount != 0 || team.ID == "" || team.UpdatedAt == "" {
		t.Fatalf("team = %+v", team)
	}
	rec := f.teamCall(t, cookie, "", http.MethodGet, "/api/teams", "")
	var list []teamResp
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list) != 1 || list[0].ID != team.ID {
		t.Fatalf("list = %d %s (%v)", rec.Code, rec.Body, err)
	}

	rec = f.addMembersAPI(t, cookie, csrf, team.ID, alice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"added":1`) {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body)
	}
	rec = f.teamCall(t, cookie, "", http.MethodGet, "/api/teams/"+team.ID, "")
	var d teamDetailResp
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get team: %d %s (%v)", rec.Code, rec.Body, err)
	}
	if d.MemberCount != 1 || len(d.Members) != 1 || d.Members[0].UserID != alice || d.Members[0].Username != "alice" || d.Members[0].Status != "active" || d.Members[0].InstanceAdmin {
		t.Fatalf("detail = %+v", d)
	}
	if n := len(f.audits(t, "team.created")); n != 1 {
		t.Fatalf("team.created entries = %d, want 1", n)
	}
	if n := len(f.audits(t, "team.member_added")); n != 1 {
		t.Fatalf("team.member_added entries = %d, want 1", n)
	}

	rec = f.userAction(t, cookie, "", http.MethodGet, alice, "", "")
	var raw struct {
		TeamCount int `json:"teamCount"`
		Teams     []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || raw.TeamCount != 1 || len(raw.Teams) != 1 ||
		raw.Teams[0].ID != team.ID || raw.Teams[0].Name != "ops" {
		t.Fatalf("the user page = %s (%v), want the team ops", rec.Body, err)
	}
}

func TestOnlyAnInstanceAdminMayManageTeams(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	adminCookie, adminCSRF := f.signIn(t, "admin", false)
	team := f.createTeamAPI(t, adminCookie, adminCSRF, "ops")

	// An environment admin, an operator and a user with nothing are all refused.
	f.addAllEnvAdmin(t, "envadmin")
	f.addUser(t, "nobody", false)
	for _, name := range []string{"envadmin", "nobody"} {
		cookie, csrf := f.signIn(t, name, false)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/teams", ""},
			{http.MethodPost, "/api/teams", `{"name":"mine"}`},
			{http.MethodGet, "/api/teams/" + team.ID, ""},
			{http.MethodPost, "/api/teams/" + team.ID + "/members", `{"userIds":["id-` + name + `"]}`},
		} {
			rec := f.teamCall(t, cookie, csrf, c.method, c.path, c.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s %s as %s: %d %s, want 403", c.method, c.path, c.body, name, rec.Code, rec.Body)
			}
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM teams`); n != 1 {
		t.Fatalf("teams = %d, want only the admin's", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM team_members`); n != 0 {
		t.Fatalf("members = %d, want none: a refused user cannot join themselves", n)
	}
}

func TestCreatingATeamValidatesTheNameAndRefusesADuplicateIgnoringCase(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.createTeamAPI(t, cookie, csrf, "Ops")

	rec := f.teamCall(t, cookie, csrf, http.MethodPost, "/api/teams", `{"name":"oPS"}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "team_name_taken" {
		t.Fatalf("duplicate: %d %s, want 409 team_name_taken", rec.Code, rec.Body)
	}
	for _, name := range []string{"", "   ", strings.Repeat("x", 65)} {
		b, _ := json.Marshal(map[string]string{"name": name})
		rec := f.teamCall(t, cookie, csrf, http.MethodPost, "/api/teams", string(b))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_team_name") {
			t.Fatalf("name %q: %d %s, want 422 invalid_team_name", name, rec.Code, rec.Body)
		}
	}
}

func TestAddingMembersAnswers404ForAnUnknownTeamOrUserAndAddsNothing(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	alice := f.addUser(t, "alice", false)
	team := f.createTeamAPI(t, cookie, csrf, "ops")

	rec := f.addMembersAPI(t, cookie, csrf, "no-team", alice)
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "team_not_found" {
		t.Fatalf("unknown team: %d %s", rec.Code, rec.Body)
	}
	rec = f.addMembersAPI(t, cookie, csrf, team.ID, alice, "id-nobody")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("unknown user: %d %s", rec.Code, rec.Body)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM team_members`); n != 0 {
		t.Fatalf("members = %d, want none", n)
	}
	if rec := f.teamCall(t, cookie, "", http.MethodGet, "/api/teams/no-team", ""); rec.Code != http.StatusNotFound || problemType(t, rec) != "team_not_found" {
		t.Fatalf("get unknown team: %d %s", rec.Code, rec.Body)
	}
	rec = f.teamCall(t, cookie, csrf, http.MethodPost, "/api/teams/"+team.ID+"/members", `{"userIds":[]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an empty list: %d %s, want 422", rec.Code, rec.Body)
	}
}
