// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// fakeNodeLister lists the nodes a test sets.
type fakeNodeLister struct{ nodes []ingresshub.Node }

func (f fakeNodeLister) ListNodes(context.Context, string) ([]ingresshub.Node, error) {
	return f.nodes, nil
}

// TestStatusCounts: three swarm nodes, two enrolled proxies (one revoked row is not counted), one of them healthy on the
// current bundle; the objects come from the fake swarm; a switched-off ingress with objects left is incomplete.
func TestStatusCounts(t *testing.T) {
	nodes := fakeNodeLister{nodes: []ingresshub.Node{
		{ID: "n1", Status: "active", Health: "ok", AppliedBundleID: "bundle-2"},
		{ID: "n2", Status: "active", Health: "ok", AppliedBundleID: "bundle-1"},
		{ID: "n3", Status: "active", Health: "unhealthy", AppliedBundleID: "bundle-2"},
		{ID: "n4", Status: "revoked", Health: "ok", AppliedBundleID: "bundle-2"},
	}}
	r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Nodes = nodes
		d.CurrentBundle = func(string) (string, bool) { return "bundle-2", true }
	}})
	r.envs.set(func(i *ingressdeploy.EnvInfo) {
		i.Nodes = append(i.Nodes,
			ingressdeploy.NodeInfo{SwarmNodeID: "node-2", AgentID: "agent-2", Name: "worker-1", Role: "worker", Connected: true},
			ingressdeploy.NodeInfo{SwarmNodeID: "node-3", AgentID: "agent-3", Name: "worker-2", Role: "worker", Connected: true})
	})
	ctx := testsupport.Context(t)

	before, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status before enable: %v", err)
	}
	if before.Enabled || before.Paused || before.AccessLogs || !before.ObjectsKnown ||
		before.Objects != (ingressdeploy.StatusObjects{}) || before.DisablingIncomplete || before.TokenDisabled {
		t.Errorf("a new environment's status = %+v, want everything off and no objects", before)
	}

	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	got, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Enabled || got.Objects != (ingressdeploy.StatusObjects{Network: true, Service: true, Secret: true}) {
		t.Errorf("enabled status = %+v, want enabled with all three objects", got)
	}
	if got.Nodes != (ingressdeploy.StatusNodes{Expected: 3, Enrolled: 3, Ready: 1}) {
		t.Errorf("nodes = %+v, want expected 3, enrolled 3, ready 1", got.Nodes)
	}
	if got.DisablingIncomplete {
		t.Error("DisablingIncomplete is true while ingress is on")
	}

	if _, err := r.istore.SetEnabled(ctx, r.envID, false, "u-admin", nil); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	off, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status after switch off: %v", err)
	}
	if off.Enabled || !off.DisablingIncomplete {
		t.Errorf("status with ingress off and objects left = %+v, want disablingIncomplete", off)
	}
}

// TestStatusWithoutManagerOrNodes: no manager connected means the objects are not known; no node lister and no current
// bundle count nobody as ready; a standalone environment is refused.
func TestStatusWithoutManagerOrNodes(t *testing.T) {
	r := newRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
	st, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ObjectsKnown || st.DisablingIncomplete || st.Nodes.Enrolled != 0 || st.Nodes.Ready != 0 || st.Nodes.Expected != 1 {
		t.Errorf("status = %+v, want unknown objects and no proxies", st)
	}

	sa := newRig(t, rigOptions{standalone: true})
	if _, err := sa.svc.Status(ctx, sa.envID); !errors.Is(err, ingressdeploy.ErrStandaloneUnavailable) {
		t.Errorf("standalone status error = %v, want ErrStandaloneUnavailable", err)
	}
}

// TestStatusListsStacksToRedeploy: only the stacks that are not attached are listed, by name.
func TestStatusListsStacksToRedeploy(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.stacks.refs = []ingressdeploy.StackRef{
		{ID: "s2", Name: "web", HoldsClaims: true},
		{ID: "s1", Name: "api", HoldsClaims: false},
		{ID: "s3", Name: "done", HoldsClaims: true, Attached: true},
	}
	st, err := r.svc.Status(testsupport.Context(t), r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Redeploy) != 2 || st.Redeploy[0].Name != "api" || st.Redeploy[1].Name != "web" {
		t.Errorf("redeploy = %+v, want api then web", st.Redeploy)
	}
}
