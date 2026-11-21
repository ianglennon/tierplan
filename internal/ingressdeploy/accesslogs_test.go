// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// accessLogEnv is the environment entry the proxy reads to switch Traefik's access log on.
const accessLogEnv = ingressbundle.EnvAccessLog + "=1"

// proxyEnv is the environment of the proxy service as the fake swarm holds it.
func (r *rig) proxyEnv() []string {
	r.t.Helper()
	return r.liveSpec().TaskTemplate.ContainerSpec.Env
}

// auditDetails is the details of the newest audit row of an action.
func (r *rig) auditDetails(action string) string {
	r.t.Helper()
	var d string
	if err := r.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = ? ORDER BY seq DESC LIMIT 1`, action).Scan(&d); err != nil {
		r.t.Fatalf("read the audit details of %s: %v", action, err)
	}
	return d
}

// TestAccessLogsToggleRollsService: switching the access log on records the change and the next pass rolls the proxy with
// the entry added; off takes it away; a repeat changes nothing and records nothing; on an environment whose ingress is
// off the value is stored and nothing is written to the swarm.
func TestAccessLogsToggleRollsService(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledRig(t, rigOptions{})
	if slices.Contains(r.proxyEnv(), accessLogEnv) {
		t.Fatalf("the proxy starts with the access log on: %v", r.proxyEnv())
	}

	res, err := r.svc.SetAccessLogs(ctx, testActor, r.envID, true)
	if err != nil {
		t.Fatalf("SetAccessLogs(on): %v", err)
	}
	if !res.AccessLogs || res.Applies != ingressdeploy.AppliesRolling {
		t.Errorf("result = %+v, want on and rolling", res)
	}
	if n := r.auditCount(audit.ActionIngressAccessLogsChanged); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	if d := r.auditDetails(audit.ActionIngressAccessLogsChanged); !strings.Contains(d, `"state":"on"`) {
		t.Errorf("audit details = %s, want state on", d)
	}
	r.round()
	if !slices.Contains(r.proxyEnv(), accessLogEnv) {
		t.Errorf("after the pass the proxy env = %v, want %s", r.proxyEnv(), accessLogEnv)
	}
	updates := len(r.fake.ServiceUpdates())

	if _, err := r.svc.SetAccessLogs(ctx, testActor, r.envID, true); err != nil {
		t.Fatalf("SetAccessLogs(on) again: %v", err)
	}
	r.round()
	if n := r.auditCount(audit.ActionIngressAccessLogsChanged); n != 1 {
		t.Errorf("a repeat recorded another row (%d in all)", n)
	}
	if got := len(r.fake.ServiceUpdates()); got != updates {
		t.Errorf("a repeat wrote an update (%d, was %d)", got, updates)
	}

	res, err = r.svc.SetAccessLogs(ctx, testActor, r.envID, false)
	if err != nil {
		t.Fatalf("SetAccessLogs(off): %v", err)
	}
	if res.AccessLogs || res.Applies != ingressdeploy.AppliesRolling {
		t.Errorf("off result = %+v", res)
	}
	r.round()
	if slices.Contains(r.proxyEnv(), accessLogEnv) {
		t.Errorf("after switching off the proxy env = %v, want no %s", r.proxyEnv(), accessLogEnv)
	}

	// An environment whose ingress is off stores the value and writes nothing.
	off := newRig(t, rigOptions{})
	res, err = off.svc.SetAccessLogs(ctx, testActor, off.envID, true)
	if err != nil {
		t.Fatalf("SetAccessLogs on a disabled environment: %v", err)
	}
	if !res.AccessLogs || res.Applies != ingressdeploy.AppliesWhenEnabled {
		t.Errorf("disabled result = %+v, want stored and when_enabled", res)
	}
	st, err := off.istore.DeployState(ctx, off.envID)
	if err != nil || !st.AccessLogs || st.Enabled {
		t.Errorf("deploy state = %+v, %v; want the access log stored and ingress off", st, err)
	}
	if _, ok := off.service(); ok || len(off.fake.ServiceCreates()) != 0 {
		t.Error("switching the access log on a disabled environment wrote to the swarm")
	}
}

// TestAccessLogsWakesTheLoop: a change asks the running loop for a pass at once.
func TestAccessLogsWakesTheLoop(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledRig(t, rigOptions{})
	if _, err := r.svc.SetAccessLogs(ctx, testActor, r.envID, true); err != nil {
		t.Fatalf("SetAccessLogs: %v", err)
	}
	if !r.svc.WakePending() {
		t.Error("the change did not wake the loop")
	}
}

// TestResumeClearsThePauseAndWakes: a paused environment is observed until Resume, which records the audit row once,
// wakes the loop and lets the next pass write again; resuming one that is not paused records nothing.
func TestResumeClearsThePauseAndWakes(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledRig(t, rigOptions{})
	if _, err := r.istore.SetPaused(ctx, r.envID, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	r.removeService()
	r.round()
	if _, ok := r.service(); ok {
		t.Fatal("a paused environment was written to")
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
	if _, ok := r.service(); !ok {
		t.Error("the pass after the resume did not write the proxy service again")
	}

	changed, err = r.svc.Resume(ctx, testActor, r.envID)
	if err != nil || changed {
		t.Errorf("Resume of an environment that is not paused = %v, %v; want false", changed, err)
	}
	if n := r.auditCount(audit.ActionIngressDeployResumed); n != 1 {
		t.Errorf("resuming an unpaused environment recorded a row (%d in all)", n)
	}
}
