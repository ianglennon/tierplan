// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// The objects TierPlan's ingress creates, on the network, volume and secret routes (plan 24-20, D-29, D-41,
// D-46, DEP-08). Docker is a fake daemon behind the real monitor, snapshot cache and aggregator; the
// router, policy, CSRF guard and audit writer are the real ones.

// ingressNetEngine is a node with the ingress network, labelled as the enable path writes it or, when
// labelled is false, a user's own network of the same name, and a container on the network and one off it.
func ingressNetEngine(labelled bool) dockertest.FakeConfig {
	var labels map[string]string
	if labelled {
		labels = ingress.ObjectLabels()
	}
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "ing0", Name: ingress.NetworkName, Driver: "bridge", Scope: "local", Subnets: []string{"10.30.0.0/24"}, Labels: labels},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/web"}, State: "running"},
			{ID: "c2", Names: []string{"/db"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{
				ingress.NetworkName: {NetworkID: "ing0", IPv4: "10.30.0.3"}}},
		},
	}
}

// ingressFlag is the part of a network, volume or secret row and detail the ingress flag is read from.
type ingressFlag struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Ingress bool   `json:"ingress"`
}

func wantIngressOwnedRec(t *testing.T, what string, rec *envRec) {
	t.Helper()
	if rec.code != http.StatusConflict || rec.ptyp != "ingress_owned" {
		t.Errorf("%s = %d %s (%s), want 409 ingress_owned", what, rec.code, rec.body, rec.ptyp)
	}
}

func TestIngressNetworkRefused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, ingressNetEngine(true))
	awaitOnline(t, f.mon, e.ID)

	// The flag is on the row and on the detail; another network has none.
	list := decode[listBody[ingressFlag]](t, f.get(t, e, "networks"))
	flags := map[string]bool{}
	for _, it := range list.Items {
		flags[it.Name] = it.Ingress
	}
	if !flags[ingress.NetworkName] || flags["bridge"] {
		t.Errorf("list flags = %v, want ingress true for %s only", flags, ingress.NetworkName)
	}
	if d := decode[ingressFlag](t, f.get(t, e, "networks/ing0")); !d.Ingress {
		t.Errorf("detail = %+v, want ingress true", d)
	}
	if d := decode[ingressFlag](t, f.get(t, e, "networks/br0")); d.Ingress {
		t.Errorf("detail of bridge = %+v, want ingress false", d)
	}
	if rec := f.get(t, e, "networks/ing0/inspect"); rec.code != http.StatusOK {
		t.Errorf("inspect = %d %s, want it open", rec.code, rec.body)
	}

	// Every write on it is refused before the engine is touched.
	wantIngressOwnedRec(t, "delete", f.deleteNet(t, e, "ing0"))
	wantIngressOwnedRec(t, "delete orphan", f.delOrphan(t, e, "ing0", ""))
	wantIngressOwnedRec(t, "connect", f.connectNet(t, e, "ing0", `{"containerId":"c1"}`))
	wantIngressOwnedRec(t, "disconnect", f.disconnectNet(t, e, "ing0", `{"containerId":"c2"}`))
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none: every refusal comes before the engine is touched", w)
	}

	// The same network, with the sentence of the refusal.
	rec := f.deleteNet(t, e, "ing0")
	if !strings.Contains(string(rec.body), ingressOwnedSentence) {
		t.Errorf("refusal body = %s, want the ingress_owned sentence", rec.body)
	}
	// A refused delete leaves a failure row, like any refused delete of a network that exists.
	rows := f.networkAudits(t)
	if len(rows) == 0 || rows[0].Reason != "ingress_owned" {
		t.Errorf("audit rows = %+v, want failures with the code ingress_owned", rows)
	}

	// A create that sets the label is refused with the label's own problem.
	rec = f.createNet(t, e, `{"name":"mine","driver":"bridge","labels":{"tierplan.component":"ingress"}}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "ingress_component_label" {
		t.Errorf("create with the label = %d %s (%s), want 422 ingress_component_label", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestIngressNetworkNameWithoutTheLabelIsNotRefused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, ingressNetEngine(false))
	awaitOnline(t, f.mon, e.ID)

	list := decode[listBody[ingressFlag]](t, f.get(t, e, "networks"))
	for _, it := range list.Items {
		if it.Ingress {
			t.Errorf("list item %+v has the flag without the label", it)
		}
	}
	if d := decode[ingressFlag](t, f.get(t, e, "networks/ing0")); d.Ingress {
		t.Errorf("detail = %+v, want ingress false without the label", d)
	}
	if rec := f.connectNet(t, e, "ing0", `{"containerId":"c1"}`); rec.code != http.StatusNoContent {
		t.Fatalf("connect = %d %s (%s), want 204", rec.code, rec.body, rec.ptyp)
	}
	if rec := f.disconnectNet(t, e, "ing0", `{"containerId":"c2"}`); rec.code != http.StatusNoContent {
		t.Fatalf("disconnect = %d %s (%s), want 204", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /networks/ing0/connect", "POST /networks/ing0/disconnect"}) {
		t.Errorf("writes = %v, want the connect and the disconnect", w)
	}
}
