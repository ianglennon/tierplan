// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitsync_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/coaming/internal/access"
	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/gitsync"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/store/sqlc"
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
	// baseSeq is the revision the draft started from when the deploy was made: what the concurrent gate would compare.
	baseSeq int64
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
	// reverts are the lines a deploy would put back, and revertsErr makes working them out fail.
	reverts []string
	// whileWorkingOut, when set, runs while a deploy's drift is worked out: after the pull, before the deploy starts.
	whileWorkingOut func()
	revertsErr      error
}

func (d *fakeDeployer) RevertedDrift(_ context.Context, _ *rbac.Principal, _, _ string, _ int64) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.whileWorkingOut != nil {
		d.whileWorkingOut()
	}
	return d.reverts, d.revertsErr
}

func (d *fakeDeployer) DeployDraft(ctx context.Context, p *rbac.Principal, envID, stackID string, version int64, trigger string) (gitsync.DeployStarted, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	draft, err := d.f.rr.Draft(ctx, stackID)
	if err != nil {
		return gitsync.DeployStarted{}, err
	}
	d.calls = append(d.calls, deployCall{p: p, version: version, trigger: trigger, baseSeq: draft.BaseSeq})
	if d.refuse != nil {
		r := *d.refuse
		return gitsync.DeployStarted{}, &r
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

// A roll back or a drift restore makes a newer good revision and leaves the draft where it started. The automation has
// just proven the draft holds nothing but the head, so the deploy it makes starts from the newest good revision and is
// not taken for an overwrite of a newer deploy nobody saw (G-09-9).
func TestAutoDeployRebasesTheDraftOnTheNewestGoodRevision(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	ctx := context.Background()
	// Two more good revisions, as roll backs make them: the draft stays at the base it started from.
	a.deploy(t)
	newest := a.deploy(t)
	if before, err := a.rr.Draft(ctx, a.stackID); err != nil || before.BaseSeq >= newest.Seq {
		t.Fatalf("draft base = #%d (%v), want a draft behind the newest good revision #%d", before.BaseSeq, err, newest.Seq)
	}
	a.changeWeb(t, "1")

	if out := a.poll(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if a.dep.count() != 1 {
		t.Fatalf("deploys started = %d, want exactly one", a.dep.count())
	}
	if c := a.dep.call(0); c.baseSeq != newest.Seq {
		t.Errorf("the deploy saw a draft based on #%d, want the newest good revision #%d", c.baseSeq, newest.Seq)
	}
}

// A person who saves the draft after the automation read it keeps it: nothing is rebased, deployed or held back, and the
// next round looks again (T-09-103).
func TestAutoDeployDoesNotRebaseADraftSavedMeanwhile(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	ctx := context.Background()
	a.deploy(t)
	var saved stackops.Snapshot
	a.dep.set(func(d *fakeDeployer) {
		d.whileWorkingOut = func() {
			draft, err := a.rr.Draft(ctx, a.stackID)
			if err != nil {
				t.Errorf("Draft: %v", err)
				return
			}
			c := draft.Copy
			c.Vars = append([]stacks.Var(nil), c.Vars...)
			c.Vars = append(c.Vars, stacks.Var{Name: "PERSON", Value: "1"})
			if saved, _, err = a.rr.SaveDraft(ctx, a.stackID, draft.Version, "ada", c); err != nil {
				t.Errorf("SaveDraft: %v", err)
			}
		}
	})
	a.changeWeb(t, "1")

	a.poll(t)
	if n := a.dep.count(); n != 0 {
		t.Fatalf("deploys started = %d, want none: a person saved the draft meanwhile", n)
	}
	if s := a.state(t); s.ReasonCode == string(gitsync.ReasonAutoDeployBlocked) || s.PendingCommit != "" {
		t.Errorf("state = %+v, want no hold-back: the next round looks again", s)
	}
	after, err := a.rr.Draft(ctx, a.stackID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != saved.Version || after.BaseSeq != saved.BaseSeq || after.CreatedBy != "ada" {
		t.Errorf("draft = version %d base #%d by %s, want what the person left: version %d base #%d by ada", after.Version, after.BaseSeq, after.CreatedBy, saved.Version, saved.BaseSeq)
	}
}

func TestAutoDeployIsOffUntilAUserTurnsItOn(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	if err := a.rr.SetAutoDeploy(context.Background(), a.stackID, false, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	a.changeWeb(t, "1")
	if out := a.poll(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	if a.dep.count() != 0 || a.state(t).Status != stackops.GitStatusBehind {
		t.Errorf("deploys = %d, state %+v; want none and a stack waiting for review", a.dep.count(), a.state(t))
	}

	// A pause after a roll back holds it too, whoever owns it.
	if err := a.rr.SetAutoDeploy(context.Background(), a.stackID, true, stackops.PausedRollback, 1, nil); err != nil {
		t.Fatal(err)
	}
	a.changeWeb(t, "2")
	a.poll(t)
	if a.dep.count() != 0 {
		t.Errorf("deploys = %d with auto-deploy paused for a roll back, want none", a.dep.count())
	}
}

func TestAutoDeployBlockedPolicy(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	const refusal = "This stack uses settings that need Admin: a bind mount of /etc at compose.yaml:5. Ask an administrator to deploy it."
	a.dep.set(func(d *fakeDeployer) { d.refuse = &gitsync.DeployRefusal{Code: "policy_refused", Detail: refusal} })
	head := a.changeWeb(t, "bind")

	if out := a.poll(t); out != gitsync.OutcomeBehind {
		t.Fatalf("check = %s, want behind", out)
	}
	s := a.state(t)
	if a.dep.count() != 1 || s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) ||
		s.ReasonDetail != refusal || s.PendingCommit != head || s.EventKind != stackops.GitEventSkipped || s.EventDetail != refusal {
		t.Fatalf("after the refusal: %d deploys, state %+v; want error auto_deploy_blocked with the policy reason, the commit pending and the skipped event", a.dep.count(), s)
	}
	if rev := a.newestRevision(t); rev.Seq != 1 {
		t.Errorf("newest good revision = #%d, a refused deploy leaves none", rev.Seq)
	}

	// The same head is not tried again, however many rounds go by, and it keeps saying why.
	for range 3 {
		a.clock.advance(1)
		a.poll(t)
	}
	s = a.state(t)
	if a.dep.count() != 1 || s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) || s.ReasonDetail != refusal {
		t.Errorf("after more rounds: %d deploys, state %+v; want the one attempt and the same reason", a.dep.count(), s)
	}
	if n := len(a.syncFailures(t)); n != 1 {
		t.Errorf("git.sync_failed entries = %d, want 1: the same error seen again is not recorded again", n)
	}

	// A new head that passes is deployed, and the held back commit and its event are over.
	a.dep.set(func(d *fakeDeployer) { d.refuse = nil })
	next := a.changeWeb(t, "fixed")
	a.poll(t)
	s = a.state(t)
	if a.dep.count() != 2 || s.Status != stackops.GitStatusInSync || s.DeployedCommit != next || s.PendingCommit != "" || s.EventKind != "" {
		t.Errorf("after a fix: %d deploys, state %+v; want a second deploy and in sync at %s with nothing pending", a.dep.count(), s, next)
	}
}

func TestAutoDeployBlockedVarsAndClash(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		refusal gitsync.DeployRefusal
	}{
		"a missing variable":  {gitsync.DeployRefusal{Code: "not_deployable", Detail: "compose.yaml:4: required variable REGISTRY is missing a value"}},
		"a name clash":        {gitsync.DeployRefusal{Code: "name_clash", Detail: "Something Coaming doesn't manage already uses the name web."}},
		"invalid objects":     {gitsync.DeployRefusal{Code: "objects_invalid", Detail: "The secret db is not defined."}},
		"a refusal with none": {gitsync.DeployRefusal{Code: "convert_failed"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newAutoFixture(t, "operator")
			r := tc.refusal
			a.dep.set(func(d *fakeDeployer) { d.refuse = &r })
			head := a.changeWeb(t, "x")
			a.poll(t)
			s := a.state(t)
			if s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) || s.PendingCommit != head ||
				s.EventKind != stackops.GitEventSkipped || s.ReasonDetail == "" || (r.Detail != "" && s.ReasonDetail != r.Detail) {
				t.Errorf("state = %+v, want error auto_deploy_blocked with the first problem and the commit pending", s)
			}
			if rev := a.newestRevision(t); rev.Seq != 1 {
				t.Errorf("a revision #%d was made, want none", rev.Seq)
			}
		})
	}

	// A commit the checks refuse before any deploy is held back the same way for a stack that deploys by itself.
	t.Run("a required variable in the commit", func(t *testing.T) {
		t.Parallel()
		a := newAutoFixture(t, "operator")
		head := a.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": "services:\n  web:\n    image: nginx:${NEEDED:?set NEEDED}\n"}, "needs a variable")
		if out := a.poll(t); out != gitsync.OutcomeError {
			t.Fatalf("check = %s, want error", out)
		}
		s := a.state(t)
		if a.dep.count() != 0 || s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) || s.PendingCommit != head || s.EventKind != stackops.GitEventSkipped ||
			!strings.Contains(s.ReasonDetail, "NEEDED") || s.HeadCommit != head {
			t.Errorf("%d deploys, state %+v; want nothing deployed and the commit held back with its problem", a.dep.count(), s)
		}
	})
}

func TestAutoDeployRetryOnLock(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"operation_locked", "jobs_busy", "internal"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			a := newAutoFixture(t, "operator")
			a.dep.set(func(d *fakeDeployer) {
				d.refuse = &gitsync.DeployRefusal{Code: code, Detail: "busy", Retry: true}
			})
			head := a.changeWeb(t, "1")
			a.poll(t)
			s := a.state(t)
			if a.dep.count() != 1 || s.Status != stackops.GitStatusBehind || s.ReasonCode != "" || s.PendingCommit != "" || s.EventKind != "" {
				t.Fatalf("after a busy round: %d deploys, state %+v; want no error, nothing pending and no event", a.dep.count(), s)
			}
			if n := len(a.syncFailures(t)); n != 0 {
				t.Errorf("git.sync_failed entries = %d, a busy environment is not a failure", n)
			}

			// The next round, answered from what is already stored because the repository still advertises the head,
			// tries again and gets through.
			a.dep.set(func(d *fakeDeployer) { d.refuse = nil })
			a.clock.advance(1)
			a.checkAs(t, gitsync.CheckOptions{Trigger: gitsync.TriggerPoll, RemoteHead: head})
			s = a.state(t)
			if a.dep.count() != 2 || s.Status != stackops.GitStatusInSync || s.DeployedCommit != head {
				t.Errorf("after the retry: %d deploys, state %+v; want a second deploy and in sync", a.dep.count(), s)
			}
		})
	}
}

