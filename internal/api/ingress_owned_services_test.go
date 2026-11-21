// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// The ingress proxy service on the services pages and routes (plan 24-16, D-29, D-41, DEP-08). Docker is
// a fake swarm manager holding the service as the enable path writes it; the router, policy, CSRF guard
// and audit writer are the real ones.

const ingressOwnedSentence = "This is managed by TierPlan ingress. Switch ingress off to remove it. Nothing was changed."

// ingressProxyService is the proxy service built from the values ProxyServiceSpec writes: its reserved
// name, its component label and its image.
func ingressProxyService() dockertest.FakeService {
	spec := ingressdeploy.ProxyServiceSpec(ingressdeploy.SpecInput{
		Image: ingressdeploy.IngressImage("0.1.0"), URL: "wss://dev.homelab.org.uk:7480/ingress/connect", SecretName: "tierplan-ingress-token",
	})
	return dockertest.FakeService{
		ID: "svc-ingress", Name: ingress.ServiceName, Mode: "global", Running: 1, Desired: 1,
		Image: spec.TaskTemplate.ContainerSpec.Image, Labels: spec.Labels, Version: 3, Spec: &spec,
	}
}

type ingressFlagItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Agent   bool   `json:"agent"`
	Ingress bool   `json:"ingress"`
}

