// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

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
	return serviceEnvOf(t, swarmManagerEngine(services...))
}

// serviceEnvOf is serviceEnv for a manager the caller configured, tasks and all.
func serviceEnvOf(t *testing.T, cfg dockertest.FakeConfig) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
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

	Ports           []string `json:"ports"`
	UpdateMessage   string   `json:"updateMessage"`
	HasPreviousSpec bool     `json:"hasPreviousSpec"`
	CreatedAt       string   `json:"createdAt"`

	RestartLoop *restartLoopBody `json:"restartLoop"`
}

// restartLoopBody is the restartLoop field of a service row and of the detail.
type restartLoopBody struct {
	State         string `json:"state"`
	Count         int    `json:"count"`
	WindowMinutes int    `json:"windowMinutes"`
	LatestError   string `json:"latestError"`
	Since         string `json:"since"`
}

type serviceStatusCounts struct {
	Degraded int `json:"degraded"`
	Updating int `json:"updating"`
	Problem  int `json:"problem"`
	Loop     int `json:"loop"`
}

type serviceList struct {
	Items           []serviceItem       `json:"items"`
	Total           int                 `json:"total"`
	UnfilteredTotal int                 `json:"unfilteredTotal"`
	Capped          bool                `json:"capped"`
	StatusCounts    serviceStatusCounts `json:"statusCounts"`
	NoManager       bool                `json:"noManager"`
	FetchedAt       string              `json:"fetchedAt"`
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
	t.Parallel()
	f, e := serviceEnv(t, webService(),
		dockertest.FakeService{ID: "svc-agent", Name: "bayplan-agent", Mode: "global", Running: 3, Desired: 3, Image: "bayplan/agent:1"},
		dockertest.FakeService{ID: "svc-job", Name: "backup", Mode: "replicated-job", UpdateState: "completed"})
	rec := f.get(t, e, "services")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[serviceList](t, rec)
	if got.Total != 3 || got.UnfilteredTotal != 3 || got.Capped || got.NoManager || got.FetchedAt == "" || len(got.Items) != 3 {
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	awaitFake(t, f.fake, "the update to reach the manager", func() bool { return len(f.fake.Writes()) == 1 })
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func namesOf(items []serviceItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func listServicesQuery(t *testing.T, f *envFix, e envBody, query string) (*envRec, serviceList) {
	t.Helper()
	rec := f.get(t, e, "services"+query)
	var got serviceList
	if rec.code == http.StatusOK {
		got = decode[serviceList](t, rec)
	}
	return rec, got
}

// Search and status filters run on the server before the 500-row cap, and the chip counts
// are counted before them (plan 06-04, RES-02, P4 D-02).
func TestServicesListFiltersAndCounts(t *testing.T) {
	t.Parallel()
	stackLabel := func(stack string) map[string]string { return map[string]string{"com.docker.stack.namespace": stack} }
	f, e := serviceEnv(t,
		dockertest.FakeService{ID: "s1", Name: "web-1", Mode: "replicated", Running: 2, Desired: 2, Image: "nginx:1"},
		dockertest.FakeService{ID: "s2", Name: "api", Mode: "replicated", Running: 1, Desired: 3, Image: "ghcr.io/acme/web:2", UpdateState: "updating"},
		dockertest.FakeService{ID: "s3", Name: "worker", Mode: "replicated", Running: 2, Desired: 2, Labels: map[string]string{"owner.web": "x"}, UpdateState: "paused"},
		dockertest.FakeService{ID: "s4", Name: "cache", Mode: "replicated", Running: 1, Desired: 1, Labels: map[string]string{"team": "Webops"}, UpdateState: "rollback_started"},
		dockertest.FakeService{ID: "s5", Name: "db", Mode: "replicated", Running: 1, Desired: 1, Labels: stackLabel("web"), UpdateState: "rollback_paused"},
		dockertest.FakeService{ID: "s6", Name: "mail", Mode: "replicated", Running: 0, Desired: 2, Image: "mail:1", UpdateState: "rollback_completed"},
		dockertest.FakeService{ID: "s7", Name: "queue", Mode: "replicated", Running: 0, Desired: 0, Image: "queue:1"},
		dockertest.FakeService{ID: "s8", Name: "proxy", Mode: "global", Running: 3, Desired: 3, Image: "traefik:3", UpdateState: "completed"},
	)

	t.Run("q matches the name, image, label key or value and stack, ignoring case", func(t *testing.T) {
		for _, q := range []string{"web", "WEB", "  Web "} {
			rec, got := listServicesQuery(t, f, e, "?q="+strings.ReplaceAll(q, " ", "%20"))
			if rec.code != http.StatusOK {
				t.Fatalf("q=%q: %d %s", q, rec.code, rec.body)
			}
			// api (image), cache (label value), db (stack), web-1 (name), worker (label key)
			if want := []string{"api", "cache", "db", "web-1", "worker"}; !slices.Equal(namesOf(got.Items), want) {
				t.Errorf("q=%q names = %v, want %v", q, namesOf(got.Items), want)
			}
			if got.Total != 5 || got.UnfilteredTotal != 8 {
				t.Errorf("q=%q total/unfiltered = %d/%d, want 5/8", q, got.Total, got.UnfilteredTotal)
			}
		}
	})

	t.Run("status keeps degraded, updating or problem services and several values are a union", func(t *testing.T) {
		cases := []struct {
			status string
			want   []string
		}{
			{"degraded", []string{"api"}},
			{"updating", []string{"api", "cache"}},
			{"problem", []string{"db", "mail", "worker"}},
			{"degraded,problem", []string{"api", "db", "mail", "worker"}},
			{"updating,degraded,problem", []string{"api", "cache", "db", "mail", "worker"}},
			{"DEGRADED, degraded", []string{"api"}},
		}
		for _, c := range cases {
			rec, got := listServicesQuery(t, f, e, "?status="+strings.ReplaceAll(c.status, " ", "%20"))
			if rec.code != http.StatusOK {
				t.Fatalf("status=%q: %d %s", c.status, rec.code, rec.body)
			}
			if !slices.Equal(namesOf(got.Items), c.want) || got.Total != len(c.want) {
				t.Errorf("status=%q names = %v (total %d), want %v", c.status, namesOf(got.Items), got.Total, c.want)
			}
		}
	})

	t.Run("q and status together, and the counts ignore both", func(t *testing.T) {
		rec, got := listServicesQuery(t, f, e, "?q=web&status=updating")
		if rec.code != http.StatusOK {
			t.Fatalf("%d %s", rec.code, rec.body)
		}
		if want := []string{"api", "cache"}; !slices.Equal(namesOf(got.Items), want) {
			t.Errorf("names = %v, want %v", namesOf(got.Items), want)
		}
		if got.Total != 2 || got.UnfilteredTotal != 8 {
			t.Errorf("total/unfiltered = %d/%d, want 2/8", got.Total, got.UnfilteredTotal)
		}
		if got.StatusCounts != (serviceStatusCounts{Degraded: 1, Updating: 2, Problem: 3}) {
			t.Errorf("statusCounts = %+v, want degraded 1, updating 2, problem 3 from all 8 services", got.StatusCounts)
		}
		_, bare := listServicesQuery(t, f, e, "")
		if bare.StatusCounts != got.StatusCounts || len(bare.Items) != 8 {
			t.Errorf("unfiltered list = %d items, counts %+v, want 8 and the same counts", len(bare.Items), bare.StatusCounts)
		}
	})

	t.Run("a filter that matches nothing is an empty list with the counts kept", func(t *testing.T) {
		rec, got := listServicesQuery(t, f, e, "?q=nothing-like-this")
		if rec.code != http.StatusOK || len(got.Items) != 0 || got.Total != 0 || got.UnfilteredTotal != 8 || got.Capped {
			t.Fatalf("%d %+v", rec.code, got)
		}
		if got.Items == nil {
			t.Error("items is null, want []")
		}
	})

	t.Run("refusals", func(t *testing.T) {
		rec, _ := listServicesQuery(t, f, e, "?status=degraded,sleepy")
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "status_invalid" {
			t.Errorf("unknown status = %d %s (%s), want 422 status_invalid", rec.code, rec.body, rec.ptyp)
		}
		rec, _ = listServicesQuery(t, f, e, "?q="+strings.Repeat("a", 129))
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "query_too_long" {
			t.Errorf("129-character q = %d %s (%s), want 422 query_too_long", rec.code, rec.body, rec.ptyp)
		}
		rec, got := listServicesQuery(t, f, e, "?q="+strings.Repeat("a", 128))
		if rec.code != http.StatusOK || len(got.Items) != 0 {
			t.Errorf("128-character q = %d %s, want 200 and no rows", rec.code, rec.body)
		}
	})

	t.Run("labels never leave the server", func(t *testing.T) {
		rec := f.get(t, e, "services")
		for _, secret := range []string{"owner.web", "Webops", "team"} {
			if strings.Contains(string(rec.body), secret) {
				t.Errorf("the list body carries the label text %q: %s", secret, rec.body)
			}
		}
	})
}

func TestServicesListRowsCarryPortsUpdateMessagePreviousSpecAndCreatedAt(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	f, e := serviceEnv(t,
		dockertest.FakeService{
			ID: "s1", Name: "web", Mode: "replicated", Running: 1, Desired: 2, UpdateState: "paused", UpdateMessage: "update paused", CreatedAt: created,
			Ports: []swarm.PortConfig{
				{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: "ingress"},
				{Protocol: "udp", TargetPort: 9000, PublishedPort: 9000, PublishMode: "host"},
			},
			PreviousSpec: &swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "web"}},
		},
		dockertest.FakeService{ID: "s2", Name: "plain", Mode: "global", Running: 1, Desired: 1},
	)
	rec, got := listServicesQuery(t, f, e, "")
	if rec.code != http.StatusOK || len(got.Items) != 2 {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	plain, web := got.Items[0], got.Items[1]
	if want := []string{"8080->80/tcp", "9000->9000/udp (host)"}; !slices.Equal(web.Ports, want) {
		t.Errorf("web ports = %v, want %v", web.Ports, want)
	}
	if web.UpdateMessage != "update paused" || !web.HasPreviousSpec || web.CreatedAt != "2026-09-01T08:30:00Z" {
		t.Errorf("web = %+v", web)
	}
	if plain.Ports == nil || len(plain.Ports) != 0 || plain.HasPreviousSpec || plain.UpdateMessage != "" {
		t.Errorf("plain = %+v, want an empty ports list and no previous spec", plain)
	}
	if !strings.Contains(string(rec.body), `"ports":[]`) {
		t.Errorf("a service with no ports must answer [] and not null: %s", rec.body)
	}
}

func TestServicesListCapsAt500(t *testing.T) {
	t.Parallel()
	rows := make([]dockertest.FakeService, 600)
	for i := range rows {
		rows[i] = dockertest.FakeService{ID: fmt.Sprintf("s%03d", i), Name: fmt.Sprintf("svc-%03d", i), Mode: "replicated", Running: 1, Desired: 1}
	}
	rows[599].Running = 0                       // none running is "not running", not degraded
	rows[598].Running, rows[598].Desired = 1, 2 // the one degraded service, past the cap
	f, e := serviceEnv(t, rows...)

	rec, got := listServicesQuery(t, f, e, "")
	if rec.code != http.StatusOK {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	if got.Total != 600 || got.UnfilteredTotal != 600 || len(got.Items) != 500 || !got.Capped {
		t.Fatalf("total/unfiltered/items/capped = %d/%d/%d/%v, want 600/600/500/true", got.Total, got.UnfilteredTotal, len(got.Items), got.Capped)
	}
	if got.Items[0].Name != "svc-000" || got.Items[499].Name != "svc-499" {
		t.Errorf("the cap keeps the first 500 by name: %s .. %s", got.Items[0].Name, got.Items[499].Name)
	}

	// The filter runs before the cap: the one degraded service is past row 500 and is still found.
	rec, got = listServicesQuery(t, f, e, "?status=degraded")
	if rec.code != http.StatusOK || got.Total != 1 || got.Capped || namesOf(got.Items)[0] != "svc-598" {
		t.Fatalf("status=degraded = %d total %d capped %v items %v", rec.code, got.Total, got.Capped, namesOf(got.Items))
	}
	if got.StatusCounts.Degraded != 1 || got.UnfilteredTotal != 600 {
		t.Errorf("counts = %+v unfiltered %d", got.StatusCounts, got.UnfilteredTotal)
	}

	rec, got = listServicesQuery(t, f, e, "?q=svc-5")
	if rec.code != http.StatusOK || got.Total != 100 || got.Capped || len(got.Items) != 100 {
		t.Errorf("q=svc-5 = %d total %d items %d capped %v, want 100 rows", rec.code, got.Total, len(got.Items), got.Capped)
	}
}

// Bulk scale (plan 06-05, RES-02, D-10, P4 D-13): the targets array of the scale route
// behaves as the container bulk actions do.

func bulkScaleServices() []dockertest.FakeService {
	rep := func(id, name string) dockertest.FakeService {
		return dockertest.FakeService{ID: id, Name: name, Mode: "replicated", Running: 2, Desired: 2, Image: scaleImage, Version: 3}
	}
	return []dockertest.FakeService{
		rep("svc-a", "alpha"), rep("svc-b", "bravo"), rep("svc-c", "charlie"),
		{ID: "svc-g", Name: "bayplan-agent", Mode: "global", Running: 1, Desired: 1},
		{ID: "svc-j", Name: "backup", Mode: "replicated-job"},
	}
}

const bulkScaleBody = `{"targets":[` +
	`{"serviceId":"svc-a","replicas":4},{"serviceId":"svc-b","replicas":0},{"serviceId":"svc-c","replicas":7},` +
	`{"serviceId":"svc-g","replicas":2},{"serviceId":"svc-j","replicas":2}]}`

func TestBulkScaleSharesOneBulkID(t *testing.T) {
	t.Parallel()
	f, e := serviceEnv(t, bulkScaleServices()...)
	rec := f.scale(t, e, bulkScaleBody)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	ans := decode[scaleAnswer](t, rec)
	if ans.BulkID == "" || len(ans.Results) != 5 {
		t.Fatalf("answer = %s", rec.body)
	}
	var done, skipped int
	for i, r := range ans.Results {
		wantID := []string{"svc-a", "svc-b", "svc-c", "svc-g", "svc-j"}[i]
		if r.ServiceID != wantID {
			t.Errorf("result %d is %s, want %s (request order)", i, r.ServiceID, wantID)
		}
		switch r.Result {
		case "done":
			done++
		case "skipped":
			skipped++
			if r.Code != "not_scalable" {
				t.Errorf("%s skipped with code %q, want not_scalable", r.ServiceID, r.Code)
			}
		}
	}
	if done != 3 || skipped != 2 {
		t.Errorf("done %d, skipped %d, want 3 and 2: %s", done, skipped, rec.body)
	}

	rows := f.serviceAudits(t)
	if len(rows) != 5 {
		t.Fatalf("audit rows = %+v, want one per target, skipped ones included", rows)
	}
	var ok, failed int
	for _, row := range rows {
		if row.Action != audit.ActionServiceScaled {
			t.Errorf("row action = %s, want %s", row.Action, audit.ActionServiceScaled)
		}
		if row.Details["bulk"] != ans.BulkID {
			t.Errorf("row %s has bulk %q, want the response's %q", row.TargetID, row.Details["bulk"], ans.BulkID)
		}
		switch row.Result {
		case string(audit.ResultSuccess):
			ok++
		case string(audit.ResultFailure):
			failed++
			if row.Reason != "not_scalable" || row.Details["code"] != "not_scalable" {
				t.Errorf("failed row %s = reason %q code %q, want not_scalable", row.TargetID, row.Reason, row.Details["code"])
			}
		}
	}
	if ok != 3 || failed != 2 {
		t.Errorf("audit success %d, failure %d, want 3 and 2: %+v", ok, failed, rows)
	}

	t.Run("a bulk ID the caller sends is the one recorded", func(t *testing.T) {
		const mine = "0192d7a4-3c4b-7c1e-9d6a-5b1f2e3a4c5d"
		rec := f.scale(t, e, `{"bulkId":"`+mine+`","targets":[{"serviceId":"svc-a","replicas":9},{"serviceId":"svc-b","replicas":9}]}`)
		if rec.code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.code, rec.body)
		}
		if got := decode[scaleAnswer](t, rec).BulkID; got != mine {
			t.Errorf("bulkId = %q, want %q", got, mine)
		}
	})
}

func TestBulkScaleIsANoOpTheSecondTime(t *testing.T) {
	t.Parallel()
	f, e := serviceEnv(t, bulkScaleServices()...)
	const body = `{"targets":[{"serviceId":"svc-a","replicas":4},{"serviceId":"svc-b","replicas":0},{"serviceId":"svc-c","replicas":7}]}`
	if rec := f.scale(t, e, body); rec.code != http.StatusOK {
		t.Fatalf("first scale: %d %s", rec.code, rec.body)
	}
	writes := len(f.fake.Writes())
	before := len(f.serviceAudits(t))

	rec := f.scale(t, e, body)
	if rec.code != http.StatusOK {
		t.Fatalf("second scale: %d %s", rec.code, rec.body)
	}
	ans := decode[scaleAnswer](t, rec)
	if len(ans.Results) != 3 {
		t.Fatalf("answer = %s", rec.body)
	}
	for _, r := range ans.Results {
		if r.Result != "done" || r.Changed {
			t.Errorf("%s = %+v, want done with changed false", r.ServiceID, r)
		}
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("writes grew from %d to %d on a repeat, want none", writes, got)
	}
	rows := f.serviceAudits(t)[before:]
	if len(rows) != 3 {
		t.Fatalf("the repeat wrote %d audit rows, want 3: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Result != string(audit.ResultSuccess) || row.Details["code"] != "unchanged" || row.Details["bulk"] != ans.BulkID {
			t.Errorf("repeat row %s = %+v, want success, code unchanged, bulk %q", row.TargetID, row, ans.BulkID)
		}
	}
}

func TestBulkScaleRefusesDuplicatesAndTooMany(t *testing.T) {
	t.Parallel()
	f, e := serviceEnv(t, bulkScaleServices()...)
	before := len(f.fake.Writes())

	t.Run("a service named twice", func(t *testing.T) {
		rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-a","replicas":1},{"serviceId":"svc-b","replicas":1},{"serviceId":"svc-a","replicas":2}]}`)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "duplicate_target" {
			t.Errorf("= %d %s (%s), want 422 duplicate_target", rec.code, rec.body, rec.ptyp)
		}
	})

	build := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"targets":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"serviceId":"svc-%d","replicas":1}`, i)
		}
		b.WriteString(`]}`)
		return b.String()
	}

	t.Run("more than 100 targets", func(t *testing.T) {
		rec := f.scale(t, e, build(101))
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "too_many_targets" {
			t.Errorf("= %d (%s) %.200s, want 422 too_many_targets", rec.code, rec.ptyp, rec.body)
		}
	})

	t.Run("exactly 100 targets is accepted", func(t *testing.T) {
		rec := f.scale(t, e, build(100))
		if rec.code != http.StatusOK {
			t.Fatalf("status = %d: %.200s", rec.code, rec.body)
		}
		if got := len(decode[scaleAnswer](t, rec).Results); got != 100 {
			t.Errorf("results = %d, want 100", got)
		}
	})

	if got := len(f.fake.Writes()); got != before {
		t.Errorf("writes grew from %d to %d, want no write for a refused or unknown-service request", before, got)
	}
}

// Restart loops on the services list and the detail (plan 06-34, RES-13, D-14, D-15).

// failedTask is a task of a service that ended failed or rejected ago before now, with the
// desired state a finished task has.
func failedTask(id, service, state, errText string, now time.Time, ago time.Duration) swarm.Task {
	tk := swarm.Task{ID: id, ServiceID: service, NodeID: "node-1", Slot: 1, DesiredState: swarm.TaskStateShutdown}
	tk.CreatedAt = now.Add(-ago - time.Second)
	tk.Status = swarm.TaskStatus{State: swarm.TaskState(state), Err: errText, Timestamp: now.Add(-ago)}
	return tk
}

// loopEnv is a swarm manager whose history holds a service in a restart loop (web: three
// failures in three minutes, the newest says why), one close to it (api: two), one that
// was only updated by force (calm: four shutdown tasks that exited 137) and a job whose
// finished tasks are complete.
func loopEnv(t *testing.T) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(
		dockertest.FakeService{ID: "svc-web", Name: "web", Mode: "replicated", Running: 0, Desired: 1, Image: scaleImage},
		dockertest.FakeService{ID: "svc-api", Name: "api", Mode: "replicated", Running: 1, Desired: 1},
		dockertest.FakeService{ID: "svc-calm", Name: "calm", Mode: "replicated", Running: 1, Desired: 1},
		dockertest.FakeService{ID: "svc-job", Name: "nightly", Mode: "replicated-job", Running: 0, Desired: 0},
	)
	f, e := serviceEnvOf(t, cfg)
	// The tasks are dated from the fixture's clock, which the session also reads; the
	// cluster is not read until the first request, after the manager is given its history.
	now := f.clk.now()
	cfg.Tasks = []swarm.Task{
		failedTask("w1", "svc-web", "failed", "task: non-zero exit (1)", now, time.Minute),
		failedTask("w2", "svc-web", "failed", "task: non-zero exit (2)", now, 2*time.Minute),
		failedTask("w3", "svc-web", "rejected", "No such image: web:9", now, 3*time.Minute),
		failedTask("a1", "svc-api", "failed", "boom", now, time.Minute),
		failedTask("a2", "svc-api", "failed", "boom", now, 2*time.Minute),
		failedTask("c1", "svc-calm", "shutdown", "", now, time.Minute),
		failedTask("c2", "svc-calm", "shutdown", "", now, 2*time.Minute),
		failedTask("c3", "svc-calm", "shutdown", "", now, 3*time.Minute),
		failedTask("c4", "svc-calm", "shutdown", "", now, 4*time.Minute),
		failedTask("j1", "svc-job", "complete", "", now, time.Minute),
		failedTask("j2", "svc-job", "complete", "", now, 2*time.Minute),
		failedTask("j3", "svc-job", "complete", "", now, 3*time.Minute),
	}
	cfg.SwarmClusterID, cfg.HistoryLimit = "cluster-1", 5
	f.fake.Reconfigure(cfg)
	return f, e
}

func TestServicesListFlagsAServiceInARestartLoopAndNoOther(t *testing.T) {
	t.Parallel()
	f, e := loopEnv(t)
	rec, got := listServicesQuery(t, f, e, "")
	if rec.code != http.StatusOK {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	byName := map[string]serviceItem{}
	for _, it := range got.Items {
		byName[it.Name] = it
	}
	loop := byName["web"].RestartLoop
	if loop == nil || loop.State != "flagged" || loop.Count != 3 || loop.WindowMinutes != 10 || loop.LatestError != "task: non-zero exit (1)" {
		t.Fatalf("web restartLoop = %+v, want flagged, 3 failures in 10 minutes, the newest task's error", loop)
	}
	if _, err := time.Parse(time.RFC3339, loop.Since); err != nil {
		t.Errorf("since = %q: %v", loop.Since, err)
	}
	for _, name := range []string{"api", "calm", "nightly"} {
		if byName[name].RestartLoop != nil {
			t.Errorf("%s restartLoop = %+v, want none: two failures, forced-update shutdowns and a job's complete tasks are no loop", name, byName[name].RestartLoop)
		}
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, it := range raw.Items {
		if _, ok := it["restartLoop"]; ok != (string(it["name"]) == `"web"`) {
			t.Errorf("row %s has restartLoop present = %v, want it only on web", it["name"], ok)
		}
	}
	if got.StatusCounts.Loop != 1 {
		t.Errorf("statusCounts.loop = %d, want 1", got.StatusCounts.Loop)
	}
}

func TestServicesListFiltersByTheLoopStatusBeforeTheCapAndCountsItBeforeTheSearch(t *testing.T) {
	t.Parallel()
	f, e := loopEnv(t)
	rec, got := listServicesQuery(t, f, e, "?status=loop")
	if rec.code != http.StatusOK || !slices.Equal(namesOf(got.Items), []string{"web"}) || got.Total != 1 || got.UnfilteredTotal != 4 {
		t.Fatalf("status=loop = %d %v (total %d of %d), want only web of 4", rec.code, namesOf(got.Items), got.Total, got.UnfilteredTotal)
	}
	// The chip number does not shrink with the search, and the search still narrows the rows.
	_, got = listServicesQuery(t, f, e, "?q=api&status=loop")
	if len(got.Items) != 0 || got.StatusCounts.Loop != 1 {
		t.Errorf("q=api&status=loop = %v with loop count %d, want no rows and the chip still 1", namesOf(got.Items), got.StatusCounts.Loop)
	}
	// It joins the other statuses as a union.
	_, got = listServicesQuery(t, f, e, "?status=loop,degraded")
	if want := []string{"web"}; !slices.Equal(namesOf(got.Items), want) {
		// web has 0 of 1 running (not degraded) and is in a loop; api and calm run fully.
		t.Errorf("status=loop,degraded = %v, want %v", namesOf(got.Items), want)
	}
	rec, _ = listServicesQuery(t, f, e, "?status=sleepy")
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "status_invalid" || !strings.Contains(string(rec.body), "loop") {
		t.Errorf("unknown status = %d %s, want 422 status_invalid naming loop among the values", rec.code, rec.body)
	}
}

func TestTheServiceDetailCarriesTheRestartLoopOnlyWhileItHolds(t *testing.T) {
	t.Parallel()
	f, e := loopEnv(t)
	web := decode[struct {
		RestartLoop *restartLoopBody `json:"restartLoop"`
	}](t, f.get(t, e, "services/svc-web"))
	if web.RestartLoop == nil || web.RestartLoop.State != "flagged" || web.RestartLoop.Count != 3 || web.RestartLoop.WindowMinutes != 10 || web.RestartLoop.LatestError != "task: non-zero exit (1)" {
		t.Fatalf("web detail restartLoop = %+v", web.RestartLoop)
	}
	rec := f.get(t, e, "services/svc-api")
	if rec.code != http.StatusOK || strings.Contains(string(rec.body), `restartLoop`) {
		t.Fatalf("api detail = %d %s, want no restartLoop", rec.code, rec.body)
	}
}

// TestDetectingARestartLoopChangesNothingInDocker is the prohibition of RES-13: detection
// only flags. Listing and reading a flagged service sends the manager no request that
// could restart, stop, scale or update anything.
func TestDetectingARestartLoopChangesNothingInDocker(t *testing.T) {
	t.Parallel()
	f, e := loopEnv(t)
	listServicesQuery(t, f, e, "")
	f.get(t, e, "services/svc-web")
	for _, req := range f.fake.Requests() {
		for _, write := range []string{"/update", "/restart", "/stop", "/kill", "/create", "/remove", "/delete", "/scale", "/rollback"} {
			if strings.Contains(req, write) {
				t.Errorf("a request to the manager, %s, could change a service", req)
			}
		}
	}
	rec := f.get(t, e, "services/svc-web")
	if d := decode[sdDetail](t, rec); d.Running != 0 || d.Desired != 1 {
		t.Errorf("web = %d of %d after being flagged, want it left at 0 of 1", d.Running, d.Desired)
	}
}
