// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Every path that deploys a revision claims its hostnames as a manual deploy does, and nothing else touches the claims
// (plan 24-18, D-17, DEP-05, DEP-06). The seeding is that of plan 18-11 (seedIngressWorld); a stack that raced another
// for a name is staged through the BeforeJob seam, which runs after the gate and the lock and before the job.

// claimsOf are the hostnames a stack holds, sorted.
func claimsOf(t *testing.T, f *envFix, envID, stackID string) []string {
	t.Helper()
	world, err := f.stackDeps.Ingress.World(context.Background(), envID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for host, c := range world.Claims {
		if c.StackID == stackID {
			held = append(held, host)
		}
	}
	slices.Sort(held)
	return held
}

// configVersion is the config_version of the environment's ingress row.
func configVersion(t *testing.T, f *envFix, envID string) int64 {
	t.Helper()
	set, err := f.stackDeps.Ingress.Settings(context.Background(), envID)
	if err != nil {
		t.Fatal(err)
	}
	return set.ConfigVersion
}

// claimAudits counts the audit rows of claim writes.
func claimAudits(t *testing.T, f *envFix, action string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// claimActors are the actor ID and the actor name of each audit row of action, oldest first.
func claimActors(t *testing.T, f *envFix, action string) [][2]string {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_id, actor_name FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]string{id, name})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// wantNamedActor fails the test unless the rows of action are each by the person with their display name, and not
// by their ID in the name column.
func wantNamedActor(t *testing.T, f *envFix, action, name string) {
	t.Helper()
	rows := claimActors(t, f, action)
	if len(rows) == 0 {
		t.Fatalf("no %s row", action)
	}
	for _, r := range rows {
		if r[1] != name || r[0] == "" || r[0] == name {
			t.Errorf("%s actor (id, name) = %v, want the user's ID and the name %q", action, r, name)
		}
	}
}

// jobOf waits for the job and returns it whole, with the messages of its items.
func jobOf(t *testing.T, f *envFix, jobID string) jobs.Job {
	t.Helper()
	job, err := f.jobHub.Wait(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// jobItemOf is the item with the id, failing the test when the job has none.
func jobItemOf(t *testing.T, job jobs.Job, id string) jobs.Item {
	t.Helper()
	for _, it := range job.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("job has no item %q: %+v", id, job.Items)
	return jobs.Item{}
}

// deployRow is the state and code of the newest deploy row of the stack.
func deployRow(t *testing.T, f *envFix, stackID string) (state, code string) {
	t.Helper()
	if err := f.st.Read.QueryRow(`SELECT state, code FROM stack_deploys WHERE stack_id = ? ORDER BY started_at DESC, rowid DESC LIMIT 1`, stackID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	return state, code
}

// claimBeforeJob, set on the fixture before the server is built, makes the next deploy lose a race: just before its job
// starts, the stack in the second environment takes host, which the deploy's gate found free.
func claimBeforeJob(f *envFix, host string) {
	var once atomic.Bool
	f.stackBeforeJob = func(ctx context.Context) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		decl := ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 80}
		if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: ingressOtherEnvID, StackID: "stack-in-b", Actor: "bob", Decls: []ingress.Declaration{decl}}); err != nil {
			panic("the competing claim was refused: " + err.Error())
		}
	}
}

