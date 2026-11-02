// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
)

// The per-user last-used record (plan 02-12, ENV-06, D-31): kept on the server so it
// follows a user across devices, the newest 3 per user, only for environments the
// user may read.

func (f *agentFix) recents(t *testing.T) []string {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/me/recent-environments", "")
	if rec.code != http.StatusOK {
		t.Fatalf("GET recent: %d %s", rec.code, rec.body)
	}
	return decodeRecents(t, rec.body)
}

func decodeRecents(t *testing.T, body []byte) []string {
	t.Helper()
	var b struct {
		EnvironmentIDs []string `json:"environmentIds"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.EnvironmentIDs == nil {
		t.Fatalf("recent body = %s (%v), want environmentIds as a list, never null", body, err)
	}
	return b.EnvironmentIDs
}

func (f *agentFix) use(t *testing.T, envID string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPut, "/api/me/recent-environments/"+envID, "")
}

func (f *agentFix) tick() { f.clk.t = f.clk.t.Add(time.Minute) }

func (f *agentFix) rows(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM user_recent_environments WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecentUsingTheSameEnvironmentTwiceKeepsOneRowWithTheNewerTime(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "A")
	if rec := f.use(t, a.ID); rec.code != http.StatusNoContent {
		t.Fatalf("PUT = %d %s, want 204", rec.code, rec.body)
	}
	var first int64
	if err := f.st.Read.QueryRow(`SELECT used_at FROM user_recent_environments`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if rec := f.use(t, a.ID); rec.code != http.StatusNoContent {
		t.Fatalf("second PUT = %d %s", rec.code, rec.body)
	}
	var second int64
	if err := f.st.Read.QueryRow(`SELECT used_at FROM user_recent_environments`).Scan(&second); err != nil || f.rows(t, "id-admin") != 1 {
		t.Fatalf("rows = %d (%v), want one", f.rows(t, "id-admin"), err)
	}
	if second != first+int64(time.Minute) {
		t.Fatalf("used_at went %d then %d, want the newer time kept", first, second)
	}
	// A repeat that arrives with an older time (a second device with a slow clock)
	// must not move the use backwards.
	f.clk.t = f.clk.t.Add(-time.Hour)
	f.use(t, a.ID)
	var third int64
	if err := f.st.Read.QueryRow(`SELECT used_at FROM user_recent_environments`).Scan(&third); err != nil || third != second {
		t.Fatalf("used_at = %d (%v) after an older repeat, want it kept at %d", third, err, second)
	}
}

func TestRecentOnlyTheNewestThreeAreKeptAndRemovingAnEnvironmentDropsItFromTheList(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	ids := map[string]string{}
	for _, n := range []string{"A", "B", "C", "D"} {
		ids[n] = f.srv.NewEnvironment(t, n).ID
		f.tick()
		if rec := f.use(t, ids[n]); rec.code != http.StatusNoContent {
			t.Fatalf("use %s: %d %s", n, rec.code, rec.body)
		}
	}
	want := []string{ids["D"], ids["C"], ids["B"]}
	if got := f.recents(t); !slices.Equal(got, want) {
		t.Fatalf("recent = %v, want D, C, B = %v", got, want)
	}
	if n := f.rows(t, "id-admin"); n != 3 {
		t.Fatalf("%d rows kept, want 3: A was the oldest and must be gone", n)
	}

	f.srv.Exec(t, `DELETE FROM environments WHERE id = ?`, ids["B"])
	if got := f.recents(t); !slices.Equal(got, []string{ids["D"], ids["C"]}) {
		t.Fatalf("recent after B was removed = %v, want D, C", got)
	}
}

func TestRecentUsesAtTheSameInstantAreOrderedByEnvironmentID(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	var ids []string
	for _, n := range []string{"A", "B", "C"} {
		id := f.srv.NewEnvironment(t, n).ID
		ids = append(ids, id)
		f.use(t, id) // the clock does not move
	}
	slices.Sort(ids)
	if got := f.recents(t); !slices.Equal(got, ids) {
		t.Fatalf("recent = %v, want the tie broken by environment ID: %v", got, ids)
	}
	// A fourth at the same instant: the three smallest IDs are kept.
	extra := f.srv.NewEnvironment(t, "D").ID
	f.use(t, extra)
	all := append(slices.Clone(ids), extra)
	slices.Sort(all)
	if got := f.recents(t); !slices.Equal(got, all[:3]) {
		t.Fatalf("recent = %v, want %v", got, all[:3])
	}
}

func TestRecentUsingAnEnvironmentTheCallerCannotReadOrThatDoesNotExistIsTheSame404(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	secret := f.srv.NewEnvironment(t, "Secret").ID
	f.addUser(t, "nobody", false)
	cookie, csrf := f.signIn(t, "nobody", false)

	put := func(id string) *envRec {
		rec := f.call(t, authReq{method: http.MethodPut, path: "/api/me/recent-environments/" + id, cookie: cookie, csrf: csrf})
		return &envRec{rec.Code, rec.Body.Bytes(), problemType(t, rec)}
	}
	unreadable, missing := put(secret), put("00000000-0000-7000-8000-000000000000")
	for name, rec := range map[string]*envRec{"unreadable": unreadable, "unknown": missing} {
		if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
			t.Fatalf("%s environment: %d %s (%s), want 404 environment_not_found", name, rec.code, rec.body, rec.ptyp)
		}
	}
	// Nothing about the answer tells the two apart.
	var u, m struct{ Detail, Title string }
	_ = json.Unmarshal(unreadable.body, &u)
	_ = json.Unmarshal(missing.body, &m)
	if u != m {
		t.Fatalf("the two refusals differ: %s vs %s", unreadable.body, missing.body)
	}
	if f.rows(t, "id-nobody") != 0 {
		t.Fatal("a refused use was recorded")
	}
}

func TestRecentEachUserHasTheirOwnListAndOnlyReadableEnvironmentsAreReturned(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a, b := f.srv.NewEnvironment(t, "A").ID, f.srv.NewEnvironment(t, "B").ID
	f.addUser(t, "viewer", false)
	f.grantRead(t, "viewer", a)
	f.grantRead(t, "viewer", b)
	cookie, csrf := f.signIn(t, "viewer", false)
	call := func(method, path string) *envRec {
		rec := f.call(t, authReq{method: method, path: path, cookie: cookie, csrf: csrf})
		return &envRec{rec.Code, rec.Body.Bytes(), ""}
	}

	call(http.MethodPut, "/api/me/recent-environments/"+a)
	f.tick()
	call(http.MethodPut, "/api/me/recent-environments/"+b)
	if got := decodeRecents(t, call(http.MethodGet, "/api/me/recent-environments").body); !slices.Equal(got, []string{b, a}) {
		t.Fatalf("viewer's recent = %v, want B, A", got)
	}
	if got := f.recents(t); len(got) != 0 {
		t.Fatalf("admin's recent = %v, want none: the list is per user", got)
	}

	// B's grant is withdrawn: the row stays, but B is not returned.
	f.srv.Exec(t, `DELETE FROM role_assignments WHERE principal_id = 'id-viewer' AND env_id = ?`, b)
	if got := decodeRecents(t, call(http.MethodGet, "/api/me/recent-environments").body); !slices.Equal(got, []string{a}) {
		t.Fatalf("viewer's recent after losing B = %v, want A only", got)
	}
}

func TestRecentEnvironmentsNeedASession(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "A").ID
	for _, c := range []authReq{
		{method: http.MethodGet, path: "/api/me/recent-environments"},
		{method: http.MethodPut, path: "/api/me/recent-environments/" + a},
	} {
		if rec := f.call(t, c); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

// D-08 says the 404 for an environment the caller cannot see is still audited as a
// refusal. The policy gate does that on every /api/env route; this route is
// self-service, so it has to do it itself (T-03-19).

// deniedRows reads the permission.denied entries with the environment they name.
func (f *agentFix) deniedRows(t *testing.T) []struct{ Actor, Target, Reason, Env string } {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_id, target_id, reason, COALESCE(env_id, '') FROM audit_log WHERE action = ? ORDER BY seq`,
		audit.ActionPermissionDenied)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []struct{ Actor, Target, Reason, Env string }
	for rows.Next() {
		var r struct{ Actor, Target, Reason, Env string }
		if err := rows.Scan(&r.Actor, &r.Target, &r.Reason, &r.Env); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecentUsingAnEnvironmentTheCallerCannotSeeIsAuditedAsPermissionDeniedLikeThePolicyGate(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	secret := f.srv.NewEnvironment(t, "Secret").ID
	f.addUser(t, "nobody", false)
	cookie, csrf := f.signIn(t, "nobody", false)
	put := func(id string) *httptest.ResponseRecorder {
		return f.call(t, authReq{method: http.MethodPut, path: "/api/me/recent-environments/" + id, cookie: cookie, csrf: csrf})
	}
	const missing = "00000000-0000-7000-8000-000000000000"

	for _, id := range []string{secret, missing} {
		if rec := put(id); rec.Code != http.StatusNotFound {
			t.Fatalf("PUT %s = %d %s, want 404", id, rec.Code, rec.Body)
		}
	}
	got := f.deniedRows(t)
	if len(got) != 2 {
		t.Fatalf("permission.denied entries = %+v, want one for the hidden environment and one for the unknown ID", got)
	}
	for i, id := range []string{secret, missing} {
		want := struct{ Actor, Target, Reason, Env string }{
			"id-nobody", "PUT /api/me/recent-environments/{envId}", "environment_not_visible", id,
		}
		if got[i] != want {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestRecentAnEnvironmentTheCallerMayReadButIsGoneIsNotAuditedAsARefusal(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	// The administrator may read every environment, so an unknown ID is a plain
	// 404, which the policy gate does not audit either.
	if rec := f.use(t, "00000000-0000-7000-8000-000000000000"); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("PUT = %d %s (%s), want 404 environment_not_found", rec.code, rec.body, rec.ptyp)
	}
	if got := f.deniedRows(t); len(got) != 0 {
		t.Fatalf("permission.denied entries = %+v, want none", got)
	}
}