func TestAutoDeployOwnerLostPermission(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.unassign(t)
	head := a.changeWeb(t, "1")

	a.poll(t)
	if a.dep.count() != 0 {
		t.Fatalf("deploys = %d, an owner who lost the permission deploys nothing (T-09-76)", a.dep.count())
	}
	if sg := a.settings(t); !sg.AutoDeploy || sg.AutoDeployPausedReason != stackops.PausedOwner {
		t.Errorf("settings = %+v, want auto-deploy paused for the owner", sg)
	}
	if state, _ := a.ownedRow(t); state != access.OwnedPausedLacksPermission {
		t.Errorf("owned row state = %q, want %q", state, access.OwnedPausedLacksPermission)
	}
	s := a.state(t)
	if s.EventKind != stackops.GitEventPaused || !strings.Contains(s.EventDetail, "owner lacks permission") || s.Status != stackops.GitStatusBehind {
		t.Errorf("state = %+v, want the paused event and the stack still behind", s)
	}
	paused := a.auditRows(t, audit.ActionAutomationPaused)
	if len(paused) != 1 || paused[0].ActorID != "system:gitsync" || paused[0].Details["kind"] != gitsync.KindGitAutoDeploy || paused[0].Details["owner"] != ownerID {
		t.Fatalf("automation.paused rows = %+v, want one, by system:gitsync, for the owner", paused)
	}

	// Rounds that follow do not audit again or deploy.
	a.clock.advance(1)
	a.poll(t)
	if got := len(a.auditRows(t, audit.ActionAutomationPaused)); got != 1 || a.dep.count() != 0 {
		t.Errorf("after another round: %d pause entries and %d deploys, want 1 and 0", got, a.dep.count())
	}

	// Once the owner holds the permission again, the automation resumes and deploys the head.
	a.assign(t, "operator")
	a.clock.advance(1)
	a.poll(t)
	if a.dep.count() != 1 || a.state(t).DeployedCommit != head {
		t.Errorf("after the owner got the role back: %d deploys, state %+v; want the head deployed", a.dep.count(), a.state(t))
	}
	if sg := a.settings(t); sg.AutoDeployPausedReason != "" {
		t.Errorf("settings = %+v, want the pause cleared", sg)
	}
}

