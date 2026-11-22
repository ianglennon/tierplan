// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/base64"
	"encoding/json"
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

// ingressTokenSecret is a join token's swarm secret as the enable path writes it, or, without labels, a
// user's own secret of a name that starts the same way.
func ingressTokenSecret(id string, labelled bool) dockertest.FakeSecret {
	s := dockertest.FakeSecret{ID: id, Name: ingress.SecretPrefix + "g1-" + id}
	if labelled {
		s.Labels = ingress.ObjectLabels()
	}
	return s
}

// ingressVolumeEngine is a standalone node holding the proxy's cache volume, labelled or not, and a plain one.
func ingressVolumeEngine(labelled bool) dockertest.FakeConfig {
	var labels map[string]string
	if labelled {
		labels = ingress.ObjectLabels()
	}
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Volumes: []dockertest.FakeVolume{
			{Name: ingress.VolumeName, Driver: "local", Labels: labels},
			{Name: "plain", Driver: "local"},
		},
	}
}

func TestIngressVolumesAndSecretsRefused(t *testing.T) {
	t.Parallel()

	t.Run("volumes", func(t *testing.T) {
		t.Parallel()
		f, e := dashEnv(t, ingressVolumeEngine(true))
		post := f.volumeDeleter(t, e, "trusted")

		list := decode[listBody[ingressFlag]](t, f.get(t, e, "volumes"))
		flags := map[string]bool{}
		for _, it := range list.Items {
			flags[it.Name] = it.Ingress
		}
		if !flags[ingress.VolumeName] || flags["plain"] {
			t.Errorf("list flags = %v, want ingress true for %s only", flags, ingress.VolumeName)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "volumes/"+ingress.VolumeName)); !d.Ingress {
			t.Errorf("detail = %+v, want ingress true", d)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "volumes/plain")); d.Ingress {
			t.Errorf("detail of plain = %+v, want ingress false", d)
		}
		if rec := f.get(t, e, "volumes/"+ingress.VolumeName+"/inspect"); rec.code != http.StatusOK {
			t.Errorf("inspect = %d %s, want it open", rec.code, rec.body)
		}

		// A batch with the cache volume and a plain one removes the plain one and refuses the other.
		rec := post(deleteVolumesJSON("delete 2 volumes", [2]string{e.ID, ingress.VolumeName}, [2]string{e.ID, "plain"}))
		if rec.Code != http.StatusOK {
			t.Fatalf("delete = %d %s", rec.Code, rec.Body)
		}
		var got volumeDeleted
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 2 {
			t.Fatalf("body = %s (%v)", rec.Body, err)
		}
		if r := got.Results[0]; r.Name != ingress.VolumeName || r.Result != "failed" || r.Code != "ingress_owned" || r.Detail != ingressOwnedSentence {
			t.Errorf("cache volume result = %+v, want failed ingress_owned with the sentence", r)
		}
		if r := got.Results[1]; r.Name != "plain" || r.Result != "done" {
			t.Errorf("plain volume result = %+v, want done", r)
		}
		if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /volumes/plain"}) {
			t.Errorf("writes = %v, want only the plain volume removed", w)
		}
		// The single-volume form is refused the same way.
		rec = post(deleteVolumesJSON(ingress.VolumeName, [2]string{e.ID, ingress.VolumeName}))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ingress_owned"`) {
			t.Errorf("single delete = %d %s, want a failed result with ingress_owned", rec.Code, rec.Body)
		}
	})

	t.Run("a volume of the same name without the label", func(t *testing.T) {
		t.Parallel()
		f, e := dashEnv(t, ingressVolumeEngine(false))
		post := f.volumeDeleter(t, e, "trusted")
		list := decode[listBody[ingressFlag]](t, f.get(t, e, "volumes"))
		for _, it := range list.Items {
			if it.Ingress {
				t.Errorf("list item %+v has the flag without the label", it)
			}
		}
		rec := post(deleteVolumesJSON(ingress.VolumeName, [2]string{e.ID, ingress.VolumeName}))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done"`) {
			t.Fatalf("delete = %d %s, want it done", rec.Code, rec.Body)
		}
		if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /volumes/" + ingress.VolumeName}) {
			t.Errorf("writes = %v, want the volume removed", w)
		}
	})

	t.Run("secrets", func(t *testing.T) {
		t.Parallel()
		tok, plain := ingressTokenSecret("sec-tok", true), dockertest.FakeSecret{ID: "sec-plain", Name: "plain"}
		f, e := secretsEnv(t, []dockertest.FakeSecret{tok, plain})

		list := decode[listBody[ingressFlag]](t, f.get(t, e, "secrets"))
		flags := map[string]bool{}
		for _, it := range list.Items {
			flags[it.Name] = it.Ingress
		}
		if !flags[tok.Name] || flags["plain"] {
			t.Errorf("list flags = %v, want ingress true for %s only", flags, tok.Name)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "secrets/sec-tok")); !d.Ingress {
			t.Errorf("detail = %+v, want ingress true", d)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "secrets/sec-plain")); d.Ingress {
			t.Errorf("detail of plain = %+v, want ingress false", d)
		}
		if rec := f.get(t, e, "secrets/sec-tok/inspect"); rec.code != http.StatusOK {
			t.Errorf("inspect = %d %s, want it open", rec.code, rec.body)
		}

		wantIngressOwnedRec(t, "delete", f.deleteSecret(t, e, "sec-tok", "confirm="+tok.Name))
		wantIngressOwnedRec(t, "delete without the name", f.deleteSecret(t, e, "sec-tok", ""))
		wantIngressOwnedRec(t, "rotate preview", f.rotateCall(t, e, "secrets", "sec-tok", "rotate/preview", `{}`))
		wantIngressOwnedRec(t, "rotate", f.rotateCall(t, e, "secrets", "sec-tok", "rotate",
			`{"data":"`+base64.StdEncoding.EncodeToString([]byte("new-value"))+`"}`))
		if w := f.fake.Writes(); len(w) != 0 {
			t.Errorf("writes = %v, want none: every refusal comes before the engine is touched", w)
		}
		rows := f.secretAudits(t)
		if len(rows) == 0 || rows[0].Reason != "ingress_owned" {
			t.Errorf("audit rows = %+v, want the delete refusal recorded with ingress_owned", rows)
		}
		// A plain secret is still removed.
		if rec := f.deleteSecret(t, e, "sec-plain", "confirm=plain"); rec.code != http.StatusNoContent {
			t.Errorf("delete of a plain secret = %d %s", rec.code, rec.body)
		}
	})

	t.Run("a secret of the same name without the label", func(t *testing.T) {
		t.Parallel()
		tok := ingressTokenSecret("sec-tok", false)
		f, e := secretsEnv(t, []dockertest.FakeSecret{tok})
		if d := decode[ingressFlag](t, f.get(t, e, "secrets/sec-tok")); d.Ingress {
			t.Errorf("detail = %+v, want ingress false without the label", d)
		}
		if rec := f.rotateCall(t, e, "secrets", "sec-tok", "rotate/preview", `{}`); rec.code != http.StatusOK {
			t.Errorf("rotate preview = %d %s (%s), want 200", rec.code, rec.body, rec.ptyp)
		}
		if rec := f.deleteSecret(t, e, "sec-tok", "confirm="+tok.Name); rec.code != http.StatusNoContent {
			t.Errorf("delete = %d %s (%s), want 204", rec.code, rec.body, rec.ptyp)
		}
	})
}

