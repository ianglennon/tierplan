// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestStandaloneAccessLogsRecreate: with ingress on the switch is stored, audited once and answered as a recreate of the
// proxy's container, which the next pass does; a repeat changes nothing and audits nothing; with ingress off the value
// waits for the enable, and on a paused host it waits for the resume.
func TestStandaloneAccessLogsRecreate(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	if r.held().Proxy.AccessLog {
		t.Fatal("the proxy starts with the access log on")
	}

	res, err := r.svc.SetAccessLogs(ctx, testActor, r.envID, true)
	if err != nil {
		t.Fatalf("SetAccessLogs(on): %v", err)
	}
	if !res.AccessLogs || res.Applies != ingressdeploy.AppliesRecreate {
		t.Errorf("result = %+v, want on and recreate", res)
	}
	if n := r.auditCount(audit.ActionIngressAccessLogsChanged); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	if d := r.auditDetails(audit.ActionIngressAccessLogsChanged); !strings.Contains(d, `"state":"on"`) {
		t.Errorf("audit details = %s, want state on", d)
	}
	if !r.svc.WakePending() {
		t.Error("the change did not wake the loop")
	}
	r.round()
	if !r.held().Proxy.AccessLog {
		t.Error("after the pass the proxy's container does not carry the access-log variable")
	}
	deploys := r.sys.deployCount()

	if _, err := r.svc.SetAccessLogs(ctx, testActor, r.envID, true); err != nil {
		t.Fatalf("SetAccessLogs(on) again: %v", err)
	}
	if r.svc.WakePending() {
		t.Error("a repeat woke the loop")
	}
	r.round()
	if n := r.auditCount(audit.ActionIngressAccessLogsChanged); n != 1 {
		t.Errorf("a repeat recorded another row (%d in all)", n)
	}
	if got := r.sys.deployCount(); got != deploys {
		t.Errorf("a repeat called the deployer (%d, was %d)", got, deploys)
	}

	res, err = r.svc.SetAccessLogs(ctx, testActor, r.envID, false)
	if err != nil || res.AccessLogs || res.Applies != ingressdeploy.AppliesRecreate {
		t.Errorf("off result = %+v, %v; want off and recreate", res, err)
	}
	r.round()
	if r.held().Proxy.AccessLog {
		t.Error("after switching off the proxy's container still carries the access-log variable")
	}

	// A paused host stores the value and writes nothing until the resume.
	if _, err := r.istore.SetPaused(ctx, r.envID, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	deploys = r.sys.deployCount()
	res, err = r.svc.SetAccessLogs(ctx, testActor, r.envID, true)
	if err != nil || !res.AccessLogs || res.Applies != ingressdeploy.AppliesWhenResumed {
		t.Errorf("paused result = %+v, %v; want stored and when_resumed", res, err)
	}
	r.round()
	if got := r.sys.deployCount(); got != deploys {
		t.Errorf("a paused host was written to (%d deploys, was %d)", got, deploys)
	}

	// With ingress off the value is stored and nothing is written to the host.
	off := newStandaloneRig(t, standaloneRigOptions{})
	res, err = off.svc.SetAccessLogs(ctx, testActor, off.envID, true)
	if err != nil || !res.AccessLogs || res.Applies != ingressdeploy.AppliesWhenEnabled {
		t.Errorf("disabled result = %+v, %v; want stored and when_enabled", res, err)
	}
	st, err := off.istore.DeployState(ctx, off.envID)
	if err != nil || !st.AccessLogs || st.Enabled {
		t.Errorf("deploy state = %+v, %v; want the access log stored and ingress off", st, err)
	}
	if off.sys.deployCount() != 0 {
		t.Error("switching the access log on a disabled standalone host wrote to it")
	}
}

// TestStandaloneResume: a standalone host a restore paused is only observed until an Admin resumes it, which is audited
// once, wakes the loop and lets the next pass write again; resuming a host that is not paused records nothing.
func TestStandaloneResume(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	if _, err := r.istore.SetPaused(ctx, r.envID, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	h := r.held()
	h.Proxy, h.NetworkSubnet = nil, ""
	r.sys.setHost(h)
	r.round()
	if got := r.sys.deployCount(); got != 1 {
		t.Fatalf("a paused host was written to (%d deploys, want the enable's 1)", got)
	}

	changed, err := r.svc.Resume(ctx, testActor, r.envID)
	if err != nil || !changed {
		t.Fatalf("Resume = %v, %v; want true", changed, err)
	}
	if n := r.auditCount(audit.ActionIngressDeployResumed); n != 1 {
		t.Errorf("resume audit rows = %d, want 1", n)
	}
	if !r.svc.WakePending() {
		t.Error("resume did not wake the loop")
	}
	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Errorf("the pass after the resume made %d deploys in all, want 2", got)
	}

	changed, err = r.svc.Resume(ctx, testActor, r.envID)
	if err != nil || changed {
		t.Errorf("Resume of a host that is not paused = %v, %v; want false", changed, err)
	}
	if n := r.auditCount(audit.ActionIngressDeployResumed); n != 1 {
		t.Errorf("a second resume recorded a row (%d in all)", n)
	}
}
