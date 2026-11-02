// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package access_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
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

// The listing, the eligible owners and the transfer (plan 03-22, D-14, AUTH-10).

func (f *ownedFixture) ownerOf(t *testing.T, kind, id string) string {
	t.Helper()
	var o string
	if err := f.st.Read.QueryRow(`SELECT owner_id FROM owned_resources WHERE kind = ? AND resource_id = ?`, kind, id).Scan(&o); err != nil {
		t.Fatal(err)
	}
	return o
}

func usernames(refs []access.UserRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Username)
	}
	return out
}

// eligibilityFixture is alice owning r1 on env-a, with one candidate of each
// kind: bob (Operator there), carol (no grant), dave (Viewer, below the kind's
// Operator minimum), erin (Operator but disabled), frank (Operator but deleted),
// grace (instance admin with no assignments) and heidi (Admin there).
func eligibilityFixture(t *testing.T) *ownedFixture {
	t.Helper()
	f := newOwnedFixture(t)
	f.addEnv(t, "env-a", "Alpha")
	for _, u := range []string{"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi"} {
		f.addUser(t, u)
	}
	f.assign(t, "alice", "operator", "env-a")
	f.assign(t, "bob", "operator", "env-a")
	f.assign(t, "dave", "viewer", "env-a")
	f.assign(t, "erin", "operator", "env-a")
	f.assign(t, "frank", "operator", "env-a")
	f.assign(t, "heidi", "admin", "env-a")
	f.setUser(t, "erin", "disabled = 1")
	f.setUser(t, "frank", "deleted_at = 5")
	f.setUser(t, "grace", "instance_admin = 1")
	f.register(t, syncResource("alice"))
	return f
}

