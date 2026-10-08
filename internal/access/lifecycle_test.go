// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
)

// The account lifecycle (plan 03-14, AUTH-04, D-22c, D-02). Negative cases are
// mandatory (D-81).

// addAdmin adds an active instance admin.
func (f *ownedFixture) addAdmin(t *testing.T, id string) {
	t.Helper()
	f.addUser(t, id)
	f.exec(t, `UPDATE users SET instance_admin = 1 WHERE id = ?`, id)
}

// addSessions gives a user n live sessions.
func (f *ownedFixture) addSessions(t *testing.T, userID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%d", userID, i)
		f.exec(t, `INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, absolute_expires_at, remember, ip, user_agent, public_id)
			VALUES (?, ?, 1, 1, NULL, 0, '', '', ?)`, []byte(id), userID, id)
	}
}

func (f *ownedFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *ownedFixture) auditDetails(t *testing.T, action string) string {
	t.Helper()
	var d string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = ?`, action).Scan(&d); err != nil {
		t.Fatalf("no %s entry: %v", action, err)
	}
	return d
}

func TestDisableUserEndsEverySessionAndKeepsGrantsAndMembershipsAndAudits(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.exec(t, `INSERT INTO teams (id, name, created_at, updated_at) VALUES ('t1', 'Ops', 1, 1)`)
	f.exec(t, `INSERT INTO team_members (team_id, user_id, created_at) VALUES ('t1', 'alice', 1)`)
	f.addSessions(t, "alice", 2)
	f.addSessions(t, "root", 1)

	if err := f.svc.DisableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}

	if n := f.count(t, `SELECT disabled FROM users WHERE id = 'alice'`); n != 1 {
		t.Fatalf("disabled = %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'alice'`); n != 0 {
		t.Fatalf("alice has %d sessions left, want none", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'root'`); n != 1 {
		t.Fatalf("another user's sessions = %d, want them untouched", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_id = 'alice'`); n != 1 {
		t.Fatalf("assignments = %d, want the grant kept", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM team_members WHERE user_id = 'alice'`); n != 1 {
		t.Fatalf("memberships = %d, want the membership kept", n)
	}
	if got := f.auditDetails(t, "user.disabled"); got != `{"ended":"2","paused":"0","username":"alice"}` {
		t.Fatalf("details = %s", got)
	}
}

func TestDisableUserDeletesTheUsersOneTimeLink(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	_, _, err := f.svc.CreateUser(f.ctx, testAdmin, access.CreateUserInput{Username: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.st.Read.QueryRow(`SELECT id FROM users WHERE username = 'bob'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DisableUser(f.ctx, testAdmin, id); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_links WHERE user_id = ?`, id); n != 0 {
		t.Fatalf("links = %d, want none for a disabled user", n)
	}
}

func TestDisableUserPausesTheAutomationsTheyOwnAndEnableResumesThem(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))

	if err := f.svc.DisableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "paused_disabled" {
		t.Fatalf("state = %s, want paused_disabled", got)
	}
	if got := f.auditDetails(t, "user.disabled"); got != `{"ended":"0","paused":"1","username":"alice"}` {
		t.Fatalf("details = %s", got)
	}

	if err := f.svc.EnableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "active" {
		t.Fatalf("state after enable = %s, want active", got)
	}
}

func TestEnableUserKeepsTheGrantAndAudits(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	if err := f.svc.DisableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.EnableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT disabled FROM users WHERE id = 'alice'`); n != 0 {
		t.Fatalf("disabled = %d, want 0", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_id = 'alice'`); n != 1 {
		t.Fatalf("assignments = %d, want the grant kept", n)
	}
	if got := f.auditDetails(t, "user.enabled"); got != `{"username":"alice"}` {
		t.Fatalf("details = %s", got)
	}
}

func TestDisablingADisabledUserAndEnablingAnEnabledOneChangeNothingAndWriteNoEntry(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")

	if err := f.svc.EnableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if n := f.auditCount(t, "user.enabled"); n != 0 {
		t.Fatalf("enabling an enabled user wrote %d entries", n)
	}
	if err := f.svc.DisableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DisableUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if n := f.auditCount(t, "user.disabled"); n != 1 {
		t.Fatalf("disabling twice wrote %d entries, want 1", n)
	}
}

func TestTheLastActiveInstanceAdminCannotBeDisabledButOneOfTwoMayDisableThemselves(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addSessions(t, "root", 1)

	if err := f.svc.DisableUser(f.ctx, testAdmin, "root"); !errors.Is(err, access.ErrLastInstanceAdmin) {
		t.Fatalf("disabling the only admin = %v, want ErrLastInstanceAdmin", err)
	}
	if n := f.count(t, `SELECT disabled FROM users WHERE id = 'root'`); n != 0 {
		t.Fatal("the refusal left the admin disabled")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'root'`); n != 1 {
		t.Fatal("the refusal ended the admin's session")
	}
	if n := f.auditCount(t, "user.disabled"); n != 0 {
		t.Fatalf("a refusal wrote %d entries", n)
	}

	f.addAdmin(t, "second")
	if err := f.svc.DisableUser(f.ctx, testAdmin, "root"); err != nil {
		t.Fatalf("disabling one of two admins = %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'root'`); n != 0 {
		t.Fatal("their own sessions did not end")
	}
}

func TestDisableAndEnableAnUnknownUserAreNotFound(t *testing.T) {
	f := newOwnedFixture(t)
	if err := f.svc.DisableUser(f.ctx, testAdmin, "nobody"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("disable unknown = %v", err)
	}
	if err := f.svc.EnableUser(f.ctx, testAdmin, "nobody"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("enable unknown = %v", err)
	}
	if _, err := f.svc.GetUser(f.ctx, "nobody"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("get unknown = %v", err)
	}
}

func TestGetUserReturnsTheProfileStatusTeamsAndVersion(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	f.exec(t, `UPDATE users SET display_name = 'Alice L', email = 'a@example.test', updated_at = 42 WHERE id = 'alice'`)
	f.exec(t, `INSERT INTO teams (id, name, created_at, updated_at) VALUES ('t1', 'Ops', 1, 1)`)
	f.exec(t, `INSERT INTO team_members (team_id, user_id, created_at) VALUES ('t1', 'alice', 1)`)

	d, err := f.svc.GetUser(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if d.Username != "alice" || d.DisplayName != "Alice L" || d.Email != "a@example.test" || d.Status != access.StatusActive ||
		d.TeamCount != 1 || d.InstanceAdmin || d.UpdatedAt.UnixNano() != 42 {
		t.Fatalf("detail = %+v", d)
	}
}
