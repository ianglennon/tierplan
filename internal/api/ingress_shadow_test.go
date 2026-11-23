// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// ingressNameSentence is the refusal of a start that would put a name of its own on the ingress network.
const ingressNameSentence = "This would put the container on TierPlan's ingress network under a name of its own, where it could take another stack's route. Disconnect it from that network first. Nothing was changed."

const shadowedCtrID = "5c5c5c5c5c5c333333333333333333333333333333333333333333333333cccc"

// shadowEngine is the standalone node with the ingress network (ID ing0) plus a network named "ing", the start of that
// ID, and a stopped container stored on "ing" under another stack's route name: once "ing" is gone the engine would
// resolve that endpoint to the ingress network at start (T-25-47).
func shadowEngine() dockertest.FakeConfig {
	cfg := standaloneEngine(true)
	cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{ID: "dec0dec0", Name: "ing", Driver: "bridge", Scope: "local", Subnets: []string{"10.40.0.0/24"}})
	cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
		ID: shadowedCtrID, Names: []string{"/impostor"}, Image: "nginx:1.27", State: "exited", Status: "Exited (0)",
		Networks: map[string]dockertest.FakeEndpoint{"ing": {Aliases: []string{ingress.Alias("zoo", "web")}}},
	})
	return cfg
}

func TestNetworkNamedWithTheIngressIDPrefixIsRefused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, shadowEngine())

	wantIngressOwnedRec(t, "connect to the shadowing network", f.connectNet(t, e, "dec0dec0", `{"containerId":"`+userWebID+`"}`))
	wantIngressOwnedRec(t, "create a shadowing network", f.createNet(t, e, `{"name":"in","driver":"bridge"}`))
	f.wantNothingSent(t, "a connect to or create of a network named with the ingress ID's start")

	// Another name is created as before.
	if rec := f.createNet(t, e, `{"name":"fresh","driver":"bridge"}`); rec.code != http.StatusCreated {
		t.Errorf("create fresh = %d %s (%s), want 201", rec.code, rec.body, rec.ptyp)
	}
}

func TestStartOfAContainerStoredUnderTheIngressIDPrefixIsRefused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, shadowEngine())

	for _, verb := range []string{"start", "restart"} {
		rec := f.act(t, e, actionJSON(verb, [2]string{e.ID, shadowedCtrID}))
		if rec.code != http.StatusOK {
			t.Fatalf("%s = %d %s", verb, rec.code, rec.body)
		}
		res := decode[actionBody](t, rec).Results
		if len(res) != 1 || res[0].Result != "failed" || res[0].Code != "ingress_owned" || res[0].Detail != ingressNameSentence {
			t.Errorf("%s results = %+v, want failed ingress_owned with the name sentence", verb, res)
		}
	}
	f.wantNothingSent(t, "a start of a container stored under the ingress ID's start")

	// A container off the ingress network starts as before.
	rec := f.act(t, e, actionJSON("restart", [2]string{e.ID, userWebID}))
	if res := decode[actionBody](t, rec).Results; len(res) != 1 || res[0].Result != "done" {
		t.Errorf("restart of web = %+v, want done", res)
	}
	if w := f.fake.Writes(); !slices.Contains(w, "POST /containers/"+userWebID+"/restart") {
		t.Errorf("writes = %v, want the restart of web", w)
	}
}

// TestSwarmIngressTaskContainersRestart: on a swarm the ingress network is a non-attachable overlay whose task containers
// are the orchestrator's, so the standalone name rules do not hold them (D-46, U-07). A routed task container restarts as
// any container does, and the proxy's task restarts with its name typed; a network named with the overlay ID's start is
// created as before.
func TestSwarmIngressTaskContainersRestart(t *testing.T) {
	t.Parallel()
	cfg := ingressTaskEngine()
	cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{ID: "ovl0ovl0", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm", Subnets: []string{"10.50.0.0/24"}})
	cfg.Containers[0].Networks = map[string]dockertest.FakeEndpoint{ingress.NetworkName: {NetworkID: "ovl0ovl0"}}
	cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
		ID: "routed-1", Names: []string{"/shop_web.1.xyz"}, State: "running", Status: "Up 1 hour",
		Networks: map[string]dockertest.FakeEndpoint{ingress.NetworkName: {NetworkID: "ovl0ovl0", Aliases: []string{ingress.Alias("shop", "web")}}},
	})
	f, e := dashEnv(t, cfg)

	for label, body := range map[string]string{
		"routed task":    actionJSON("restart", [2]string{e.ID, "routed-1"}),
		"the proxy task": withConfirm(actionJSON("restart", [2]string{e.ID, "ing-task-1"}), "tierplan-ingress.abc.def"),
	} {
		rec := f.act(t, e, body)
		if rec.code != http.StatusOK {
			t.Fatalf("%s: restart = %d %s", label, rec.code, rec.body)
		}
		if res := decode[actionBody](t, rec).Results; len(res) != 1 || res[0].Result != "done" {
			t.Errorf("%s: restart results = %+v, want done", label, res)
		}
	}
	if rec := f.createNet(t, e, `{"name":"ovl","driver":"bridge"}`); rec.code != http.StatusCreated {
		t.Errorf("create ovl = %d %s (%s), want 201", rec.code, rec.body, rec.ptyp)
	}
}