func TestAutoDeployNeedsEnvAdmin(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "admin")
	a.dep.set(func(d *fakeDeployer) { d.escalating = true })
	head := a.changeWeb(t, "host")

	a.poll(t)
	if a.dep.count() != 1 || a.state(t).DeployedCommit != head {
		t.Fatalf("%d deploys, state %+v; want an Admin owner's host-escalating deploy to run", a.dep.count(), a.state(t))
	}
	if state, needs := a.ownedRow(t); state != access.OwnedActive || needs != 1 {
		t.Errorf("owned row = %q, needs env admin %d; want active and marked", state, needs)
	}

	// An owner who is only an Operator now no longer qualifies for it.
	a.assign(t, "operator")
	a.changeWeb(t, "next")
	a.poll(t)
	if a.dep.count() != 1 {
		t.Errorf("deploys = %d, an Operator may not keep running what needed an Admin", a.dep.count())
	}
	if state, _ := a.ownedRow(t); state != access.OwnedPausedLacksPermission {
		t.Errorf("owned row state = %q, want paused", state)
	}
}

func TestAutoDeployDraftPending(t *testing.T) {
	t.Parallel()
	edits := map[string]func(copy *stacks.WorkingCopy){
		"a variable": func(c *stacks.WorkingCopy) { c.Vars = append(c.Vars, stacks.Var{Name: "EXTRA", Value: "1"}) },
		"a changed variable": func(c *stacks.WorkingCopy) {
			c.Vars = []stacks.Var{{Name: "TAG", Value: "changed"}}
		},
		"an overlay file": func(c *stacks.WorkingCopy) {
			c.Files = append(c.Files, stacks.File{Path: "extra.env", Content: []byte("A=1\n"), Origin: stacks.OriginOverlay})
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newAutoFixture(t, "operator")
			ctx := context.Background()
			if name == "a changed variable" {
				// The deployed revision holds TAG=1; the edit changes it.
				d, err := a.rr.Draft(ctx, a.stackID)
				if err != nil {
					t.Fatal(err)
				}
				c := d.Copy
				c.Vars = []stacks.Var{{Name: "TAG", Value: "1"}}
				if _, _, err := a.rr.SaveDraft(ctx, a.stackID, d.Version, "ada", c); err != nil {
					t.Fatal(err)
				}
				a.deploy(t)
			}
			draft, err := a.rr.Draft(ctx, a.stackID)
			if err != nil {
				t.Fatal(err)
			}
			copy := draft.Copy
			copy.Vars = append([]stacks.Var(nil), copy.Vars...)
			copy.Files = append([]stacks.File(nil), copy.Files...)
			edit(&copy)
			if _, _, err := a.rr.SaveDraft(ctx, a.stackID, draft.Version, "ada", copy); err != nil {
				t.Fatalf("SaveDraft: %v", err)
			}
			head := a.changeWeb(t, "1")

			a.poll(t)
			s := a.state(t)
			if a.dep.count() != 0 || s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonAutoDeployBlocked) ||
				!strings.Contains(s.ReasonDetail, "local changes") || s.PendingCommit != head || s.EventKind != stackops.GitEventSkipped {
				t.Fatalf("%d deploys, state %+v; want nothing deployed, error auto_deploy_blocked about local changes and the commit pending (D-29)", a.dep.count(), s)
			}
			if after, _ := a.rr.Draft(ctx, a.stackID); after.Version != draft.Version+1 {
				t.Errorf("draft version = %d, want %d: the person's edits are not touched", after.Version, draft.Version+1)
			} else if after.BaseSeq != draft.BaseSeq {
				t.Errorf("draft base = #%d, want #%d: a held-back draft is not rebased", after.BaseSeq, draft.BaseSeq)
			}

			// It keeps waiting for a person, who reviews it and deploys it by hand: the state is whole again.
			a.clock.advance(1)
			a.poll(t)
			if a.dep.count() != 0 || a.state(t).ReasonCode != string(gitsync.ReasonAutoDeployBlocked) {
				t.Errorf("after another round: %d deploys, state %+v; want the same wait", a.dep.count(), a.state(t))
			}
			if _, err := a.svc.Pull(ctx, "env-1", a.stackID, "ada", draft.Version+1); err != nil {
				t.Fatalf("Pull: %v", err)
			}
			rev := a.deploy(t)
			a.svc.Deployed(ctx, a.stackID, rev)
			s = a.state(t)
			if s.Status != stackops.GitStatusInSync || s.PendingCommit != "" || s.EventKind != "" || s.DeployedCommit != head {
				t.Errorf("after the manual deploy: state %+v, want in sync with nothing pending", s)
			}
		})
	}
}

