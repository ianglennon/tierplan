// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// The owned automations of a user and their transfer (plan 03-22, D-14, AUTH-10).
// Negative cases are mandatory (D-81).

const ownedTestKind = "test.sync"

var registerOwnedKindOnce sync.Once

// ownedFix is the auth fixture with the test kind registered (containers.read, so
// a Viewer can run it) and alice owning r1 on env-a, which is named Alpha.
type ownedFix struct {
	*authFixture
	cookie, csrf string
}

func newOwnedFix(t *testing.T) *ownedFix {
	t.Helper()
	registerOwnedKindOnce.Do(func() {
		rbac.RegisterOwnedKind(rbac.OwnedKind{Kind: ownedTestKind, Label: "Test sync", Perm: rbac.PermContainersRead})
	})
	f := &ownedFix{authFixture: newAuthFixture(t, config.Config{})}
	f.cookie, f.csrf = f.signIn(t, "admin", false)
	for _, u := range []string{"alice", "bob", "carol"} {
		f.addUser(t, u, false)
	}
	f.addEnvRole(t, "alice", "env-a", "viewer")
	f.addEnvRole(t, "bob", "env-a", "viewer")
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-a', 'agent', 'Alpha', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Write.Exec(`INSERT INTO owned_resources (kind, resource_id, env_id, name, owner_id, state, needs_env_admin, created_at, updated_at)
		VALUES (?, 'r1', 'env-a', 'Sync r1', 'id-alice', 'active', 0, 1, 1)`, ownedTestKind); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *ownedFix) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodGet, path: path, cookie: f.cookie})
}

func (f *ownedFix) transfer(t *testing.T, kind, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPost, path: "/api/owned/" + kind + "/" + id + "/transfer", body: body, cookie: f.cookie, csrf: f.csrf})
}

func (f *ownedFix) disableAlice(t *testing.T) {
	t.Helper()
	if rec := f.disable(t, f.cookie, f.csrf, "id-alice"); rec.Code != http.StatusOK {
		t.Fatalf("disable alice: %d %s", rec.Code, rec.Body)
	}
}

type ownedResp struct {
	Kind          string `json:"kind"`
	Label         string `json:"label"`
	ResourceID    string `json:"resourceId"`
	Name          string `json:"name"`
	EnvID         string `json:"envId"`
	EnvName       string `json:"envName"`
	State         string `json:"state"`
	NeedsEnvAdmin bool   `json:"needsEnvAdmin"`
}

type userRefResp struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not the expected body: %v: %s", err, rec.Body)
	}
	return v
}

func TestOwnedListOfAUserShowsTheAutomationPausedBecauseTheirOwnerWasDisabled(t *testing.T) {
	t.Parallel()
	f := newOwnedFix(t)
	f.disableAlice(t)

	rec := f.get(t, "/api/users/id-alice/owned")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decodeJSON[[]ownedResp](t, rec)
	want := ownedResp{Kind: ownedTestKind, Label: "Test sync", ResourceID: "r1", Name: "Sync r1", EnvID: "env-a", EnvName: "Alpha", State: "paused_disabled"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("owned = %+v, want [%+v]", got, want)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
}

func TestOwnedListOfAUserWhoOwnsNothingIsAnEmptyListAndOfAnUnknownUserIs404(t *testing.T) {
	t.Parallel()
	f := newOwnedFix(t)
	rec := f.get(t, "/api/users/id-carol/owned")
	if rec.Code != http.StatusOK || rec.Body.String() != "[]\n" && rec.Body.String() != "[]" {
		t.Fatalf("owned of a user who owns nothing: %d %q, want 200 []", rec.Code, rec.Body)
	}
	if rec := f.get(t, "/api/users/no-such-user/owned"); rec.Code != http.StatusNotFound || problemType(t, rec) != "user_not_found" {
		t.Fatalf("unknown user: %d %s, want 404 user_not_found", rec.Code, rec.Body)
	}
}

func TestOwnedEligibleOwnersAreTheUsersWhoCouldRunItNow(t *testing.T) {
	t.Parallel()
	f := newOwnedFix(t)
	f.disableAlice(t)

	rec := f.get(t, "/api/owned/"+ownedTestKind+"/r1/eligible-owners")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decodeJSON[[]userRefResp](t, rec)
	// bob is a Viewer on env-a, carol has no grant, alice is the owner; the
	// instance admin passes everywhere.
	byName := map[string]bool{}
	for _, u := range got {
		byName[u.Username] = true
	}
	if !byName["bob"] || !byName["admin"] || byName["carol"] || byName["alice"] || len(got) != 2 {
		t.Fatalf("eligible = %+v, want bob and admin only", got)
	}
	if rec := f.get(t, "/api/owned/"+ownedTestKind+"/nope/eligible-owners"); rec.Code != http.StatusNotFound || problemType(t, rec) != "automation_not_found" {
		t.Fatalf("unknown automation: %d %s, want 404 automation_not_found", rec.Code, rec.Body)
	}
}

func TestOwnedTransferToAnEligibleUserMakesTheAutomationActiveUnderThemAndIsAudited(t *testing.T) {
	t.Parallel()
	f := newOwnedFix(t)
	f.disableAlice(t)

	rec := f.transfer(t, ownedTestKind, "r1", `{"ownerId":"id-bob"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("transfer: %d %s", rec.Code, rec.Body)
	}
	if got := decodeJSON[ownedResp](t, rec); got.ResourceID != "r1" || got.State != "active" {
		t.Fatalf("answer = %+v, want r1 active", got)
	}
	var owner, state string
	if err := f.st.Read.QueryRow(`SELECT owner_id, state FROM owned_resources WHERE resource_id = 'r1'`).Scan(&owner, &state); err != nil || owner != "id-bob" || state != "active" {
		t.Fatalf("row = %s %s (%v), want id-bob active", owner, state, err)
	}
	entries := f.audits(t, "owner.transferred")
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != "r1" || entries[0].Result != "success" {
		t.Fatalf("owner.transferred entries = %+v", entries)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'owner.transferred'`).Scan(&details); err != nil ||
		details != `{"from":"id-alice","kind":"test.sync","resource":"r1","to":"id-bob"}` {
		t.Fatalf("details = %s (%v)", details, err)
	}
	if left := decodeJSON[[]ownedResp](t, f.get(t, "/api/users/id-alice/owned")); len(left) != 0 {
		t.Fatalf("alice still owns %+v", left)
	}
}

func TestOwnedTransferToSomeoneWhoCouldNotRunItAnswers422OwnerNotEligibleAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newOwnedFix(t)
	for _, to := range []string{"id-carol", "id-alice", "no-such-user"} {
		rec := f.transfer(t, ownedTestKind, "r1", `{"ownerId":"`+to+`"}`)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "owner_not_eligible" {
			t.Fatalf("transfer to %s: %d %s, want 422 owner_not_eligible", to, rec.Code, rec.Body)
		}
	}
	var owner string
	if err := f.st.Read.QueryRow(`SELECT owner_id FROM owned_resources WHERE resource_id = 'r1'`).Scan(&owner); err != nil || owner != "id-alice" {
		t.Fatalf("owner = %s (%v), want id-alice", owner, err)
	}
	if n := len(f.audits(t, "owner.transferred")); n != 0 {
		t.Fatalf("owner.transferred entries = %d after refused transfers, want none", n)
	}
	if rec := f.transfer(t, ownedTestKind, "nope", `{"ownerId":"id-bob"}`); rec.Code != http.StatusNotFound || problemType(t, rec) != "automation_not_found" {
		t.Fatalf("unknown automation: %d %s, want 404 automation_not_found", rec.Code, rec.Body)
	}
	if rec := f.transfer(t, "test.gone", "r1", `{"ownerId":"id-bob"}`); rec.Code != http.StatusNotFound || problemType(t, rec) != "automation_not_found" {
		t.Fatalf("unknown kind: %d %s, want 404 automation_not_found", rec.Code, rec.Body)
	}
}

func TestOwnedRoutesNeedUsersManage(t *testing.T) {
	t.Parallel()
	f := newOwnedFix(t)
	bob, bobCSRF := f.signIn(t, "bob", false)

	for _, path := range []string{"/api/users/id-alice/owned", "/api/owned/" + ownedTestKind + "/r1/eligible-owners"} {
		if rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: bob}); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as a Viewer: %d, want 403", path, rec.Code)
		}
	}
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/owned/" + ownedTestKind + "/r1/transfer", body: `{"ownerId":"id-bob"}`, cookie: bob, csrf: bobCSRF})
	if rec.Code != http.StatusForbidden {
		t.Errorf("transfer as a Viewer: %d, want 403", rec.Code)
	}
	var owner string
	if err := f.st.Read.QueryRow(`SELECT owner_id FROM owned_resources WHERE resource_id = 'r1'`).Scan(&owner); err != nil || owner != "id-alice" {
		t.Fatalf("owner = %s (%v), want a refused transfer to change nothing", owner, err)
	}
}
