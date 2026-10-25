// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The drift route (plan 08-19, STK-14): what runs against the deployed revision, read by anyone who may read stacks,
// with the live values a reader may not reveal withheld.

func driftPath(stackID string, fresh bool) string {
	if fresh {
		return "stacks/" + stackID + "/drift?fresh=true"
	}
	return "stacks/" + stackID + "/drift"
}

type driftAnswer struct {
	State      string `json:"state"`
	Revision   int64  `json:"revision"`
	DeployedAt string `json:"deployedAt"`
	DeployedBy string `json:"deployedBy"`
	CheckedAt  string `json:"checkedAt"`
	Count      int    `json:"count"`
	Services   []struct {
		Name, Short, Change string
		Current, Next       map[string]any
		HiddenChanges       []string `json:"hiddenChanges"`
		Settings            int
	} `json:"services"`
}

// editLiveService changes a deployed service of the stack as somebody outside Bayplan would.
func (f *envFix) editLiveService(t *testing.T, e envBody, stack, service string, edit func(*swarm.ServiceSpec)) {
	t.Helper()
	ctx := testsupport.Context(t)
	var svc dockerx.ServiceDetail
	err := f.clusters.ClusterRead(ctx, e.ID, func(c *dockerx.Client) error {
		rows, err := c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, stack)
		for _, r := range rows {
			if r.Name == stack+"_"+service {
				svc = r
			}
		}
		return err
	})
	if err != nil || svc.ID == "" {
		t.Fatalf("read service %s: %v", service, err)
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(svc.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	edit(&spec)
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.agg.WriteOnManager(ctx, e.ID, func(c *dockerx.Client) error {
		_, err := c.ServiceUpdate(ctx, svc.ID, svc.Version, raw, "")
		return err
	}); err != nil {
		t.Fatalf("update %s: %v", service, err)
	}
}

// setEnv replaces the value of an environment entry of the service.
func setEnv(name, value string) func(*swarm.ServiceSpec) {
	return func(s *swarm.ServiceSpec) {
		env := s.TaskTemplate.ContainerSpec.Env
		for i, e := range env {
			if strings.HasPrefix(e, name+"=") {
				env[i] = name + "=" + value
			}
		}
	}
}

func scaleTo(n uint64) func(*swarm.ServiceSpec) {
	return func(s *swarm.ServiceSpec) { s.Mode.Replicated.Replicas = &n }
}

func TestStackDriftRoute(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "drifty")
	f.deployGood(t, operator, st.ID, liveValuesCompose("nginx:1.27-alpine", "one"))
	writes := len(f.fake.Writes())

	// What was just deployed has not drifted.
	rec := viewer.do(http.MethodGet, driftPath(st.ID, false), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("drift as a viewer: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	got := answerOf[driftAnswer](t, rec)
	if got.State != "none" || got.Revision != 1 || got.Count != 0 || len(got.Services) != 0 || got.DeployedBy == "" || got.DeployedAt == "" || got.CheckedAt == "" {
		t.Fatalf("drift = %+v, want none at revision 1, with who deployed it and when", got)
	}

	// Somebody changes the service on the swarm: its replicas and the value of a secret environment variable.
	f.editLiveService(t, e, "drifty", "web", func(s *swarm.ServiceSpec) {
		scaleTo(3)(s)
		setEnv("API_KEY", "key-live-77aa")(s)
	})

	// The newest check is answered until a fresh one is asked for.
	if cached := answerOf[driftAnswer](t, viewer.do(http.MethodGet, driftPath(st.ID, false), "")); cached.State != "none" {
		t.Errorf("a read without fresh = %+v, want the newest check, which found none", cached)
	}
	rec = viewer.do(http.MethodGet, driftPath(st.ID, true), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh drift as a viewer: %d %s", rec.Code, rec.Body)
	}
	for _, v := range append(slices.Clone(liveValues), "key-live-77aa") {
		if strings.Contains(rec.Body.String(), v) {
			t.Fatalf("a live value %q reached a viewer: %s", v, rec.Body)
		}
	}
	got = answerOf[driftAnswer](t, rec)
	if got.State != "drifted" || len(got.Services) != 1 || got.Count < 2 {
		t.Fatalf("drift = %+v, want drifted with the one service and at least two settings", got)
	}
	svc := got.Services[0]
	if svc.Name != "drifty_web" || svc.Short != "web" || svc.Change != "changed" || svc.Current == nil || svc.Next == nil {
		t.Errorf("service = %+v, want drifty_web changed with both sides", svc)
	}
	if !slices.Contains(svc.HiddenChanges, "TaskTemplate.ContainerSpec.Env[API_KEY]") {
		t.Errorf("hidden changes = %v, want the secret variable listed by its path", svc.HiddenChanges)
	}
	// The service reads from what the revision deployed to what runs now.
	replicas := func(spec map[string]any) any {
		mode, _ := spec["Mode"].(map[string]any)
		rep, _ := mode["Replicated"].(map[string]any)
		return rep["Replicas"]
	}
	if replicas(svc.Current) != float64(1) || replicas(svc.Next) != float64(3) {
		t.Errorf("replicas = %v then %v, want the deployed 1 as current and the running 3 as next", replicas(svc.Current), replicas(svc.Next))
	}

	// An Admin may reveal secrets and sees what runs now and what was deployed.
	rec = admin.do(http.MethodGet, driftPath(st.ID, false), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("drift as an admin: %d %s", rec.Code, rec.Body)
	}
	for _, v := range []string{"key-live-77aa", "key-one-8c1d"} {
		if !strings.Contains(rec.Body.String(), v) {
			t.Errorf("an admin's answer lacks %q: %s", v, rec.Body)
		}
	}
	// An Operator holds no secrets.reveal and is redacted like a Viewer.
	rec = operator.do(http.MethodGet, driftPath(st.ID, false), "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "key-live-77aa") {
		t.Errorf("drift as an operator: %d, the live value may have reached them: %s", rec.Code, rec.Body)
	}

	// The stack's page says it has drifted, without the services.
	detail := answerOf[struct {
		DeployedRevision int64 `json:"deployedRevision"`
		Drift            *struct {
			State    string `json:"state"`
			Revision int64  `json:"revision"`
			Count    int    `json:"count"`
		} `json:"drift"`
	}](t, viewer.do(http.MethodGet, "stacks/"+st.ID, ""))
	if detail.DeployedRevision != 1 || detail.Drift == nil || detail.Drift.State != "drifted" || detail.Drift.Revision != 1 || detail.Drift.Count != got.Count {
		t.Errorf("stack = %+v %+v, want revision 1 and the drift", detail, detail.Drift)
	}

	if n := len(f.fake.Writes()); n != writes+1 { // the one update this test made
		t.Errorf("writes went from %d to %d, reading drift writes nothing", writes, n)
	}
}

func TestStackDriftIsReadAgainAfterADeployAndWhenTheStackIsStopped(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "moves")

	// A stack never deployed has no drift, and its page has no drift to show.
	if got := answerOf[driftAnswer](t, operator.do(http.MethodGet, driftPath(st.ID, false), "")); got.State != "not_deployed" || got.Revision != 0 {
		t.Errorf("a stack never deployed = %+v, want not_deployed", got)
	}
	f.deployGood(t, operator, st.ID, composeWeb127)
	// The newest check was about no revision: it is not the answer for the stack that now has one.
	if got := answerOf[driftAnswer](t, operator.do(http.MethodGet, driftPath(st.ID, false), "")); got.State != "none" || got.Revision != 1 {
		t.Errorf("after the first deploy = %+v, want none at revision 1", got)
	}
	f.editLiveService(t, e, "moves", "web", scaleTo(4))
	if got := answerOf[driftAnswer](t, operator.do(http.MethodGet, driftPath(st.ID, true), "")); got.State != "drifted" {
		t.Fatalf("after a change on the swarm = %+v, want drifted", got)
	}

	f.deployGood(t, operator, st.ID, composeWeb128)
	if got := answerOf[driftAnswer](t, operator.do(http.MethodGet, driftPath(st.ID, false), "")); got.State != "none" || got.Revision != 2 {
		t.Errorf("after a deploy = %+v, want the stale drift not to be answered: none at revision 2", got)
	}

	// A stopped stack is scaled down on purpose.
	f.lifecycle(t, operator, stopPath(st.ID))
	reads := len(f.fake.Requests())
	got := answerOf[driftAnswer](t, operator.do(http.MethodGet, driftPath(st.ID, true), ""))
	if got.State != "stopped" || len(got.Services) != 0 {
		t.Errorf("a stopped stack = %+v, want stopped and nothing listed", got)
	}
	if n := len(f.fake.Requests()); n != reads {
		t.Errorf("the engine was asked %d more times to check a stopped stack", n-reads)
	}
	f.lifecycle(t, operator, startPath(st.ID))
	if got := answerOf[driftAnswer](t, operator.do(http.MethodGet, driftPath(st.ID, false), "")); got.State == "stopped" {
		t.Errorf("after a start = %+v, want the stopped answer not to be kept", got)
	}
}

func TestStackDriftOfAnotherEnvironmentsStackIsNotFound(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	_ = newStack(t, admin, "here")
	ctx := testsupport.Context(t)
	if _, err := f.st.Write.ExecContext(ctx, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-elsewhere', 'agent', 'elsewhere', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	foreign, _, err := repo.CreateStack(ctx, "env-elsewhere", "foreign", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services: {}\n")}}})
	if err != nil {
		t.Fatal(err)
	}

	rec := viewer.do(http.MethodGet, driftPath(foreign.ID, false), "")

	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("drift of another environment's stack: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
}

func TestStackDriftChecksAreLimitedPerUser(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	f, e := stackEnvTuned(t, func(f *envFix) {
		// The first two reads are held in flight; the others pass through.
		f.stackOnDrift = func(context.Context) {
			if calls.Add(1) <= 2 {
				entered <- struct{}{}
				<-release
			}
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	holder := f.caller(t, e, "holder-1", "viewer")
	other := f.caller(t, e, "other-1", "viewer")
	st := newStack(t, admin, "limited")
	f.deployGood(t, operator, st.ID, composeWeb127)

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- holder.do(http.MethodGet, driftPath(st.ID, true), "").Code
		}()
	}
	testsupport.Await(t, "the first read to be in flight", entered)
	testsupport.Await(t, "the second read to be in flight", entered)

	// Two are in flight for this user: the third is refused, and says why.
	rec := holder.do(http.MethodGet, driftPath(st.ID, true), "")
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "check_busy" {
		t.Errorf("a third read: %d %s, want 429 check_busy", rec.Code, rec.Body)
	}
	// A read that needs no check is not refused, and another user is unaffected.
	if rec := other.do(http.MethodGet, driftPath(st.ID, true), ""); rec.Code != http.StatusOK {
		t.Errorf("another user's read: %d %s, want 200", rec.Code, rec.Body)
	}

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a held read ended with %d, want 200", code)
		}
	}
	// The slots are given back.
	if rec := holder.do(http.MethodGet, driftPath(st.ID, true), ""); rec.Code != http.StatusOK {
		t.Errorf("a read after the others finished: %d %s, want 200", rec.Code, rec.Body)
	}
}

// driftStandaloneCompose is a standalone stack with a secret-looking environment value, so the redaction of a compose
// service's live values is seen.
const driftStandaloneCompose = "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: key-one-8c1d\n    restart: unless-stopped\n"

func TestStackDriftStandalone(t *testing.T) {
	t.Parallel()
	f, e, up := standaloneEnv(t, nil)
	_, st, h := deployedStandalone(t, f, e, driftStandaloneCompose)
	standOnHost(f, h)
	admin := f.caller(t, e, "revealer-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")

	// The containers are as the deploy left them: no drift.
	got := answerOf[driftAnswer](t, viewer.do(http.MethodGet, driftPath(st.ID, true), ""))
	if got.State != "none" || got.Revision != 1 || got.Count != 0 || len(got.Services) != 0 {
		t.Fatalf("drift right after a deploy = %+v, want none at revision 1", got)
	}
	_, calls := up.lastProject()
	writes := len(f.fake.Writes())

	// Somebody changes the container on the host: its secret environment value and its restart policy.
	env := h.Docs["web"].Config.Env
	for i, entry := range env {
		if strings.HasPrefix(entry, "API_KEY=") {
			env[i] = "API_KEY=key-live-77aa"
		}
	}
	h.Docs["web"].HostConfig.RestartPolicy = container.RestartPolicy{Name: "always"}
	standOnHost(f, h)

	rec := viewer.do(http.MethodGet, driftPath(st.ID, true), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh drift as a viewer: %d %s", rec.Code, rec.Body)
	}
	for _, v := range []string{"key-live-77aa", "key-one-8c1d"} {
		if strings.Contains(rec.Body.String(), v) {
			t.Fatalf("an environment value %q reached a viewer: %s", v, rec.Body)
		}
	}
	got = answerOf[driftAnswer](t, rec)
	if got.State != "drifted" || got.Count != 2 || len(got.Services) != 1 {
		t.Fatalf("drift = %+v, want drifted with web's two settings", got)
	}
	svc := got.Services[0]
	if svc.Name != "web" || svc.Short != "web" || svc.Change != "changed" || svc.Settings != 2 {
		t.Errorf("service = %+v, want web changed at two settings", svc)
	}
	if !slices.Contains(svc.HiddenChanges, "environment.API_KEY") {
		t.Errorf("hidden changes = %v, want the environment variable listed by its path", svc.HiddenChanges)
	}
	// The service reads from what the revision deployed to what runs now.
	if svc.Current["restart"] != "unless-stopped" || svc.Next["restart"] != "always" {
		t.Errorf("restart = %v then %v, want the deployed policy as current and the running one as next", svc.Current["restart"], svc.Next["restart"])
	}

	// An Admin may reveal secrets and sees both values.
	rec = admin.do(http.MethodGet, driftPath(st.ID, false), "")
	for _, v := range []string{"key-live-77aa", "key-one-8c1d"} {
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), v) {
			t.Errorf("an admin's answer lacks %q: %d %s", v, rec.Code, rec.Body)
		}
	}

	// Reading drift asked the Compose SDK for nothing and wrote nothing to the host.
	if _, now := up.lastProject(); now != calls {
		t.Errorf("the Compose SDK was run %d more times by drift checks", now-calls)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d, reading drift writes nothing", writes, n)
	}
}

