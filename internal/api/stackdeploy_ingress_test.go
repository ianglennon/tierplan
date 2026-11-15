// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Every deploy path refuses a hostname the ingress policy refuses (plan 18-13, D-18): the manual deploy of a swarm
// and of a standalone stack, the git auto-deploy, rollback and restore, and Update images. The refusal comes before
// anything is stored, and no path writes a claim (D-21). The seeding is that of plan 18-11 (seedIngressWorld).

// ingressStandaloneCompose is a standalone compose file with one service per hostname, each declaring its host on
// the service's labels (a standalone stack carries its labels there, a swarm stack under deploy.labels).
func ingressStandaloneCompose(hosts ...string) string {
	var b strings.Builder
	b.WriteString("services:\n")
	for i, h := range hosts {
		name := "web" + string(rune('a'+i))
		b.WriteString("  " + name + ":\n    image: nginx:1.27\n    labels:\n")
		b.WriteString("      tierplan.ingress.host: " + h + "\n      tierplan.ingress.port: \"80\"\n")
	}
	return b.String()
}

// deployRefusedAs fails unless the deploy of the saved draft is refused 422 stack_invalid with a problem that names
// want, and unless nothing was stored or written.
func deployRefusedAs(t *testing.T, f *envFix, c *roleCaller, stackID string, version int64, want string) {
	t.Helper()
	writes := len(f.fake.Writes())
	rec := c.deploy(stackID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_invalid" {
		t.Fatalf("deploy: %d %.400s, want 422 stack_invalid", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("answer = %.600s, want a problem that names %q", rec.Body, want)
	}
	if revs := f.revisions(t, stackID); len(revs) != 0 {
		t.Errorf("revisions = %+v, a refused deploy stores none", revs)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d, a refused deploy writes nothing", writes, n)
	}
}

func TestIngressGateDeploySwarmRefusesAReservedHost(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "gate-reserved")
	v := saveCompose(t, admin, st.ID, ingressCompose(seed.reservedHost), "[]")

	deployRefusedAs(t, f, admin, st.ID, v, seed.reservedHost)
}

func TestIngressGateDeploySwarmStartsForAFreeHostUnderTheDomain(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "gate-free")

	f.deployGood(t, admin, st.ID, ingressCompose("free.apps.example.com"))
	// The read-only gate wrote no claim: the host is still free for another stack to take.
	world, err := f.stackDeps.Ingress.World(testsupport.Context(t), e.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := world.Claims["free.apps.example.com"]; held {
		t.Error("the deploy claimed a hostname, want it left to a later phase (D-21)")
	}
}

