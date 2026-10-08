// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
)

// The grant routes (plan 03-19, AUTH-06, AUTH-07, D-05, D-06, D-15 to D-17,
// D-25). Negative cases are mandatory (D-81).

type grantResp struct {
	ID          string   `json:"id"`
	SubjectType string   `json:"subjectType"`
	SubjectID   string   `json:"subjectId"`
	Scope       string   `json:"scope"`
	EnvID       string   `json:"envId"`
	EnvName     string   `json:"envName"`
	Role        string   `json:"role"`
	Extras      []string `json:"extras"`
	CreatedAt   string   `json:"createdAt"`
}

type userAccessResp struct {
	InstanceAdmin bool `json:"instanceAdmin"`
	Environments  []struct {
		EnvID   string   `json:"envId"`
		EnvName string   `json:"envName"`
		Role    string   `json:"role"`
		Extras  []string `json:"extras"`
		Sources []struct {
			Kind     string `json:"kind"`
			TeamID   string `json:"teamId"`
			TeamName string `json:"teamName"`
		} `json:"sources"`
		Inherited bool `json:"inherited"`
	} `json:"environments"`
	Grants []grantResp `json:"grants"`
}

// twoEnvironments makes environment A (the local host, which answers its
// summary) and environment B (an agent environment) as the signed-in admin.
func (f *envFix) twoEnvironments(t *testing.T) (a, b envBody) {
	t.Helper()
	f.inst.Apply("Bayplan", "https://example.com/bayplan")
	a = f.add(t)
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated {
		t.Fatalf("add the agent environment: %d %s", rec.code, rec.body)
	}
	return a, envBody{ID: got.ID}
}

func (f *authFixture) putGrant(t *testing.T, cookie, csrf, subject, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPut, path: "/api/" + subject + "/" + id + "/grants", body: body, cookie: cookie, csrf: csrf})
}

func decodeRec[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

func TestAnInstanceAdminGrantsAUserViewerOnEnvironmentAWhoThenReadsAAndGetsNotFoundOnB(t *testing.T) {
	f := newEnvFix(t, richEngine())
	a, b := f.twoEnvironments(t)
	alice := f.addUser(t, "alice", false)

	rec := f.putGrant(t, f.cookie, f.csrf, "users", alice, `{"scope":"env","envId":"`+a.ID+`","role":"viewer","extras":[]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT grant: %d %s, want 201", rec.Code, rec.Body)
	}
	g := decodeRec[grantResp](t, rec)
	if g.ID == "" || g.Role != "viewer" || g.Scope != "env" || g.EnvID != a.ID || g.SubjectType != "user" || g.SubjectID != alice || len(g.Extras) != 0 {
		t.Fatalf("grant = %+v", g)
	}

	cookie, _ := f.signIn(t, "alice", false)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + a.ID + "/summary", cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("alice on A: %d %s, want 200", rec.Code, rec.Body)
	}
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + b.ID + "/summary", cookie: cookie})
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "environment_not_found" {
		t.Fatalf("alice on B: %d %s, want 404 environment_not_found", rec.Code, rec.Body)
	}

	entries := f.auditDetails(t, audit.ActionAccessGranted)
	if len(entries) != 1 {
		t.Fatalf("access.granted entries = %d, want 1", len(entries))
	}
	d := entries[0]
	if d["subject_type"] != "user" || d["subject"] != "alice" || d["role"] != "viewer" || d["scope"] != a.ID || d["extras"] != "" {
		t.Fatalf("details = %v", d)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'access.granted' AND env_id = ?`, a.ID); got != 1 {
		t.Fatalf("access.granted entries carrying env_id A = %d, want 1", got)
	}
}

func TestTheUserAccessViewListsTheEffectiveRoleTheSourcesAndTheDirectGrants(t *testing.T) {
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	alice := f.addUser(t, "alice", false)
	f.putGrant(t, f.cookie, f.csrf, "users", alice, `{"scope":"env","envId":"`+a.ID+`","role":"viewer"}`)

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/users/" + alice + "/access", cookie: f.cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET access: %d %s", rec.Code, rec.Body)
	}
	got := decodeRec[userAccessResp](t, rec)
	if got.InstanceAdmin || len(got.Environments) != 1 || len(got.Grants) != 1 {
		t.Fatalf("access = %s", rec.Body)
	}
	e := got.Environments[0]
	if e.EnvID != a.ID || e.EnvName == "" || e.Role != "viewer" || e.Inherited || e.Extras == nil || len(e.Extras) != 0 ||
		len(e.Sources) != 1 || e.Sources[0].Kind != "direct" {
		t.Fatalf("environment row = %+v", e)
	}
	if g := got.Grants[0]; g.Role != "viewer" || g.EnvID != a.ID || g.Scope != "env" || g.EnvName != e.EnvName {
		t.Fatalf("grant = %+v", g)
	}

	// An instance admin has the flag and no rows.
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/users/id-admin/access", cookie: f.cookie})
	if admin := decodeRec[userAccessResp](t, rec); !admin.InstanceAdmin || len(admin.Environments) != 0 {
		t.Fatalf("an instance admin's access = %s", rec.Body)
	}
}

