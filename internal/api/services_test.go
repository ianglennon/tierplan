// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// Services list and scale (plan 06-01, RES-02, D-08, D-10). Docker is a fake daemon behind
// the real monitor, snapshot cache, aggregator and cluster reader; the router, policy, CSRF
// guard, registry resolver and audit writer are the real ones.

const scaleImage = "localhost:5000/private/web:1"

func swarmManagerEngine(services ...dockertest.FakeService) dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "mgr-1", Version: "29.1.0", SwarmState: "active", SwarmControl: true,
		NodeRows: []dockertest.FakeNode{
			{ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, EngineVersion: "29.1.0", OS: "linux"},
		},
		ServiceRows: services,
	}
}

func webService() dockertest.FakeService {
	return dockertest.FakeService{
		ID: "svc-web", Name: "web", Mode: "replicated", Running: 2, Desired: 2, Image: scaleImage, Version: 4,
		Labels: map[string]string{"com.docker.stack.namespace": "shop"},
	}
}

// serviceEnv is a local environment whose socket is a swarm manager, with the cluster
// reader a real server has.
func serviceEnv(t *testing.T, services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, swarmManagerEngine(services...))
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f, e
}

type serviceItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Mode        string `json:"mode"`
	Running     uint64 `json:"running"`
	Desired     uint64 `json:"desired"`
	UpdateState string `json:"updateState"`
	Image       string `json:"image"`
	Stack       string `json:"stack"`
	Agent       bool   `json:"agent"`
	UpdatedAt   string `json:"updatedAt"`
}

type serviceList struct {
	Items     []serviceItem `json:"items"`
	Total     int           `json:"total"`
	Truncated bool          `json:"truncated"`
	NoManager bool          `json:"noManager"`
	FetchedAt string        `json:"fetchedAt"`
}

