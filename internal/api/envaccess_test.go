// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
)

// The environment Access tab's routes (plan 03-20, AUTH-06, D-06, D-08, D-12,
// D-25). Negative cases are mandatory (D-81).

type envAccessResp struct {
	Teams []struct {
		TeamID  string   `json:"teamId"`
		Name    string   `json:"name"`
		Role    string   `json:"role"`
		Extras  []string `json:"extras"`
		Sources []struct {
			Kind string `json:"kind"`
		} `json:"sources"`
		Grant   *grantRefResp `json:"grant"`
		Members []struct {
			UserID   string `json:"userId"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"members"`
	} `json:"teams"`
	People []envAccessPerson `json:"people"`
}

type grantRefResp struct {
	ID     string   `json:"id"`
	Role   string   `json:"role"`
	Extras []string `json:"extras"`
}

type envAccessPerson struct {
	UserID        string   `json:"userId"`
	Username      string   `json:"username"`
	Role          string   `json:"role"`
	Extras        []string `json:"extras"`
	InstanceAdmin bool     `json:"instanceAdmin"`
	Self          bool     `json:"self"`
	Sources       []struct {
		Kind string `json:"kind"`
	} `json:"sources"`
	Grant *grantRefResp `json:"grant"`
}

type candidatesResp struct {
	Users []struct {
		ID          string `json:"id"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	} `json:"users"`
	Teams []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"teams"`
}

func personNamed(t *testing.T, got envAccessResp, name string) envAccessPerson {
	t.Helper()
	for _, p := range got.People {
		if p.Username == name {
			return p
		}
	}
	t.Fatalf("%s is not listed in %+v", name, got.People)
	return envAccessPerson{}
}

func decodeInto(t *testing.T, r *envRec, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (f *authFixture) envAccess(t *testing.T, name, envID string) *envRec {
	t.Helper()
	return f.asUser(t, name, http.MethodGet, "/api/env/"+envID+"/access", "")
}

func TestEnvAccessAnEnvAdminSeesWhoCanUseTheirEnvironmentAndWhyWithTheInstanceAdminsAlwaysThere(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	f.addEnvUser(t, "dave", a.ID, "viewer")

	rec := f.envAccess(t, "carol", a.ID)
	if rec.code != http.StatusOK {
		t.Fatalf("GET access as an env Admin: %d %s, want 200", rec.code, rec.body)
	}
	var got envAccessResp
	decodeInto(t, rec, &got)
	if got.Teams == nil || len(got.Teams) != 0 {
		t.Fatalf("teams = %+v, want an empty list", got.Teams)
	}
	carol := personNamed(t, got, "carol")
	if !carol.Self || carol.Role != "admin" || carol.Grant == nil || carol.Grant.Role != "admin" || len(carol.Sources) != 1 || carol.Sources[0].Kind != "direct" {
		t.Fatalf("carol = %+v", carol)
	}
	admin := personNamed(t, got, "admin")
	if !admin.InstanceAdmin || admin.Self || admin.Role != "admin" || admin.Grant != nil || len(admin.Sources) != 1 || admin.Sources[0].Kind != "instance" || len(admin.Extras) == 0 {
		t.Fatalf("admin = %+v, want the instance admin with every extra and nothing to edit", admin)
	}
	if dave := personNamed(t, got, "dave"); dave.Role != "viewer" || dave.Self {
		t.Fatalf("dave = %+v", dave)
	}
	if strings.Contains(string(rec.body), "email") {
		t.Fatalf("the Access list carries an email: %s", string(rec.body))
	}
}

func TestEnvAccessTheWhoListOffersUsernamesDisplayNamesAndTeamNamesNeverEmailsAndLeavesOutWhoHasAGrantHere(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	f.addUser(t, "dave", false)
	f.addUser(t, "erin", false)
	f.addEnvRole(t, "erin", a.ID, "viewer")
	if _, err := f.st.Write.Exec(`UPDATE users SET email = 'dave@example.com', display_name = 'Dave D' WHERE id = 'id-dave'`); err != nil {
		t.Fatal(err)
	}
	f.addTeam(t, "ops")

	rec := f.asUser(t, "carol", http.MethodGet, "/api/env/"+a.ID+"/access/candidates", "")
	if rec.code != http.StatusOK {
		t.Fatalf("GET candidates: %d %s", rec.code, rec.body)
	}
	var got candidatesResp
	decodeInto(t, rec, &got)
	var names []string
	for _, u := range got.Users {
		names = append(names, u.Username)
	}
	if !slices.Equal(names, []string{"dave"}) || got.Users[0].DisplayName != "Dave D" || got.Users[0].ID != "id-dave" {
		t.Fatalf("users = %+v, want dave only (carol and erin have a grant here, admin holds everything)", got.Users)
	}
	if len(got.Teams) != 1 || got.Teams[0].Name != "ops" {
		t.Fatalf("teams = %+v", got.Teams)
	}
	if strings.Contains(string(rec.body), "example.com") || strings.Contains(string(rec.body), "email") {
		t.Fatalf("the Who list carries an email: %s", string(rec.body))
	}
}

func TestEnvAccessAnEnvAdminGrantsOperatorWithTheShellOnTheirEnvironmentAndTheGranteeHasItThereAndNowhereElse(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, b := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	dave := f.addUser(t, "dave", false)

	rec := f.asUser(t, "carol", http.MethodPut, "/api/env/"+a.ID+"/access",
		`{"subjectType":"user","subjectId":"`+dave+`","role":"operator","extras":["containers.exec"]}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("PUT access as an env Admin: %d %s, want 201", rec.code, rec.body)
	}
	var g grantResp
	decodeInto(t, rec, &g)
	if g.Role != "operator" || g.EnvID != a.ID || g.Scope != "env" || g.SubjectID != dave || !slices.Equal(g.Extras, []string{"containers.exec"}) {
		t.Fatalf("grant = %+v", g)
	}

	me, _ := f.getMe(t, "dave")
	if got := me.Access[a.ID]; got.Role != "operator" || !slices.Equal(got.Extras, []string{"containers.exec"}) {
		t.Fatalf("dave on A = %+v, want Operator with the shell on his next request", got)
	}
	if !slices.Contains(me.Permissions.Environments[a.ID], "containers.exec") {
		t.Fatalf("dave's permissions on A = %v", me.Permissions.Environments[a.ID])
	}
	rec2 := f.asUser(t, "dave", http.MethodGet, "/api/env/"+b.ID+"/summary", "")
	if rec2.code != http.StatusNotFound || rec2.ptyp != "environment_not_found" {
		t.Fatalf("dave on B: %d %s, want 404 environment_not_found", rec2.code, rec2.body)
	}

	granted := f.audits(t, audit.ActionAccessGranted)
	if len(granted) != 1 || granted[0].ActorID != "id-carol" || f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'access.granted' AND env_id = ?`, a.ID) != 1 {
		t.Fatalf("access.granted = %+v, want one by carol with env_id A", granted)
	}

	// The same call edits the row: the grantee's role changes and the entry is access.updated.
	rec = f.asUser(t, "carol", http.MethodPut, "/api/env/"+a.ID+"/access",
		`{"subjectType":"user","subjectId":"`+dave+`","role":"viewer"}`)
	if rec.code != http.StatusOK {
		t.Fatalf("PUT access again: %d %s, want 200", rec.code, rec.body)
	}
	if updated := f.audits(t, audit.ActionAccessUpdated); len(updated) != 1 || updated[0].ActorID != "id-carol" {
		t.Fatalf("access.updated = %+v", updated)
	}
}

func TestEnvAccessAnEnvAdminGivesATeamAccessToTheirEnvironmentAndTheTeamRowListsItsMembers(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	f.addUser(t, "dave", false)
	team := f.addTeam(t, "ops", "dave")

	rec := f.asUser(t, "carol", http.MethodPut, "/api/env/"+a.ID+"/access",
		`{"subjectType":"team","subjectId":"`+team+`","role":"viewer"}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("PUT team access: %d %s, want 201", rec.code, rec.body)
	}
	rec = f.envAccess(t, "carol", a.ID)
	var got envAccessResp
	decodeInto(t, rec, &got)
	if len(got.Teams) != 1 || got.Teams[0].Name != "ops" || got.Teams[0].Role != "viewer" || got.Teams[0].Grant == nil ||
		len(got.Teams[0].Members) != 1 || got.Teams[0].Members[0].Username != "dave" {
		t.Fatalf("teams = %+v", got.Teams)
	}
	if dave := personNamed(t, got, "dave"); dave.Role != "viewer" || len(dave.Sources) != 1 || dave.Sources[0].Kind != "team" || dave.Grant != nil {
		t.Fatalf("dave = %+v, want Viewer through the team only", dave)
	}
}

// deniedByDelegation counts the permission.denied entries that carry the reason
// of a refused delegation.
func deniedByDelegation(t *testing.T, f *authFixture) int {
	t.Helper()
	n := 0
	for _, e := range f.audits(t, audit.ActionPermissionDenied) {
		if e.Reason == "delegation_denied" {
			n++
		}
	}
	return n
}

func (f *authFixture) assignmentCount(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM role_assignments`)
}

func TestDelegationRefusesAnInstanceAdminSubjectAndEveryAttemptToAimElsewhereAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, b := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	dave := f.addUser(t, "dave", false)
	root := f.addInstanceAdmin(t, "root")
	before := f.assignmentCount(t)

	// An instance admin's access is not an environment grant's to give or change.
	rec := f.asUser(t, "carol", http.MethodPut, "/api/env/"+a.ID+"/access",
		`{"subjectType":"user","subjectId":"`+root+`","role":"viewer"}`)
	if rec.code != http.StatusForbidden || rec.ptyp != "delegation_denied" {
		t.Fatalf("PUT for an instance admin: %d %s, want 403 delegation_denied", rec.code, rec.body)
	}
	if got := deniedByDelegation(t, f.authFixture); got != 1 {
		t.Fatalf("permission.denied entries with the reason delegation_denied = %d, want 1", got)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	last := denied[len(denied)-1]
	if last.ActorID != "id-carol" || last.Result != "denied" || last.TargetID != "PUT /api/env/{envId}/access" {
		t.Fatalf("the refusal is audited as %+v", last)
	}

	// The body names no scope and no environment: a body that does is refused by
	// the schema, so nothing can be aimed at all environments or at another one.
	for _, body := range []string{
		`{"subjectType":"user","subjectId":"` + dave + `","role":"viewer","scope":"all"}`,
		`{"subjectType":"user","subjectId":"` + dave + `","role":"viewer","envId":"` + b.ID + `"}`,
		`{"subjectType":"user","subjectId":"` + dave + `","role":"viewer","scope":"env","envId":"` + b.ID + `"}`,
	} {
		rec := f.asUser(t, "carol", http.MethodPut, "/api/env/"+a.ID+"/access", body)
		if rec.code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT %s: %d %s, want 422", body, rec.code, rec.body)
		}
	}
	if after := f.assignmentCount(t); after != before {
		t.Fatalf("assignments %d -> %d, want nothing changed", before, after)
	}
	if f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action LIKE 'access.%'`) != 0 {
		t.Fatal("a refused request wrote an access entry")
	}
}

func TestDelegationAnEnvAdminOfAGetsNotFoundOnBAndAViewerOrOperatorOnAGetsForbidden(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, b := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	f.addEnvUser(t, "vera", a.ID, "viewer")
	f.addEnvUser(t, "oscar", a.ID, "operator")
	dave := f.addUser(t, "dave", false)
	body := `{"subjectType":"user","subjectId":"` + dave + `","role":"viewer"}`
	before := f.assignmentCount(t)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/env/" + b.ID + "/access", ""},
		{http.MethodGet, "/api/env/" + b.ID + "/access/candidates", ""},
		{http.MethodPut, "/api/env/" + b.ID + "/access", body},
		{http.MethodPost, "/api/env/" + b.ID + "/access/any/impact", `{"op":"delete"}`},
		{http.MethodDelete, "/api/env/" + b.ID + "/access/any", ""},
	} {
		rec := f.asUser(t, "carol", c.method, c.path, c.body)
		if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
			t.Fatalf("%s %s as an env Admin of A: %d %s, want 404 environment_not_found", c.method, c.path, rec.code, rec.body)
		}
	}
	for _, name := range []string{"vera", "oscar"} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/env/" + a.ID + "/access", ""},
			{http.MethodGet, "/api/env/" + a.ID + "/access/candidates", ""},
			{http.MethodPut, "/api/env/" + a.ID + "/access", body},
			{http.MethodPost, "/api/env/" + a.ID + "/access/any/impact", `{"op":"delete"}`},
			{http.MethodDelete, "/api/env/" + a.ID + "/access/any", ""},
		} {
			rec := f.asUser(t, name, c.method, c.path, c.body)
			if rec.code != http.StatusForbidden {
				t.Fatalf("%s %s as %s: %d %s, want 403", c.method, c.path, name, rec.code, rec.body)
			}
		}
	}
	if after := f.assignmentCount(t); after != before {
		t.Fatalf("assignments %d -> %d, want nothing changed", before, after)
	}
}

func TestDelegationRemovesAGrantOnTheEnvironmentWithItsImpactAndRefusesOtherScopes(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, b := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	dave := f.addUser(t, "dave", false)
	erin := f.addUser(t, "erin", false)
	frank := f.addUser(t, "frank", false)
	f.addEnvRole(t, "dave", a.ID, "operator")
	f.addEnvRole(t, "erin", "", "viewer") // every environment
	f.addEnvRole(t, "frank", b.ID, "viewer")
	root := f.addInstanceAdmin(t, "root")
	f.addEnvRole(t, "root", a.ID, "viewer") // a stale grant of an instance admin
	grantOf := func(user, envID string) string {
		t.Helper()
		var id string
		q := `SELECT id FROM role_assignments WHERE principal_id = ? AND COALESCE(env_id, '') = ?`
		if err := f.st.Read.QueryRow(q, user, envID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	daveGrant, erinGrant, frankGrant, rootGrant := grantOf(dave, a.ID), grantOf(erin, ""), grantOf(frank, b.ID), grantOf(root, a.ID)

	// The preview says what the removal then does.
	rec := f.asUser(t, "carol", http.MethodPost, "/api/env/"+a.ID+"/access/"+daveGrant+"/impact", `{"op":"delete"}`)
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s, want 200", rec.code, rec.body)
	}
	var preview impactResp
	decodeInto(t, rec, &preview)
	if len(preview.Impact) != 1 || preview.Impact[0].Username != "dave" || preview.Impact[0].Before != "operator" || preview.Impact[0].After != "none" {
		t.Fatalf("preview = %s", rec.body)
	}
	if f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE id = ?`, daveGrant) != 1 {
		t.Fatal("asking changed the database")
	}

	rec = f.asUser(t, "carol", http.MethodDelete, "/api/env/"+a.ID+"/access/"+daveGrant, "")
	if rec.code != http.StatusOK {
		t.Fatalf("DELETE: %d %s, want 200", rec.code, rec.body)
	}
	var done impactResp
	decodeInto(t, rec, &done)
	if len(done.Impact) != len(preview.Impact) || done.Impact[0] != preview.Impact[0] {
		t.Fatalf("the removal produced %s, the preview said %+v", rec.body, preview.Impact)
	}
	revoked := f.audits(t, audit.ActionAccessRevoked)
	if len(revoked) != 1 || revoked[0].ActorID != "id-carol" || f.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'access.revoked' AND env_id = ?`, a.ID) != 1 {
		t.Fatalf("access.revoked = %+v, want one by carol with env_id A", revoked)
	}

	// A grant on every environment, on another environment, an instance admin's,
	// and one that does not exist: nothing changes, and the first three are audited.
	before := f.assignmentCount(t)
	for _, c := range []struct {
		name, id string
		status   int
		problem  string
	}{
		{"every environment", erinGrant, http.StatusForbidden, "delegation_denied"},
		{"another environment", frankGrant, http.StatusForbidden, "delegation_denied"},
		{"an instance admin", rootGrant, http.StatusForbidden, "delegation_denied"},
		{"nothing", "no-such-grant", http.StatusNotFound, "grant_not_found"},
	} {
		for _, call := range []struct{ method, path, body string }{
			{http.MethodPost, "/api/env/" + a.ID + "/access/" + c.id + "/impact", `{"op":"delete"}`},
			{http.MethodDelete, "/api/env/" + a.ID + "/access/" + c.id, ""},
		} {
			rec := f.asUser(t, "carol", call.method, call.path, call.body)
			if rec.code != c.status || rec.ptyp != c.problem {
				t.Fatalf("%s the grant of %s: %d %s, want %d %s", call.method, c.name, rec.code, rec.body, c.status, c.problem)
			}
		}
	}
	if got := deniedByDelegation(t, f.authFixture); got != 6 {
		t.Fatalf("delegation refusals audited = %d, want 6 (three grants, preview and removal)", got)
	}
	if after := f.assignmentCount(t); after != before {
		t.Fatalf("assignments %d -> %d, want nothing changed", before, after)
	}
}

func TestDelegationAnEnvAdminMakesAnotherAdminAndMayRemoveTheirOwnAdminGrant(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	dave := f.addUser(t, "dave", false)

	rec := f.asUser(t, "carol", http.MethodPut, "/api/env/"+a.ID+"/access", `{"subjectType":"user","subjectId":"`+dave+`","role":"admin"}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("make dave Admin: %d %s, want 201", rec.code, rec.body)
	}
	if rec := f.asUser(t, "dave", http.MethodGet, "/api/env/"+a.ID+"/access", ""); rec.code != http.StatusOK {
		t.Fatalf("dave as Admin: %d %s, want 200", rec.code, rec.body)
	}

	var carolGrant string
	if err := f.st.Read.QueryRow(`SELECT id FROM role_assignments WHERE principal_id = 'id-carol' AND env_id = ?`, a.ID).Scan(&carolGrant); err != nil {
		t.Fatal(err)
	}
	rec = f.asUser(t, "carol", http.MethodDelete, "/api/env/"+a.ID+"/access/"+carolGrant, "")
	if rec.code != http.StatusOK {
		t.Fatalf("carol removes her own Admin grant: %d %s, want 200", rec.code, rec.body)
	}
	rec = f.asUser(t, "carol", http.MethodGet, "/api/env/"+a.ID+"/access", "")
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("carol on A afterwards: %d %s, want 404 environment_not_found", rec.code, rec.body)
	}
}

