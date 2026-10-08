// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

const permTestDeploy rbac.Perm = "test.deploy"

const (
	kindTestSync = "test.sync"
	kindUnknown  = "test.gone"
)

var ownedRegistry = rbac.Registry{
	{Name: permTestDeploy, Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleOperator},
}

var registerOnce sync.Once

// ownedFixture is the access fixture with a Checker over the test registry and
// the test kind registered.
type ownedFixture struct {
	*fixture
	ctx context.Context
}

func newOwnedFixture(t *testing.T) *ownedFixture {
	t.Helper()
	registerOnce.Do(func() {
		rbac.RegisterOwnedKindIn(ownedRegistry, rbac.OwnedKind{Kind: kindTestSync, Label: "Test sync", Perm: permTestDeploy})
	})
	f := newFixture(t)
	f.svc.Checker = rbac.NewChecker(f.st, rbac.WithRegistry(ownedRegistry))
	return &ownedFixture{fixture: f, ctx: context.Background()}
}

func (f *ownedFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.ExecContext(f.ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *ownedFixture) addUser(t *testing.T, id string) {
	t.Helper()
	f.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?, ?, 'x', 1, 1, 1)`, id, id)
}

func (f *ownedFixture) assign(t *testing.T, userID, role, envID string) {
	t.Helper()
	f.exec(t, `INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES (?, 'user', ?, ?, ?, 1)`,
		"ra-"+userID+"-"+envID, userID, role, envID)
}

func (f *ownedFixture) unassign(t *testing.T, userID string) {
	t.Helper()
	f.exec(t, `DELETE FROM role_assignments WHERE principal_id = ?`, userID)
}

// inTx runs fn in a write transaction and commits it.
func (f *ownedFixture) inTx(t *testing.T, fn func(tx *sql.Tx, q *sqlc.Queries) error) {
	t.Helper()
	tx, err := f.st.Write.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx, f.st.Q.WithTx(tx)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (f *ownedFixture) register(t *testing.T, r access.OwnedResource) {
	t.Helper()
	f.inTx(t, func(tx *sql.Tx, q *sqlc.Queries) error { return f.svc.RegisterOwnedTx(f.ctx, tx, q, r) })
}

func (f *ownedFixture) recheck(t *testing.T, owners ...string) int {
	t.Helper()
	var n int
	f.inTx(t, func(tx *sql.Tx, q *sqlc.Queries) (err error) {
		n, err = f.svc.RecheckOwnedTx(f.ctx, tx, q, owners, audit.Actor{Type: audit.ActorSystem, ID: "system:cli", Name: "system:cli"})
		return err
	})
	return n
}

func (f *ownedFixture) state(t *testing.T, kind, id string) string {
	t.Helper()
	var s string
	if err := f.st.Read.QueryRow(`SELECT state FROM owned_resources WHERE kind = ? AND resource_id = ?`, kind, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *ownedFixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func syncResource(owner string) access.OwnedResource {
	return access.OwnedResource{Kind: kindTestSync, ResourceID: "r1", EnvID: "env-a", Name: "Sync r1", OwnerID: owner}
}

func TestRunAsAnAutomationRunsAsItsOwnerAndPausesAuditedWhenTheOwnerLosesThePermission(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))

	p, res, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:gitsync")
	if err != nil {
		t.Fatalf("RunAs for an Operator owner: %v", err)
	}
	if p.Type != rbac.PrincipalSystem || p.ID != "system:gitsync" || p.OnBehalfOf != "alice" || res.EnvID != "env-a" || res.State != "active" {
		t.Fatalf("RunAs = %+v, %+v", p, res)
	}
	req := rbac.Requirement{Perm: permTestDeploy, Scope: rbac.ScopeEnvironment}
	if d := f.svc.Checker.Check(f.ctx, p, req, "env-a"); !d.Allowed {
		t.Fatalf("the on-behalf principal was refused the owner's permission: %+v", d)
	}

	f.unassign(t, "alice")
	if n := f.recheck(t, "alice"); n != 1 {
		t.Fatalf("recheck changed %d rows, want 1", n)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "paused_lacks_permission" {
		t.Fatalf("state = %s, want paused_lacks_permission", got)
	}
	var target, details string
	if err := f.st.Read.QueryRow(`SELECT target_id, details FROM audit_log WHERE action = 'automation.paused'`).Scan(&target, &details); err != nil {
		t.Fatalf("no automation.paused entry: %v", err)
	}
	if target != "r1" || details != `{"kind":"test.sync","owner":"alice","resource":"r1","state":"paused_lacks_permission"}` {
		t.Errorf("entry = %q %s", target, details)
	}

	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:gitsync"); !errors.Is(err, access.ErrOwnerLacksPermission) {
		t.Fatalf("RunAs after the owner lost the permission = %v, want ErrOwnerLacksPermission", err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "paused_lacks_permission" {
		t.Errorf("a refused run changed the state to %s", got)
	}
}

func TestRecheckOwnedResumesAPausedAutomationOnceWhenTheOwnerQualifiesAgain(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))
	f.unassign(t, "alice")
	f.recheck(t, "alice")

	f.assign(t, "alice", "operator", "env-a")
	if n := f.recheck(t, "alice"); n != 1 {
		t.Fatalf("recheck changed %d rows, want 1", n)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "active" {
		t.Fatalf("state = %s, want active", got)
	}
	if n := f.auditCount(t, "automation.resumed"); n != 1 {
		t.Errorf("%d automation.resumed entries, want 1", n)
	}
	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:gitsync"); err != nil {
		t.Errorf("RunAs after resuming: %v", err)
	}
}

func TestRecheckOwnedInsideATransactionSeesThatTransactionsOwnChanges(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))

	f.inTx(t, func(tx *sql.Tx, q *sqlc.Queries) error {
		if _, err := tx.ExecContext(f.ctx, `DELETE FROM role_assignments WHERE principal_id = 'alice'`); err != nil {
			return err
		}
		n, err := f.svc.RecheckOwnedTx(f.ctx, tx, q, []string{"alice"}, audit.Actor{Type: audit.ActorSystem, ID: "system:cli", Name: "system:cli"})
		if err != nil || n != 1 {
			t.Errorf("recheck in the same transaction = %d, %v, want the deletion seen", n, err)
		}
		return nil
	})
	if got := f.state(t, kindTestSync, "r1"); got != "paused_lacks_permission" {
		t.Errorf("state = %s", got)
	}
}

func TestOwnedRowThatNeedsEnvAdminPausesWhenItsOwnerIsOnlyAnOperator(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "op")
	f.assign(t, "op", "operator", "env-a")
	f.addUser(t, "boss")
	f.assign(t, "boss", "admin", "env-a")
	f.register(t, access.OwnedResource{Kind: kindTestSync, ResourceID: "by-op", EnvID: "env-a", Name: "by op", OwnerID: "op"})
	f.register(t, access.OwnedResource{Kind: kindTestSync, ResourceID: "by-boss", EnvID: "env-a", Name: "by boss", OwnerID: "boss"})
	f.inTx(t, func(tx *sql.Tx, q *sqlc.Queries) error {
		for _, id := range []string{"by-op", "by-boss"} {
			if err := q.SetOwnedNeedsEnvAdmin(f.ctx, sqlc.SetOwnedNeedsEnvAdminParams{NeedsEnvAdmin: 1, UpdatedAt: 2, Kind: kindTestSync, ResourceID: id}); err != nil {
				return err
			}
		}
		return nil
	})

	if n := f.recheck(t, "op", "boss"); n != 1 {
		t.Fatalf("recheck changed %d rows, want only the Operator's", n)
	}
	if got := f.state(t, kindTestSync, "by-op"); got != "paused_lacks_permission" {
		t.Errorf("the Operator's automation is %s, want paused_lacks_permission", got)
	}
	if got := f.state(t, kindTestSync, "by-boss"); got != "active" {
		t.Errorf("the Admin's automation is %s, want active", got)
	}
}

func (f *ownedFixture) setUser(t *testing.T, id, set string) {
	t.Helper()
	f.exec(t, `UPDATE users SET `+set+` WHERE id = ?`, id)
}

func TestOwnedRowFollowsADisabledOwnerAndComesBackWhenTheyDoAndStillQualify(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))

	f.setUser(t, "alice", "disabled = 1")
	if n := f.recheck(t, "alice"); n != 1 || f.state(t, kindTestSync, "r1") != "paused_disabled" {
		t.Fatalf("after disabling: changed %d, state %s, want 1 and paused_disabled", n, f.state(t, kindTestSync, "r1"))
	}
	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:gitsync"); !errors.Is(err, access.ErrOwnerLacksPermission) {
		t.Errorf("RunAs for a disabled owner = %v", err)
	}

	f.setUser(t, "alice", "disabled = 0")
	if n := f.recheck(t, "alice"); n != 1 || f.state(t, kindTestSync, "r1") != "active" {
		t.Fatalf("after re-enabling: changed %d, state %s, want 1 and active", n, f.state(t, kindTestSync, "r1"))
	}
	if f.auditCount(t, "automation.paused") != 1 || f.auditCount(t, "automation.resumed") != 1 {
		t.Errorf("entries: %d paused, %d resumed, want 1 each", f.auditCount(t, "automation.paused"), f.auditCount(t, "automation.resumed"))
	}
}

func TestOwnedRowOfAnOwnerWhoWasDisabledAndLostTheGrantStaysPausedForTheNewReason(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))
	f.setUser(t, "alice", "disabled = 1")
	f.recheck(t, "alice")

	f.unassign(t, "alice")
	f.setUser(t, "alice", "disabled = 0")
	if n := f.recheck(t, "alice"); n != 1 || f.state(t, kindTestSync, "r1") != "paused_lacks_permission" {
		t.Fatalf("changed %d, state %s, want 1 and paused_lacks_permission", n, f.state(t, kindTestSync, "r1"))
	}
	if f.auditCount(t, "automation.resumed") != 0 {
		t.Error("a row that is still paused was reported as resumed")
	}
}

func TestOwnedRowOfADeletedOwnerPausesAndNeverResumesByItself(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "admin", "env-a")
	f.register(t, syncResource("alice"))

	f.setUser(t, "alice", "deleted_at = 5")
	if n := f.recheck(t, "alice"); n != 1 || f.state(t, kindTestSync, "r1") != "paused_deleted" {
		t.Fatalf("changed %d, state %s, want 1 and paused_deleted", n, f.state(t, kindTestSync, "r1"))
	}
	// Even if the tombstone were somehow lifted with the grant intact, only a
	// transfer of ownership revives the row.
	f.setUser(t, "alice", "deleted_at = NULL")
	if n := f.recheck(t, "alice"); n != 0 || f.state(t, kindTestSync, "r1") != "paused_deleted" {
		t.Fatalf("later recheck changed %d, state %s, want 0 and paused_deleted", n, f.state(t, kindTestSync, "r1"))
	}
	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:gitsync"); !errors.Is(err, access.ErrOwnerLacksPermission) {
		t.Errorf("RunAs = %v, want ErrOwnerLacksPermission", err)
	}
}

func TestOwnedRowOfAnInstanceAdminWithNoAssignmentsIsActiveOnAnyEnvironment(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "root")
	f.setUser(t, "root", "instance_admin = 1")
	for _, env := range []string{"env-a", "env-never-seen"} {
		r := syncResource("root")
		r.ResourceID, r.EnvID = "r-"+env, env
		f.register(t, r)
	}
	if n := f.recheck(t, "root"); n != 0 {
		t.Fatalf("recheck changed %d rows of an instance admin", n)
	}
	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r-env-never-seen", "system:webhook"); err != nil {
		t.Errorf("RunAs for an instance admin: %v", err)
	}
}

func TestOwnedRowOfAnUnregisteredKindIsPausedAndNeverRun(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "root")
	f.setUser(t, "root", "instance_admin = 1")
	f.inTx(t, func(_ *sql.Tx, q *sqlc.Queries) error {
		return q.InsertOwnedResource(f.ctx, sqlc.InsertOwnedResourceParams{
			Kind: kindUnknown, ResourceID: "x", EnvID: "env-a", Name: "gone", OwnerID: "root", State: "active", CreatedAt: 1, UpdatedAt: 1,
		})
	})
	if n := f.recheck(t, "root"); n != 1 || f.state(t, kindUnknown, "x") != "paused_lacks_permission" {
		t.Fatalf("changed %d, state %s, want 1 and paused_lacks_permission", n, f.state(t, kindUnknown, "x"))
	}
	if _, _, err := f.svc.RunAs(f.ctx, kindUnknown, "x", "system:gitsync"); !errors.Is(err, access.ErrOwnerLacksPermission) {
		t.Errorf("RunAs of an unregistered kind = %v, want ErrOwnerLacksPermission", err)
	}
}

func TestOwnedRecheckOfAnOwnerWithNoRowsChangesNothingAndTwiceWritesEntriesOnce(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))

	if n := f.recheck(t, "bob"); n != 0 {
		t.Errorf("an owner with no rows changed %d", n)
	}
	if n := f.recheck(t); n != 0 {
		t.Errorf("no owners changed %d", n)
	}
	if n := f.recheck(t, "alice"); n != 0 {
		t.Errorf("a qualifying owner changed %d rows", n)
	}
	if f.auditCount(t, "automation.paused")+f.auditCount(t, "automation.resumed") != 0 {
		t.Fatal("a recheck that changed nothing wrote an entry")
	}

	f.unassign(t, "alice")
	if first, second := f.recheck(t, "alice"), f.recheck(t, "alice"); first != 1 || second != 0 {
		t.Errorf("rechecks changed %d then %d, want 1 then 0", first, second)
	}
	if n := f.auditCount(t, "automation.paused"); n != 1 {
		t.Errorf("%d automation.paused entries after two rechecks, want 1", n)
	}
}

func TestOwnedRegistrationRefusesAnUnknownKindAnOwnerWhoLacksThePermissionAndAnUnlistedActor(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "viewer")
	f.assign(t, "viewer", "viewer", "env-a")
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))

	tx, err := f.st.Write.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	q := f.st.Q.WithTx(tx)
	if err := f.svc.RegisterOwnedTx(f.ctx, tx, q, access.OwnedResource{Kind: kindUnknown, ResourceID: "z", EnvID: "env-a", OwnerID: "alice"}); !errors.Is(err, access.ErrUnknownOwnedKind) {
		t.Errorf("an unregistered kind = %v", err)
	}
	if err := f.svc.RegisterOwnedTx(f.ctx, tx, q, access.OwnedResource{Kind: kindTestSync, ResourceID: "v", EnvID: "env-a", OwnerID: "viewer"}); !errors.Is(err, access.ErrOwnerLacksPermission) {
		t.Errorf("a Viewer owner = %v", err)
	}
	if err := f.svc.RegisterOwnedTx(f.ctx, tx, q, access.OwnedResource{Kind: kindTestSync, ResourceID: "e", EnvID: "env-b", OwnerID: "alice"}); !errors.Is(err, access.ErrOwnerLacksPermission) {
		t.Errorf("an Operator of another environment = %v", err)
	}
	if err := f.svc.RegisterOwnedTx(f.ctx, tx, q, access.OwnedResource{Kind: kindTestSync, ResourceID: "n", EnvID: "env-a", OwnerID: "nobody"}); !errors.Is(err, access.ErrOwnerLacksPermission) {
		t.Errorf("an owner who does not exist = %v", err)
	}
	_ = tx.Rollback()

	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:nobody"); !errors.Is(err, access.ErrUnknownSystemPrincipal) {
		t.Errorf("an unlisted system principal = %v", err)
	}
	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "missing", "system:gitsync"); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("a resource that is not recorded = %v", err)
	}
}
