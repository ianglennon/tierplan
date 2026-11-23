// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// standaloneDisableRig is a standalone host with ingress switched on, its proxy up and enrolled, and the deployer's
// switch-off dependencies (the kicker and the node rows) in place.
type standaloneDisableRig struct {
	*standaloneRig
	kicker *kickRecorder
}

// newStandaloneDisableRig enables ingress on an empty standalone host through the job, so the fake host holds the proxy
// container, the network and the volume the enable leaves, and enrols the proxy's node row.
func newStandaloneDisableRig(t *testing.T, deps func(*ingressdeploy.Deps)) *standaloneDisableRig {
	t.Helper()
	k := &kickRecorder{}
	r := newStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Kicker = k
		rows, ok := d.Tokens.(*ingresshub.Service)
		if !ok {
			t.Fatalf("the tokens are %T, want the hub service", d.Tokens)
		}
		d.NodeRows = rows
		if deps != nil {
			deps(d)
		}
	}})
	wantStandaloneDone(t, r.enable(ingressdeploy.EnableRequest{}))
	r.addNodeRow("active")
	k.mu.Lock()
	k.calls = nil
	k.mu.Unlock()
	return &standaloneDisableRig{standaloneRig: r, kicker: k}
}

// disable runs a switch-off and waits for its job to end.
func (r *standaloneDisableRig) disable() jobs.Job {
	r.t.Helper()
	id, err := r.svc.Disable(testsupport.Context(r.t), testActor, testOwner, r.envID, seesAll)
	if err != nil {
		r.t.Fatalf("Disable: %v", err)
	}
	return r.wait(id)
}

// hostHolds reports what the standalone host holds of the ingress proxy's objects, read through the engine as a person
// would: its containers by the component label, its networks by name and its volumes by name.
func (r *standaloneDisableRig) hostHolds() (container, network, volume bool) {
	r.t.Helper()
	ctx := testsupport.Context(r.t)
	cs, err := r.cli.ContainersWithLabel(ctx, ingress.LabelComponent, ingress.ComponentIngress)
	if err != nil {
		r.t.Fatalf("ContainersWithLabel: %v", err)
	}
	nets, err := r.cli.Networks(ctx)
	if err != nil {
		r.t.Fatalf("Networks: %v", err)
	}
	vol, err := r.cli.VolumeExists(ctx, ingress.VolumeName)
	if err != nil {
		r.t.Fatalf("VolumeExists: %v", err)
	}
	return len(cs) > 0, slices.ContainsFunc(nets, func(n dockerx.NetworkRow) bool { return n.Name == ingress.NetworkName }), vol
}

// wantStandaloneDisableDone fails the test unless the job is the standalone switch-off and every item ended done.
func wantStandaloneDisableDone(t *testing.T, job jobs.Job) {
	t.Helper()
	want := []string{"switch_off", "proxy", "network", "volumes", "token", "nodes"}
	var got []string
	for _, it := range job.Items {
		got = append(got, it.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if job.Kind != ingressdeploy.JobKindDisable {
		t.Errorf("job kind = %q, want %q", job.Kind, ingressdeploy.JobKindDisable)
	}
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, it := range job.Items {
		if it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", it.ID, it.State, it.Code)
		}
	}
}

// TestStandaloneDisableRemovesEverything is the tracer: a switch-off of a standalone host whose proxy has enrolled stops
// delivery, closes the proxy's session, takes the project down, confirms the network gone, removes the cache volume,
// disables the join token and removes the node row, each in its item and in that order, and leaves the host with none of the
// three objects.
func TestStandaloneDisableRemovesEverything(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	ctx := testsupport.Context(t)
	if c, n, v := r.hostHolds(); !c || !n || !v {
		t.Fatalf("before the switch-off the host holds container %v, network %v, volume %v; want all three", c, n, v)
	}

	prev, err := r.svc.PreviewDisable(ctx, r.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	if len(prev.Findings) != 0 || prev.Removes.Container == "" || prev.Removes.Network != ingress.NetworkName ||
		len(prev.Removes.Volumes) != 1 || prev.Removes.Volumes[0].ID != testAgentID || len(prev.Unreachable) != 0 {
		t.Fatalf("preview = %+v, want nothing in the way, the container, the network and the one host's volume", prev)
	}

	job := r.disable()
	wantStandaloneDisableDone(t, job)

	if st, err := r.istore.DeployState(ctx, r.envID); err != nil || st.Enabled {
		t.Fatalf("DeployState = %+v, %v; want ingress off", st, err)
	}
	if got := r.auditCount("ingress.disabled"); got != 1 {
		t.Errorf("ingress.disabled audit rows = %d, want 1", got)
	}
	if got := r.kicker.calls; len(got) != 1 || got[0] != r.envID+" ingress_disabled" {
		t.Errorf("KickEnv calls = %v, want exactly one for the environment", got)
	}
	if got := r.sys.downs; len(got) != 1 || got[0] != r.envID+"/"+testAgentID {
		t.Errorf("DownSystem calls = %v, want one for the host's node", got)
	}

	if c, n, v := r.hostHolds(); c || n || v {
		t.Errorf("after the switch-off the host holds container %v, network %v, volume %v; want none", c, n, v)
	}
	if it := itemOf(job, ingressdeploy.ItemVolumes); it.Result["removed"] != "true" {
		t.Errorf("the volumes item's result = %v, want the volume removed", it.Result)
	}

	tok, err := r.tokens.TokenStatus(ctx, r.envID)
	if err != nil || !tok.Exists || !tok.Disabled {
		t.Errorf("TokenStatus = %+v, %v; want a disabled token that still exists", tok, err)
	}
	if rows, err := r.tokens.ListNodes(ctx, r.envID); err != nil || len(rows) != 0 {
		t.Errorf("ListNodes = %+v, %v; want no node row", rows, err)
	}
	for action, want := range map[string]int{
		"ingress.disabled": 1, "ingress.join_token_disabled": 1, "ingress.node_removed": 1, "ingress.wipe_queued": 0,
	} {
		if got := r.auditCount(action); got != want {
			t.Errorf("%s audit rows = %d, want %d", action, got, want)
		}
	}

	// The operation lock is free: it was given back before the job ended.
	if holder, held := r.locks.Held(r.envID); held {
		t.Errorf("the operation lock is still held after the job: %+v", holder)
	}
}