func TestOnlyAnInstanceAdminMayReadOrChangeGrantsAndNothingIsWritten(t *testing.T) {
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	alice := f.addUser(t, "alice", false)
	team := f.createTeamAPI(t, f.cookie, f.csrf, "ops")
	f.addAllEnvAdmin(t, "envadmin")
	f.addUser(t, "nobody", false)

	body := `{"scope":"env","envId":"` + a.ID + `","role":"operator"}`
	for _, name := range []string{"envadmin", "nobody"} {
		cookie, csrf := f.signIn(t, name, false)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/users/" + alice + "/access", ""},
			{http.MethodPut, "/api/users/" + alice + "/grants", body},
			{http.MethodGet, "/api/teams/" + team.ID + "/grants", ""},
			{http.MethodPut, "/api/teams/" + team.ID + "/grants", body},
		} {
			rec := f.call(t, authReq{method: c.method, path: c.path, body: c.body, cookie: cookie, csrf: csrf})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s as %s: %d %s, want 403", c.method, c.path, name, rec.Code, rec.Body)
			}
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_id IN (?, ?)`, alice, team.ID); n != 0 {
		t.Fatalf("grants written by refused callers = %d, want none", n)
	}
}

func TestAGrantRouteAnswers404ForAnUnknownUserTeamOrEnvironmentAnd422ForABadRole(t *testing.T) {
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	alice := f.addUser(t, "alice", false)

	for _, c := range []struct {
		subject, id, body string
		code              int
		problem           string
	}{
		{"users", "id-ghost", `{"scope":"env","envId":"` + a.ID + `","role":"viewer"}`, http.StatusNotFound, "user_not_found"},
		{"teams", "no-team", `{"scope":"env","envId":"` + a.ID + `","role":"viewer"}`, http.StatusNotFound, "team_not_found"},
		{"users", alice, `{"scope":"env","envId":"env-gone","role":"viewer"}`, http.StatusNotFound, "environment_not_found"},
		{"users", alice, `{"scope":"env","envId":"` + a.ID + `","role":"owner"}`, http.StatusUnprocessableEntity, ""},
		{"users", alice, `{"scope":"galaxy","role":"viewer"}`, http.StatusUnprocessableEntity, ""},
		{"users", alice, `{"scope":"env","role":"viewer"}`, http.StatusUnprocessableEntity, "validation_failed"},
	} {
		rec := f.putGrant(t, f.cookie, f.csrf, c.subject, c.id, c.body)
		if rec.Code != c.code || (c.problem != "" && problemType(t, rec) != c.problem) {
			t.Fatalf("PUT %s/%s %s: %d %s, want %d %s", c.subject, c.id, c.body, rec.Code, rec.Body, c.code, c.problem)
		}
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/users/id-ghost/access", cookie: f.cookie}); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("access of an unknown user: %d %s", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/teams/no-team/grants", cookie: f.cookie}); rec.Code != http.StatusNotFound || problemType(t, rec) != "team_not_found" {
		t.Fatalf("grants of an unknown team: %d %s", rec.Code, rec.Body)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments`); n != 0 {
		t.Fatalf("grants = %d, want none after the refusals", n)
	}
}

func TestATeamGrantReachesEveryMemberAndTheAllEnvironmentsScopeReachesEveryEnvironment(t *testing.T) {
	f := newEnvFix(t, richEngine())
	a, b := f.twoEnvironments(t)
	alice := f.addUser(t, "alice", false)
	team := f.createTeamAPI(t, f.cookie, f.csrf, "ops")
	if rec := f.addMembersAPI(t, f.cookie, f.csrf, team.ID, alice); rec.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body)
	}

	rec := f.putGrant(t, f.cookie, f.csrf, "teams", team.ID, `{"scope":"env","envId":"`+a.ID+`","role":"operator","extras":["containers.exec"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT team grant: %d %s", rec.Code, rec.Body)
	}
	if rec := f.putGrant(t, f.cookie, f.csrf, "users", alice, `{"scope":"all","role":"viewer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("PUT all-environments grant: %d %s", rec.Code, rec.Body)
	}

	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/users/" + alice + "/access", cookie: f.cookie})
	got := decodeRec[userAccessResp](t, rec)
	if len(got.Environments) != 2 || len(got.Grants) != 1 || got.Grants[0].Scope != "all" {
		t.Fatalf("access = %s", rec.Body)
	}
	byID := map[string]int{got.Environments[0].EnvID: 0, got.Environments[1].EnvID: 1}
	ea, eb := got.Environments[byID[a.ID]], got.Environments[byID[b.ID]]
	if ea.Role != "operator" || ea.Inherited || !slices.Equal(ea.Extras, []string{"containers.exec"}) || len(ea.Sources) != 2 ||
		ea.Sources[0].Kind != "all" || ea.Sources[1].Kind != "team" || ea.Sources[1].TeamName != "ops" {
		t.Fatalf("environment A = %+v, want Operator with the container shell from All environments and Team ops", ea)
	}
	if eb.Role != "viewer" || !eb.Inherited || len(eb.Sources) != 1 || eb.Sources[0].Kind != "all" {
		t.Fatalf("environment B = %+v, want an inherited Viewer through All environments", eb)
	}

	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/teams/" + team.ID + "/grants", cookie: f.cookie})
	if list := decodeRec[[]grantResp](t, rec); rec.Code != http.StatusOK || len(list) != 1 || list[0].Role != "operator" || list[0].SubjectType != "team" {
		t.Fatalf("team grants = %d %s", rec.Code, rec.Body)
	}
}