func TestIngressClaimsOnManualDeploy(t *testing.T) {
	t.Parallel()

	t.Run("a deploy stores its claim and attaches nothing while ingress is off", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-manual")

		f.deployGood(t, admin, st.ID, ingressCompose("manual.apps.example.com"))

		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"manual.apps.example.com"}) {
			t.Errorf("claims = %v, want the host of the deployed revision", got)
		}
		if n := claimAudits(t, f, "ingress.claim_replaced"); n != 1 {
			t.Errorf("ingress.claim_replaced rows = %d, want 1", n)
		}
		// The row names the person by their display name as well as their ID.
		wantNamedActor(t, f, "ingress.claim_replaced", "envadmin-1")
		creates := f.fake.ServiceCreates()
		if len(creates) == 0 {
			t.Fatal("no service was created")
		}
		for _, c := range creates {
			for _, n := range c.Spec.TaskTemplate.Networks {
				if n.Target == ingress.NetworkName {
					t.Errorf("service %s joined %s with ingress off, want nothing attached", c.Spec.Name, ingress.NetworkName)
				}
			}
		}
	})

	t.Run("a good deploy that declares no route releases the claims the stack held", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-dropped")
		f.deployGood(t, admin, st.ID, ingressCompose("dropped.apps.example.com"))
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"dropped.apps.example.com"}) {
			t.Fatalf("claims = %v, want the host of the first revision", got)
		}
		released := claimAudits(t, f, "ingress.claim_replaced") + claimAudits(t, f, "ingress.claim_released")

		f.deployGood(t, admin, st.ID, "services:\n  web:\n    image: nginx:1.27\n")

		if got := claimsOf(t, f, e.ID, st.ID); len(got) != 0 {
			t.Errorf("claims = %v, want none: the revision that runs declares no route", got)
		}
		if n := claimAudits(t, f, "ingress.claim_replaced") + claimAudits(t, f, "ingress.claim_released"); n != released+1 {
			t.Errorf("claim audit rows = %d, want %d (the release is audited)", n, released+1)
		}
	})

	t.Run("a name another stack took since the gate refuses the job", func(t *testing.T) {
		t.Parallel()
		const host = "race.apps.example.com"
		f, e := stackEnvTuned(t, func(f *envFix) { claimBeforeJob(f, host) })
		seed := seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-race")
		writes := len(f.fake.Writes())

		jobID := f.deployDraft(t, admin, st.ID, ingressCompose(host))
		job := jobOf(t, f, jobID)

		if job.State == "done" {
			t.Fatalf("job = %+v, want it failed", job)
		}
		check := jobItemOf(t, job, stackops.ItemCheck)
		if check.Code != stackops.CodeIngressRefused || !strings.HasPrefix(check.Message, stackops.CodeMessage(stackops.CodeIngressRefused)) {
			t.Errorf("check item = %+v, want ingress_refused with its sentence", check)
		}
		if strings.Contains(check.Message, seed.otherEnvID) {
			t.Errorf("message = %q, must not name the other environment's id", check.Message)
		}
		if state, code := deployRow(t, f, st.ID); state != "failed" || code != stackops.CodeIngressRefused {
			t.Errorf("deploy row = %s/%s, want failed/ingress_refused", state, code)
		}
		if n := len(f.fake.Writes()); n != writes {
			t.Errorf("writes went from %d to %d, a refused claim leaves the engine alone", writes, n)
		}
		if got := claimsOf(t, f, e.ID, st.ID); len(got) != 0 {
			t.Errorf("claims = %v, want none", got)
		}
		if got := claimsOf(t, f, seed.otherEnvID, seed.otherStackID); !slices.Equal(got, []string{host}) {
			t.Errorf("the other stack holds %v, want it left holding %s", got, host)
		}
	})

	t.Run("a standalone deploy claims the same way", func(t *testing.T) {
		t.Parallel()
		f, e, _ := standaloneEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-standalone")
		v := saveCompose(t, admin, st.ID, ingressStandaloneCompose("stand.apps.example.com"), "[]")

		rec := admin.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("deploy: %d %.300s, want 202", rec.Code, rec.Body)
		}
		if job := f.awaitDeploy(t, answerOf[struct {
			JobID string `json:"jobId"`
		}](t, rec).JobID); job.State != "done" {
			t.Fatalf("job = %+v, want it done", job)
		}
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"stand.apps.example.com"}) {
			t.Errorf("claims = %v, want the host of the deployed revision", got)
		}
	})
}