type scaleResult struct {
	ServiceID string `json:"serviceId"`
	Name      string `json:"name"`
	Result    string `json:"result"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
	Previous  *int64 `json:"previous"`
	Replicas  int64  `json:"replicas"`
	Changed   bool   `json:"changed"`
}

type scaleAnswer struct {
	BulkID  string        `json:"bulkId"`
	Results []scaleResult `json:"results"`
}

func (f *envFix) scale(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services/scale", body)
}

func (f *envFix) serviceAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'service.%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListServicesOfALocalSwarmManagerAnswersItsRows(t *testing.T) {
	f, e := serviceEnv(t, webService(),
		dockertest.FakeService{ID: "svc-agent", Name: "bayplan-agent", Mode: "global", Running: 3, Desired: 3, Image: "bayplan/agent:1"},
		dockertest.FakeService{ID: "svc-job", Name: "backup", Mode: "replicated-job", UpdateState: "completed"})
	rec := f.get(t, e, "services")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[serviceList](t, rec)
	if got.Total != 3 || got.Truncated || got.NoManager || got.FetchedAt == "" || len(got.Items) != 3 {
		t.Fatalf("list = %+v", got)
	}
	names := []string{got.Items[0].Name, got.Items[1].Name, got.Items[2].Name}
	if !slices.Equal(names, []string{"backup", "bayplan-agent", "web"}) {
		t.Errorf("names = %v, want sorted by name", names)
	}
	var web serviceItem
	for _, it := range got.Items {
		if it.Name == "web" {
			web = it
		}
	}
	if web.ID != "svc-web" || web.Mode != "replicated" || web.Running != 2 || web.Desired != 2 || web.Image != scaleImage || web.Stack != "shop" || web.Agent {
		t.Errorf("web = %+v", web)
	}
	if !got.Items[1].Agent || got.Items[0].UpdateState != "completed" {
		t.Errorf("agent / update state = %+v", got.Items)
	}
}

func TestListServicesOfAStandaloneHostIsNotASwarm(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	rec := f.get(t, e, "services")
	if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Fatalf("list = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
	rec = f.scale(t, e, `{"targets":[{"serviceId":"web","replicas":2}]}`)
	if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Fatalf("scale = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestScaleAServiceOfALocalSwarmManagerEndToEnd(t *testing.T) {
	f, e := serviceEnv(t, webService())
	cred := f.addPullCredential(t, "local-bot", "")
	_ = cred
	rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":5}]}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	ans := decode[scaleAnswer](t, rec)
	if ans.BulkID != "" || len(ans.Results) != 1 {
		t.Fatalf("answer = %s", rec.body)
	}
	r := ans.Results[0]
	if r.ServiceID != "svc-web" || r.Name != "web" || r.Result != "done" || r.Code != "" || r.Previous == nil || *r.Previous != 2 || r.Replicas != 5 || !r.Changed {
		t.Fatalf("result = %+v", r)
	}

	ups := f.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.ServiceID != "svc-web" || u.Version != "4" {
		t.Errorf("update = %s at version %s, want svc-web at 4", u.ServiceID, u.Version)
	}
	user, password, server := decodedPullAuth(t, u.Auth)
	if user != "bot" || password != pullCredSecret || server != "localhost:5000" {
		t.Errorf("the update carried the credential %q/%q at %q", user, password, server)
	}
	if rep := u.Spec.Mode.Replicated; rep == nil || rep.Replicas == nil || *rep.Replicas != 5 {
		t.Errorf("replicas = %+v, want 5", u.Spec.Mode.Replicated)
	}
	if u.Spec.Name != "web" || u.Spec.TaskTemplate.ContainerSpec == nil || u.Spec.TaskTemplate.ContainerSpec.Image != scaleImage || u.Spec.Labels["com.docker.stack.namespace"] != "shop" {
		t.Errorf("the rest of the spec changed: %+v", u.Spec)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /services/svc-web/update?version=4"}) {
		t.Errorf("writes = %v", w)
	}
	if strings.Contains(string(rec.body), pullCredSecret) {
		t.Error("the answer holds the credential")
	}

	rows := f.serviceAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.Action != audit.ActionServiceScaled || row.Result != string(audit.ResultSuccess) || row.TargetType != "service" || row.TargetID != "svc-web" || row.EnvID != e.ID {
		t.Errorf("audit row = %+v", row)
	}
	for k, want := range map[string]string{"replicas": "5", "previous": "2", "code": "done", "name": "web"} {
		if row.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, row.Details[k], want, row.Details)
		}
	}
	for k, v := range row.Details {
		if strings.Contains(v, pullCredSecret) || strings.Contains(k, "auth") {
			t.Errorf("audit detail %s=%q holds a credential", k, v)
		}
	}

	// The list reads the count the write made.
	list := decode[serviceList](t, f.get(t, e, "services"))
	if len(list.Items) != 1 || list.Items[0].Desired != 5 {
		t.Errorf("services after the scale = %+v, want desired 5", list.Items)
	}
}

func TestScaleWithNoMatchingCredentialSendsNoHeader(t *testing.T) {
	f, e := serviceEnv(t, webService())
	if rec := f.scale(t, e, `{"targets":[{"serviceId":"web","replicas":1}]}`); rec.code != http.StatusOK {
		t.Fatalf("scale: %d %s", rec.code, rec.body)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].Auth != "" {
		t.Fatalf("updates = %+v, want one with no X-Registry-Auth", ups)
	}
}

func TestScaleRefusals(t *testing.T) {
	f, e := serviceEnv(t, webService(),
		dockertest.FakeService{ID: "svc-agent", Name: "bayplan-agent", Mode: "global", Running: 1, Desired: 1},
		dockertest.FakeService{ID: "svc-job", Name: "backup", Mode: "replicated-job"})

	t.Run("a viewer is refused and sees nothing change", func(t *testing.T) {
		f.addUser(t, "viewer", false)
		f.addEnvRole(t, "viewer", e.ID, "viewer")
		cookie, csrf := f.signIn(t, "viewer", false)
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/services/scale",
			body: `{"targets":[{"serviceId":"svc-web","replicas":3}]}`, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		// A viewer may still read the list.
		rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/services", cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusOK {
			t.Errorf("a viewer's list = %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("replicas outside 0 to 1000", func(t *testing.T) {
		for _, body := range []string{
			`{"targets":[{"serviceId":"svc-web","replicas":1001}]}`,
			`{"targets":[{"serviceId":"svc-web","replicas":-1}]}`,
			`{"targets":[{"serviceId":"svc-web","replicas":2},{"serviceId":"web","replicas":5000}]}`,
		} {
			rec := f.scale(t, e, body)
			if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "replicas_invalid" {
				t.Errorf("%s = %d %s (%s), want 422 replicas_invalid", body, rec.code, rec.body, rec.ptyp)
			}
		}
		if rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":1000}]}`); rec.code != http.StatusOK {
			t.Errorf("1000 replicas = %d %s, want it accepted", rec.code, rec.body)
		}
		if rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":0}]}`); rec.code != http.StatusOK {
			t.Errorf("0 replicas = %d %s, want it accepted", rec.code, rec.body)
		}
	})

	t.Run("a service named twice", func(t *testing.T) {
		rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":2},{"serviceId":"svc-web","replicas":3}]}`)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "duplicate_target" {
			t.Errorf("= %d %s (%s), want 422 duplicate_target", rec.code, rec.body, rec.ptyp)
		}
	})

	t.Run("a global or job service is skipped and nothing is sent", func(t *testing.T) {
		before := len(f.fake.Writes())
		rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-agent","replicas":2},{"serviceId":"svc-job","replicas":2}]}`)
		if rec.code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.code, rec.body)
		}
		ans := decode[scaleAnswer](t, rec)
		if ans.BulkID == "" || len(ans.Results) != 2 {
			t.Fatalf("answer = %s", rec.body)
		}
		for _, r := range ans.Results {
			if r.Result != "skipped" || r.Code != "not_scalable" || r.Changed {
				t.Errorf("result = %+v, want skipped not_scalable", r)
			}
		}
		if got := len(f.fake.Writes()); got != before {
			t.Errorf("writes grew from %d to %d, want no write for a global or job service", before, got)
		}
	})

	t.Run("an unknown service is service_not_found and is not audited", func(t *testing.T) {
		before := len(f.serviceAudits(t))
		rec := f.scale(t, e, `{"targets":[{"serviceId":"nope","replicas":2}]}`)
		ans := decode[scaleAnswer](t, rec)
		if rec.code != http.StatusOK || len(ans.Results) != 1 || ans.Results[0].Result != "failed" || ans.Results[0].Code != "service_not_found" {
			t.Fatalf("= %d %s", rec.code, rec.body)
		}
		if got := len(f.serviceAudits(t)); got != before {
			t.Errorf("audit rows grew from %d to %d for a service that does not exist", before, got)
		}
	})
}

