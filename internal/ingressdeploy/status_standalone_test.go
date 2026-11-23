// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// healthyNode is the one proxy node of a standalone host: enrolled, healthy and on the current bundle.
func healthyNode(o *standaloneRigOptions) {
	o.deps = func(d *ingressdeploy.Deps) {
		d.Nodes = fakeNodeLister{nodes: []ingresshub.Node{{ID: "n1", Status: "active", Health: "ok", AppliedBundleID: "bundle-1"}}}
		d.CurrentBundle = func(string) (string, bool) { return "bundle-1", true }
	}
}

// TestStandaloneStatusReadsTheHost is the tracer: with ingress on and the proxy up, the status names the host's three
// objects, expects one node, counts it enrolled and ready from the node rows and the current bundle, and carries the
// ensure result as recorded and no token.
func TestStandaloneStatusReadsTheHost(t *testing.T) {
	ctx := testsupport.Context(t)
	var o standaloneRigOptions
	healthyNode(&o)
	r := enabledStandaloneRig(t, o)
	at := time.Date(2026, 11, 21, 12, 30, 0, 0, time.UTC)
	if err := r.istore.RecordEnsure(ctx, r.envID, at, ""); err != nil {
		t.Fatalf("RecordEnsure: %v", err)
	}

	got, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Enabled || got.Paused || !got.ObjectsKnown {
		t.Errorf("status = %+v, want ingress on, not paused and the objects known", got)
	}
	if want := (ingressdeploy.StatusObjects{Container: true, Network: true, Volume: true}); got.Objects != want {
		t.Errorf("objects = %+v, want %+v: the container, the network and the volume, no service and no secret", got.Objects, want)
	}
	if want := (ingressdeploy.StatusNodes{Expected: 1, Enrolled: 1, Ready: 1}); got.Nodes != want {
		t.Errorf("nodes = %+v, want %+v", got.Nodes, want)
	}
	if !got.LastEnsure.At.Equal(at) || got.LastEnsure.Class != "" {
		t.Errorf("last ensure = %+v, want %v with no class", got.LastEnsure, at)
	}
	if got.DisablingIncomplete || got.EnableFailed {
		t.Errorf("status = %+v, flags leftovers while ingress is on", got)
	}
	if out := fmt.Sprintf("%+v", got); strings.Contains(out, ingresswire.TokenPrefix) {
		t.Errorf("the status carries a join token: %s", out)
	}
}

// TestStandaloneStatusNotEnrolledAndUnreachable: the class the ensure loop recorded for a proxy that never enrolled is
// the status's own; a host that cannot be reached leaves the objects unknown and is not an error.
func TestStandaloneStatusNotEnrolledAndUnreachable(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	at := time.Date(2026, 11, 21, 12, 30, 0, 0, time.UTC)
	if err := r.istore.RecordEnsure(ctx, r.envID, at, "not_enrolled"); err != nil {
		t.Fatalf("RecordEnsure: %v", err)
	}

	got, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.LastEnsure.Class != "not_enrolled" || !got.LastEnsure.At.Equal(at) {
		t.Errorf("last ensure = %+v, want not_enrolled at %v", got.LastEnsure, at)
	}
	if want := (ingressdeploy.StatusNodes{Expected: 1}); got.Nodes != want {
		t.Errorf("nodes = %+v, want %+v: nothing enrolled", got.Nodes, want)
	}
	if !got.ObjectsKnown || !got.Objects.Container {
		t.Errorf("status = %+v, want the running proxy read", got)
	}

	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
	got, err = r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status of an unreachable host: %v", err)
	}
	if got.ObjectsKnown || got.Objects != (ingressdeploy.StatusObjects{}) {
		t.Errorf("status = %+v, want unknown and empty objects", got)
	}
	if got.Nodes.Expected != 1 || !got.Enabled || got.LastEnsure.Class != "not_enrolled" {
		t.Errorf("status = %+v, want the switches and the class read from the store", got)
	}
}

// TestStandaloneStatusLeftovers: with ingress off, an object the host still holds is a failed enable when that is what
// the ensure record says and an unfinished switch-off otherwise; a clean host is neither.
func TestStandaloneStatusLeftovers(t *testing.T) {
	ctx := testsupport.Context(t)
	left := hostFixture{Proxy: &proxyFixture{Image: "img"}, NetworkSubnet: "10.0.8.0/24", Volume: true}
	cases := []struct {
		name               string
		host               hostFixture
		class              string
		wantFailed, wantDI bool
	}{
		{"a clean host", hostFixture{}, "", false, false},
		{"a clean host after a failed enable", hostFixture{}, ingressdeploy.EnsureEnableFailed, false, false},
		{"everything left", left, "", false, true},
		{"everything left after a failed enable", left, ingressdeploy.EnsureEnableFailed, true, false},
		{"only the container", hostFixture{Proxy: &proxyFixture{Image: "img"}}, "", false, true},
		{"only the network", hostFixture{NetworkSubnet: "10.0.8.0/24"}, "", false, true},
		{"only the volume", hostFixture{Volume: true}, ingressdeploy.EnsureEnableFailed, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newStandaloneRig(t, standaloneRigOptions{host: c.host})
			if c.class != "" {
				if err := r.istore.RecordEnsure(ctx, r.envID, time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), c.class); err != nil {
					t.Fatalf("RecordEnsure: %v", err)
				}
			}
			got, err := r.svc.Status(ctx, r.envID)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got.Enabled || !got.ObjectsKnown {
				t.Fatalf("status = %+v, want ingress off and the host read", got)
			}
			if got.EnableFailed != c.wantFailed || got.DisablingIncomplete != c.wantDI {
				t.Errorf("enableFailed = %v, disablingIncomplete = %v; want %v and %v", got.EnableFailed, got.DisablingIncomplete, c.wantFailed, c.wantDI)
			}
		})
	}
}
