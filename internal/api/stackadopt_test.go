// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/stackops/stackopstest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Adopting a stack TierPlan did not make (plan 08-13, STK-13, D-14): the route takes the fingerprint the page read,
// rebuilds the definition itself and stores the stack with revision 1, and sends nothing to an engine.

// adoptPath is the path of the adopt route of a name, after /api/env/{envId}/.
func adoptPath(name string) string { return unmanagedPath(name, "/adopt") }

// fingerprintOf reads the fingerprint of an unmanaged stack's page, as the SPA does before it adopts.
func fingerprintOf(t *testing.T, c *roleCaller, name string) string {
	t.Helper()
	rec := c.do(http.MethodGet, unmanagedPath(name, ""), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged stack %s: %d %s", name, rec.Code, rec.Body)
	}
	var got struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint == "" {
		t.Fatalf("the page of %s carries no fingerprint: %s", name, rec.Body)
	}
	return got.Fingerprint
}

func adoptBody(fingerprint string) string {
	b, _ := json.Marshal(map[string]string{"fingerprint": fingerprint})
	return string(b)
}

func TestAdoptStackRoute(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 2, 2, nil), foreignSecretService("legacy", "db", 1, 1, nil))
	fp := fingerprintOf(t, admin, "legacy")
	writesBefore := len(f.fake.Writes())

	// D-50: adopting needs stacks.create, which an Operator does not hold.
	rec := operator.do(http.MethodPost, adoptPath("legacy"), adoptBody(fp))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("adopt as an operator: %d %s, want 403", rec.Code, rec.Body)
	}
	if got := f.stackAudits(t); len(got) != 0 {
		t.Errorf("a refused adoption was audited as %+v", got)
	}

	rec = admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fp))
	if rec.Code != http.StatusCreated {
		t.Fatalf("adopt as an admin: %d %s, want 201", rec.Code, rec.Body)
	}
	got := answerOf[stackAnswer](t, rec)
	if got.ID == "" || got.Name != "legacy" || got.Target != "swarm" || got.Source != "adopted" || !got.Managed {
		t.Errorf("answer = %+v, want a managed adopted swarm stack named legacy", got)
	}
	if got.HasDraft {
		t.Errorf("an adopted stack shows a draft with changes, want none (D-14)")
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("adopting sent %d writes to the engine, want none (D-14)", after-writesBefore)
	}

	audits := f.stackAudits(t)
	if len(audits) != 1 || audits[0].Action != audit.ActionStackAdopted || audits[0].TargetID != got.ID || audits[0].EnvID != e.ID || audits[0].Result != "success" {
		t.Fatalf("audit rows = %+v, want one stack.adopted for the stack", audits)
	}
	d := audits[0].Details
	if d["stack"] != "legacy" || d["target"] != "swarm" || d["source"] != "legacy" || d["services"] != "2" || d["revision"] != "1" {
		t.Errorf("audit details = %v, want the stack, target, source, 2 services and revision 1", d)
	}
	for k, v := range d {
		for _, secret := range sdSecrets {
			if strings.Contains(v, secret) {
				t.Errorf("audit detail %s carries a value of the stack: %q", k, v)
			}
		}
	}

	// The definition is the server's: the stored draft is the rebuild from what runs, with its values as literals.
	draft := admin.do(http.MethodGet, "stacks/"+got.ID+"/draft", "")
	if draft.Code != http.StatusOK {
		t.Fatalf("draft: %d %s", draft.Code, draft.Body)
	}
	da := answerOf[draftAnswer](t, draft)
	if len(da.Files) != 1 || !strings.Contains(da.Files[0].Content, "image: nginx:1.27") || !strings.Contains(da.Files[0].Content, sdSecrets["env"]) {
		t.Errorf("draft = %+v, want the rebuilt compose file with the environment values as literals (D-15)", da)
	}
}