// A drift caused inside Bayplan names who did it and when; one found with no such action says nothing of a cause, and
// nothing of an audit row but the action, the name, the time and the replica counts leaves (plan 08-22, D-24, T-08-78).
func TestStackDriftCarriesTheCause(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	// The audit log keeps the fixture's clock, which starts in the past, and a revision is taken by the wall clock: the
	// clock moves past it before anyone signs in, so the scale is after the deploy as it is in life.
	f.clk.t = time.Now().Add(time.Minute)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "caused")
	f.deployGood(t, operator, st.ID, composeWeb127)

	type drifted struct {
		Cause    map[string]any `json:"cause"`
		Causes   int            `json:"causes"`
		Services []struct {
			Short string
			Cause map[string]any `json:"cause"`
		} `json:"services"`
	}
	read := func() (drifted, string) {
		rec := operator.do(http.MethodGet, driftPath(st.ID, true), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("drift: %d %s", rec.Code, rec.Body)
		}
		return answerOf[drifted](t, rec), rec.Body.String()
	}

	// Somebody adds an environment variable to the service on the swarm: nothing in the audit log explains it.
	f.editLiveService(t, e, "caused", "web", func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Env = append(s.TaskTemplate.ContainerSpec.Env, "MODE=other")
	})
	got, _ := read()
	if len(got.Services) != 1 || got.Services[0].Cause != nil || got.Cause != nil || got.Causes != 0 {
		t.Fatalf("a change from outside = %+v, want no cause: the reader says it was made outside Bayplan", got)
	}

	// A scale from the service page is Bayplan's own action and drift as well.
	var webID string
	for _, s := range answerOf[stackServicesAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/services", "")).Items {
		if s.Short == "web" {
			webID = s.ID
		}
	}
	if rec := operator.do(http.MethodPost, "services/scale", `{"targets":[{"serviceId":"`+webID+`","replicas":3}]}`); rec.Code != http.StatusOK {
		t.Fatalf("scale: %d %s", rec.Code, rec.Body)
	}
	got, body := read()
	if len(got.Services) != 1 || got.Services[0].Cause == nil || got.Cause == nil || got.Causes != 1 {
		t.Fatalf("after a scale = %s, want the scale as the cause of web and of the stack", body)
	}
	for name, cause := range map[string]map[string]any{"service": got.Services[0].Cause, "stack": got.Cause} {
		keys := make([]string, 0, len(cause))
		for k := range cause {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"action", "at", "by", "from", "service", "to"}; !slices.Equal(keys, want) {
			t.Errorf("the %s cause holds %v, want only %v: no other audit detail leaves", name, keys, want)
		}
		if cause["action"] != "scaled" || cause["service"] != "web" || cause["from"] != "1" || cause["to"] != "3" || cause["by"] == "" || cause["at"] == "" {
			t.Errorf("the %s cause = %v, want web scaled from 1 to 3 by the operator, with the time", name, cause)
		}
	}
	for _, leak := range []string{"userAgent", "ip", "environment", "details", "target"} {
		if strings.Contains(body, `"`+leak+`"`) {
			t.Errorf("the answer holds %q, an audit field that must not leave", leak)
		}
	}

	// The stack's page and the lists carry the cause with the summary.
	detail := answerOf[struct {
		Drift *struct {
			Cause  map[string]any `json:"cause"`
			Causes int            `json:"causes"`
		} `json:"drift"`
	}](t, operator.do(http.MethodGet, "stacks/"+st.ID, ""))
	if detail.Drift == nil || detail.Drift.Causes != 1 || detail.Drift.Cause["action"] != "scaled" {
		t.Errorf("the stack's page = %+v, want the scale in its drift summary", detail.Drift)
	}
}