func TestCreateObjectsRefuseComponentLabel(t *testing.T) {
	t.Parallel()
	label := `"labels":{"tierplan.component":"ingress"}`
	wantLabel := func(what string, rec *envRec) {
		t.Helper()
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "ingress_component_label" {
			t.Errorf("%s = %d %s (%s), want 422 ingress_component_label", what, rec.code, rec.body, rec.ptyp)
		}
	}

	f, e := secretsEnv(t, nil)
	wantLabel("secret create", f.createSecret(t, e, secretBody("mine", []byte("v"), ","+label)))
	// Whatever the value of the label, a create does not set it.
	wantLabel("secret create with another value", f.createSecret(t, e, secretBody("mine", []byte("v"), `,"labels":{"tierplan.component":"x"}`)))
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("secret writes = %v, want none", w)
	}
	if rec := f.createSecret(t, e, secretBody("mine", []byte("v"), `,"labels":{"team":"a"}`)); rec.code != http.StatusCreated {
		t.Errorf("secret create with another label = %d %s, want 201", rec.code, rec.body)
	}

	g, ge := dashEnv(t, volumeEngine())
	wantLabel("volume create", g.createVol(t, ge, `{"name":"mine",`+label+`}`))
	if w := g.fake.Writes(); len(w) != 0 {
		t.Errorf("volume writes = %v, want none", w)
	}

	h, he := dashEnv(t, networkEngine())
	wantLabel("network create", h.createNet(t, he, `{"name":"mine","driver":"bridge",`+label+`}`))
	if w := h.fake.Writes(); len(w) != 0 {
		t.Errorf("network writes = %v, want none", w)
	}
}