func TestIngressGateDeployStandaloneRefusesAHostOutsideEveryDomain(t *testing.T) {
	t.Parallel()
	f, e, up := standaloneEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "gate-standalone")
	v := saveCompose(t, admin, st.ID, ingressStandaloneCompose(seed.outsideHost), "[]")

	deployRefusedAs(t, f, admin, st.ID, v, seed.outsideHost)
	if _, calls := up.lastProject(); calls != 0 {
		t.Errorf("the Compose stand-in ran %d times, want none", calls)
	}

	// A name under the registered domain deploys: the refusal was the host's.
	v = saveCompose(t, admin, st.ID, ingressStandaloneCompose("free.apps.example.com"), "[]")
	if rec := admin.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`); rec.Code != http.StatusAccepted {
		t.Fatalf("deploy of a free name: %d %.300s, want 202", rec.Code, rec.Body)
	}
}

func TestIngressGateDeployGitAutoDeployNamesNoOtherEnvironment(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	instanceAdmin := &roleCaller{f: f, t: t, e: e, cookie: f.cookie, csrf: f.csrf}
	st := newStack(t, admin, "gate-git")
	v := saveCompose(t, admin, st.ID, ingressCompose(seed.takenHost), "[]")

	// The same draft refused to an instance admin at the screen names the stack and the environment that hold it.
	rec := instanceAdmin.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_invalid" {
		t.Fatalf("deploy: %d %.400s, want 422 stack_invalid", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, seed.otherStack) || !strings.Contains(body, seed.otherEnvName) {
		t.Errorf("answer = %.600s, want the holder %q in %q named for an instance admin", body, seed.otherStack, seed.otherEnvName)
	}

	// The automatic deploy acts for an instance admin too, but its refusal is recorded where every viewer of the
	// environment reads it, so it names no holder outside the stack's own environment (D-20).
	owner := f.addUser(t, "ia-owner", true)
	_, err := api.NewStackDeployer(f.stackDeps).DeployDraft(testsupport.Context(t), rbac.OnBehalf("system:gitsync", owner), e.ID, st.ID, v, "poll")
	ref := refusalOf(t, err)
	if ref.Code != "not_deployable" || ref.Retry {
		t.Fatalf("refusal = %+v, want not_deployable without retry", ref)
	}
	for _, leak := range []string{seed.otherStack, seed.otherEnvName, seed.otherEnvID, seed.otherStackID} {
		if strings.Contains(ref.Detail, leak) {
			t.Errorf("detail = %q, must not name %q", ref.Detail, leak)
		}
	}
	if !strings.Contains(ref.Detail, "an environment you can't see") {
		t.Errorf("detail = %q, want the redacted sentence", ref.Detail)
	}
	if revs := f.revisions(t, st.ID); len(revs) != 0 {
		t.Errorf("revisions = %+v, a refused deploy stores none", revs)
	}
}

// closedIngressDeps are the deps of f with an ingress store whose database is closed, so every read of the world fails.
func closedIngressDeps(t *testing.T, f *envFix) api.StacksDeps {
	t.Helper()
	closed := storetest.New(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	deps := f.stackDeps
	deps.Ingress = ingressstore.New(ingressstore.Deps{St: closed})
	return deps
}

func TestIngressGateDeployFailsClosedWhenTheWorldCannotBeRead(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	owner := f.addUser(t, "operator-1", false)
	f.addEnvRole(t, "operator-1", e.ID, "operator")
	st := newStack(t, admin, "gate-closed")
	v := saveCompose(t, admin, st.ID, ingressCompose("free.apps.example.com"), "[]")

	_, err := api.NewStackDeployer(closedIngressDeps(t, f)).DeployDraft(testsupport.Context(t), rbac.OnBehalf("system:gitsync", owner), e.ID, st.ID, v, "poll")

	ref := refusalOf(t, err)
	if ref.Code != "internal" || !ref.Retry {
		t.Errorf("refusal = %+v, want the internal refusal, which may be retried", ref)
	}
	if revs := f.revisions(t, st.ID); len(revs) != 0 {
		t.Errorf("revisions = %+v, a refused deploy stores none", revs)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a refused deploy writes nothing", w)
	}
}

// A stack with no ingress label never reads the world, so a read that fails cannot refuse it.
func TestIngressGateDeployNoLabelsSkipsRead(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	owner := f.addUser(t, "operator-1", false)
	f.addEnvRole(t, "operator-1", e.ID, "operator")
	st := newStack(t, admin, "gate-nolabels")
	v := saveCompose(t, admin, st.ID, composeWeb127, "[]")

	started, err := api.NewStackDeployer(closedIngressDeps(t, f)).DeployDraft(testsupport.Context(t), rbac.OnBehalf("system:gitsync", owner), e.ID, st.ID, v, "poll")

	if err != nil {
		t.Fatalf("deploy: %v, want a stack with no ingress label to deploy though the world cannot be read", err)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Errorf("job = %+v, want it done", job)
	}
}

// What the compose file says decides whether the world is read, not what a variable is called: a label that
// arrives through a variable value still counts as declared.
func TestIngressGateDeployReadsTheWorldForALabelDeclaredThroughAVariable(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	owner := f.addUser(t, "operator-1", false)
	f.addEnvRole(t, "operator-1", e.ID, "operator")
	st := newStack(t, admin, "gate-var")
	v := saveCompose(t, admin, st.ID,
		"services:\n  web:\n    image: nginx:1.27\n    deploy:\n      labels:\n        - \"${ROUTE_LABEL}=free.apps.example.com\"\n",
		`[{"name":"ROUTE_LABEL","value":"tierplan.ingress.host","secret":false}]`)

	_, err := api.NewStackDeployer(closedIngressDeps(t, f)).DeployDraft(testsupport.Context(t), rbac.OnBehalf("system:gitsync", owner), e.ID, st.ID, v, "poll")

	if ref := refusalOf(t, err); ref.Code != "internal" {
		t.Errorf("refusal = %+v, want the internal refusal: the world was read because the copy names an ingress label", ref)
	}
}

// A stack that holds a hostname may deploy it again: the policy is judged for the stack itself, so its own claim is not
// "taken".
func TestIngressGateDeployDoesNotRefuseAStackItsOwnHostname(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "gate-own")
	if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(testsupport.Context(t), ingressstore.ReplaceInput{
		EnvID: e.ID, StackID: st.ID, Actor: "admin",
		Decls: []ingress.Declaration{{Service: "weba", Host: "own.apps.example.com", Label: "tierplan.ingress.host", Port: 80}},
	}); err != nil {
		t.Fatalf("claim the hostname for the stack: %v", err)
	}

	f.deployGood(t, admin, st.ID, ingressCompose("own.apps.example.com"))
}
