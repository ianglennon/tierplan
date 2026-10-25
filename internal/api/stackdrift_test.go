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
	if !strings.Contains(rec.Body.String(), `"Replicas":3`) && !strings.Contains(rec.Body.String(), `"Replicas": 3`) {
		t.Errorf("the replica count is not shown: %s", rec.Body)
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
