// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// hexStaged is a stopped container joined to a network by the hexadecimal name "2c8a", and a running one on an ordinary
// network: the first could be started by the engine onto an ingress network whose ID starts with "2c8a" (T-25-47).
func hexStaged(c *dockertest.FakeConfig) {
	c.Containers = append(c.Containers,
		dockertest.FakeContainer{ID: "s1", Names: []string{"/staged"}, State: "exited",
			Networks: map[string]dockertest.FakeEndpoint{"2c8a": {}, "bridge": {NetworkID: "br0"}}},
		dockertest.FakeContainer{ID: "w1", Names: []string{"/web"}, State: "running",
			Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0"}}},
	)
}

// TestStandalonePreflightRefusesHexNetworkNames: a container stored on a network name made only of hexadecimal digits
// refuses the preview and the enable, named with that network, because the ingress network's ID is not known before it is
// made (T-25-47). A host without one has no such finding.
func TestStandalonePreflightRefusesHexNetworkNames(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	r.withBase(hexStaged)

	want := []string{"container staged on network name 2c8a"}
	f, ok := findingOf(previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodeNameShadowed)
	if !ok || f.Severity != ingressdeploy.SeverityError || !slices.Equal(f.Items, want) {
		t.Fatalf("finding = %+v (found %v), want an ingress_name_shadowed error with %q", f, ok, want)
	}
	if got := wantRefusal(t, r.rig, ingressdeploy.EnableRequest{}, ingressdeploy.CodeNameShadowed); !slices.Equal(got.Items, want) {
		t.Errorf("Enable's items = %q, want %q", got.Items, want)
	}
	if r.sys.deployCount() != 0 {
		t.Error("a refused enable deployed")
	}

	clean := newStandaloneRig(t, standaloneRigOptions{})
	if _, ok := findingOf(previewStandalone(t, clean, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodeNameShadowed); ok {
		t.Error("a host with no hexadecimal network name has an ingress_name_shadowed finding")
	}
}

// TestStandaloneEnsureDoesNotRemakeTheNetworkOverAHexName: a pass that would make a missing ingress network again (a new
// ID) does not while a container is stored on a hexadecimal network name, and says so; a missing container alone, with
// the network in place, is still repaired.
func TestStandaloneEnsureDoesNotRemakeTheNetworkOverAHexName(t *testing.T) {
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	r.withBase(hexStaged)

	h := r.held()
	h.Proxy = nil
	r.sys.setHost(h)
	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Fatalf("deploys after a pass with the network in place = %d, want 2: the container is repaired", got)
	}

	h = r.held()
	h.Proxy, h.NetworkSubnet = nil, ""
	r.sys.setHost(h)
	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Errorf("deploys after a pass with the network missing = %d, want 2: no new network over a hexadecimal name", got)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsureNameShadowed {
		t.Errorf("ensure class = %q, want %q", class, ingressdeploy.EnsureNameShadowed)
	}
}