func TestAutoDeployJobFailed(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.dep.set(func(d *fakeDeployer) { d.failed = "web" })
	head := a.changeWeb(t, "1")

	a.poll(t)
	s := a.state(t)
	if a.dep.count() != 1 || s.Status != stackops.GitStatusError || s.ReasonCode != string(gitsync.ReasonAutoDeployFailed) ||
		!strings.Contains(s.ReasonDetail, "web") || s.PendingCommit != head || s.DeployedCommit != a.sha {
		t.Fatalf("%d deploys, state %+v; want error auto_deploy_failed naming web, the last good version still the deployed one", a.dep.count(), s)
	}
	// Nothing was rolled back: the failed revision is recorded and the good one stays the newest good.
	if rev := a.newestRevision(t); rev.Seq != 1 {
		t.Errorf("newest good revision = #%d, want #1", rev.Seq)
	}

	// The failed commit is not deployed again in a loop.
	for range 3 {
		a.clock.advance(1)
		a.poll(t)
	}
	s = a.state(t)
	if a.dep.count() != 1 || s.ReasonCode != string(gitsync.ReasonAutoDeployFailed) {
		t.Errorf("after more rounds: %d deploys, state %+v; want the one attempt and the same failure", a.dep.count(), s)
	}

	// The next commit is tried again.
	a.dep.set(func(d *fakeDeployer) { d.failed = "" })
	next := a.changeWeb(t, "2")
	a.poll(t)
	if s = a.state(t); a.dep.count() != 2 || s.Status != stackops.GitStatusInSync || s.DeployedCommit != next || s.PendingCommit != "" {
		t.Errorf("after the next commit: %d deploys, state %+v; want a second try that is in sync", a.dep.count(), s)
	}
}