func TestScaleToTheCountTheServiceHasSendsNothingAndSaysSo(t *testing.T) {
	f, e := serviceEnv(t, webService())
	rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":2}]}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	r := decode[scaleAnswer](t, rec).Results[0]
	if r.Result != "done" || r.Changed || r.Previous == nil || *r.Previous != 2 {
		t.Errorf("result = %+v, want done, unchanged, previous 2", r)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultSuccess) || rows[0].Details["code"] != "unchanged" {
		t.Errorf("audit rows = %+v, want one success row with code unchanged", rows)
	}
}

func TestAServiceChangedBetweenReadAndWriteIsServiceChangedAndNeverRetried(t *testing.T) {
	svc := webService()
	f, e := serviceEnv(t, svc)
	f.fake.Reconfigure(func() dockertest.FakeConfig {
		c := swarmManagerEngine(svc)
		// The manager takes a moment over the write, in which somebody else changes the service.
		c.WriteDelay = 150 * time.Millisecond
		return c
	}())
	var wg sync.WaitGroup
	var rec *envRec
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec = f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":4}]}`)
	}()
	waitFor(t, "the update to reach the manager", func() bool { return len(f.fake.Writes()) == 1 })
	f.fake.BumpService("svc-web")
	wg.Wait()
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	r := decode[scaleAnswer](t, rec).Results[0]
	if r.Result != "failed" || r.Code != "service_changed" || r.Changed {
		t.Errorf("result = %+v, want failed service_changed", r)
	}
	if w := f.fake.Writes(); len(w) != 1 {
		t.Errorf("writes = %v, want exactly one: a refused write is never retried", w)
	}
	if ups := f.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("accepted updates = %+v, want none", ups)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "service_changed" || rows[0].Details["code"] != "service_changed" {
		t.Errorf("audit rows = %+v, want one failure row with the code", rows)
	}
}

func TestAnAgentThatRefusesTheServiceUpdateByItsPolicyIsAgentPolicyDenied(t *testing.T) {
	f, e := serviceEnv(t, webService())
	f.fake.Reconfigure(func() dockertest.FakeConfig {
		c := swarmManagerEngine(webService())
		c.PolicyDeny = true
		return c
	}())
	rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":4}]}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	r := decode[scaleAnswer](t, rec).Results[0]
	// The fake refuses the update in the words an agent uses for a call its policy does not allow.
	if r.Result != "failed" || r.Code != "agent_policy_denied" {
		t.Errorf("result = %+v, want failed agent_policy_denied", r)
	}
}

// D-36: a manager's agent refuses a service spec that gives tasks host access unless its owner
// opted in. The fake answers the update in the words an agent uses for that refusal, and the
// scale says which setting lifts it.
func TestAScaleTheManagersAgentRefusesByHostPolicyIsAgentHostPolicyAndAuditedWithThatReason(t *testing.T) {
	f, e := serviceEnv(t, webService())
	f.fake.Reconfigure(func() dockertest.FakeConfig {
		c := swarmManagerEngine(webService())
		c.WriteError = "agent_policy_denied: host_policy: POST /services/svc-web/update"
		return c
	}())
	rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":4}]}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	r := decode[scaleAnswer](t, rec).Results[0]
	if r.Result != "failed" || r.Code != "agent_host_policy" {
		t.Fatalf("result = %+v, want failed agent_host_policy", r)
	}
	if !strings.Contains(r.Detail, "BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES") {
		t.Errorf("detail = %q, want it to name the setting that allows host access", r.Detail)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "agent_host_policy" || rows[0].Details["code"] != "agent_host_policy" {
		t.Errorf("audit rows = %+v, want one failure row with agent_host_policy", rows)
	}
}

// A refusal by the agent's plain policy keeps its own code, so the two are never confused.
func TestAScaleTheAgentRefusesByPlainPolicyIsStillAgentPolicyDenied(t *testing.T) {
	f, e := serviceEnv(t, webService())
	f.fake.Reconfigure(func() dockertest.FakeConfig {
		c := swarmManagerEngine(webService())
		c.WriteError = "agent_policy_denied: POST /services/svc-web/update"
		return c
	}())
	rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-web","replicas":4}]}`)
	r := decode[scaleAnswer](t, rec).Results[0]
	if r.Result != "failed" || r.Code != "agent_policy_denied" {
		t.Fatalf("result = %+v, want failed agent_policy_denied", r)
	}
}
