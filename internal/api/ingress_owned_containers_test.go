// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// The task containers of TierPlan's ingress proxy (plan 24-20, D-29, D-46, U-07). They are not refused
// (Swarm replaces a stopped task), but they carry the TierPlan ingress kind and need their name typed for a
// single action, as the agent's own container does. Docker is a fake daemon behind the real monitor, snapshot
// cache and aggregator; the router, policy, CSRF guard and audit writer are the real ones.

// proxyTaskLabels are the labels of a task container of the proxy: the ones the container spec carries and
// the swarm's own service-name label.
func proxyTaskLabels() map[string]string {
	l := ingress.ObjectLabels()
	l["com.docker.swarm.service.name"] = ingress.ServiceName
	return l
}

func ingressTaskEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: "ing-task-1", Names: []string{"/tierplan-ingress.abc.def"}, State: "running", Status: "Up 1 hour", Labels: proxyTaskLabels()},
			// A container of a user's own that carries only the component label is not the proxy: the label alone
			// is user-controlled (D-41).
			{ID: "forged-1", Names: []string{"/forged"}, State: "running", Status: "Up 1 hour", Labels: ingress.ObjectLabels()},
			{ID: "plain-1", Names: []string{"/plain"}, State: "running", Status: "Up 1 hour"},
		},
	}
}

func TestIngressTaskContainerSelfKind(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, ingressTaskEngine())

	kinds := map[string]string{}
	for _, it := range listContainersFor(t, f, e, "").Items {
		kinds[it.Name] = it.SelfKind
	}
	want := map[string]string{"tierplan-ingress.abc.def": "ingress", "forged": "", "plain": ""}
	for name, kind := range want {
		if kinds[name] != kind {
			t.Errorf("list: selfKind of %s = %q, want %q", name, kinds[name], kind)
		}
	}

	for id, kind := range map[string]string{"ing-task-1": "ingress", "forged-1": "", "plain-1": ""} {
		rec := f.get(t, e, "containers/"+id+"?nodeId="+e.ID)
		if rec.code != http.StatusOK {
			t.Fatalf("detail %s = %d %s", id, rec.code, rec.body)
		}
		var d struct {
			SelfKind string `json:"selfKind"`
			IsSelf   bool   `json:"isSelf"`
		}
		if err := json.Unmarshal(rec.body, &d); err != nil {
			t.Fatal(err)
		}
		if d.SelfKind != kind || d.IsSelf {
			t.Errorf("detail %s = %+v, want selfKind %q and not isSelf", id, d, kind)
		}
	}

	// A single action needs the name typed, as for the agent's own container; nothing is sent without it.
	one := actionJSON("stop", [2]string{e.ID, "ing-task-1"})
	for label, body := range map[string]string{"no confirm": one, "a wrong name": withConfirm(one, "yes"), "the id": withConfirm(one, "ing-task-1")} {
		rec := f.act(t, e, body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
			t.Errorf("%s: %d %q, want 422 confirmation_required: %s", label, rec.code, rec.ptyp, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("an unconfirmed stop sent %v", w)
	}
	rec := f.act(t, e, withConfirm(one, "tierplan-ingress.abc.def"))
	if rec.code != http.StatusOK {
		t.Fatalf("confirmed stop = %d %s", rec.code, rec.body)
	}
	if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" {
		t.Errorf("confirmed stop result = %+v, want done", res)
	}
	if w := f.fake.Writes(); len(w) != 1 {
		t.Errorf("writes = %v, want exactly the confirmed stop", w)
	}
	rows := f.actionAudits(t)
	if len(rows) == 0 || rows[len(rows)-1].Details["own"] != "ingress" {
		t.Errorf("audit rows = %+v, want the stop recorded as an action on an ingress container", rows)
	}

	// A plain container and the lookalike stop with no name typed.
	for _, id := range []string{"plain-1", "forged-1"} {
		if rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, id})); rec.code != http.StatusOK {
			t.Errorf("stop of %s = %d %s, want 200 with no name typed", id, rec.code, rec.body)
		}
	}

	// In a bulk, the proxy's container is skipped and the others run, as an own container is.
	g, ge := dashEnv(t, ingressTaskEngine())
	rec = g.act(t, ge, actionJSON("stop", [2]string{ge.ID, "ing-task-1"}, [2]string{ge.ID, "plain-1"}))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk stop = %d %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec).Results
	if len(got) != 2 || got[0].Result != "skipped" || got[1].Result != "done" {
		t.Errorf("bulk results = %+v, want the proxy's container skipped and the plain one done", got)
	}
	if w := g.fake.Writes(); !slices.Equal(w, []string{"POST /containers/plain-1/stop"}) {
		t.Errorf("bulk writes = %v, want only the plain container stopped", w)
	}
}
