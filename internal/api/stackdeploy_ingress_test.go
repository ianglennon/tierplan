// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Every deploy path refuses a hostname the ingress policy refuses (plan 18-13, D-18): the manual deploy of a swarm
// and of a standalone stack, the git auto-deploy, rollback and restore, and Update images. The refusal comes before
// anything is stored; the claims are written by the job once the check passed (plan 24-18). The seeding is that of plan 18-11 (seedIngressWorld).

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
	// The job claimed the host once the gate and the check had passed (D-15), so the name is held by this stack.
	world, err := f.stackDeps.Ingress.World(testsupport.Context(t), e.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, held := world.Claims["free.apps.example.com"]; !held || c.StackName != "gate-free" {
		t.Errorf("claim = %+v (held %v), want the host held by the deployed stack", c, held)
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

// A label key assembled by interpolation, so that no file or value contains the prefix, is judged all the same: the
// gate reads the world for every copy, and a reserved host declared that way is refused (review WR-01, D-18).
func TestIngressGateDeployRefusesAReservedHostWhoseKeyIsAssembledFromAVariable(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "gate-assembled")
	v := saveCompose(t, admin, st.ID,
		"services:\n  web:\n    image: nginx:1.27\n    deploy:\n      labels:\n        - \"${P}.ingress.host="+seed.reservedHost+"\"\n        - \"${P}.ingress.port=80\"\n",
		`[{"name":"P","value":"tierplan","secret":false}]`)

	deployRefusedAs(t, f, admin, st.ID, v, seed.reservedHost)
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

// gateHost is a free name under the domain the seeded environment registers: a revision may name it when it deploys.
const gateHost = "gate.apps.example.com"

// reserveHost reserves host under the seeded domain after a revision that names it has deployed, as an Admin does when a
// name that was free becomes reserved.
func reserveHost(t *testing.T, f *envFix, e envBody, seed ingressWorldSeed, host string) {
	t.Helper()
	if _, _, err := f.stackDeps.Ingress.Reserve(testsupport.Context(t), ingressstore.ReserveInput{EnvID: e.ID, DomainID: seed.domainID, Hostname: host, Actor: "admin"}, nil); err != nil {
		t.Fatalf("reserve %s: %v", host, err)
	}
}

// unreserveHost lets the name go again, so the same request can be made with nothing refused.
func unreserveHost(t *testing.T, f *envFix, e envBody, seed ingressWorldSeed, host string) {
	t.Helper()
	if _, err := f.stackDeps.Ingress.Unreserve(testsupport.Context(t), e.ID, seed.domainID, host, nil); err != nil {
		t.Fatalf("unreserve %s: %v", host, err)
	}
}

// refusedAndStoredNothing fails unless rec is a 422 stack_invalid that names want, and the stack still has revisions revisions.
func refusedAndStoredNothing(t *testing.T, f *envFix, rec *httptest.ResponseRecorder, stackID, want string, revisions int) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_invalid" {
		t.Fatalf("answer: %d %.400s, want 422 stack_invalid", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("answer = %.600s, want a problem that names %q", rec.Body, want)
	}
	if n := len(f.revisions(t, stackID)); n != revisions {
		t.Errorf("revisions = %d, want %d: a refusal stores none", n, revisions)
	}
}

func TestIngressGateRollbackRefusesAHostReservedSinceTheRevisionRanOnASwarmStack(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "gate-rb")
	f.deployGood(t, operator, st.ID, ingressCompose(gateHost))
	f.deployGood(t, operator, st.ID, composeWeb128)
	reserveHost(t, f, e, seed, gateHost)
	writes := len(f.fake.Writes())

	refusedAndStoredNothing(t, f, operator.do(http.MethodPost, rollbackPath(st.ID), ""), st.ID, gateHost, 2)
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d, a refused roll back writes nothing", writes, n)
	}

	// With the name free again the same roll back starts.
	unreserveHost(t, f, e, seed, gateHost)
	rec := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("roll back with nothing reserved: %d %.300s, want 202", rec.Code, rec.Body)
	}
	if job := f.awaitDeploy(t, answerOf[rollbackStarted](t, rec).JobID); job.State != "done" {
		t.Errorf("job = %+v, want it done", job)
	}
}

// releaseClaims lets go of everything the stack holds, as happens when the stack that held a name is deployed again without
// it or deleted: a name a deployed stack still holds cannot be reserved or claimed by another, so a test that makes a name
// unavailable since a revision ran frees it first.
func releaseClaims(t *testing.T, f *envFix, e envBody, stackID string) {
	t.Helper()
	if err := f.stackDeps.Ingress.ReleaseStack(testsupport.Context(t), e.ID, stackID, "admin", nil); err != nil {
		t.Fatal(err)
	}
}