func TestAutoDeployCheckNowClearsBlock(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.dep.set(func(d *fakeDeployer) { d.refuse = &gitsync.DeployRefusal{Code: "name_clash", Detail: "taken"} })
	head := a.changeWeb(t, "1")
	a.poll(t)
	if a.dep.count() != 1 || a.state(t).PendingCommit != head {
		t.Fatalf("%d deploys, state %+v; want one refusal and the commit pending", a.dep.count(), a.state(t))
	}

	// Someone fixes the cause and presses Check now: the same head is tried once more.
	a.dep.set(func(d *fakeDeployer) { d.refuse = nil })
	a.clock.advance(1)
	if out := a.checkAs(t, gitsync.CheckOptions{Trigger: gitsync.TriggerManual}); out != gitsync.OutcomeBehind {
		t.Fatalf("Check now = %s, want behind", out)
	}
	s := a.state(t)
	if a.dep.count() != 2 || s.Status != stackops.GitStatusInSync || s.DeployedCommit != head || s.PendingCommit != "" {
		t.Errorf("after Check now: %d deploys, state %+v; want the head deployed", a.dep.count(), s)
	}
	if c := a.dep.call(1); c.p.ID != "system:gitsync" || c.trigger != stackops.SyncPoll {
		t.Errorf("Check now deployed as %+v with trigger %q, want system:gitsync from a poll", c.p, c.trigger)
	}
}

func TestAutoDeployWebhookActor(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	head := a.changeWeb(t, "1")

	a.checkAs(t, gitsync.CheckOptions{Trigger: gitsync.TriggerWebhook})
	if a.dep.count() != 1 {
		t.Fatalf("deploys = %d, want one", a.dep.count())
	}
	c := a.dep.call(0)
	if c.p.ID != "system:webhook" || c.p.OnBehalfOf != ownerID || c.trigger != stackops.SyncWebhook {
		t.Errorf("deploy as %+v for trigger %q, want system:webhook on behalf of %s from a webhook", c.p, c.trigger, ownerID)
	}
	if rev := a.newestRevision(t); rev.Git.Commit != head || rev.SyncTrigger != stackops.SyncWebhook {
		t.Errorf("newest good revision = commit %s trigger %q, want %s from a webhook", rev.Git.Commit, rev.SyncTrigger, head)
	}
	rows := a.auditRows(t, audit.ActionGitAutoDeployed)
	if len(rows) != 1 || rows[0].ActorID != "system:webhook" || rows[0].Details["trigger"] != "webhook" {
		t.Errorf("git.autodeployed rows = %+v, want one by system:webhook from a webhook", rows)
	}
}

