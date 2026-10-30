// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The StackDeployer starts a deploy for a principal that is not on a request (plan 09-03, D-20, RESEARCH
// Pitfall 9). It goes through the very code the deploy route does, so these tests check what is different:
// who the job and the audit row belong to.

// deployerOf is the deployer of the dependencies the fixture registered the stack routes with.
func deployerOf(f *envFix) *api.StackDeployer { return api.NewStackDeployer(f.stackDeps) }

// auditedDeploy is the stack.deployed audit rows of a stack, with the actor.
type auditedDeploy struct {
	ActorType, ActorID, ActorName string
	Result                        string
	Details                       map[string]string
}

func (f *envFix) deployAudits(t *testing.T, stackID string) []auditedDeploy {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_type, actor_id, actor_name, result, details FROM audit_log WHERE action = 'stack.deployed' AND target_id = ? ORDER BY seq`, stackID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditedDeploy
	for rows.Next() {
		var a auditedDeploy
		var details string
		if err := rows.Scan(&a.ActorType, &a.ActorID, &a.ActorName, &a.Result, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &a.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, a)
	}
	return out
}

func TestStackDeployerDeploysOnBehalf(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	f.caller(t, e, "operator-1", "operator")
	const ownerID = "id-operator-1"
	st := newStack(t, admin, "auto")
	version := saveCompose(t, admin, st.ID, composeWeb127, `[]`)

	p := rbac.OnBehalf("system:gitsync", ownerID)
	started, err := deployerOf(f).DeployDraft(testsupport.Context(t), p, e.ID, st.ID, version, "poll")
	if err != nil {
		t.Fatalf("DeployDraft: %v", err)
	}
	if started.JobID == "" || started.Seq != 1 {
		t.Fatalf("started = %+v, want a job and revision 1", started)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v, want it done", job)
	}

	// The job is the system's share of the owner's slots, not the owner's own and not another owner's.
	job, ok := f.jobHub.Get(started.JobID)
	if !ok || job.Owner != "system:gitsync:"+ownerID || job.Kind != "stack_deploy" || job.EnvID != e.ID {
		t.Errorf("job = %+v (found %v), want a stack_deploy of %s owned by system:gitsync:%s", job, ok, e.ID, ownerID)
	}

	// The deploy took a revision from the draft, as a manual one does.
	revs := f.revisions(t, st.ID)
	if len(revs) != 1 || revs[0].Seq != started.Seq || revs[0].Outcome != "good" {
		t.Errorf("revisions = %+v, want revision %d good", revs, started.Seq)
	}

	// The audit row names the system actor, the user it acted for and what started it.
	audits := f.deployAudits(t, st.ID)
	if len(audits) != 1 {
		t.Fatalf("stack.deployed rows = %+v, want one", audits)
	}
	a := audits[0]
	if a.ActorType != "system" || a.ActorID != "system:gitsync" || a.Result != "success" {
		t.Errorf("audit actor = %s %s result %s, want the system actor system:gitsync", a.ActorType, a.ActorID, a.Result)
	}
	if a.Details["trigger"] != "poll" || a.Details["onBehalfOf"] != ownerID || a.Details["mode"] != "deploy" || a.Details["revision"] != "1" {
		t.Errorf("audit details = %v, want trigger poll, onBehalfOf %s, mode deploy, revision 1", a.Details, ownerID)
	}
}

// bindMountCompose asks for a host path outside the data roots, which only an env Admin may deploy.
const bindMountCompose = "services:\n  web:\n    image: nginx:1.27-alpine\n    volumes:\n      - /etc:/host-etc\n"

// refusalOf is the *DeployRefusal an error is, failing the test for any other.
func refusalOf(t *testing.T, err error) *api.DeployRefusal {
	t.Helper()
	var r *api.DeployRefusal
	if !errors.As(err, &r) {
		t.Fatalf("error = %v, want a *DeployRefusal", err)
	}
	// What an automatic deploy holds back on is read by nobody who is editing a draft or who can ask an administrator
	// at once: it is worded for an automatic deploy (plan 09-34, G-09-8). Every refusal read here meets that.
	for _, phrase := range []string{"you started editing", "your draft", "ask an administrator"} {
		if strings.Contains(strings.ToLower(r.Detail), phrase) {
			t.Errorf("refusal %s says %q, a sentence for a person at the screen: %q", r.Code, phrase, r.Detail)
		}
	}
	return r
}

// holdJobSlots starts n running jobs of owner in the hub, which end with the test.
func holdJobSlots(t *testing.T, f *envFix, envID, owner string, n int) {
	t.Helper()
	for range n {
		if _, err := f.jobHub.Start(jobs.Spec{
			Kind: "stack_deploy", EnvID: envID, Owner: owner,
			Items: []jobs.ItemSpec{{ID: "check", Label: "Check the stack", Kind: "step"}},
		}, func(ctx context.Context, _ *jobs.Reporter) { <-ctx.Done() }); err != nil {
			t.Fatal(err)
		}
	}
}

// deployerRig is an environment of one kind with an Operator owner and a stack whose draft is saved, for the
// refusal cases.
type deployerRig struct {
	f      *envFix
	e      envBody
	admin  *roleCaller
	owner  *roleCaller
	st     stackAnswer
	up     *standaloneUp
	p      *rbac.Principal
	deploy func(version int64, trigger string) error
}

func newDeployerRig(t *testing.T, target string, foreign ...dockertest.FakeService) *deployerRig {
	t.Helper()
	r := &deployerRig{}
	if target == "standalone" {
		r.f, r.e, r.up = standaloneEnv(t, nil)
	} else {
		r.f, r.e = stackEnv(t, nil, foreign...)
	}
	r.admin = r.f.caller(t, r.e, "envadmin-1", "admin")
	r.owner = r.f.caller(t, r.e, "operator-1", "operator")
	r.p = rbac.OnBehalf("system:gitsync", "id-operator-1")
	r.deploy = func(version int64, trigger string) error {
		_, err := deployerOf(r.f).DeployDraft(testsupport.Context(t), r.p, r.e.ID, r.st.ID, version, trigger)
		return err
	}
	return r
}

// stack creates the stack as the Admin and saves compose over its draft, returning the draft's version.
func (r *deployerRig) stack(t *testing.T, name, compose string) int64 {
	t.Helper()
	r.st = newStack(t, r.admin, name)
	return saveCompose(t, r.admin, r.st.ID, compose, `[]`)
}

func TestStackDeployerRefusals(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"swarm", "standalone"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			t.Run("a bind mount an Operator may not deploy is policy_refused and stores nothing", func(t *testing.T) {
				t.Parallel()
				r := newDeployerRig(t, target)
				v := r.stack(t, "bind", bindMountCompose)
				ref := refusalOf(t, r.deploy(v, "poll"))
				if ref.Code != "policy_refused" || ref.Retry {
					t.Errorf("refusal = %+v, want policy_refused without retry", ref)
				}
				if want := "This needs Admin: a bind mount of /etc at compose.yaml:5."; ref.Detail != want {
					t.Errorf("detail = %q, want %q", ref.Detail, want)
				}
				if revs := r.f.revisions(t, r.st.ID); len(revs) != 0 {
					t.Errorf("revisions = %+v, a refused deploy leaves none", revs)
				}
				if w := r.f.fake.Writes(); len(w) != 0 {
					t.Errorf("writes = %v, a refused deploy writes nothing", w)
				}
			})
			t.Run("a held operation lock is operation_locked and may be retried", func(t *testing.T) {
				t.Parallel()
				r := newDeployerRig(t, target)
				v := r.stack(t, "locked", composeWeb127)
				release, _, ok := r.f.stackLocks.TryAcquire(r.e.ID, stackops.Holder{StackID: "other-id", StackName: "other", Kind: "deploy", By: "ada", Started: time.Now().UTC()})
				if !ok {
					t.Fatal("could not take the lock")
				}
				t.Cleanup(release)
				ref := refusalOf(t, r.deploy(v, "webhook"))
				if ref.Code != "operation_locked" || !ref.Retry {
					t.Errorf("refusal = %+v, want operation_locked with retry", ref)
				}
				if revs := r.f.revisions(t, r.st.ID); len(revs) != 0 {
					t.Errorf("revisions = %+v, a refused deploy leaves none", revs)
				}
			})
			t.Run("an owner with every job slot running is jobs_busy and may be retried", func(t *testing.T) {
				t.Parallel()
				r := newDeployerRig(t, target)
				v := r.stack(t, "busy", composeWeb127)
				holdJobSlots(t, r.f, r.e.ID, "system:gitsync:id-operator-1", 5)
				ref := refusalOf(t, r.deploy(v, "poll"))
				if ref.Code != "jobs_busy" || !ref.Retry {
					t.Errorf("refusal = %+v, want jobs_busy with retry", ref)
				}
				// The lock the refused deploy took is free again.
				release, _, ok := r.f.stackLocks.TryAcquire(r.e.ID, stackops.Holder{StackID: "x", StackName: "x", Kind: "deploy", By: "ada", Started: time.Now().UTC()})
				if !ok {
					t.Error("the operation lock is still held after a refused deploy")
				} else {
					release()
				}
			})
			t.Run("a draft that moved is draft_changed", func(t *testing.T) {
				t.Parallel()
				r := newDeployerRig(t, target)
				v := r.stack(t, "moved", composeWeb127)
				ref := refusalOf(t, r.deploy(v+1, "poll"))
				if ref.Code != "draft_changed" || ref.Retry {
					t.Errorf("refusal = %+v, want draft_changed without retry", ref)
				}
			})
			t.Run("a draft that does not check clean is not_deployable", func(t *testing.T) {
				t.Parallel()
				r := newDeployerRig(t, target)
				v := r.stack(t, "broken", "services:\n  web:\n    image: [\n")
				ref := refusalOf(t, r.deploy(v, "poll"))
				if ref.Code != "not_deployable" || ref.Retry {
					t.Errorf("refusal = %+v, want not_deployable without retry", ref)
				}
			})
			t.Run("a deploy that lost a race with a roll back is concurrent_unacknowledged in automation words", func(t *testing.T) {
				t.Parallel()
				r := newDeployerRig(t, target)
				r.stack(t, "raced", composeWeb127)
				// A deploy through the route rebases the draft on revision 1; a restore then makes a newer good revision and,
				// as a roll back does, leaves the draft where it started. This is what an automatic deploy meets when the
				// roll back lands between its comparison and its deploy.
				r.f.deployGood(t, r.owner, r.st.ID, composeWeb127)
				rec := r.owner.do(http.MethodPost, rollbackPath(r.st.ID), `{"mode":"restore","revision":1}`)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("restore: %d %s", rec.Code, rec.Body)
				}
				if job := r.f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
					t.Fatalf("restore job = %+v, want it done", job)
				}
				revs := len(r.f.revisions(t, r.st.ID))

				ref := refusalOf(t, r.deploy(draftVersion(t, r.owner, r.st.ID), "poll"))
				if ref.Code != "concurrent_unacknowledged" || ref.Retry {
					t.Fatalf("refusal = %+v, want concurrent_unacknowledged without retry", ref)
				}
				if want := "Another deploy or roll back of this stack ran while the automatic deploy was starting."; ref.Detail != want {
					t.Errorf("detail = %q, want %q", ref.Detail, want)
				}
				if n := len(r.f.revisions(t, r.st.ID)); n != revs {
					t.Errorf("revisions = %d, want still %d: a refused deploy leaves none", n, revs)
				}
			})
			t.Run("a user without stacks.deploy is forbidden", func(t *testing.T) {
				t.Parallel()
				r := newDeployerRig(t, target)
				v := r.stack(t, "viewed", composeWeb127)
				r.f.caller(t, r.e, "viewer-1", "viewer")
				r.p = rbac.OnBehalf("system:gitsync", "id-viewer-1")
				ref := refusalOf(t, r.deploy(v, "poll"))
				if ref.Code != "forbidden" || ref.Retry {
					t.Errorf("refusal = %+v, want forbidden without retry", ref)
				}
			})
		})
	}

	t.Run("swarm: a name an unmanaged stack holds is name_clash", func(t *testing.T) {
		t.Parallel()
		foreign := dockertest.FakeService{ID: "s1", Name: "taken_app", Mode: "replicated", Image: "x", Labels: map[string]string{"com.docker.stack.namespace": "taken"}}
		r := newDeployerRig(t, "swarm", foreign)
		r.st = stackAnswer{ID: r.f.seedStack(t, r.e.ID, "taken", stacks.TargetSwarm)}
		v := draftVersion(t, r.admin, r.st.ID)
		ref := refusalOf(t, r.deploy(v, "poll"))
		if ref.Code != "name_clash" || ref.Retry {
			t.Errorf("refusal = %+v, want name_clash without retry", ref)
		}
	})

	t.Run("standalone: a refusal starts no Compose run", func(t *testing.T) {
		t.Parallel()
		r := newDeployerRig(t, "standalone")
		v := r.stack(t, "solo", bindMountCompose)
		if ref := refusalOf(t, r.deploy(v, "poll")); ref.Code != "policy_refused" {
			t.Errorf("refusal = %+v, want policy_refused", ref)
		}
		if _, calls := r.up.lastProject(); calls != 0 {
			t.Errorf("Compose runs = %d, want none", calls)
		}
	})
}

func TestStackDeployerOwnerSlots(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	a := f.caller(t, e, "operator-a", "operator")
	f.caller(t, e, "operator-b", "operator")
	st := newStack(t, admin, "shared")
	v := saveCompose(t, admin, st.ID, composeWeb127, `[]`)

	// Owner A has every slot of the system's share running: A's next automated deploy waits.
	holdJobSlots(t, f, e.ID, "system:gitsync:id-operator-a", 5)
	_, err := deployerOf(f).DeployDraft(testsupport.Context(t), rbac.OnBehalf("system:gitsync", "id-operator-a"), e.ID, st.ID, v, "poll")
	if ref := refusalOf(t, err); ref.Code != "jobs_busy" || !ref.Retry {
		t.Fatalf("owner A: %+v, want jobs_busy with retry", ref)
	}

	// Owner B's automated deploy runs at the same time as A's five jobs.
	started, err := deployerOf(f).DeployDraft(testsupport.Context(t), rbac.OnBehalf("system:gitsync", "id-operator-b"), e.ID, st.ID, v, "poll")
	if err != nil {
		t.Fatalf("owner B: %v", err)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v, want it done", job)
	}

	// The webhook has a share of its own, and the person's own jobs are not the system's.
	started, err = deployerOf(f).DeployDraft(testsupport.Context(t), rbac.OnBehalf("system:webhook", "id-operator-a"), e.ID, st.ID, v, "webhook")
	if err != nil {
		t.Fatalf("owner A through the webhook: %v", err)
	}
	f.awaitDeploy(t, started.JobID)
	if rec := a.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`); rec.Code != http.StatusAccepted {
		t.Errorf("owner A's own deploy: %d %s, want 202", rec.Code, rec.Body)
	}
}