func TestDelegationNoEnvironmentRouteCreatesUsersOrTeamsOrChangesMembership(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, richEngine())
	a, _ := f.twoEnvironments(t)
	f.addEnvUser(t, "carol", a.ID, "admin")
	users, teams, members := f.count(t, `SELECT COUNT(*) FROM users`), f.count(t, `SELECT COUNT(*) FROM teams`), f.count(t, `SELECT COUNT(*) FROM team_members`)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/env/" + a.ID + "/access/users", `{"username":"mallory"}`},
		{http.MethodPost, "/api/env/" + a.ID + "/access/teams", `{"name":"owners"}`},
		{http.MethodPost, "/api/env/" + a.ID + "/access/members", `{"teamId":"x","userIds":["id-carol"]}`},
		{http.MethodPost, "/api/env/" + a.ID + "/users", `{"username":"mallory"}`},
		{http.MethodPost, "/api/env/" + a.ID + "/teams", `{"name":"owners"}`},
		{http.MethodPatch, "/api/env/" + a.ID + "/access", `{"instanceAdmin":true}`},
	} {
		rec := f.asUser(t, "carol", c.method, c.path, c.body)
		if rec.code != http.StatusNotFound && rec.code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: %d %s, want 404 or 405 (there is no such route)", c.method, c.path, rec.code, rec.body)
		}
	}
	if f.count(t, `SELECT COUNT(*) FROM users`) != users || f.count(t, `SELECT COUNT(*) FROM teams`) != teams || f.count(t, `SELECT COUNT(*) FROM team_members`) != members {
		t.Fatal("an environment route changed users, teams or membership")
	}
}
