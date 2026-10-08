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

// --- rename, delete, remove and the impact preview (plan 03-17 task 2) ---

type impactResp struct {
	Impact []struct {
		UserID          string `json:"userId"`
		Username        string `json:"username"`
		EnvironmentID   string `json:"environmentId"`
		EnvironmentName string `json:"environmentName"`
		Before          string `json:"before"`
		After           string `json:"after"`
	} `json:"impact"`
}

func decodeImpact(t *testing.T, rec *httptest.ResponseRecorder) impactResp {
	t.Helper()
	var out impactResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not an impact: %v: %s", err, rec.Body)
	}
	return out
}

func (f *authFixture) preview(t *testing.T, cookie, csrf, teamID, op, userID string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"op": op, "userId": userID})
	return f.teamCall(t, cookie, csrf, http.MethodPost, "/api/teams/"+teamID+"/impact", string(b))
}

func TestThePreviewEqualsWhatTheRemovalProducesAndTheUsersAccessMatchesItsAfterValue(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.addUser(t, "alice", false)
	f.addUser(t, "bob", false)
	f.addEnvRole(t, "alice", "env-a", "viewer")
	team := f.addTeam(t, "ops", "alice", "bob")
	f.addTeamRole(t, team, "env-a", "operator")

	rec := f.preview(t, cookie, csrf, team, "remove_member", "id-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	got := decodeImpact(t, rec)
	if len(got.Impact) != 1 || got.Impact[0].Username != "alice" || got.Impact[0].EnvironmentID != "env-a" ||
		got.Impact[0].Before != "operator" || got.Impact[0].After != "viewer" {
		t.Fatalf("preview = %+v", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM team_members WHERE team_id = ?`, team); n != 2 {
		t.Fatalf("members after the preview = %d, want 2: a preview changes nothing", n)
	}
	if n := len(f.audits(t, "team.member_removed")); n != 0 {
		t.Fatalf("the preview wrote %d audit entries", n)
	}

	rec = f.teamCall(t, cookie, csrf, http.MethodDelete, "/api/teams/"+team+"/members/id-alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if applied := decodeImpact(t, rec); len(applied.Impact) != 1 || applied.Impact[0] != got.Impact[0] {
		t.Fatalf("the applied impact %+v differs from the preview %+v", applied, got)
	}
	me, _ := f.getMe(t, "alice")
	a := me.Access["env-a"]
	if a.Role != got.Impact[0].After || len(a.Sources) != 1 || a.Sources[0].Kind != "direct" {
		t.Fatalf("alice's access = %+v, want %s from the direct grant only", a, got.Impact[0].After)
	}
	if bob, _ := f.getMe(t, "bob"); bob.Access["env-a"].Role != "operator" {
		t.Fatalf("bob lost access he keeps: %+v", bob.Access)
	}
	entries := f.audits(t, "team.member_removed")
	if len(entries) != 1 || entries[0].ActorID != "id-admin" {
		t.Fatalf("audit = %+v", entries)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'team.member_removed'`).Scan(&details); err != nil ||
		!strings.Contains(details, `"impact_count":"1"`) || !strings.Contains(details, `"impact":"alice: env-a operator→viewer"`) {
		t.Fatalf("details = %s (%v)", details, err)
	}
}

func TestDeletingATeamRemovesItsAccessAtOnceAndAuditsOnce(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.addUser(t, "alice", false)
	team := f.addTeam(t, "ops", "alice")
	f.addTeamRole(t, team, "env-a", "operator")
	if me, _ := f.getMe(t, "alice"); me.Access["env-a"].Role != "operator" {
		t.Fatalf("alice before = %+v", me.Access)
	}
	aliceCookie, _ := f.signIn(t, "alice", false)

	rec := f.preview(t, cookie, csrf, team, "delete", "")
	if got := decodeImpact(t, rec); rec.Code != http.StatusOK || len(got.Impact) != 1 || got.Impact[0].After != "none" {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	rec = f.teamCall(t, cookie, csrf, http.MethodDelete, "/api/teams/"+team, "")
	if rec.Code != http.StatusOK || len(decodeImpact(t, rec).Impact) != 1 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	// On her very next request alice no longer has the team's access.
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: aliceCookie})
	var me meResp
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK || len(me.Access) != 0 {
		t.Fatalf("alice after the delete: %d %s", rec.Code, rec.Body)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_id = ?`, team); n != 0 {
		t.Fatalf("%d assignments refer to the deleted team", n)
	}
	if n := len(f.audits(t, "team.deleted")); n != 1 {
		t.Fatalf("team.deleted entries = %d, want 1", n)
	}
	rec = f.teamCall(t, cookie, csrf, http.MethodDelete, "/api/teams/"+team, "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "team_not_found" {
		t.Fatalf("delete again: %d %s", rec.Code, rec.Body)
	}
}

func TestRemovingANonMemberAnswers404MemberNotFoundAndTheEmptyPreviewIsAnEmptyList(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.addUser(t, "alice", false)
	team := f.createTeamAPI(t, cookie, csrf, "ops")

	rec := f.teamCall(t, cookie, csrf, http.MethodDelete, "/api/teams/"+team.ID+"/members/id-alice", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "member_not_found" {
		t.Fatalf("remove a non-member: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.audits(t, "team.member_removed")); n != 0 {
		t.Fatalf("a refused removal wrote %d entries", n)
	}
	rec = f.preview(t, cookie, csrf, team.ID, "delete", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"impact":[]`) {
		t.Fatalf("an empty preview: %d %s, want an empty list and not null", rec.Code, rec.Body)
	}
	rec = f.preview(t, cookie, csrf, team.ID, "remove_member", "id-alice")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "member_not_found" {
		t.Fatalf("preview of a non-member: %d %s", rec.Code, rec.Body)
	}
	rec = f.preview(t, cookie, csrf, team.ID, "remove_member", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("remove_member with no user: %d %s, want 422", rec.Code, rec.Body)
	}
	rec = f.preview(t, cookie, csrf, team.ID, "explode", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown op: %d %s, want 422", rec.Code, rec.Body)
	}
	rec = f.preview(t, cookie, csrf, "no-team", "delete", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "team_not_found" {
		t.Fatalf("preview of an unknown team: %d %s", rec.Code, rec.Body)
	}
}

func TestRenamingATeamIsCheckedForStaleVersionsAndClashesAndAuditsTheOldName(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	team := f.createTeamAPI(t, cookie, csrf, "ops")
	f.createTeamAPI(t, cookie, csrf, "Dev")
	patch := func(id, name, updatedAt string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"name": name, "updatedAt": updatedAt})
		return f.teamCall(t, cookie, csrf, http.MethodPatch, "/api/teams/"+id, string(b))
	}

	rec := patch(team.ID, "dEV", team.UpdatedAt)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "team_name_taken" {
		t.Fatalf("clash: %d %s", rec.Code, rec.Body)
	}
	rec = patch(team.ID, "", team.UpdatedAt)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_team_name") {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	rec = patch("no-team", "x", team.UpdatedAt)
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "team_not_found" {
		t.Fatalf("unknown team: %d %s", rec.Code, rec.Body)
	}
	rec = patch(team.ID, "Ops", team.UpdatedAt)
	var renamed teamResp
	if err := json.Unmarshal(rec.Body.Bytes(), &renamed); err != nil || rec.Code != http.StatusOK || renamed.Name != "Ops" || renamed.UpdatedAt == team.UpdatedAt {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	rec = patch(team.ID, "Other", team.UpdatedAt)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stale_change" {
		t.Fatalf("stale: %d %s", rec.Code, rec.Body)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'team.renamed'`).Scan(&details); err != nil ||
		!strings.Contains(details, `"old_name":"ops"`) {
		t.Fatalf("details = %s (%v)", details, err)
	}
	if n := len(f.audits(t, "team.renamed")); n != 1 {
		t.Fatalf("team.renamed entries = %d, want 1", n)
	}
}

func TestEnvAdminsCannotRenameDeleteOrPreviewTeams(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addAllEnvAdmin(t, "envadmin")
	team := f.addTeam(t, "ops", "envadmin")
	f.addTeamRole(t, team, "env-a", "admin")
	cookie, csrf := f.signIn(t, "envadmin", false)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/teams/" + team, `{"name":"mine","updatedAt":"2026-01-01T00:00:00Z"}`},
		{http.MethodDelete, "/api/teams/" + team, ""},
		{http.MethodDelete, "/api/teams/" + team + "/members/id-envadmin", ""},
		{http.MethodPost, "/api/teams/" + team + "/impact", `{"op":"delete"}`},
	} {
		if rec := f.teamCall(t, cookie, csrf, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: %d %s, want 403", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM teams`); n != 1 {
		t.Fatalf("teams = %d: the team went", n)
	}
}