func TestEligibleOwnersAreTheActiveUsersWhoPassTheKindsPermissionOnItsEnvironmentRightNow(t *testing.T) {
	f := eligibilityFixture(t)

	got, err := f.svc.EligibleOwners(f.ctx, kindTestSync, "r1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bob", "grace", "heidi"}
	if g := usernames(got); !slices.Equal(g, want) {
		t.Fatalf("eligible = %v, want %v (a Viewer, a user with no grant, a disabled one, a deleted one and the owner are not offered)", g, want)
	}

	// The list follows the grants as they are now.
	f.unassign(t, "bob")
	got, err = f.svc.EligibleOwners(f.ctx, kindTestSync, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if g := usernames(got); !slices.Equal(g, []string{"grace", "heidi"}) {
		t.Fatalf("eligible after bob lost his grant = %v", g)
	}
}

func TestEligibleOwnersOfARowThatNeedsEnvAdminAreEnvAdminsAndInstanceAdminsOnly(t *testing.T) {
	f := eligibilityFixture(t)
	f.exec(t, `UPDATE owned_resources SET needs_env_admin = 1 WHERE kind = ? AND resource_id = 'r1'`, kindTestSync)

	got, err := f.svc.EligibleOwners(f.ctx, kindTestSync, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if g := usernames(got); !slices.Equal(g, []string{"grace", "heidi"}) {
		t.Fatalf("eligible = %v, want only the env Admin and the instance admin", g)
	}
	if err := f.svc.TransferOwned(f.ctx, testAdmin, kindTestSync, "r1", "bob"); !errors.Is(err, access.ErrNotEligible) {
		t.Fatalf("transfer of an env-Admin-only row to an Operator = %v, want ErrNotEligible", err)
	}
}

func TestEligibleOwnersOfAnUnknownAutomationIsNotFound(t *testing.T) {
	f := eligibilityFixture(t)
	if _, err := f.svc.EligibleOwners(f.ctx, kindTestSync, "nope"); !errors.Is(err, access.ErrOwnedNotFound) {
		t.Fatalf("eligible owners of an unknown automation = %v, want ErrOwnedNotFound", err)
	}
	if _, err := f.svc.EligibleOwners(f.ctx, kindUnknown, "r1"); !errors.Is(err, access.ErrOwnedNotFound) {
		t.Fatalf("eligible owners of an unknown kind = %v, want ErrOwnedNotFound", err)
	}
}

func TestListOwnedNamesTheKindTheEnvironmentAndTheStateOfEveryRowOfAnOwner(t *testing.T) {
	f := eligibilityFixture(t)
	f.exec(t, `INSERT INTO owned_resources (kind, resource_id, env_id, name, owner_id, state, needs_env_admin, created_at, updated_at)
		VALUES (?, 'r2', 'env-gone', 'Sync r2', 'alice', 'active', 1, 1, 1), (?, 'x1', 'env-a', 'Other', 'bob', 'active', 0, 1, 1)`, kindTestSync, kindTestSync)
	f.setUser(t, "alice", "disabled = 1")
	f.recheck(t, "alice")

	got, err := f.svc.ListOwned(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("alice owns %d rows, want 2: %+v", len(got), got)
	}
	r1, r2 := got[0], got[1]
	if r1.Kind != kindTestSync || r1.Label != "Test sync" || r1.ResourceID != "r1" || r1.Name != "Sync r1" ||
		r1.EnvID != "env-a" || r1.EnvName != "Alpha" || r1.State != access.OwnedPausedDisabled || r1.NeedsEnvAdmin {
		t.Errorf("r1 = %+v", r1)
	}
	// An environment that no longer exists is named by its ID rather than left blank.
	if r2.ResourceID != "r2" || r2.EnvName != "env-gone" || !r2.NeedsEnvAdmin {
		t.Errorf("r2 = %+v", r2)
	}
	if none, err := f.svc.ListOwned(f.ctx, "carol"); err != nil || len(none) != 0 {
		t.Errorf("carol owns %+v (%v), want nothing", none, err)
	}
}

func TestTransferOwnedMovesAPausedAutomationToAnEligibleUserAndItRunsAgainUnderThem(t *testing.T) {
	f := eligibilityFixture(t)
	f.setUser(t, "alice", "disabled = 1")
	f.recheck(t, "alice")
	if got := f.state(t, kindTestSync, "r1"); got != "paused_disabled" {
		t.Fatalf("state = %s, want paused_disabled", got)
	}

	if err := f.svc.TransferOwned(f.ctx, testAdmin, kindTestSync, "r1", "bob"); err != nil {
		t.Fatal(err)
	}
	if o, s := f.ownerOf(t, kindTestSync, "r1"), f.state(t, kindTestSync, "r1"); o != "bob" || s != "active" {
		t.Fatalf("owner %s, state %s, want bob and active", o, s)
	}
	if n := f.auditCount(t, "owner.transferred"); n != 1 {
		t.Fatalf("owner.transferred entries = %d, want 1", n)
	}
	if got := f.auditDetails(t, "owner.transferred"); got != `{"from":"alice","kind":"test.sync","resource":"r1","to":"bob"}` {
		t.Fatalf("details = %s", got)
	}
	if n := f.auditCount(t, "automation.resumed"); n != 1 {
		t.Fatalf("automation.resumed entries = %d, want the move back to active written once", n)
	}
	p, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:gitsync")
	if err != nil || p.OnBehalfOf != "bob" {
		t.Fatalf("RunAs after the transfer = %+v, %v, want it to run as bob", p, err)
	}
	if left, err := f.svc.ListOwned(f.ctx, "alice"); err != nil || len(left) != 0 {
		t.Fatalf("alice still lists %+v (%v)", left, err)
	}
}

func TestTransferOwnedGivesADeletedOwnersAutomationALivingOwnerAgain(t *testing.T) {
	f := eligibilityFixture(t)
	f.setUser(t, "alice", "deleted_at = 5")
	f.recheck(t, "alice")
	if got := f.state(t, kindTestSync, "r1"); got != "paused_deleted" {
		t.Fatalf("state = %s, want paused_deleted", got)
	}
	if err := f.svc.TransferOwned(f.ctx, testAdmin, kindTestSync, "r1", "heidi"); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "active" {
		t.Fatalf("state after the transfer = %s, want active: only a transfer revives a paused_deleted row", got)
	}
}

func TestTransferOwnedToAnIneligibleUserIsRefusedAndChangesNothing(t *testing.T) {
	f := eligibilityFixture(t)
	for _, who := range []string{"carol", "dave", "erin", "frank", "alice", "nobody"} {
		err := f.svc.TransferOwned(f.ctx, testAdmin, kindTestSync, "r1", who)
		if !errors.Is(err, access.ErrNotEligible) {
			t.Errorf("transfer to %s = %v, want ErrNotEligible", who, err)
		}
	}
	if o := f.ownerOf(t, kindTestSync, "r1"); o != "alice" {
		t.Fatalf("owner = %s after refused transfers, want alice", o)
	}
	if n := f.auditCount(t, "owner.transferred"); n != 0 {
		t.Fatalf("owner.transferred entries = %d after refused transfers, want none", n)
	}
	if err := f.svc.TransferOwned(f.ctx, testAdmin, kindTestSync, "gone", "bob"); !errors.Is(err, access.ErrOwnedNotFound) {
		t.Errorf("transfer of an unknown automation = %v, want ErrOwnedNotFound", err)
	}
}

func TestTransferOwnedRechecksTheNewOwnerInsideTheTransactionAgainstTheirGrantAsItIsThen(t *testing.T) {
	f := eligibilityFixture(t)
	// The eligible list was read while bob held the grant; it is gone by the time
	// the admin confirms. The transfer reads the grant again.
	if _, err := f.svc.EligibleOwners(f.ctx, kindTestSync, "r1"); err != nil {
		t.Fatal(err)
	}
	f.unassign(t, "bob")
	if err := f.svc.TransferOwned(f.ctx, testAdmin, kindTestSync, "r1", "bob"); !errors.Is(err, access.ErrNotEligible) {
		t.Fatalf("transfer to a user who just lost the grant = %v, want ErrNotEligible", err)
	}
	if o := f.ownerOf(t, kindTestSync, "r1"); o != "alice" {
		t.Fatalf("owner = %s, want alice", o)
	}
}

// Deregistering an automation (plan 09-23): the row goes with the resource it ran for, nothing else does, and doing it
// twice is not an error.
func TestDeregisterOwnedForgetsOnlyThatAutomationAndTwiceIsNotAnError(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "operator", "env-a")
	f.register(t, syncResource("alice"))
	other := syncResource("alice")
	other.ResourceID = "r2"
	f.register(t, other)

	for range 2 {
		f.inTx(t, func(tx *sql.Tx, q *sqlc.Queries) error {
			return f.svc.DeregisterOwnedTx(f.ctx, tx, q, kindTestSync, "r1")
		})
	}
	if _, err := f.svc.GetOwned(f.ctx, kindTestSync, "r1"); !errors.Is(err, access.ErrOwnedNotFound) {
		t.Errorf("the deregistered automation = %v, want not found", err)
	}
	if got := f.state(t, kindTestSync, "r2"); got != access.OwnedActive {
		t.Errorf("the other automation = %q, want it untouched", got)
	}
	if _, _, err := f.svc.RunAs(f.ctx, kindTestSync, "r1", "system:gitsync"); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("RunAs of a deregistered automation = %v, want not found", err)
	}
}