func TestIngressServiceListedAndRemoveRefused(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil, ingressProxyService(), loneService())

	rec := f.get(t, e, "services")
	if rec.code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.code, rec.body)
	}
	var list struct {
		Items []ingressFlagItem `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &list); err != nil {
		t.Fatal(err)
	}
	byName := map[string]ingressFlagItem{}
	for _, it := range list.Items {
		byName[it.Name] = it
	}
	if got := byName[ingress.ServiceName]; got.ID != "svc-ingress" || !got.Ingress || got.Agent {
		t.Errorf("list item = %+v, want the proxy with ingress true and agent false", got)
	}
	if got := byName["lone"]; got.ID != "svc-lone" || got.Ingress {
		t.Errorf("list item = %+v, want a plain service without the ingress flag", got)
	}

	rec = f.get(t, e, "services/svc-ingress")
	if rec.code != http.StatusOK {
		t.Fatalf("detail = %d %s", rec.code, rec.body)
	}
	var detail ingressFlagItem
	if err := json.Unmarshal(rec.body, &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.Ingress || detail.Agent {
		t.Errorf("detail = %+v, want ingress true and agent false", detail)
	}

	rec = f.removeService(t, e, "svc-ingress", "")
	if rec.code != http.StatusConflict || rec.ptyp != "ingress_owned" {
		t.Fatalf("remove = %d %s (%s), want 409 ingress_owned", rec.code, rec.body, rec.ptyp)
	}
	var p actionProblem
	if err := json.Unmarshal(rec.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Detail != ingressOwnedSentence {
		t.Errorf("detail = %q, want %q", p.Detail, ingressOwnedSentence)
	}
	for _, w := range f.fake.Writes() {
		if strings.HasPrefix(w, "DELETE") {
			t.Errorf("writes = %v, want no ServiceRemove to reach the engine", f.fake.Writes())
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	// Typing the name does not lift it.
	if rec := f.removeService(t, e, "svc-ingress", "confirm="+ingress.ServiceName); rec.code != http.StatusConflict || rec.ptyp != "ingress_owned" {
		t.Errorf("remove with confirm = %d %s (%s), want 409 ingress_owned", rec.code, rec.body, rec.ptyp)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 2 || rows[0].Action != "service.removed" || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "ingress_owned" {
		t.Errorf("audit rows = %+v, want service.removed failures ingress_owned", rows)
	}
}

// ingressEnv is an environment whose swarm holds the proxy service with one running task and a log line
// of it, besides a plain service, so the reads that stay open can be asked.
func ingressEnv(t *testing.T) (*envFix, envBody) {
	t.Helper()
	return actionsEnv(t, func(c *dockertest.FakeConfig) {
		c.SwarmNodeID, c.SwarmClusterID = "node-1", "cluster-1"
		c.Tasks = []swarm.Task{tkRunning("tk-1", "svc-ingress", "node-1", 0, "c1", time.Minute)}
		c.ServiceLogs = map[string][]dockertest.FakeServiceLogLine{"svc-ingress": {svcLine("tk-1", "node-1", 10, "GET /health 200")}}
	}, ingressProxyService(), loneService())
}

func wantIngressOwned(t *testing.T, what string, rec *envRec) {
	t.Helper()
	if rec.code != http.StatusConflict || rec.ptyp != "ingress_owned" {
		t.Errorf("%s = %d %s (%s), want 409 ingress_owned", what, rec.code, rec.body, rec.ptyp)
	}
}

func TestIngressServiceWritesRefusedReadsAllowed(t *testing.T) {
	t.Parallel()
	f, e := ingressEnv(t)

	wantIngressOwned(t, "force update", f.forceUpdate(t, e, "svc-ingress", `{}`))
	wantIngressOwned(t, "rollback", f.rollback(t, e, "svc-ingress", `{"version":3}`))
	wantIngressOwned(t, "review", f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services/svc-ingress/review", `{"version":3,"spec":{}}`))
	wantIngressOwned(t, "apply", f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services/svc-ingress/apply", `{"version":3,"spec":{}}`))
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none: every refusal comes before the engine is touched", w)
	}
	if ups := f.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("updates = %+v, want none", ups)
	}
	for _, r := range f.serviceAudits(t) {
		if r.Result != string(audit.ResultFailure) || r.Reason != "ingress_owned" {
			t.Errorf("audit row = %+v, want a failure ingress_owned", r)
		}
	}

	// Scale: the plain service is scaled, the proxy is reported skipped.
	rec := f.scale(t, e, `{"targets":[{"serviceId":"svc-ingress","replicas":2},{"serviceId":"svc-lone","replicas":3}]}`)
	if rec.code != http.StatusOK {
		t.Fatalf("scale = %d %s", rec.code, rec.body)
	}
	got := decode[scaleAnswer](t, rec)
	byID := map[string]scaleResult{}
	for _, r := range got.Results {
		byID[r.ServiceID] = r
	}
	if r := byID["svc-ingress"]; r.Result != "skipped" || r.Code != "ingress_owned" || r.Detail != ingressOwnedSentence || r.Changed {
		t.Errorf("proxy result = %+v, want skipped with ingress_owned", r)
	}
	if r := byID["svc-lone"]; r.Result != "done" || !r.Changed || r.Replicas != 3 {
		t.Errorf("plain result = %+v, want scaled to 3", r)
	}
	for _, w := range f.fake.Writes() {
		if strings.Contains(w, "svc-ingress") {
			t.Errorf("write %q reached the proxy service", w)
		}
	}

	// Reads stay open: its logs are where the access log is read (RTE-05).
	for _, path := range []string{
		"services/svc-ingress/inspect", "services/svc-ingress/spec", "services/svc-ingress/tasks",
		"services/svc-ingress/logs?sid=" + sidA + "&tail=50&until=" + svcT0.Add(time.Minute).Format(time.RFC3339Nano),
	} {
		if rec := f.get(t, e, path); rec.code != http.StatusOK {
			t.Errorf("GET %s = %d %s, want 200", path, rec.code, rec.body)
		}
	}
}

func TestForgedLabelIsNotRefused(t *testing.T) {
	t.Parallel()
	// A user's own service that carries TierPlan's label under another name is theirs (D-41).
	forged := dockertest.FakeService{
		ID: "svc-forged", Name: "web", Mode: "replicated", Running: 1, Desired: 1, Version: 2,
		Image:  "localhost:5000/private/web@sha256:" + strings.Repeat("7", 64),
		Labels: map[string]string{ingress.LabelComponent: ingress.ComponentIngress},
	}
	f, e := actionsEnv(t, nil, forged)

	rec := f.list(t, e)
	if it := rec["web"]; it.Ingress {
		t.Errorf("list item = %+v, want ingress false for a forged label", it)
	}
	if rec := f.forceUpdate(t, e, "svc-forged", `{}`); rec.code != http.StatusOK {
		t.Fatalf("force update = %d %s (%s), want it done", rec.code, rec.body, rec.ptyp)
	}
	if ups := f.fake.ServiceUpdates(); len(ups) != 1 || ups[0].ServiceID != "svc-forged" {
		t.Errorf("updates = %+v, want one on the forged service", ups)
	}
	if rec := f.removeService(t, e, "svc-forged", ""); rec.code != http.StatusNoContent {
		t.Errorf("remove = %d %s (%s), want 204", rec.code, rec.body, rec.ptyp)
	}
}

func TestCreateServiceRefusesComponentLabel(t *testing.T) {
	t.Parallel()
	for name, mut := range map[string]func(map[string]any){
		"on the service": func(spec map[string]any) {
			spec["Labels"] = map[string]any{ingress.LabelComponent: ingress.ComponentIngress}
		},
		"on the container": func(spec map[string]any) {
			spec["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)["Labels"] = map[string]any{ingress.LabelComponent: "x"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := actionsEnv(t, nil, loneService())
			body := chatBody(t, mut, nil)
			rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services", body)
			if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "ingress_component_label" {
				t.Fatalf("create = %d %s (%s), want 422 ingress_component_label", rec.code, rec.body, rec.ptyp)
			}
			if w := f.fake.Writes(); len(w) != 0 {
				t.Errorf("writes = %v, want nothing created", w)
			}
			if rows := f.serviceAudits(t); len(rows) != 1 || rows[0].Action != audit.ActionServiceCreated || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "ingress_component_label" {
				t.Errorf("audit rows = %+v, want one service.created failure ingress_component_label", rows)
			}
			rec = f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services/review", body)
			if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "ingress_component_label" {
				t.Errorf("review = %d %s (%s), want 422 ingress_component_label", rec.code, rec.body, rec.ptyp)
			}
		})
	}
	// The same spec without the label is made.
	f, e := actionsEnv(t, nil, loneService())
	if rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services", chatBody(t, nil, nil)); rec.code != http.StatusCreated {
		t.Errorf("create without the label = %d %s (%s), want 201", rec.code, rec.body, rec.ptyp)
	}
}

// list reads the services list into its items by name.
func (f *envFix) list(t *testing.T, e envBody) map[string]ingressFlagItem {
	t.Helper()
	rec := f.get(t, e, "services")
	if rec.code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.code, rec.body)
	}
	var list struct {
		Items []ingressFlagItem `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]ingressFlagItem{}
	for _, it := range list.Items {
		out[it.Name] = it
	}
	return out
}
