// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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

// Delete, instance admin, unlock and profile (plan 03-14 Task 2).

func ptr(s string) *string { return &s }

// addFullUser adds a user with every kind of thing that must go with a delete.
func (f *ownedFixture) addFullUser(t *testing.T, id string) {
	t.Helper()
	f.addUser(t, id)
	f.exec(t, `UPDATE users SET email = 'a@example.test', display_name = 'Alice L', password_hash = 'hash' WHERE id = ?`, id)
	f.assign(t, id, "operator", "env-a")
	f.exec(t, `INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'containers.exec')`, "ra-"+id+"-env-a")
	f.exec(t, `INSERT INTO teams (id, name, created_at, updated_at) VALUES ('t1', 'Ops', 1, 1)`)
	f.exec(t, `INSERT INTO team_members (team_id, user_id, created_at) VALUES ('t1', ?, 1)`, id)
	f.exec(t, `INSERT INTO user_links (id, user_id, kind, token_hash, created_at, expires_at, created_by) VALUES ('l1', ?, 'reset', x'01', 1, 9999999999999999, 'x')`, id)
	f.addSessions(t, id, 2)
}

func TestDeleteUserLeavesATombstoneAndRemovesEverythingThatLetThemIn(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addFullUser(t, "alice")
	f.register(t, syncResource("alice"))

	if err := f.svc.DeleteUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}

	var disabled, admin int
	var deletedAt sql.NullInt64
	var hash, email, name, username string
	if err := f.st.Read.QueryRow(`SELECT disabled, deleted_at, password_hash, email, display_name, instance_admin, username FROM users WHERE id = 'alice'`).
		Scan(&disabled, &deletedAt, &hash, &email, &name, &admin, &username); err != nil {
		t.Fatalf("the row is gone, want a tombstone: %v", err)
	}
	if disabled != 1 || !deletedAt.Valid || hash != "" || email != "" || name != "" || admin != 0 || username != "alice" {
		t.Fatalf("tombstone = disabled %d deleted_at %v hash %q email %q name %q admin %d username %q", disabled, deletedAt, hash, email, name, admin, username)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM sessions WHERE user_id = 'alice'`,
		`SELECT COUNT(*) FROM user_links WHERE user_id = 'alice'`,
		`SELECT COUNT(*) FROM role_assignments WHERE principal_type = 'user' AND principal_id = 'alice'`,
		`SELECT COUNT(*) FROM assignment_extras`,
		`SELECT COUNT(*) FROM team_members WHERE user_id = 'alice'`,
	} {
		if n := f.count(t, q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	if got := f.state(t, kindTestSync, "r1"); got != "paused_deleted" {
		t.Fatalf("owned state = %s, want paused_deleted", got)
	}
	if got := f.auditDetails(t, "user.deleted"); got != `{"ended":"2","paused":"1","reassigned":"0","username":"alice"}` {
		t.Fatalf("details = %s", got)
	}
}

func TestADeletedUsersNameStaysReservedAndTheUserIsGone(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addFullUser(t, "alice")
	if err := f.svc.DeleteUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.GetUser(f.ctx, "alice"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("get a tombstone = %v, want ErrNotFound", err)
	}
	if err := f.svc.DeleteUser(f.ctx, testAdmin, "alice"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("delete a tombstone = %v, want ErrNotFound", err)
	}
	if n := f.auditCount(t, "user.deleted"); n != 1 {
		t.Fatalf("user.deleted entries = %d, want 1", n)
	}
	if _, _, err := f.svc.CreateUser(f.ctx, testAdmin, access.CreateUserInput{Username: "Alice"}); !errors.Is(err, access.ErrUsernameReserved) {
		t.Fatalf("create the same name again = %v, want ErrUsernameReserved", err)
	}
	if err := f.svc.DisableUser(f.ctx, testAdmin, "alice"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("disable a tombstone = %v, want ErrNotFound", err)
	}
	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "alice", true); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("make a tombstone an admin = %v, want ErrNotFound", err)
	}
	if err := f.svc.UpdateProfile(f.ctx, testAdmin, "alice", access.ProfileInput{DisplayName: ptr("x")}); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("edit a tombstone = %v, want ErrNotFound", err)
	}
}

func TestTheLastActiveInstanceAdminCannotBeDeletedOrDemotedAndNothingChanges(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addSessions(t, "root", 1)

	if err := f.svc.DeleteUser(f.ctx, testAdmin, "root"); !errors.Is(err, access.ErrLastInstanceAdmin) {
		t.Fatalf("delete the only admin = %v, want ErrLastInstanceAdmin", err)
	}
	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "root", false); !errors.Is(err, access.ErrLastInstanceAdmin) {
		t.Fatalf("demote the only admin = %v, want ErrLastInstanceAdmin", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM users WHERE id = 'root' AND instance_admin = 1 AND disabled = 0 AND deleted_at IS NULL`); n != 1 {
		t.Fatal("a refusal changed the admin")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM sessions WHERE user_id = 'root'`); n != 1 {
		t.Fatal("a refusal ended the admin's session")
	}
	if n := f.auditCount(t, "user.deleted") + f.auditCount(t, "user.admin_revoked"); n != 0 {
		t.Fatalf("a refusal wrote %d entries", n)
	}

	// A disabled admin does not count as active: the only active one is still protected.
	f.addAdmin(t, "idle")
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'idle'`)
	if err := f.svc.DeleteUser(f.ctx, testAdmin, "root"); !errors.Is(err, access.ErrLastInstanceAdmin) {
		t.Fatalf("delete the only active admin with a disabled one around = %v", err)
	}
	// With a second active admin one may delete themselves.
	f.addAdmin(t, "second")
	if err := f.svc.DeleteUser(f.ctx, testAdmin, "root"); err != nil {
		t.Fatalf("delete one of two active admins = %v", err)
	}
}

