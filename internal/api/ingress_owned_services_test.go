// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
