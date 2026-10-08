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