func TestSetInstanceAdminGrantsAndRevokesPerUserAndAuditsOnlyRealChanges(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")

	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "alice", true); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT instance_admin FROM users WHERE id = 'alice'`); n != 1 {
		t.Fatal("alice is not an instance admin")
	}
	if got := f.auditDetails(t, "user.admin_granted"); got != `{"username":"alice"}` {
		t.Fatalf("details = %s", got)
	}
	// Granting to an admin changes nothing and writes nothing.
	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "alice", true); err != nil {
		t.Fatal(err)
	}
	if n := f.auditCount(t, "user.admin_granted"); n != 1 {
		t.Fatalf("granting twice wrote %d entries, want 1", n)
	}

	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "alice", false); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT instance_admin FROM users WHERE id = 'alice'`); n != 0 {
		t.Fatal("alice is still an instance admin")
	}
	if got := f.auditDetails(t, "user.admin_revoked"); got != `{"username":"alice"}` {
		t.Fatalf("details = %s", got)
	}
	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "alice", false); err != nil {
		t.Fatal(err)
	}
	if n := f.auditCount(t, "user.admin_revoked"); n != 1 {
		t.Fatalf("revoking twice wrote %d entries, want 1", n)
	}
	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "nobody", true); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
}

func TestRevokingInstanceAdminRechecksTheAutomationsTheyOwn(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addAdmin(t, "alice")
	f.register(t, syncResource("alice"))

	if err := f.svc.SetInstanceAdmin(f.ctx, testAdmin, "alice", false); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "paused_lacks_permission" {
		t.Fatalf("state = %s, want paused_lacks_permission", got)
	}
}