// brokenGoodStack is a stack that holds host and whose newest good revision no longer loads (a file compose-go now
// rejects), so its last good routes cannot be read.
func brokenGoodStack(t *testing.T, f *envFix, e envBody, admin *roleCaller, name, host string) stackAnswer {
	t.Helper()
	st := newStack(t, admin, name)
	saveCompose(t, admin, st.ID, "services: [this is not a compose file", "[]")
	ctx := testsupport.Context(t)
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	rev, err := repo.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	decl := ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 80}
	if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: e.ID, StackID: st.ID, Actor: "ada", Decls: []ingress.Declaration{decl}}); err != nil {
		t.Fatal(err)
	}
	return st
}

// deployOverGood deploys compose over the stack's good revision, acknowledging that someone deployed since the draft was
// started, and waits for the job to end.
func deployOverGood(t *testing.T, f *envFix, c *roleCaller, stackID, compose string) jobAnswer {
	t.Helper()
	v := saveCompose(t, c, stackID, compose, "[]")
	rec := c.deploy(stackID, `{"draftVersion":`+itoaT(v)+`,"acknowledgeConcurrent":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %.300s", rec.Code, rec.Body)
	}
	return f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID)
}

// A last good revision that cannot be loaded must not decide what a deploy does with the claims the stack holds: a
// failed deploy puts back what was held, and a good deploy that declares no route lets go of it (WR-10).
func TestIngressClaimsWhenTheLastGoodRevisionDoesNotLoad(t *testing.T) {
	t.Parallel()
	const host = "running.apps.example.com"

	t.Run("a failed deploy keeps the claims of the running stack", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the swarm rejected the service" })
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := brokenGoodStack(t, f, e, admin, "claims-broken-fails", host)

		if job := deployOverGood(t, f, admin, st.ID, ingressCompose("other.apps.example.com")); job.State == "done" {
			t.Fatalf("job = %+v, want it failed", job)
		}

		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{host}) {
			t.Errorf("claims = %v, want the host the stack held before the deploy", got)
		}
	})

	t.Run("a good deploy with no route releases the old claims", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := brokenGoodStack(t, f, e, admin, "claims-broken-drops", host)

		if job := deployOverGood(t, f, admin, st.ID, "services:\n  web:\n    image: nginx:1.27\n"); job.State != "done" {
			t.Fatalf("deploy = %+v, want it done", job)
		}

		if got := claimsOf(t, f, e.ID, st.ID); len(got) != 0 {
			t.Errorf("claims = %v, want none: the revision that runs declares no route", got)
		}
	})
}

// rollbackAs posts a roll back or a restore of the stack and waits for its job.
func rollbackAs(t *testing.T, f *envFix, c *roleCaller, stackID, body string) jobs.Job {
	t.Helper()
	rec := c.do(http.MethodPost, rollbackPath(stackID), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %.300s, want 202", rec.Code, rec.Body)
	}
	return jobOf(t, f, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec).JobID)
}

func TestIngressClaimsOnEveryPath(t *testing.T) {
	t.Parallel()

	t.Run("git", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seed := seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		owner := f.addUser(t, "ia-owner", true)
		deployer := api.NewStackDeployer(f.stackDeps)
		actor := rbac.OnBehalf("system:gitsync", owner)

		// A name another stack holds is refused by the automatic deploy, as by a manual one.
		taken := newStack(t, admin, "git-taken")
		v := saveCompose(t, admin, taken.ID, ingressCompose(seed.takenHost), "[]")
		if _, err := deployer.DeployDraft(testsupport.Context(t), actor, e.ID, taken.ID, v, "poll"); refusalOf(t, err).Code != "not_deployable" {
			t.Fatalf("deploy of a taken name: %v, want a not_deployable refusal", err)
		}
		if got := claimsOf(t, f, e.ID, taken.ID); len(got) != 0 {
			t.Errorf("claims of the refused stack = %v, want none", got)
		}

		// A free name is claimed by the job the automatic deploy starts, under the system actor.
		st := newStack(t, admin, "git-free")
		v = saveCompose(t, admin, st.ID, ingressCompose("git.apps.example.com"), "[]")
		started, err := deployer.DeployDraft(testsupport.Context(t), actor, e.ID, st.ID, v, "poll")
		if err != nil {
			t.Fatalf("deploy of a free name: %v", err)
		}
		if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
			t.Fatalf("job = %+v, want it done", job)
		}
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"git.apps.example.com"}) {
			t.Errorf("claims = %v, want the host of the deployed revision", got)
		}
		world, err := f.stackDeps.Ingress.World(context.Background(), e.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if by := world.Claims["git.apps.example.com"].ClaimedBy; by != "system:gitsync" {
			t.Errorf("claimed by %q, want the system actor of the automatic deploy", by)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-rollback")
		f.deployGood(t, admin, st.ID, ingressCompose("old.apps.example.com"))
		f.deployGood(t, admin, st.ID, ingressCompose("new.apps.example.com"))
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"new.apps.example.com"}) {
			t.Fatalf("claims before = %v, want the newer revision's host", got)
		}

		job := rollbackAs(t, f, admin, st.ID, `{"revision":1}`)

		if job.State != "done" {
			t.Fatalf("job = %+v, want it done", job)
		}
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"old.apps.example.com"}) {
			t.Errorf("claims = %v, want the restored revision's host held and the newer one's released", got)
		}
	})

	t.Run("a roll back that fails puts the claims back", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-rollback-fails")
		f.deployGood(t, admin, st.ID, ingressCompose("old.apps.example.com"))
		f.deployGood(t, admin, st.ID, ingressCompose("new.apps.example.com"))
		f.fake.RefuseServiceUpdates(st.Name+"_weba", "the swarm will not take this spec")

		job := rollbackAs(t, f, admin, st.ID, `{"revision":1}`)

		if job.State == "done" {
			t.Fatalf("job = %+v, want it failed", job)
		}
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"new.apps.example.com"}) {
			t.Errorf("claims = %v, want the claims held before the roll back", got)
		}
	})

	t.Run("restore", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-restore")
		f.deployGood(t, admin, st.ID, ingressCompose("kept.apps.example.com"))
		// Something outside the deploys let go of the name; a restore puts the deployed revision back, claim included.
		if err := f.stackDeps.Ingress.ReleaseStack(context.Background(), e.ID, st.ID, "admin", nil); err != nil {
			t.Fatal(err)
		}
		if got := claimsOf(t, f, e.ID, st.ID); len(got) != 0 {
			t.Fatalf("claims after the release = %v, want none", got)
		}

		job := rollbackAs(t, f, admin, st.ID, `{"mode":"restore","revision":1}`)

		if job.State != "done" {
			t.Fatalf("job = %+v, want it done", job)
		}
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"kept.apps.example.com"}) {
			t.Errorf("claims = %v, want the deployed revision's host held again", got)
		}
	})
}

// shopRouted is the shop stack's compose file with its one service routed to host.
func shopRouted(host string) string {
	return "services:\n  web:\n    image: registry.test/team/web:1\n    deploy:\n      labels:\n" +
		"        tierplan.ingress.host: " + host + "\n        tierplan.ingress.port: \"80\"\n"
}

// routedShop is a shop stack of one service that has claimed shop.apps.example.com, with the claims' version and claim audit
// count (written and released) read after that deploy.
func routedShop(t *testing.T, quick bool) (s *shopFix, version int64, audits int) {
	t.Helper()
	if quick {
		s = shopQuick(t, "web")
	} else {
		s = newShop(t, "web")
	}
	seedIngressWorld(t, s.f, s.e)
	s.f.deployGood(t, s.operator, s.st.ID, shopRouted("shop.apps.example.com"))
	if got := claimsOf(t, s.f, s.e.ID, s.st.ID); !slices.Equal(got, []string{"shop.apps.example.com"}) {
		t.Fatalf("claims after the deploy = %v, want the shop's host", got)
	}
	return s, configVersion(t, s.f, s.e.ID), claimAudits(t, s.f, "ingress.claim_replaced") + claimAudits(t, s.f, "ingress.claim_released")
}

// wantClaimsUntouched fails unless the shop still holds its host and no claim write was made since version and audits.
func wantClaimsUntouched(t *testing.T, s *shopFix, version int64, audits int) {
	t.Helper()
	if got := claimsOf(t, s.f, s.e.ID, s.st.ID); !slices.Equal(got, []string{"shop.apps.example.com"}) {
		t.Errorf("claims = %v, want the shop's host still held", got)
	}
	if now := configVersion(t, s.f, s.e.ID); now != version {
		t.Errorf("config_version went from %d to %d, want it unchanged", version, now)
	}
	if n := claimAudits(t, s.f, "ingress.claim_replaced") + claimAudits(t, s.f, "ingress.claim_released"); n != audits {
		t.Errorf("claim audit rows = %d, want %d (no claim write)", n, audits)
	}
}

func TestIngressClaimsUpdateIsNoOp(t *testing.T) {
	t.Parallel()
	s, version, audits := routedShop(t, false)

	for _, digest := range []byte{'b', 'c'} {
		s.move(t, "web", digest)
		s.updateStack(t, s.operator, "stack", "shop", nil)
		wantClaimsUntouched(t, s, version, audits)
	}
}

func TestIngressClaimsNotWrittenByStartAndAutoRollback(t *testing.T) {
	t.Parallel()
	s, version, audits := routedShop(t, true)

	if _, job := s.f.lifecycle(t, s.operator, stopPath(s.st.ID)); job.State != "done" {
		t.Fatalf("stop = %+v, want it done", job)
	}
	if _, job := s.f.lifecycle(t, s.operator, startPath(s.st.ID)); job.State != "done" {
		t.Fatalf("start = %+v, want it done", job)
	}
	wantClaimsUntouched(t, s, version, audits)

	// An update that fails is rolled back by TierPlan itself; the roll back claims nothing.
	s.move(t, "web", 'b')
	s.rejectNewVersion("web", 'b')
	if _, job := s.startUpdate(t, "stack", "shop"); job.State == "done" {
		t.Fatalf("update = %+v, want it failed and rolled back", job)
	}
	wantClaimsUntouched(t, s, version, audits)
}

// The start-up restore goes through the API's adaptor, so each claim write it makes is audited by the system actor (D-45).
func TestInterruptedIngressClaimsAreAuditedBySystem(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "claims-crash")
	f.deployGood(t, admin, st.ID, ingressCompose("kept.apps.example.com"))
	// The next deploy claimed another name and the process stopped before it ended.
	ctx := testsupport.Context(t)
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	rev, err := repo.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StartDeploy(ctx, st.ID, e.ID, stackops.DeployKindDeploy, rev.Seq, "job-gone-with-the-process", "ada"); err != nil {
		t.Fatal(err)
	}
	decl := ingress.Declaration{Service: "weba", Host: "lost.apps.example.com", Label: "tierplan.ingress.host", Port: 80}
	if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: e.ID, StackID: st.ID, Actor: "ada", Decls: []ingress.Declaration{decl}}); err != nil {
		t.Fatal(err)
	}
	interrupted, err := repo.MarkInterrupted(ctx)
	if err != nil || len(interrupted) != 1 {
		t.Fatalf("MarkInterrupted = %+v, %v, want the stack", interrupted, err)
	}
	before := claimAudits(t, f, "ingress.claim_replaced")

	if err := api.RestoreInterruptedIngressClaims(ctx, f.stackDeps, interrupted); err != nil {
		t.Fatalf("RestoreInterruptedIngressClaims: %v", err)
	}

	if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"kept.apps.example.com"}) {
		t.Errorf("claims = %v, want the last good revision's host", got)
	}
	if n := claimAudits(t, f, "ingress.claim_replaced"); n != before+1 {
		t.Errorf("ingress.claim_replaced rows = %d, want %d", n, before+1)
	}
	var typ, id string
	if err := f.st.Read.QueryRow(`SELECT actor_type, actor_id FROM audit_log WHERE action = 'ingress.claim_replaced' ORDER BY seq DESC LIMIT 1`).Scan(&typ, &id); err != nil {
		t.Fatal(err)
	}
	if typ != "system" || !strings.HasPrefix(id, "system:") {
		t.Errorf("actor = %s/%s, want the system actor", typ, id)
	}
}