// TestAdoptRefusesAStackThatChangedSinceTheCheck: a fingerprint that is not what runs stores nothing (T-08-46).
func TestAdoptRefusesAStackThatChangedSinceTheCheck(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 2, 2, nil))
	fp := fingerprintOf(t, admin, "legacy")
	// Somebody updated the service after the page was read: the engine raised its version.
	svc := foreignSecretService("legacy", "app", 2, 2, nil)
	svc.Version = 9
	reconfigureSwarm(t, f, e, svc)

	rec := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fp))

	if rec.Code != http.StatusConflict || problemType(t, rec) != "adopt_stale" {
		t.Fatalf("adopt with an old fingerprint: %d %s, want 409 adopt_stale", rec.Code, rec.Body)
	}
	if list := admin.do(http.MethodGet, "stacks", ""); strings.Contains(list.Body.String(), `"managed":true`) {
		t.Errorf("a stale adoption stored a stack: %s", list.Body)
	}
	// Reading again gives the new fingerprint, and that adopts.
	if rec := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fingerprintOf(t, admin, "legacy"))); rec.Code != http.StatusCreated {
		t.Errorf("adopt after checking again: %d %s, want 201", rec.Code, rec.Body)
	}
}

// TestAdoptRefusesTierPlansOwnStack: the stack TierPlan runs in is refused with the reason the list shows (D-49).
func TestAdoptRefusesTierPlansOwnStack(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	agent := foreignSwarmService("fleet", "agent", 1, 1, map[string]string{"tierplan.agent": "true"})
	reconfigureSwarm(t, f, e, agent, foreignSwarmService("my.stack", "web", 1, 1, nil))

	for name, reason := range map[string]string{"fleet": "This stack runs TierPlan's agent.", "my.stack": "can't adopt my.stack"} {
		rec := admin.do(http.MethodPost, adoptPath(name), adoptBody("anything"))
		if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_adoptable" || !strings.Contains(rec.Body.String(), reason) {
			t.Errorf("adopt %s: %d %s, want 409 stack_not_adoptable saying %q", name, rec.Code, rec.Body, reason)
		}
	}
}

func TestAdoptOfANameNothingCarriesIs404(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")

	rec := admin.do(http.MethodPost, adoptPath("ghost"), adoptBody("x"))

	if rec.Code != http.StatusNotFound || problemType(t, rec) != "unmanaged_stack_not_found" {
		t.Errorf("adopt ghost: %d %s, want 404 unmanaged_stack_not_found", rec.Code, rec.Body)
	}
}