func TestAutoDeployRevertsDrift(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.dep.set(func(d *fakeDeployer) {
		d.reverts = []string{"web replicas 5 → 3", "web image nginx:1.28 → nginx:1.27"}
	})
	head := a.changeWeb(t, "1")

	// The repository wins: the deploy goes ahead although it puts things back.
	if out := a.poll(t); out != gitsync.OutcomeBehind || a.dep.count() != 1 {
		t.Fatalf("check = %s with %d deploys, want behind and one deploy", out, a.dep.count())
	}
	if s := a.state(t); s.Status != stackops.GitStatusInSync || s.DeployedCommit != head {
		t.Fatalf("state = %+v, want in sync at %s", s, head)
	}

	// What it put back is on the revision it made.
	rev := a.newestRevision(t)
	lines, err := a.rr.RevertedDrift(context.Background(), a.stackID, rev.Seq)
	if err != nil || !slices.Equal(lines, a.dep.reverts) {
		t.Errorf("revision #%d reverted drift = %q, %v; want %q", rev.Seq, lines, err, a.dep.reverts)
	}
	// In the audit entry of the deploy.
	rows := a.auditRows(t, audit.ActionGitAutoDeployed)
	if len(rows) != 1 || rows[0].Details["revertedCount"] != "2" || rows[0].Details["reverted"] != "web replicas 5 → 3; web image nginx:1.28 → nginx:1.27" {
		t.Errorf("git.autodeployed rows = %+v, want the reverted lines in the details", rows)
	}
	// And in the state's event, which names the revision for the bell.
	s := a.state(t)
	if s.EventKind != stackops.GitEventReverted || s.EventDetail != strconv.FormatInt(rev.Seq, 10) || s.EventAt == nil {
		t.Errorf("state event = %q %q, want %s naming revision %d", s.EventKind, s.EventDetail, stackops.GitEventReverted, rev.Seq)
	}

	// The next deploy, which puts nothing back, ends the notice and records an empty list on its revision.
	a.dep.set(func(d *fakeDeployer) { d.reverts = nil })
	a.changeWeb(t, "2")
	a.poll(t)
	if s := a.state(t); s.EventKind != "" {
		t.Errorf("state event after a deploy that put nothing back = %q, want none", s.EventKind)
	}
	next := a.newestRevision(t)
	if lines, err := a.rr.RevertedDrift(context.Background(), a.stackID, next.Seq); err != nil || len(lines) != 0 {
		t.Errorf("revision #%d reverted drift = %q, %v; want none", next.Seq, lines, err)
	}
	if rows := a.auditRows(t, audit.ActionGitAutoDeployed); len(rows) != 2 || rows[1].Details["reverted"] != "" {
		t.Errorf("git.autodeployed rows = %+v, want the second to list nothing put back", rows)
	}
}

func TestAutoDeployUnknownDriftStillDeploys(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	a.dep.set(func(d *fakeDeployer) { d.revertsErr = errors.New("the engine did not answer") })
	head := a.changeWeb(t, "1")

	if out := a.poll(t); out != gitsync.OutcomeBehind || a.dep.count() != 1 {
		t.Fatalf("check = %s with %d deploys, want behind and one deploy", out, a.dep.count())
	}
	if s := a.state(t); s.Status != stackops.GitStatusInSync || s.DeployedCommit != head || s.EventKind != "" {
		t.Errorf("state = %+v, want in sync at %s with no event", s, head)
	}
	// It does not pretend nothing was put back: the entry says it could not tell.
	rows := a.auditRows(t, audit.ActionGitAutoDeployed)
	if len(rows) != 1 || rows[0].Details["reverted"] != "could not be worked out" {
		t.Errorf("git.autodeployed rows = %+v, want it to say the drift could not be worked out", rows)
	}
}