func TestMarkNeedsEnvAdminMakesTheRecheckDemandAnAdmin(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "op")
	f.assign(t, "op", "operator", "env-a")
	f.register(t, access.OwnedResource{Kind: kindTestSync, ResourceID: "r1", EnvID: "env-a", Name: "r1", OwnerID: "op"})

	if err := f.svc.MarkNeedsEnvAdmin(f.ctx, kindTestSync, "r1"); err != nil {
		t.Fatalf("MarkNeedsEnvAdmin: %v", err)
	}
	// Marking again changes nothing and is not an error.
	if err := f.svc.MarkNeedsEnvAdmin(f.ctx, kindTestSync, "r1"); err != nil {
		t.Fatalf("MarkNeedsEnvAdmin again: %v", err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "active" {
		t.Errorf("state = %s, marking does not recheck by itself", got)
	}
	if n := f.recheck(t, "op"); n != 1 {
		t.Fatalf("recheck changed %d rows, want the one that now needs an Admin", n)
	}
	if got := f.state(t, kindTestSync, "r1"); got != "paused_lacks_permission" {
		t.Errorf("state = %s, want paused_lacks_permission", got)
	}
	if err := f.svc.MarkNeedsEnvAdmin(f.ctx, kindTestSync, "nope"); !errors.Is(err, access.ErrOwnedNotFound) {
		t.Errorf("MarkNeedsEnvAdmin of an unknown automation = %v, want ErrOwnedNotFound", err)
	}
}
