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

// The standalone ingress proxy container, its bridge network and its cache volume (plan 25-08, D-09, D-41).
// A swarm replaces a stopped proxy task, so Phase 24 only asks for a typed name; a standalone host has no
// orchestrator, so every write to the proxy is refused. The decision needs the component label and the
// reserved compose project and service together. Docker is a fake daemon behind the real monitor, snapshot
// cache and aggregator; the router, policy, CSRF guard and audit writer are the real ones.

const (
	standaloneProxyID  = "5a5a5a5a5a5a111111111111111111111111111111111111111111111111aaaa"
	standaloneProxyCtr = "tierplan-ingress-proxy-1"
	userWebID          = "7b7b7b7b7b7b222222222222222222222222222222222222222222222222bbbb"
)

// standaloneProxyLabels are the labels the system project puts on the proxy container: TierPlan's three
// object labels and compose's own project and service.
func standaloneProxyLabels() map[string]string {
	l := ingress.ObjectLabels()
	l["com.docker.compose.project"] = ingress.ProjectName
	l["com.docker.compose.service"] = ingress.ProxyComposeService
	return l
}

// standaloneEngine is a standalone node with the proxy container on its bridge network, a user's container, a
// second network and the cache volume. labelled false makes the network and the volume (and the proxy's
// labels) a look-alike without TierPlan's labels.
func standaloneEngine(labelled bool) dockertest.FakeConfig {
	var netLabels, volLabels, proxyLabels map[string]string
	proxyLabels = map[string]string{
		"com.docker.compose.project": ingress.ProjectName,
		"com.docker.compose.service": ingress.ProxyComposeService,
	}
	if labelled {
		netLabels, volLabels, proxyLabels = ingress.ObjectLabels(), ingress.ObjectLabels(), standaloneProxyLabels()
	}
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
			{ID: "ing0", Name: ingress.NetworkName, Driver: "bridge", Scope: "local", Subnets: []string{"10.30.0.0/24"}, Labels: netLabels},
			{ID: "app0", Name: "app-net", Driver: "bridge", Scope: "local", Subnets: []string{"10.20.0.0/24"}},
		},
		Volumes: []dockertest.FakeVolume{
			{Name: ingress.VolumeName, Driver: "local", Labels: volLabels},
		},
		Containers: []dockertest.FakeContainer{
			{ID: standaloneProxyID, Names: []string{"/" + standaloneProxyCtr}, Image: "tierplan-ingress:edge", State: "running", Status: "Up 1 hour",
				Labels: proxyLabels, Networks: map[string]dockertest.FakeEndpoint{
					ingress.NetworkName: {NetworkID: "ing0", IPv4: "10.30.0.2"},
					"app-net":           {NetworkID: "app0", IPv4: "10.20.0.2"},
				}},
			{ID: userWebID, Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
		},
	}
}

type ingressOwnedItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SelfKind     string `json:"selfKind"`
	IngressOwned bool   `json:"ingressOwned"`
}

func (f *envFix) wantNothingSent(t *testing.T, what string) {
	t.Helper()
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("%s sent %v, want nothing: the refusal comes before the engine is touched", what, w)
	}
}

func TestStandaloneProxyStopRefused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, standaloneEngine(true))

	rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, standaloneProxyID}))
	if rec.code != http.StatusOK {
		t.Fatalf("stop = %d %s", rec.code, rec.body)
	}
	res := decode[actionBody](t, rec).Results
	if len(res) != 1 || res[0].Result != "failed" || res[0].Code != "ingress_owned" || res[0].Detail != ingressOwnedSentence {
		t.Fatalf("results = %+v, want failed ingress_owned with the sentence", res)
	}
	f.wantNothingSent(t, "a stop of the standalone proxy")

	// The refusal is recorded as a failure, and no initiated row was written for a call that never happens.
	rows := f.actionAudits(t)
	if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != "ingress_owned" || rows[0].Details["stage"] != "" {
		t.Errorf("audit rows = %+v, want one failure ingress_owned with no stage", rows)
	}
}