func TestUnlockUserClearsALockAuditedAsTheAdminAndIsANoOpOtherwise(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	locked := f.clk.now().Add(30 * time.Minute).UnixNano()
	f.exec(t, `UPDATE users SET failed_count = 5, locked_until = ? WHERE id = 'alice'`, locked)

	if err := f.svc.UnlockUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	var count int
	var until sql.NullInt64
	if err := f.st.Read.QueryRow(`SELECT failed_count, locked_until FROM users WHERE id = 'alice'`).Scan(&count, &until); err != nil {
		t.Fatal(err)
	}
	if count != 0 || until.Valid {
		t.Fatalf("failed_count %d locked_until %v, want both cleared", count, until)
	}
	var actor, details string
	if err := f.st.Read.QueryRow(`SELECT actor_id, details FROM audit_log WHERE action = 'auth.unlocked'`).Scan(&actor, &details); err != nil {
		t.Fatalf("no auth.unlocked entry: %v", err)
	}
	if actor != testAdmin.ID || details != `{"username":"alice"}` {
		t.Fatalf("entry = %q %s, want the admin as actor", actor, details)
	}

	if err := f.svc.UnlockUser(f.ctx, testAdmin, "alice"); err != nil {
		t.Fatal(err)
	}
	if n := f.auditCount(t, "auth.unlocked"); n != 1 {
		t.Fatalf("unlocking an unlocked user wrote %d entries in all, want 1", n)
	}
	if err := f.svc.UnlockUser(f.ctx, testAdmin, "nobody"); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
}