func TestIngressGateRollbackRefusesARestoreOfAHostReservedSinceItRan(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "gate-restore")
	f.deployGood(t, operator, st.ID, ingressCompose(gateHost))
	releaseClaims(t, f, e, st.ID)
	reserveHost(t, f, e, seed, gateHost)

	rec := operator.do(http.MethodPost, rollbackPath(st.ID), `{"mode":"restore","revision":1}`)

	refusedAndStoredNothing(t, f, rec, st.ID, gateHost, 1)
}

func TestIngressGateRollbackRefusesAHostReservedSinceTheRevisionRanOnAStandaloneStack(t *testing.T) {
	t.Parallel()
	f, e, up := standaloneEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "gate-rb-sa")
	f.deployGood(t, operator, st.ID, ingressStandaloneCompose(gateHost))
	f.deployGood(t, operator, st.ID, composeWeb128)
	reserveHost(t, f, e, seed, gateHost)
	_, calls := up.lastProject()

	refusedAndStoredNothing(t, f, operator.do(http.MethodPost, rollbackPath(st.ID), ""), st.ID, gateHost, 2)
	if _, after := up.lastProject(); after != calls {
		t.Errorf("the Compose stand-in ran %d times, want %d: a refused roll back runs nothing", after, calls)
	}

	unreserveHost(t, f, e, seed, gateHost)
	if rec := operator.do(http.MethodPost, rollbackPath(st.ID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("roll back with nothing reserved: %d %.300s, want 202", rec.Code, rec.Body)
	}
}

// A person's roll back names a holder in another environment only when that person may view it (D-20).
func TestIngressGateRollbackNamesAHolderOnlyToACallerWhoCanViewIt(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	instanceAdmin := &roleCaller{f: f, t: t, e: e, cookie: f.cookie, csrf: f.csrf}
	st := newStack(t, admin, "gate-holder")
	f.deployGood(t, admin, st.ID, ingressCompose(gateHost))
	f.deployGood(t, admin, st.ID, composeWeb128)
	// Another stack, in an environment the Admin has no role on, takes the name since the revision ran.
	if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(testsupport.Context(t), ingressstore.ReplaceInput{
		EnvID: seed.otherEnvID, StackID: seed.otherStackID, Actor: "bob",
		Decls: []ingress.Declaration{{Service: "web", Host: gateHost, Label: "tierplan.ingress.host", Port: 80}},
	}); err != nil {
		t.Fatalf("claim %s in the other environment: %v", gateHost, err)
	}

	hidden := admin.do(http.MethodPost, rollbackPath(st.ID), "")
	refusedAndStoredNothing(t, f, hidden, st.ID, "an environment you can't see", 2)
	for _, leak := range []string{seed.otherStack, seed.otherEnvName, seed.otherEnvID, seed.otherStackID} {
		if strings.Contains(hidden.Body.String(), leak) {
			t.Errorf("answer = %.600s, must not name %q to an Admin of one environment", hidden.Body, leak)
		}
	}

	seen := instanceAdmin.do(http.MethodPost, rollbackPath(st.ID), "")
	refusedAndStoredNothing(t, f, seen, st.ID, seed.otherStack, 2)
	if !strings.Contains(seen.Body.String(), seed.otherEnvName) {
		t.Errorf("answer = %.600s, want the environment %q named for an instance admin", seen.Body, seed.otherEnvName)
	}
}

// labelledShopCompose is the compose file of a shop whose web service names gateHost, on the swarm's deploy labels.
const labelledShopCompose = "services:\n  web:\n    image: registry.test/team/web:1\n    deploy:\n      labels:\n        tierplan.ingress.host: " + gateHost + "\n        tierplan.ingress.port: \"80\"\n"

// labelledStandaloneShopCompose is the same for a standalone host, whose labels are the service's own.
const labelledStandaloneShopCompose = "services:\n  web:\n    image: registry.test/team/web:1\n    labels:\n      tierplan.ingress.host: " + gateHost + "\n      tierplan.ingress.port: \"80\"\n  db:\n    image: registry.test/team/db:1\n"