func TestStandaloneProxyListedAsIngress(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, standaloneEngine(true))

	list := decode[listBody[ingressOwnedItem]](t, f.get(t, e, "containers"))
	got := map[string]ingressOwnedItem{}
	for _, it := range list.Items {
		got[it.Name] = it
	}
	if p := got[standaloneProxyCtr]; p.SelfKind != "ingress" || !p.IngressOwned {
		t.Errorf("list row of the proxy = %+v, want selfKind ingress and ingressOwned true", p)
	}
	if w := got["web"]; w.SelfKind != "" || w.IngressOwned {
		t.Errorf("list row of web = %+v, want neither", w)
	}

	d := decode[ingressOwnedItem](t, f.get(t, e, "containers/"+standaloneProxyID+"?nodeId="+e.ID))
	if d.SelfKind != "ingress" || !d.IngressOwned {
		t.Errorf("detail of the proxy = %+v, want selfKind ingress and ingressOwned true", d)
	}
	d = decode[ingressOwnedItem](t, f.get(t, e, "containers/"+userWebID+"?nodeId="+e.ID))
	if d.SelfKind != "" || d.IngressOwned {
		t.Errorf("detail of web = %+v, want neither", d)
	}
}

func TestStandaloneProxyWritesRefused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, standaloneEngine(true))

	for _, action := range []string{"start", "restart", "kill", "pause", "unpause", "stop"} {
		rec := f.act(t, e, actionJSON(action, [2]string{e.ID, standaloneProxyID}))
		if rec.code != http.StatusOK {
			t.Fatalf("%s = %d %s", action, rec.code, rec.body)
		}
		if r := decode[actionBody](t, rec).Results[0]; r.Result != "failed" || r.Code != "ingress_owned" {
			t.Errorf("%s result = %+v, want failed ingress_owned", action, r)
		}
	}
	for _, force := range []bool{false, true} {
		rec := f.remove(t, e, removeJSON(force, false, [2]string{e.ID, standaloneProxyID}))
		if rec.code != http.StatusOK {
			t.Fatalf("remove (force %t) = %d %s", force, rec.code, rec.body)
		}
		if r := decode[actionBody](t, rec).Results[0]; r.Result != "failed" || r.Code != "ingress_owned" {
			t.Errorf("remove (force %t) result = %+v, want failed ingress_owned", force, r)
		}
	}
	// A typed name does not lift it, as it does for a swarm task.
	rec := f.act(t, e, withConfirm(actionJSON("stop", [2]string{e.ID, standaloneProxyID}), standaloneProxyCtr))
	if r := decode[actionBody](t, rec).Results[0]; r.Result != "failed" || r.Code != "ingress_owned" {
		t.Errorf("confirmed stop result = %+v, want failed ingress_owned", r)
	}
	f.wantNothingSent(t, "the single writes")

	// In a bulk the proxy is skipped with the same code and the user's container is acted on.
	rec = f.act(t, e, actionJSON("stop", [2]string{e.ID, standaloneProxyID}, [2]string{e.ID, userWebID}))
	if rec.code != http.StatusOK {
		t.Fatalf("bulk stop = %d %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec).Results
	if len(got) != 2 || got[0].Result != "skipped" || got[0].Code != "ingress_owned" || got[1].Result != "done" {
		t.Errorf("bulk results = %+v, want the proxy skipped ingress_owned and web done", got)
	}
	rec = f.remove(t, e, removeJSON(true, false, [2]string{e.ID, standaloneProxyID}, [2]string{e.ID, userWebID}))
	got = decode[actionBody](t, rec).Results
	if len(got) != 2 || got[0].Result != "skipped" || got[0].Code != "ingress_owned" || got[1].Result != "done" {
		t.Errorf("bulk remove results = %+v, want the proxy skipped ingress_owned and web done", got)
	}
	if w := f.fake.Writes(); len(w) != 2 || !strings.Contains(w[0], "/containers/"+userWebID+"/stop") || !strings.HasPrefix(w[1], "DELETE /containers/"+userWebID) {
		t.Errorf("writes = %v, want only web stopped and removed", w)
	}
}

func TestStandaloneProxyExecRefused(t *testing.T) {
	t.Parallel()
	f := newShellFixOn(t, standaloneEngine(true), nil)
	rec := f.open(t, standaloneProxyID, openBody)
	if rec.code != http.StatusConflict || rec.ptyp != "ingress_owned" {
		t.Fatalf("%d %q, want 409 ingress_owned: %s", rec.code, rec.ptyp, rec.body)
	}
	if !strings.Contains(string(rec.body), ingressOwnedSentence) {
		t.Errorf("body %s lacks the ingress_owned sentence", rec.body)
	}
	f.refusedNothingStarted(t)
	if rec := f.open(t, userWebID, openBody); rec.code != http.StatusCreated {
		t.Fatalf("web: %d %s", rec.code, rec.body)
	}
}

func TestStandaloneProxyNetworkConnectRefused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, standaloneEngine(true))
	awaitOnline(t, f.mon, e.ID)

	// Connecting it to another network, with or without its name typed, and disconnecting it from one.
	for label, body := range map[string]string{
		"connect":           `{"containerId":"` + standaloneProxyID + `"}`,
		"connect confirmed": `{"containerId":"` + standaloneProxyID + `","confirm":"` + standaloneProxyCtr + `"}`,
	} {
		// The proxy is on app-net already, so connect it to the bridge instead.
		wantIngressOwnedRec(t, label, f.connectNet(t, e, "br0", body))
	}
	wantIngressOwnedRec(t, "disconnect", f.disconnectNet(t, e, "app0", `{"containerId":"`+standaloneProxyID+`"}`))
	wantIngressOwnedRec(t, "disconnect confirmed", f.disconnectNet(t, e, "app0",
		`{"containerId":"`+standaloneProxyID+`","confirm":"`+standaloneProxyCtr+`"}`))
	f.wantNothingSent(t, "a network change of the standalone proxy")

	// A user's container is connected as before.
	if rec := f.connectNet(t, e, "br0", `{"containerId":"`+userWebID+`"}`); rec.code != http.StatusNoContent {
		t.Errorf("connect of web = %d %s (%s), want 204", rec.code, rec.body, rec.ptyp)
	}
}