func TestUpdateProfileSavesTrimmedFieldsAuditsWhichChangedAndMovesTheVersion(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	before, err := f.svc.GetUser(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	err = f.svc.UpdateProfile(f.ctx, testAdmin, "alice", access.ProfileInput{
		DisplayName: ptr("  Alice L  "), Email: ptr(" a@example.test "), UpdatedAt: before.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.GetUser(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if after.DisplayName != "Alice L" || after.Email != "a@example.test" || !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("after = %+v, want trimmed fields and a newer version", after)
	}
	if got := f.auditDetails(t, "user.updated"); got != `{"fields":"displayName,email","username":"alice"}` {
		t.Fatalf("details = %s", got)
	}

	// Only the field that changed is named, and the username is never touched.
	if err := f.svc.UpdateProfile(f.ctx, testAdmin, "alice", access.ProfileInput{DisplayName: ptr("Alice"), UpdatedAt: after.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.st.Read.Query(`SELECT details FROM audit_log WHERE action = 'user.updated' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var all []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		all = append(all, d)
	}
	if len(all) != 2 || all[1] != `{"fields":"displayName","username":"alice"}` {
		t.Fatalf("entries = %v", all)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM users WHERE username = 'alice'`); n != 1 {
		t.Fatal("the username changed")
	}
}

func TestUpdateProfileWithAnOldVersionIsStaleAndChangesNothing(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	first, _ := f.svc.GetUser(f.ctx, "alice")
	if err := f.svc.UpdateProfile(f.ctx, testAdmin, "alice", access.ProfileInput{DisplayName: ptr("One"), UpdatedAt: first.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	// A second admin still holds the first version.
	err := f.svc.UpdateProfile(f.ctx, testAdmin, "alice", access.ProfileInput{DisplayName: ptr("Two"), UpdatedAt: first.UpdatedAt})
	if !errors.Is(err, access.ErrStaleChange) {
		t.Fatalf("save on an old version = %v, want ErrStaleChange", err)
	}
	if d, _ := f.svc.GetUser(f.ctx, "alice"); d.DisplayName != "One" {
		t.Fatalf("display name = %q, want the first save kept", d.DisplayName)
	}
}

func TestUpdateProfileRefusesABadEmailOrALongNameAndSavesNothing(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	d, _ := f.svc.GetUser(f.ctx, "alice")

	err := f.svc.UpdateProfile(f.ctx, testAdmin, "alice", access.ProfileInput{
		DisplayName: ptr(strings.Repeat("x", 65)), Email: ptr("Alice <a@example.test>"), UpdatedAt: d.UpdatedAt,
	})
	if !errors.Is(err, access.ErrInvalidEmail) || !errors.Is(err, access.ErrDisplayNameTooLong) {
		t.Fatalf("err = %v, want both field errors", err)
	}
	if n := f.auditCount(t, "user.updated"); n != 0 {
		t.Fatalf("a refusal wrote %d entries", n)
	}
}

func TestUpdateProfileWithNothingNewChangesNothingAndWritesNoEntry(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	d, _ := f.svc.GetUser(f.ctx, "alice")
	if err := f.svc.UpdateProfile(f.ctx, testAdmin, "alice", access.ProfileInput{DisplayName: ptr(""), Email: ptr(""), UpdatedAt: d.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	if n := f.auditCount(t, "user.updated"); n != 0 {
		t.Fatalf("a no-op wrote %d entries", n)
	}
	if after, _ := f.svc.GetUser(f.ctx, "alice"); !after.UpdatedAt.Equal(d.UpdatedAt) {
		t.Fatal("a no-op moved the version")
	}
}

// TestLastAdminGuardRace: whatever order two admins' changes land in, one active
// instance admin is left, and exactly one of the two is refused (T-03-54).
func TestLastAdminGuardRace(t *testing.T) {
	scenarios := []struct {
		name string
		a, b func(f *ownedFixture) error
	}{
		{
			"disable A and disable B",
			func(f *ownedFixture) error { return f.svc.DisableUser(f.ctx, testAdmin, "adminA") },
			func(f *ownedFixture) error { return f.svc.DisableUser(f.ctx, testAdmin, "adminB") },
		},
		{
			"delete A and demote B",
			func(f *ownedFixture) error { return f.svc.DeleteUser(f.ctx, testAdmin, "adminA") },
			func(f *ownedFixture) error { return f.svc.SetInstanceAdmin(f.ctx, testAdmin, "adminB", false) },
		},
		{
			"demote A and delete B",
			func(f *ownedFixture) error { return f.svc.SetInstanceAdmin(f.ctx, testAdmin, "adminA", false) },
			func(f *ownedFixture) error { return f.svc.DeleteUser(f.ctx, testAdmin, "adminB") },
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := newOwnedFixture(t)
			f.addAdmin(t, "adminA")
			f.addAdmin(t, "adminB")

			var wg sync.WaitGroup
			errs := make([]error, 2)
			start := make(chan struct{})
			for i, fn := range []func(*ownedFixture) error{sc.a, sc.b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					errs[i] = fn(f)
				}()
			}
			close(start)
			wg.Wait()

			refused := 0
			for _, err := range errs {
				switch {
				case err == nil:
				case errors.Is(err, access.ErrLastInstanceAdmin):
					refused++
				default:
					t.Fatalf("unexpected error %v", err)
				}
			}
			if refused != 1 {
				t.Fatalf("errors = %v, want exactly one ErrLastInstanceAdmin", errs)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM users WHERE instance_admin = 1 AND disabled = 0 AND deleted_at IS NULL`); n != 1 {
				t.Fatalf("%d active instance admins left, want exactly 1", n)
			}
		})
	}
}

// No query may change a username: it is the one name the audit log and a
// tombstone rely on (D-22c, T-03-56). The SET list of every UPDATE of users is
// read, without its WHERE clause and without comments.
func TestNoQueryEverSetsAUsername(t *testing.T) {
	files, err := filepath.Glob("../store/queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no query files found: %v", err)
	}
	comment := regexp.MustCompile(`(?m)--.*$|(?s)/\*.*?\*/`)
	update := regexp.MustCompile(`(?is)\bupdate\s+users\s+set\b([^;]*)`)
	where := regexp.MustCompile(`(?i)\bwhere\b`)
	assignsUsername := regexp.MustCompile(`(?i)(^|[\s,])username\s*=`)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range update.FindAllStringSubmatch(comment.ReplaceAllString(string(b), ""), -1) {
			set := m[1]
			if loc := where.FindStringIndex(set); loc != nil {
				set = set[:loc[0]]
			}
			if assignsUsername.MatchString(set) {
				t.Errorf("%s changes a username: UPDATE users SET%s", file, m[1])
			}
		}
	}
}