func TestIngressGateUpdateRefusesAHostReservedSinceTheRevisionRanOnASwarmStack(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	seed := seedIngressWorld(t, s.f, s.e)
	s.f.deployGood(t, s.operator, s.st.ID, labelledShopCompose)
	s.move(t, "web", 'b')
	releaseClaims(t, s.f, s.e, s.st.ID)
	reserveHost(t, s.f, s.e, seed, gateHost)
	writtenBefore := len(s.f.fake.ServiceUpdates())

	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("service", "shop_web", map[string]string{"shop_web": updDigest('b')}))

	refusedAndStoredNothing(t, s.f, rec, s.st.ID, gateHost, 2)
	s.noNewRevision(t, 2)
	if n := len(s.f.fake.ServiceUpdates()); n != writtenBefore {
		t.Errorf("service updates went from %d to %d, a refused update writes nothing", writtenBefore, n)
	}

	// With the name free again the update goes through.
	unreserveHost(t, s.f, s.e, seed, gateHost)
	s.updateStack(t, s.operator, "service", "shop_web", map[string]string{"shop_web": updDigest('b')})
}

func TestIngressGateUpdateRefusesAHostReservedSinceTheRevisionRanOnAStandaloneStack(t *testing.T) {
	t.Parallel()
	s, up := newStandaloneShop(t, "web", "db")
	seed := seedIngressWorld(t, s.f, s.e)
	s.f.deployGood(t, s.operator, s.st.ID, labelledStandaloneShopCompose)
	s.move(t, "web", 'b')
	releaseClaims(t, s.f, s.e, s.st.ID)
	reserveHost(t, s.f, s.e, seed, gateHost)
	_, calls := up.lastProject()

	rec := s.operator.do(http.MethodPost, "updates/run", stackRunBody("stack", "solo", map[string]string{"web": updDigest('b')}))

	refusedAndStoredNothing(t, s.f, rec, s.st.ID, gateHost, 2)
	s.noNewRevision(t, 2)
	if _, after := up.lastProject(); after != calls {
		t.Errorf("the Compose stand-in ran %d times, want %d: a refused update runs nothing", after, calls)
	}

	unreserveHost(t, s.f, s.e, seed, gateHost)
	s.updateStack(t, s.operator, "stack", "solo", map[string]string{"web": updDigest('b')})
}

// A bulk update is refused by the same gate, and the item is skipped with the stack as it was. The gate passes no person
// to the policy (D-20), so a message it builds never names a holder outside the stack's own environment; the run records
// only the code, so the check below can only see that nothing of the other environment reaches the run or the job.
func TestIngressGateUpdateBulkRefusesAndNamesNoOtherEnvironment(t *testing.T) {
	t.Parallel()
	s := newShop(t, "web")
	seed := seedIngressWorld(t, s.f, s.e)
	s.f.deployGood(t, s.operator, s.st.ID, labelledShopCompose)
	s.move(t, "web", 'b')
	releaseClaims(t, s.f, s.e, s.st.ID)
	if _, err := s.f.stackDeps.Ingress.ReplaceStackRoutes(testsupport.Context(t), ingressstore.ReplaceInput{
		EnvID: seed.otherEnvID, StackID: seed.otherStackID, Actor: "bob",
		Decls: []ingress.Declaration{{Service: "web", Host: gateHost, Label: "tierplan.ingress.host", Port: 80}},
	}); err != nil {
		t.Fatalf("claim %s in the other environment: %v", gateHost, err)
	}
	instanceAdmin := &roleCaller{f: s.f, t: t, e: s.e, cookie: s.f.cookie, csrf: s.f.csrf}

	rec := s.f.call(t, authReq{
		method: http.MethodPost, path: "/api/fleet/updates/run", body: bulkBody(bulkItem(s.e.ID, "service", "shop_web", map[string]string{"shop_web": updDigest('b')})),
		cookie: instanceAdmin.cookie, csrf: instanceAdmin.csrf,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.Code, rec.Body)
	}
	ans := answerOf[bulkAnswer](t, rec)
	job := s.f.awaitUpdate(t, ans.jobOf(t, s.e.ID))

	if st := bulkItemStates(job); st["i0"] != "skipped" {
		t.Errorf("items = %v, want the stack's item skipped", st)
	}
	if revs := s.f.revisions(t, s.st.ID); len(revs) != 2 {
		t.Errorf("revisions = %+v, want the two the stack had: a refused update stores none", revs)
	}
	runs := s.f.runsOfBulk(t, ans.BulkID)
	if len(runs) != 1 || runs[0].State != updates.RunRefused || runs[0].Reason != "stack_invalid" {
		t.Fatalf("runs = %+v, want the one run refused as stack_invalid", runs)
	}
	recorded := fmt.Sprintf("%+v %+v", runs[0], job.Items)
	for _, leak := range []string{seed.otherStack, seed.otherEnvName, seed.otherEnvID, seed.otherStackID} {
		if strings.Contains(recorded, leak) {
			t.Errorf("run = %s, must not name %q", recorded, leak)
		}
	}
}