func TestStandaloneProxyLogsAndInspectOpen(t *testing.T) {
	t.Parallel()
	f := newLogsFix(t, standaloneEngine(true), nil)
	f.fake.AppendLogs(standaloneProxyCtr, dockertest.FakeLogLine{Text: "listening"})

	c := f.open(t, f.cookie, standaloneProxyID, "sid="+sidA, "")
	if ready := nextEvent(t, c, "ready"); ready.data != `{"tty":false}` {
		t.Errorf("ready = %s", ready.data)
	}
	if lines, _ := readLines(t, c, 1); lines[0].M != "listening" {
		t.Errorf("line = %+v, want the proxy's own", lines[0])
	}
	c.close()

	if rec := f.detail(t, f.cookie, f.env, standaloneProxyID+"/inspect", "?nodeId="+f.env.ID); rec.code != http.StatusOK {
		t.Errorf("inspect = %d %s, want it open", rec.code, rec.body)
	}
	if rec := f.detail(t, f.cookie, f.env, standaloneProxyID, "?nodeId="+f.env.ID); rec.code != http.StatusOK {
		t.Errorf("detail = %d %s, want it open", rec.code, rec.body)
	}
}

// A container with the component label and nothing more is a user's own (D-41): it is acted on normally, is
// no ingress and takes no refusal.
func TestComponentLabelAloneIsNotTheProxy(t *testing.T) {
	t.Parallel()
	const labelOnlyID, wrongSvcID, wrongProjID = "a1", "a2", "a3"
	label := ingress.ObjectLabels()
	wrongSvc := ingress.ObjectLabels()
	wrongSvc["com.docker.compose.project"] = ingress.ProjectName
	wrongSvc["com.docker.compose.service"] = "web"
	wrongProj := ingress.ObjectLabels()
	wrongProj["com.docker.compose.project"] = "shop"
	wrongProj["com.docker.compose.service"] = ingress.ProxyComposeService
	// The project and service without the component label are not the proxy either.
	noLabel := map[string]string{"com.docker.compose.project": ingress.ProjectName, "com.docker.compose.service": ingress.ProxyComposeService}
	cfg := dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: labelOnlyID, Names: []string{"/label-only"}, State: "running", Status: "Up 1 hour", Labels: label},
			{ID: wrongSvcID, Names: []string{"/wrong-service"}, State: "running", Status: "Up 1 hour", Labels: wrongSvc},
			{ID: wrongProjID, Names: []string{"/wrong-project"}, State: "running", Status: "Up 1 hour", Labels: wrongProj},
			{ID: "a4", Names: []string{"/names-only"}, State: "running", Status: "Up 1 hour", Labels: noLabel},
		},
	}
	f, e := dashEnv(t, cfg)

	for _, it := range decode[listBody[ingressOwnedItem]](t, f.get(t, e, "containers")).Items {
		if it.SelfKind != "" || it.IngressOwned {
			t.Errorf("list row %+v, want no selfKind and not ingressOwned", it)
		}
	}
	for _, id := range []string{labelOnlyID, wrongSvcID, wrongProjID, "a4"} {
		if d := decode[ingressOwnedItem](t, f.get(t, e, "containers/"+id+"?nodeId="+e.ID)); d.SelfKind != "" || d.IngressOwned {
			t.Errorf("detail of %s = %+v, want no selfKind and not ingressOwned", id, d)
		}
		rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, id}))
		if r := decode[actionBody](t, rec).Results[0]; rec.code != http.StatusOK || r.Result != "done" {
			t.Errorf("stop of %s = %d %+v, want done with no name typed", id, rec.code, r)
		}
	}
	if w := f.fake.Writes(); len(w) != 4 {
		t.Errorf("writes = %v, want the four stops", w)
	}
}

