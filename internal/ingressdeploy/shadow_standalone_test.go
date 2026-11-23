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

// TestStandaloneEnsureKeepsTheProxyDownOnAShadowedNetwork: a pass that finds the ingress network in place and a container
// stored on the start of its ID brings nothing up, whether the proxy is missing or running, and takes the project down.
// Compose leaves a network another container still uses, so each pass checks again rather than trusting the last down.
// Once a down removes the network, the next pass makes it again and checks the new one after the up.
func TestStandaloneEnsureKeepsTheProxyDownOnAShadowedNetwork(t *testing.T) {
	for _, missing := range []bool{true, false} {
		r := enabledStandaloneRig(t, standaloneRigOptions{})
		r.withBase(shadowStaged)
		r.sys.downKeepsNetwork = true
		if missing {
			h := r.held()
			h.Proxy = nil
			r.sys.setHost(h)
		}
		before := r.sys.deployCount()
		r.round()
		r.round()
		if got := r.sys.deployCount(); got != before {
			t.Errorf("missing=%v: deploys = %d, want %d: nothing is brought up on a shadowed network", missing, got, before)
		}
		if got := len(r.sys.downs); got != 2 {
			t.Errorf("missing=%v: downs = %d, want 2, one a pass", missing, got)
		}
		if _, class := r.ensureState(); class != ingressdeploy.EnsureNameShadowed {
			t.Errorf("missing=%v: ensure class = %q, want %q", missing, class, ingressdeploy.EnsureNameShadowed)
		}

		r.sys.mu.Lock()
		r.sys.downKeepsNetwork = false
		r.sys.mu.Unlock()
		r.round()
		r.round()
		// The first pass finds the kept network and removes it; the second makes it again and finds the name after the up.
		if got := r.sys.deployCount(); got != before+1 {
			t.Errorf("missing=%v: deploys = %d, want %d: the network is made again once it is gone", missing, got, before+1)
		}
		if got := len(r.sys.downs); got != 4 {
			t.Errorf("missing=%v: downs = %d, want 4: the up is checked once done", missing, got)
		}
	}
}
