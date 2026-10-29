// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Auto-deploy (plan 09-25, GIT-07, D-18 to D-21, D-29): a check that finds a stack with auto-deploy on behind deploys
// the head on the authority its owner holds now, through the Deployer; anything a manual deploy would refuse is held
// back and left for a person. The access service, the policy checker, the store and the repository are the real ones;
// the Deployer is a fake that does what the API's StackDeployer does to the stack's records (the real one is covered in
// the api package).

type deployCall struct {
	p       *rbac.Principal
	version int64
	trigger string
}

// fakeDeployer records the calls and takes the revision, the way the deploy route does.
type fakeDeployer struct {
	mu    sync.Mutex
	f     *autoFixture
	calls []deployCall
	// refuse, when set, is the answer of every deploy.
	refuse *gitsync.DeployRefusal
	// escalating is what a started deploy says about host access.
	escalating bool
	// failed makes the deploy end failed, naming the service, instead of good.
	failed string
}

func (d *fakeDeployer) DeployDraft(ctx context.Context, p *rbac.Principal, envID, stackID string, version int64, trigger string) (gitsync.DeployStarted, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, deployCall{p: p, version: version, trigger: trigger})
	if d.refuse != nil {
		r := *d.refuse
		return gitsync.DeployStarted{}, &r
	}
	draft, err := d.f.rr.Draft(ctx, stackID)
	if err != nil {
		return gitsync.DeployStarted{}, err
	}
	if draft.Version != version {
		return gitsync.DeployStarted{}, &gitsync.DeployRefusal{Code: "draft_changed", Detail: "The draft changed."}
	}
	rev, err := d.f.rr.NewRevision(ctx, stackID, p.Name, stackops.OriginDeploy)
	if err != nil {
		return gitsync.DeployStarted{}, err
	}
	if err := d.f.rr.SetSyncTrigger(ctx, rev.ID, trigger); err != nil {
		return gitsync.DeployStarted{}, err
	}
	rev.SyncTrigger = trigger
	if err := d.f.rr.RebaseDraft(ctx, stackID, rev.Seq); err != nil {
		return gitsync.DeployStarted{}, err
	}
	// The job ends at once here; in the server it ends later and calls the same hooks.
	if d.failed != "" {
		if err := d.f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeFailed, "", ""); err != nil {
			return gitsync.DeployStarted{}, err
		}
		st, err := d.f.rr.Stack(ctx, envID, stackID)
		if err != nil {
			return gitsync.DeployStarted{}, err
		}
		d.f.svc.DeployFailed(ctx, st, rev, d.failed)
	} else {
		if err := d.f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
			return gitsync.DeployStarted{}, err
		}
		d.f.svc.Deployed(ctx, stackID, rev)
	}
	return gitsync.DeployStarted{JobID: "job-1", Seq: rev.Seq, HostEscalating: d.escalating}, nil
}

func (d *fakeDeployer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *fakeDeployer) call(i int) deployCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[i]
}

func (d *fakeDeployer) set(fn func(*fakeDeployer)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
}

type autoFixture struct {
	*checkFixture
	acc     *access.Service
	dep     *fakeDeployer
	ownerID string
}

const ownerID = "owner-1"

// newAutoFixture is a deployed git stack, an owner with the role, and auto-deploy turned on for that owner. The stack
// is in sync at its first commit.
func newAutoFixture(t *testing.T, role string) *autoFixture {
	t.Helper()
	gitsync.RegisterOwnedKinds()
	a := &autoFixture{dep: &fakeDeployer{}, ownerID: ownerID}
	a.dep.f = a
	a.checkFixture = newCheckFixtureDeps(t, nil, func(d *gitsync.Deps) {
		a.acc = access.New(d.Repo.Store, d.Audit, rbac.NewChecker(d.Repo.Store), 0)
		d.Access, d.Deployer = a.acc, a.dep
	}, nil)
	a.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?, ?, 'x', 1, 1, 1)`, ownerID, ownerID)
	a.assign(t, role)
	a.deploy(t)
	a.enable(t)
	if out := a.poll(t); out != gitsync.OutcomeInSync {
		t.Fatalf("first check = %s, want in_sync", out)
	}
	return a
}

func (a *autoFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := a.st.Write.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (a *autoFixture) assign(t *testing.T, role string) {
	t.Helper()
	a.exec(t, `DELETE FROM role_assignments WHERE principal_id = ?`, ownerID)
	a.exec(t, `INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-1', 'user', ?, ?, 'env-1', 1)`, ownerID, role)
}

