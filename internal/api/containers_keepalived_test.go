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

// The task containers of TierPlan's virtual IP service (plan 26-11, D-14, UI-SPEC S1). They are not refused
// (Swarm replaces a stopped task), but they carry their own owner kind, ingress_vip, so the page can word them
// as the virtual IP and not as the proxy, and they get the proxy task's care: a typed name for a single action,
// a skip in a bulk. Docker is a fake daemon behind the real monitor, snapshot cache and aggregator; the router,
// policy, CSRF guard and audit writer are the real ones.

const (
	keepalivedTaskID   = "kad-task-1"
	keepalivedTaskName = "tierplan-keepalived.abc.def"
)

// keepalivedTaskLabels are the labels of a task container of the keepalived service: the ones the container
// spec carries and the swarm's own service-name label.
func keepalivedTaskLabels() map[string]string {
	l := ingress.KeepalivedLabels()
	l["com.docker.swarm.service.name"] = ingress.ServiceNameKeepalived
	return l
}

func keepalivedTaskEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: keepalivedTaskID, Names: []string{"/" + keepalivedTaskName}, State: "running", Status: "Up 1 hour", Labels: keepalivedTaskLabels()},
			// A container of a user's own that carries only the keepalived component label is not the virtual IP
			// service: the label alone is user-controlled (D-41).
			{ID: "forged-kad", Names: []string{"/forged-kad"}, State: "running", Status: "Up 1 hour", Labels: ingress.KeepalivedLabels()},
			// The keepalived label on the proxy's service name is neither a proxy task nor a keepalived one.
			{ID: "mixed-1", Names: []string{"/mixed"}, State: "running", Status: "Up 1 hour", Labels: func() map[string]string {
				l := ingress.KeepalivedLabels()
				l["com.docker.swarm.service.name"] = ingress.ServiceName
				return l
			}()},
			// A proxy task keeps its own kind.
			{ID: "ing-task-1", Names: []string{"/tierplan-ingress.abc.def"}, State: "running", Status: "Up 1 hour", Labels: proxyTaskLabels()},
			{ID: "plain-1", Names: []string{"/plain"}, State: "running", Status: "Up 1 hour"},
		},
	}
}

func TestKeepalivedTaskIsOwnContainer(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, keepalivedTaskEngine())

	// The list row and the detail say ingress_vip for the task only. The lookalikes are not it; the one
	// that carries the keepalived label on the proxy's service name is not the proxy either.
	want := map[string]string{keepalivedTaskName: "ingress_vip", "forged-kad": "", "mixed": "", "tierplan-ingress.abc.def": "ingress", "plain": ""}
	kinds := map[string]string{}
	for _, it := range listContainersFor(t, f, e, "").Items {
		kinds[it.Name] = it.SelfKind
	}
	for name, kind := range want {
		if kinds[name] != kind {
			t.Errorf("list: selfKind of %s = %q, want %q", name, kinds[name], kind)
		}
	}
	for id, kind := range map[string]string{keepalivedTaskID: "ingress_vip", "forged-kad": "", "mixed-1": "", "ing-task-1": "ingress", "plain-1": ""} {
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

	// A single action needs the name typed; nothing is sent without it.
	one := actionJSON("stop", [2]string{e.ID, keepalivedTaskID})
	for label, body := range map[string]string{"no confirm": one, "a wrong name": withConfirm(one, "yes"), "the id": withConfirm(one, keepalivedTaskID)} {
		rec := f.act(t, e, body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
			t.Errorf("%s: %d %q, want 422 confirmation_required: %s", label, rec.code, rec.ptyp, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("an unconfirmed stop sent %v", w)
	}
	rec := f.act(t, e, withConfirm(one, keepalivedTaskName))
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
	if len(rows) == 0 || rows[len(rows)-1].Details["own"] != "ingress_vip" {
		t.Errorf("audit rows = %+v, want the stop recorded as an action on an ingress_vip container", rows)
	}

	// The lookalikes stop with no name typed.
	for _, id := range []string{"plain-1", "forged-kad", "mixed-1"} {
		if rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, id})); rec.code != http.StatusOK {
			t.Errorf("stop of %s = %d %s, want 200 with no name typed", id, rec.code, rec.body)
		}
	}

	// In a bulk the task is skipped with own_container and the ordinary container runs.
	g, ge := dashEnv(t, keepalivedTaskEngine())
	rec = g.act(t, ge, actionJSON("stop", [2]string{ge.ID, keepalivedTaskID}, [2]string{ge.ID, "plain-1"}))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk stop = %d %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec).Results
	if len(got) != 2 || got[0].Result != "skipped" || got[0].Code != "own_container" || got[1].Result != "done" {
		t.Errorf("bulk results = %+v, want the task skipped with own_container and the plain one done", got)
	}
	if w := g.fake.Writes(); !slices.Equal(w, []string{"POST /containers/plain-1/stop"}) {
		t.Errorf("bulk writes = %v, want only the plain container stopped", w)
	}
}