// TestAdoptTwiceIsAlreadyManaged: the second adoption of a stack answers with the managed stack's ID, and stores nothing.
func TestAdoptTwiceIsAlreadyManaged(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 1, 1, nil))
	fp := fingerprintOf(t, admin, "legacy")
	first := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fp))
	if first.Code != http.StatusCreated {
		t.Fatalf("first adopt: %d %s", first.Code, first.Body)
	}
	id := answerOf[stackAnswer](t, first).ID

	second := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fp))

	if second.Code != http.StatusConflict || problemType(t, second) != "stack_already_managed" {
		t.Fatalf("second adopt: %d %s, want 409 stack_already_managed", second.Code, second.Body)
	}
	var problem struct {
		Errors []struct{ Location, Message string } `json:"errors"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "stackId" || problem.Errors[0].Message != id {
		t.Errorf("errors = %+v, want the managed stack's ID %s as stackId", problem.Errors, id)
	}
	if n := len(f.stackAudits(t)); n != 1 {
		t.Errorf("%d audit rows after two adoptions, want the one", n)
	}
}

// TestAdoptConcurrentOnlyOneWins: two Admins adopt the same stack at once; one stack, one 409.
func TestAdoptConcurrentOnlyOneWins(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	first := f.caller(t, e, "envadmin-1", "admin")
	second := f.caller(t, e, "envadmin-2", "admin")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 1, 1, nil))
	fp := fingerprintOf(t, first, "legacy")

	recs := make([]*httptest.ResponseRecorder, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, c := range []*roleCaller{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recs[i] = c.do(http.MethodPost, adoptPath("legacy"), adoptBody(fp))
		}()
	}
	close(start)
	wg.Wait()

	created, conflicts := 0, 0
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
			if typ := problemType(t, rec); typ != "stack_already_managed" && typ != "operation_locked" {
				t.Errorf("the loser answered %s, want stack_already_managed or operation_locked", rec.Body)
			}
		default:
			t.Errorf("an adoption answered %d %s", rec.Code, rec.Body)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("answers = %d created, %d conflicts, want one of each", created, conflicts)
	}
	list := first.do(http.MethodGet, "stacks", "")
	if n := strings.Count(list.Body.String(), `"managed":true`); n != 1 {
		t.Errorf("the list holds %d managed stacks after two adoptions, want 1: %s", n, list.Body)
	}
}

// TestAdoptedStackListsItsServices: right after adopting, the list shows the stack managed with source adopted and the
// state of what runs, and its Overview lists the services (D-14, ownership of an adopted stack).
func TestAdoptedStackListsItsServices(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	// A service of the namespace has a spec to rebuild from, as one made by docker stack deploy has.
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 2, 2, nil), foreignSecretService("legacy", "db", 0, 1, nil),
		foreignSwarmService("other", "app", 1, 1, nil))
	rec := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fingerprintOf(t, admin, "legacy")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body)
	}
	st := answerOf[stackAnswer](t, rec)

	listed := listedStack(t, viewer, st.ID)
	if !listed.Managed || listed.Source != "adopted" || listed.Unmanaged != nil {
		t.Errorf("list row = %+v, want a managed adopted stack", listed)
	}
	if listed.State != "degraded" {
		t.Errorf("list state = %q, want degraded (app runs, db does not), not Not deployed", listed.State)
	}
	wantCounts(t, listed, 1, 2)
	if page := readStackAnswer(t, viewer, st.ID); page.State != "degraded" {
		t.Errorf("page state = %q, want degraded", page.State)
	}

	svcs := viewer.do(http.MethodGet, "stacks/"+st.ID+"/services", "")
	if svcs.Code != http.StatusOK {
		t.Fatalf("services: %d %s", svcs.Code, svcs.Body)
	}
	var overview struct {
		Items []struct{ Name, Short, State string } `json:"items"`
	}
	if err := json.Unmarshal(svcs.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Items) != 2 || overview.Items[0].Name != "legacy_app" || overview.Items[1].Name != "legacy_db" {
		t.Errorf("overview = %+v, want legacy's two services and not other's", overview.Items)
	}

	// The unmanaged page of a name that is managed now is gone.
	if gone := viewer.do(http.MethodGet, unmanagedPath("legacy", ""), ""); gone.Code != http.StatusNotFound {
		t.Errorf("unmanaged page of an adopted stack: %d, want 404", gone.Code)
	}
}

// TestAdoptNeverTakesADefinitionFromTheBrowser: a request that carries a compose file stores the rebuild from what
// runs, never what the request says (prohibition STK-13).
func TestAdoptNeverTakesADefinitionFromTheBrowser(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 1, 1, nil))
	fp := fingerprintOf(t, admin, "legacy")

	rec := admin.do(http.MethodPost, adoptPath("legacy"),
		`{"fingerprint":"`+fp+`","compose":"services:\n  evil:\n    image: evil:1\n","files":[{"path":"compose.yaml","content":"services: {evil: {image: evil:1}}"}]}`)

	if rec.Code == http.StatusCreated {
		st := answerOf[stackAnswer](t, rec)
		draft := admin.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
		if strings.Contains(draft.Body.String(), "evil") {
			t.Errorf("the stored draft holds what the request said: %s", draft.Body)
		}
		return
	}
	if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Errorf("adopt with a definition in the body: %d %s, want it ignored or refused", rec.Code, rec.Body)
	}
	if list := admin.do(http.MethodGet, "stacks", ""); strings.Contains(list.Body.String(), `"managed":true`) {
		t.Errorf("a refused request stored a stack: %s", list.Body)
	}
}

// TestAdoptStandaloneProject: a docker compose up project is adopted the same way, and its stack reads Running.
func TestAdoptStandaloneProject(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	standOnHost(f, stackopstest.Foreign(t, "shop", foreignStandaloneCompose))
	fp := fingerprintOf(t, admin, "shop")
	writesBefore := len(f.fake.Writes())

	rec := admin.do(http.MethodPost, adoptPath("shop"), adoptBody(fp))

	if rec.Code != http.StatusCreated {
		t.Fatalf("adopt a standalone project: %d %s", rec.Code, rec.Body)
	}
	st := answerOf[stackAnswer](t, rec)
	if st.Target != "standalone" || st.Source != "adopted" {
		t.Errorf("stack = %+v, want an adopted standalone stack", st)
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("adopting sent %d writes to the host, want none", after-writesBefore)
	}
	listed := listedStack(t, viewer, st.ID)
	if listed.State != "running" {
		t.Errorf("list state = %q, want running", listed.State)
	}
	wantCounts(t, listed, 2, 2)
	svcs := viewer.do(http.MethodGet, "stacks/"+st.ID+"/services", "")
	if svcs.Code != http.StatusOK || !strings.Contains(svcs.Body.String(), `"web"`) || !strings.Contains(svcs.Body.String(), `"db"`) {
		t.Errorf("services: %d %s, want web and db", svcs.Code, svcs.Body)
	}
}

// TestAdoptIsRefusedWhileAnotherOperationRuns: only one stack operation at a time runs on an environment, and the
// refusal names the holder, with its stack's ID so the page can open it.
func TestAdoptIsRefusedWhileAnotherOperationRuns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil, foreignSecretService("legacy", "app", 1, 1, nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha := newStack(t, admin, "alpha")
	jobID := startStuckDeploy(t, f, operator, alpha)
	fp := fingerprintOf(t, admin, "legacy")

	rec := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fp))

	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Fatalf("adopt during a deploy: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	var named []string
	for _, er := range problemOf(t, rec).Errors {
		named = append(named, er.Location+"="+er.Message)
	}
	for _, want := range []string{"stack=alpha", "job=" + jobID, "stackId=" + alpha.ID} {
		if !slices.Contains(named, want) {
			t.Errorf("errors = %v, want %s", named, want)
		}
	}
	if list := admin.do(http.MethodGet, "stacks", ""); strings.Contains(list.Body.String(), `"name":"legacy","target":"swarm","source":"adopted"`) {
		t.Errorf("a refused adoption stored a stack: %s", list.Body)
	}
}

// TestAdoptedStackHistoryHoldsRevisionOne: the History of a freshly adopted stack is the one revision it started
// with: adopted from the namespace, good, pinned to what runs, and the one the stack stands at.
func TestAdoptedStackHistoryHoldsRevisionOne(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 1, 1, nil), foreignSecretService("legacy", "db", 1, 1, nil))
	rec := admin.do(http.MethodPost, adoptPath("legacy"), adoptBody(fingerprintOf(t, admin, "legacy")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body)
	}
	st := answerOf[stackAnswer](t, rec)

	rec = viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("revisions: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackRevisionsAnswer](t, rec)
	if len(got.Items) != 1 {
		t.Fatalf("revisions = %+v, want the one", got.Items)
	}
	r := got.Items[0]
	if r.Seq != 1 || r.Origin != "adopted" || r.Outcome != "good" || !r.Deployed || r.Pinned != 2 || r.AdoptedFrom != "legacy" || r.CreatedBy != "envadmin-1" {
		t.Errorf("revision = %+v, want #1, adopted from legacy by envadmin-1, good, deployed, 2 images pinned", r)
	}
}

// adoptCheckPath is the path of the adopt check route of a name, after /api/env/{envId}/.
func adoptCheckPath(name string) string { return unmanagedPath(name, "/adopt-check") }

// adoptCheckAnswer is the part of the check's answer the route tests read.
type adoptCheckAnswer struct {
	Fingerprint   string            `json:"fingerprint"`
	Clean         bool              `json:"clean"`
	GapCount      int               `json:"gapCount"`
	Gaps          []json.RawMessage `json:"gaps"`
	OwnershipOnly []string          `json:"ownershipOnly"`
}

// TestAdoptCheckRoute: anyone who may read stacks may run the check (a Viewer included), it changes nothing, what it
// answers is masked for a reader without secrets.reveal, and a user may hold two at once (429 check_busy).
func TestAdoptCheckRoute(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var hold atomic.Bool
	var calls atomic.Int32
	f, e := stackEnvTuned(t, func(f *envFix) {
		// While hold is set the first two checks are held in flight; the others pass through.
		f.stackOnAdoptCheck = func(context.Context) {
			if hold.Load() && calls.Add(1) <= 2 {
				entered <- struct{}{}
				<-release
			}
		}
	})
	viewer := f.caller(t, e, "viewer-1", "viewer")
	holder := f.caller(t, e, "holder-1", "viewer")
	other := f.caller(t, e, "other-1", "viewer")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 2, 2, nil), foreignSecretService("legacy", "db", 1, 1, nil))
	writesBefore := len(f.fake.Writes())

	rec := viewer.do(http.MethodPost, adoptCheckPath("legacy"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("check as a viewer: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	got := answerOf[adoptCheckAnswer](t, rec)
	if got.Fingerprint != fingerprintOf(t, viewer, "legacy") {
		t.Errorf("the check's fingerprint %q is not the page's: Adopt could never be sent with it", got.Fingerprint)
	}
	if got.Clean != (got.GapCount == 0) || got.GapCount != len(got.Gaps) {
		t.Errorf("answer = %+v, want Clean to say there are no gaps and gapCount to count them", got)
	}
	for kind, secret := range sdSecrets {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("a viewer's check carries the %s value %q: %s", kind, secret, rec.Body)
		}
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("the check sent %d writes to the engine, want none", after-writesBefore)
	}
	if list := viewer.do(http.MethodGet, "stacks", ""); strings.Contains(list.Body.String(), `"managed":true`) {
		t.Errorf("the check stored a stack: %s", list.Body)
	}

	hold.Store(true)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- holder.do(http.MethodPost, adoptCheckPath("legacy"), "").Code
		}()
	}
	testsupport.Await(t, "the first check to be in flight", entered)
	testsupport.Await(t, "the second check to be in flight", entered)

	// Two are in flight for this user: the third is refused, and says why.
	third := holder.do(http.MethodPost, adoptCheckPath("legacy"), "")
	if third.Code != http.StatusTooManyRequests || problemType(t, third) != "check_busy" {
		t.Errorf("a third check: %d %s, want 429 check_busy", third.Code, third.Body)
	}
	// Another user is unaffected.
	if rec := other.do(http.MethodPost, adoptCheckPath("legacy"), ""); rec.Code != http.StatusOK {
		t.Errorf("another user's check: %d %s, want 200", rec.Code, rec.Body)
	}

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a held check ended with %d, want 200", code)
		}
	}
	// The slots are given back.
	if rec := holder.do(http.MethodPost, adoptCheckPath("legacy"), ""); rec.Code != http.StatusOK {
		t.Errorf("a check after the held ones ended: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := viewer.do(http.MethodPost, adoptCheckPath("ghost"), ""); rec.Code != http.StatusNotFound || problemType(t, rec) != "unmanaged_stack_not_found" {
		t.Errorf("check of a name nothing carries: %d %s, want 404 unmanaged_stack_not_found", rec.Code, rec.Body)
	}
}

// TestAdoptCheckBodyCarriesObjectsAndFlags: the check names how each piece of the stack is treated with the counts of
// each kind, the settings that need an Admin to deploy (listed for the Admin who asks too), and the Portainer and
// orphan-ID flags.
func TestAdoptCheckBodyCarriesObjectsAndFlags(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	svc := foreignSecretService("legacy", "app", 2, 2, map[string]string{
		"io.portainer.accesscontrol.teams": "ops", "tierplan.stack.id": "made-before",
	})
	cs := svc.Spec.TaskTemplate.ContainerSpec
	cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"})
	reconfigureSwarm(t, f, e, svc)

	rec := admin.do(http.MethodPost, adoptCheckPath("legacy"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s, want 200", rec.Code, rec.Body)
	}

	var got struct {
		Objects []struct{ Kind, Name, Treatment string } `json:"objects"`
		Counts  struct{ Networks, Volumes, Secrets, Configs int }
		Policy  struct {
			Kinds   []string
			Entries []struct{ Text string }
		}
		Portainer bool
		Orphan    bool
		Notes     []struct{ Service, Code, Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if !got.Portainer || !got.Orphan {
		t.Errorf("portainer = %v, orphan = %v, want both: the service carries Portainer's label and another stack's ID", got.Portainer, got.Orphan)
	}
	if !slices.Contains(got.Policy.Kinds, "bind mounts") {
		t.Errorf("policy kinds = %v, want bind mounts for the Docker socket, whoever asks", got.Policy.Kinds)
	}
	var texts []string
	for _, en := range got.Policy.Entries {
		texts = append(texts, en.Text)
	}
	if want := "a bind mount of /var/run/docker.sock in app"; !slices.Contains(texts, want) {
		t.Errorf("policy entries = %q, want %q", texts, want)
	}
	counted := got.Counts.Networks + got.Counts.Volumes + got.Counts.Secrets + got.Counts.Configs
	if len(got.Objects) == 0 || counted != len(got.Objects) {
		t.Errorf("objects = %+v with counts %+v, want the counts to add up to the objects", got.Objects, got.Counts)
	}
	for _, o := range got.Objects {
		if o.Treatment != "owned" && o.Treatment != "external" {
			t.Errorf("object %+v has no treatment", o)
		}
		if (o.Kind == "secret" || o.Kind == "config") && o.Treatment != "external" {
			t.Errorf("object %+v: an existing secret or config is always external", o)
		}
	}
	if got.Notes == nil {
		t.Errorf("notes is absent, want a list")
	}
}

// pastedBody is a check or adopt body that starts from a pasted file.
func pastedBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	fields["source"] = "paste"
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAdoptPasteRoute: a pasted compose file is checked by a caller who may reveal secrets, shown with its variables and its differences,
// refused when it has an error, and adopted exactly as written by an Env Admin, whatever a check said before (T-08-56,
// T-08-57).
func TestAdoptPasteRoute(t *testing.T) {
	t.Parallel()
	const pasted = "# What Portainer deployed.\nservices:\n  app:\n    image: nginx:${TAG}   # keep\n    environment:\n      TOKEN: ${TOKEN}\n  db:\n    image: nginx:1.27\n"
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 2, 2, nil), foreignSecretService("legacy", "db", 1, 1, nil))
	vars := []map[string]any{{"name": "TAG", "value": "1.27", "secret": false}, {"name": "TOKEN", "value": "tok-value-9876", "secret": true}}
	writesBefore := len(f.fake.Writes())

	// An Env Admin may check a pasted file; the answer carries the variables and never a secret value.
	rec := admin.do(http.MethodPost, adoptCheckPath("legacy"), pastedBody(t, map[string]any{"compose": pasted, "vars": vars}))
	if rec.Code != http.StatusOK {
		t.Fatalf("check of a pasted file: %d %s, want 200", rec.Code, rec.Body)
	}
	var checked struct {
		Source    string `json:"source"`
		Blocked   bool   `json:"blocked"`
		Matches   bool   `json:"matches"`
		Variables []struct {
			Name     string `json:"name"`
			Secret   bool   `json:"secret"`
			Required bool   `json:"required"`
			Value    string `json:"value"`
		} `json:"variables"`
		Problems []json.RawMessage `json:"problems"`
		Gaps     []json.RawMessage `json:"gaps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &checked); err != nil {
		t.Fatal(err)
	}
	if checked.Source != "paste" || checked.Blocked || len(checked.Variables) != 2 {
		t.Fatalf("answer = %+v, want a pasted file that is not blocked and uses two variables", checked)
	}
	if strings.Contains(rec.Body.String(), "tok-value-9876") {
		t.Errorf("the check answered a secret value: %s", rec.Body)
	}

	// An unfilled variable is a problem and blocks; the variable says it is required.
	rec = admin.do(http.MethodPost, adoptCheckPath("legacy"), pastedBody(t, map[string]any{"compose": pasted}))
	if rec.Code != http.StatusOK {
		t.Fatalf("check without values: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &checked); err != nil || !checked.Blocked || len(checked.Gaps) != 0 {
		t.Errorf("check without values = %+v, %v, want it blocked with nothing compared", checked, err)
	}
	required := 0
	for _, v := range checked.Variables {
		if v.Required {
			required++
		}
	}
	if required != 2 {
		t.Errorf("%d variables required, want 2: %+v", required, checked.Variables)
	}

	// An include is refused, not read.
	rec = admin.do(http.MethodPost, adoptCheckPath("legacy"), pastedBody(t, map[string]any{"compose": "include:\n  - /etc/hostname\nservices:\n  app:\n    image: nginx:1.27\n"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("check of an include: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &checked); err != nil || !checked.Blocked || len(checked.Problems) == 0 {
		t.Errorf("check of an include = %+v, %v, want it blocked with a problem", checked, err)
	}

	fp := fingerprintOf(t, admin, "legacy")
	// Needs stacks.create, as the rebuild does.
	if rec := operator.do(http.MethodPost, adoptPath("legacy"), pastedBody(t, map[string]any{"fingerprint": fp, "compose": pasted, "vars": vars})); rec.Code != http.StatusForbidden {
		t.Errorf("paste-adopt as an operator: %d %s, want 403", rec.Code, rec.Body)
	}
	// The commit checks again: a file with an error is refused whatever the page was told, and nothing is stored.
	rec = admin.do(http.MethodPost, adoptPath("legacy"), pastedBody(t, map[string]any{"fingerprint": fp, "compose": "include:\n  - /etc/hostname\nservices:\n  app:\n    image: nginx:1.27\n"}))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "pasted_file_invalid" {
		t.Errorf("paste-adopt of an include: %d %s, want 422 pasted_file_invalid", rec.Code, rec.Body)
	}
	rec = admin.do(http.MethodPost, adoptPath("legacy"), pastedBody(t, map[string]any{"fingerprint": fp, "compose": pasted, "vars": []map[string]any{{"name": "TAG", "value": "1.27", "secret": false}, {"name": "TOKEN", "value": "abc", "secret": true}}}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("paste-adopt with a three-character secret: %d %s, want 422", rec.Code, rec.Body)
	}
	if list := admin.do(http.MethodGet, "stacks", ""); strings.Contains(list.Body.String(), `"managed":true`) {
		t.Errorf("a refused paste stored a stack: %s", list.Body)
	}

	rec = admin.do(http.MethodPost, adoptPath("legacy"), pastedBody(t, map[string]any{"fingerprint": fp, "compose": pasted, "vars": vars}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("paste-adopt as an admin: %d %s, want 201", rec.Code, rec.Body)
	}
	st := answerOf[stackAnswer](t, rec)
	if st.Source != "adopted" || !st.Managed {
		t.Errorf("answer = %+v, want a managed adopted stack", st)
	}
	draft := answerOf[draftAnswer](t, admin.do(http.MethodGet, "stacks/"+st.ID+"/draft", ""))
	if len(draft.Files) != 1 || draft.Files[0].Content != pasted {
		t.Errorf("draft = %+v, want the pasted file exactly as written", draft.Files)
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("adopting sent %d writes to the engine, want none", after-writesBefore)
	}
	if strings.Contains(fmt.Sprint(f.stackAudits(t)), "tok-value-9876") {
		t.Errorf("the audit trail holds the secret value: %+v", f.stackAudits(t))
	}
}

// TestAdoptCheckPasteNeedsReveal: a pasted file is compared with what runs on the real values, so a caller who cannot
// reveal secrets would learn from the answer whether a guess at DB_PASSWORD is right (T-08-52, WR-04). Paste mode is
// refused 403 secret_reveal_required for a Viewer and for an Operator without secrets.reveal, before anything is
// compared; the rebuild, which takes no input from the caller, is still open to them; an Env Admin gets the comparison.
func TestAdoptCheckPasteNeedsReveal(t *testing.T) {
	t.Parallel()
	const pasted = "services:\n  app:\n    image: nginx:1.27\n    environment:\n      DB_PASSWORD: guess\n"
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 2, 2, nil))
	body := pastedBody(t, map[string]any{"compose": pasted})

	for name, c := range map[string]*roleCaller{"viewer": viewer, "operator": operator} {
		rec := c.do(http.MethodPost, adoptCheckPath("legacy"), body)
		if rec.Code != http.StatusForbidden || problemType(t, rec) != "secret_reveal_required" {
			t.Errorf("paste check as %s: %d %s, want 403 secret_reveal_required", name, rec.Code, rec.Body)
		}
		for _, field := range []string{`"matches"`, `"gapCount"`, `"clean"`, `"hiddenChanges"`} {
			if strings.Contains(rec.Body.String(), field) {
				t.Errorf("the refusal for %s carries %s, which is a comparison: %s", name, field, rec.Body)
			}
		}
		// The rebuild takes nothing from the caller and is masked: it stays open.
		if rec := c.do(http.MethodPost, adoptCheckPath("legacy"), ""); rec.Code != http.StatusOK {
			t.Errorf("rebuild check as %s: %d %s, want 200", name, rec.Code, rec.Body)
		}
	}

	rec := admin.do(http.MethodPost, adoptCheckPath("legacy"), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("paste check as an env admin: %d %s, want 200", rec.Code, rec.Body)
	}
	var got struct {
		Source  string            `json:"source"`
		Blocked bool              `json:"blocked"`
		Gaps    []json.RawMessage `json:"gaps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "paste" || got.Blocked || len(got.Gaps) == 0 {
		t.Errorf("answer = %+v, want a pasted file compared with what runs and differing from it", got)
	}
}

// TestAdoptComparesStackNamesLikeCreate: swarm looks object names up regardless of case, so create refuses a case
// variant of a managed name; adopting an outside Foo beside a managed foo is refused the same way (409
// stack_already_managed with foo's ID), by the adoption and by its check, and the outside Foo stays listed, read-only,
// with the reason (T-08-95, WR-07).
func TestAdoptComparesStackNamesLikeCreate(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	managed := newStack(t, admin, "foo")
	reconfigureSwarm(t, f, e, foreignSecretService("Foo", "app", 1, 1, nil), foreignSecretService("other", "app", 1, 1, nil))

	holder := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_already_managed" {
			t.Fatalf("%d %s, want 409 stack_already_managed", rec.Code, rec.Body)
		}
		var problem struct {
			Errors []struct{ Location, Message string } `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if len(problem.Errors) != 1 || problem.Errors[0].Location != "stackId" || problem.Errors[0].Message != managed.ID {
			t.Errorf("errors = %+v, want the managed stack's ID %s as stackId", problem.Errors, managed.ID)
		}
	}
	t.Run("the adoption", func(t *testing.T) {
		holder(t, admin.do(http.MethodPost, adoptPath("Foo"), adoptBody("anything")))
	})
	t.Run("the check", func(t *testing.T) {
		holder(t, viewer.do(http.MethodPost, adoptCheckPath("Foo"), ""))
	})

	t.Run("the list and the page keep it read-only", func(t *testing.T) {
		by := map[string]stackAnswer{}
		for _, it := range listWith(t, admin, "").Items {
			by[it.Name] = it
		}
		if !by["foo"].Managed {
			t.Errorf("foo = %+v, want the managed stack", by["foo"])
		}
		u := by["Foo"].Unmanaged
		if by["Foo"].Managed || u == nil || u.Adoptable || !strings.Contains(u.Reason, "foo") {
			t.Fatalf("Foo = %+v, want listed unmanaged, not adoptable, with a reason naming foo", by["Foo"])
		}
		if o := by["other"].Unmanaged; o == nil || !o.Adoptable {
			t.Errorf("other = %+v, want it adoptable", by["other"])
		}
		rec := viewer.do(http.MethodGet, unmanagedPath("Foo", ""), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET Foo: %d %s, want 200", rec.Code, rec.Body)
		}
		if got := answerOf[unmanagedAnswer](t, rec); got.Adoptable || got.Reason != u.Reason {
			t.Errorf("GET Foo = adoptable %v reason %q, want not adoptable with the list's reason", got.Adoptable, got.Reason)
		}
	})
}
