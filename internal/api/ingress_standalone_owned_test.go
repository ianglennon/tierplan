// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
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