// The accept route (plan 08-24, STK-14, D-23, D-54): what runs becomes the stack's next draft, on top of the deployed
// revision, by an Operator, with nothing sent to an engine.

const acceptPath = "/drift/accept"

func acceptURL(stackID string) string { return "stacks/" + stackID + acceptPath }

const acceptCompose = "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: ${API_KEY}\n      MODE: prod\n    deploy:\n      replicas: 1\n"

const acceptVars = `[{"name":"API_KEY","value":"key-live-abcd","secret":true}]`

func TestAcceptLiveRoute(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "accepted")
	v := saveCompose(t, operator, st.ID, acceptCompose, acceptVars)
	if job := f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, operator.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)).JobID); job.State != "done" {
		t.Fatalf("deploy = %+v, want it done", job)
	}
	// Somebody scales the service outside Bayplan.
	f.editLiveService(t, e, "accepted", "web", scaleTo(3))
	seen := draftVersion(t, operator, st.ID)
	writes := len(f.fake.Writes())

	// A Viewer may read the stack, not change its draft.
	if rec := viewer.do(http.MethodPost, acceptURL(st.ID), `{"draftVersion":`+itoaT(seen)+`}`); rec.Code != http.StatusForbidden {
		t.Fatalf("accept as a viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	// A draft that moved since the person looked is not overwritten.
	if rec := operator.do(http.MethodPost, acceptURL(st.ID), `{"draftVersion":`+itoaT(seen+3)+`}`); rec.Code != http.StatusConflict || problemType(t, rec) != "draft_conflict" {
		t.Fatalf("accept on a stale version: %d %s, want 409 draft_conflict", rec.Code, rec.Body)
	}
	if got := draftVersion(t, operator, st.ID); got != seen {
		t.Fatalf("draft version = %d after a refused accept, want %d", got, seen)
	}

	rec := operator.do(http.MethodPost, acceptURL(st.ID), `{"draftVersion":`+itoaT(seen)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept as an operator: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	got := answerOf[struct {
		Version  int64  `json:"version"`
		SavedBy  string `json:"savedBy"`
		Revision int64  `json:"revision"`
		Services int    `json:"services"`
	}](t, rec)
	if got.Version != seen+1 || got.SavedBy != "operator-1" || got.Revision != 1 || got.Services != 1 {
		t.Errorf("answer = %+v, want version %d saved by operator-1 from revision 1 with 1 service", got, seen+1)
	}

	// The draft is the deployed revision's files and variables with the compose file rebuilt from what runs, and the
	// secret variable's value is not in it.
	draft := answerOf[draftAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", ""))
	if draft.Version != got.Version || len(draft.Files) != 1 {
		t.Fatalf("draft = %+v", draft)
	}
	text := draft.Files[0].Content
	for _, want := range []string{"replicas: 3", "API_KEY: ${API_KEY}", "MODE: prod", "image: nginx:1.27-alpine"} {
		if !strings.Contains(text, want) {
			t.Errorf("the draft lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "key-live-abcd") {
		t.Errorf("the secret variable's value is in the compose file:\n%s", text)
	}
	if len(draft.Vars) != 1 || draft.Vars[0].Name != "API_KEY" || !draft.Vars[0].Secret {
		t.Errorf("vars = %+v, want the revision's secret API_KEY", draft.Vars)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d: accepting live must not change an engine", writes, n)
	}

	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.drift_accepted" {
			row = &r
		}
	}
	if row == nil || row.TargetID != st.ID || row.Result != "success" || row.Details["stack"] != "accepted" ||
		row.Details["revision"] != "1" || row.Details["version"] != itoaT(got.Version) || row.Details["services"] != "1" {
		t.Errorf("stack.drift_accepted = %+v, want the stack, revision 1, the new version and 1 service", row)
	}
	if row != nil {
		for k, v := range row.Details {
			if strings.Contains(v, "key-live-abcd") || strings.Contains(k, "secret") {
				t.Errorf("the audit detail %s=%q carries a secret", k, v)
			}
		}
	}
}

func TestAcceptLiveRouteIsRefusedWhileAnOperationRunsAndWithoutADeployedRevision(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha, beta := newStack(t, admin, "alpha"), newStack(t, admin, "beta")

	// A stack never deployed has nothing to take as the base.
	rec := operator.do(http.MethodPost, acceptURL(alpha.ID), `{"draftVersion":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "no_deployed_revision" {
		t.Fatalf("accept with no deployed revision: %d %s, want 409 no_deployed_revision", rec.Code, rec.Body)
	}

	jobID := startStuckDeploy(t, f, operator, beta)
	writes := len(f.fake.Writes())
	rec = operator.do(http.MethodPost, acceptURL(alpha.ID), `{"draftVersion":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Fatalf("accept during another operation: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d", writes, n)
	}
	if rec := operator.do(http.MethodPost, cancelPath(beta.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, jobID)
}