func (a *autoFixture) unassign(t *testing.T) {
	t.Helper()
	a.exec(t, `DELETE FROM role_assignments WHERE principal_id = ?`, ownerID)
}

// enable turns auto-deploy on for the owner, as the route does.
func (a *autoFixture) enable(t *testing.T) {
	t.Helper()
	err := a.rr.SetAutoDeploy(context.Background(), a.stackID, true, "", 0, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		return a.acc.RegisterOwnedTx(ctx, tx, q, access.OwnedResource{
			Kind: gitsync.KindGitAutoDeploy, ResourceID: a.stackID, EnvID: "env-1", Name: "web", OwnerID: ownerID,
		})
	})
	if err != nil {
		t.Fatalf("enable auto-deploy: %v", err)
	}
}

func (a *autoFixture) checkAs(t *testing.T, o gitsync.CheckOptions) gitsync.Outcome {
	t.Helper()
	out, err := a.svc.CheckWith(context.Background(), "env-1", a.stackID, o)
	if err != nil {
		t.Fatalf("CheckWith: %v", err)
	}
	return out
}

func (a *autoFixture) poll(t *testing.T) gitsync.Outcome {
	t.Helper()
	return a.checkAs(t, gitsync.CheckOptions{Trigger: gitsync.TriggerPoll})
}

// revisions are the revisions of the stack, oldest first, with the trigger that made each.
func (a *autoFixture) newestRevision(t *testing.T) stackops.Snapshot {
	t.Helper()
	rev, ok, err := a.rr.LatestGoodRevision(context.Background(), a.stackID)
	if err != nil || !ok {
		t.Fatalf("latest good revision: %v %v", ok, err)
	}
	return rev
}

func (a *autoFixture) ownedRow(t *testing.T) (state string, needsAdmin int) {
	t.Helper()
	if err := a.st.Read.QueryRow(`SELECT state, needs_env_admin FROM owned_resources WHERE kind = ? AND resource_id = ?`,
		gitsync.KindGitAutoDeploy, a.stackID).Scan(&state, &needsAdmin); err != nil {
		t.Fatal(err)
	}
	return state, needsAdmin
}

func (a *autoFixture) settings(t *testing.T) stackops.StackGit {
	t.Helper()
	sg, err := a.rr.StackGit(context.Background(), a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

type auditRow struct {
	ActorType, ActorID string
	Details            map[string]string
}

func (a *autoFixture) auditRows(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := a.st.Read.Query(`SELECT actor_type, actor_id, details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var details string
		if err := rows.Scan(&r.ActorType, &r.ActorID, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	return out
}

func TestAutoDeployOnFolderCommit(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	head := a.changeWeb(t, "1")

	if out := a.poll(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if n := a.dep.count(); n != 1 {
		t.Fatalf("deploys started = %d, want exactly one", n)
	}
	c := a.dep.call(0)
	if c.p.Type != rbac.PrincipalSystem || c.p.ID != "system:gitsync" || c.p.OnBehalfOf != ownerID || c.trigger != stackops.SyncPoll {
		t.Errorf("deploy as %+v for trigger %q, want system:gitsync on behalf of %s from a poll", c.p, c.trigger, ownerID)
	}

	// The revision records the commit and what started it; the job ending good brings the stack in sync.
	rev := a.newestRevision(t)
	if rev.Seq != 2 || rev.Git.Commit != head || rev.SyncTrigger != stackops.SyncPoll {
		t.Errorf("newest good revision = #%d commit %s trigger %q, want #2 at %s from a poll", rev.Seq, rev.Git.Commit, rev.SyncTrigger, head)
	}
	s := a.state(t)
	if s.Status != stackops.GitStatusInSync || s.DeployedCommit != head || s.HeadCommit != head || s.Behind != 0 || s.PendingCommit != "" {
		t.Errorf("state = %+v, want in sync at %s", s, head)
	}

	// The audit entry names the system actor, the owner, the commit and the trigger.
	rows := a.auditRows(t, audit.ActionGitAutoDeployed)
	if len(rows) != 1 {
		t.Fatalf("git.autodeployed rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.ActorType != "system" || r.ActorID != "system:gitsync" || r.Details["owner"] != ownerID || r.Details["commit"] != head ||
		r.Details["trigger"] != "poll" || r.Details["stack"] != "web" {
		t.Errorf("audit = %+v, want the system actor with the owner, commit and trigger", r)
	}

	// Looking again deploys nothing more.
	if out := a.poll(t); out != gitsync.OutcomeInSync || a.dep.count() != 1 {
		t.Errorf("second check = %s with %d deploys, want in_sync and still one", out, a.dep.count())
	}
}