// A swarm proxy task keeps Phase 24's behaviour: a typed name for a single action, a skip in a bulk, and no
// ingressOwned flag.
func TestSwarmProxyTaskStillConfirmed(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, ingressTaskEngine())

	if d := decode[ingressOwnedItem](t, f.get(t, e, "containers/ing-task-1?nodeId="+e.ID)); d.SelfKind != "ingress" || d.IngressOwned {
		t.Errorf("detail of the swarm task = %+v, want selfKind ingress and ingressOwned false", d)
	}
	one := actionJSON("stop", [2]string{e.ID, "ing-task-1"})
	rec := f.act(t, e, one)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
		t.Fatalf("stop without the name = %d %q, want 422 confirmation_required: %s", rec.code, rec.ptyp, rec.body)
	}
	f.wantNothingSent(t, "an unconfirmed stop")
	rec = f.act(t, e, withConfirm(one, "tierplan-ingress.abc.def"))
	if r := decode[actionBody](t, rec).Results[0]; rec.code != http.StatusOK || r.Result != "done" || r.Code != "" {
		t.Errorf("confirmed stop = %d %+v, want done", rec.code, r)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /containers/ing-task-1/stop"}) {
		t.Errorf("writes = %v, want the confirmed stop", w)
	}
}

func TestStandaloneIngressNetworkAndVolumeRows(t *testing.T) {
	t.Parallel()

	t.Run("with the labels", func(t *testing.T) {
		t.Parallel()
		f, e := dashEnv(t, standaloneEngine(true))
		post := f.volumeDeleter(t, e, "trusted")

		nets := map[string]bool{}
		for _, it := range decode[listBody[ingressFlag]](t, f.get(t, e, "networks")).Items {
			nets[it.Name] = it.Ingress
		}
		if !nets[ingress.NetworkName] || nets["bridge"] || nets["app-net"] {
			t.Errorf("network list flags = %v, want ingress true for %s only", nets, ingress.NetworkName)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "networks/ing0")); !d.Ingress {
			t.Errorf("network detail = %+v, want ingress true", d)
		}
		vols := map[string]bool{}
		for _, it := range decode[listBody[ingressFlag]](t, f.get(t, e, "volumes")).Items {
			vols[it.Name] = it.Ingress
		}
		if !vols[ingress.VolumeName] {
			t.Errorf("volume list flags = %v, want ingress true for %s", vols, ingress.VolumeName)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "volumes/"+ingress.VolumeName)); !d.Ingress {
			t.Errorf("volume detail = %+v, want ingress true", d)
		}

		wantIngressOwnedRec(t, "delete network", f.deleteNet(t, e, "ing0"))
		wantIngressOwnedRec(t, "connect", f.connectNet(t, e, "ing0", `{"containerId":"`+userWebID+`"}`))
		wantIngressOwnedRec(t, "disconnect", f.disconnectNet(t, e, "ing0", `{"containerId":"`+standaloneProxyID+`"}`))
		rec := post(deleteVolumesJSON(ingress.VolumeName, [2]string{e.ID, ingress.VolumeName}))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ingress_owned"`) {
			t.Errorf("delete volume = %d %s, want a failed result with ingress_owned", rec.Code, rec.Body)
		}
		f.wantNothingSent(t, "a write on the ingress network or volume")
	})

	t.Run("a look-alike without the labels", func(t *testing.T) {
		t.Parallel()
		f, e := dashEnv(t, standaloneEngine(false))
		post := f.volumeDeleter(t, e, "trusted")

		for _, it := range decode[listBody[ingressFlag]](t, f.get(t, e, "networks")).Items {
			if it.Ingress {
				t.Errorf("network row %+v has the flag without the label", it)
			}
		}
		for _, it := range decode[listBody[ingressFlag]](t, f.get(t, e, "volumes")).Items {
			if it.Ingress {
				t.Errorf("volume row %+v has the flag without the label", it)
			}
		}
		if d := decode[ingressFlag](t, f.get(t, e, "networks/ing0")); d.Ingress {
			t.Errorf("network detail = %+v, want ingress false", d)
		}
		if d := decode[ingressFlag](t, f.get(t, e, "volumes/"+ingress.VolumeName)); d.Ingress {
			t.Errorf("volume detail = %+v, want ingress false", d)
		}
		if rec := f.connectNet(t, e, "ing0", `{"containerId":"`+userWebID+`"}`); rec.code != http.StatusNoContent {
			t.Fatalf("connect = %d %s (%s), want 204", rec.code, rec.body, rec.ptyp)
		}
		rec := post(deleteVolumesJSON(ingress.VolumeName, [2]string{e.ID, ingress.VolumeName}))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done"`) {
			t.Fatalf("delete volume = %d %s, want it done", rec.Code, rec.Body)
		}
		// The look-alike network is in use, so its delete is answered by the network rules, never by the
		// ingress refusal.
		if rec := f.deleteNet(t, e, "ing0"); rec.ptyp == "ingress_owned" {
			t.Errorf("delete network = %d %s, want no ingress_owned refusal", rec.code, rec.body)
		}
		if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /networks/ing0/connect", "DELETE /volumes/" + ingress.VolumeName}) {
			t.Errorf("writes = %v, want the connect and the volume delete to reach the engine", w)
		}
	})
}
