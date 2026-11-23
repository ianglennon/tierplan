// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The rig's ingress network has the ID "net-ingress". staged is a stopped container joined to a network by "net-i", the
// start of that ID, and web one on an ordinary network: once no network is named "net-i", the engine would start staged
// onto the ingress network at boot (T-25-47).
func shadowStaged(c *dockertest.FakeConfig) {
	c.Containers = append(c.Containers,
		dockertest.FakeContainer{ID: "s1", Names: []string{"/staged"}, State: "exited",
			Networks: map[string]dockertest.FakeEndpoint{"net-i": {}, "bridge": {NetworkID: "br0"}}},
		dockertest.FakeContainer{ID: "w1", Names: []string{"/web"}, State: "running",
			Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0"}}},
	)
}

// TestStandaloneEnableRefusesANameShadowingTheNewNetwork: the enable makes the network, finds a container stored on the
// start of its ID, and ends the switch item failed with ingress_name_shadowed, naming it; ingress stays off. A host where
// no stored name is the start of the ID, an innocent hexadecimal name included, is switched on.
func TestStandaloneEnableRefusesANameShadowingTheNewNetwork(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	r.withBase(shadowStaged)

	job := r.enable(ingressdeploy.EnableRequest{})
	it := itemOf(job, ingressdeploy.ItemSwitch)
	if it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeNameShadowed || !strings.Contains(it.Detail, "staged on network name net-i") {
		t.Fatalf("switch item = %+v, want failed ingress_name_shadowed naming staged", it)
	}
	if state, err := r.istore.DeployState(testsupport.Context(t), r.envID); err != nil || state.Enabled {
		t.Errorf("state = %+v (%v), want ingress still off", state, err)
	}

	clean := newStandaloneRig(t, standaloneRigOptions{})
	clean.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers, dockertest.FakeContainer{ID: "d1", Names: []string{"/db"}, State: "exited",
			Networks: map[string]dockertest.FakeEndpoint{"cafe": {}}})
	})
	wantStandaloneDone(t, clean.enable(ingressdeploy.EnableRequest{}))
}

// TestStandaloneEnsureTakesDownANetworkMadeOverAShadowingName: a pass that makes a missing ingress network again and finds
// a container stored on the start of its new ID takes the project down and says so; a pass that only repairs the
// container, with the network in place, is not judged.
func TestStandaloneEnsureTakesDownANetworkMadeOverAShadowingName(t *testing.T) {
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	r.withBase(shadowStaged)

	h := r.held()
	h.Proxy, h.NetworkSubnet = nil, ""
	r.sys.setHost(h)
	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Fatalf("deploys after the pass = %d, want 2", got)
	}
	if got := len(r.sys.downs); got != 1 {
		t.Errorf("downs = %d, want 1: the network made over a shadowing name is taken down", got)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureNameShadowed {
		t.Errorf("ensure class = %q, want %q", class, ingressdeploy.EnsureNameShadowed)
	}
}
