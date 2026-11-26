// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// onlyDeployRow fails the test unless rows holds exactly one row, and returns it.
func onlyDeployRow(t *testing.T, rows []audit.Row, action string) audit.Row {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%d %s rows, want 1: %+v", len(rows), action, rows)
	}
	return rows[0]
}

// wantDeployRow fails the test unless row is a success of actor on the environment envID.
func wantDeployRow(t *testing.T, row audit.Row, actor audit.Actor, envID string) {
	t.Helper()
	if row.ActorType != string(actor.Type) || row.ActorName != actor.Name {
		t.Errorf("actor = %s/%s, want %s/%s", row.ActorType, row.ActorName, actor.Type, actor.Name)
	}
	if row.Result != string(audit.ResultSuccess) || row.TargetType != "environment" || row.TargetID != envID || row.EnvID != envID {
		t.Errorf("row = result %q target %s/%q env %q, want success on environment %s", row.Result, row.TargetType, row.TargetID, row.EnvID, envID)
	}
}

// A-01 class A tracer: switching the proxy access log on through the service writes one ingress.access_logs_changed success
// row by the acting Admin with the new state and nothing else; the environment's join token is in no row.
func TestAuditProofAccessLogsChanged(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledRig(t, rigOptions{})
	tok, err := r.svc.IssueJoinToken(ctx, testActor, testOwner, r.envID, "203.0.113.5", "test-ua")
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}
	if _, err := r.svc.SetAccessLogs(ctx, testActor, r.envID, true); err != nil {
		t.Fatalf("SetAccessLogs: %v", err)
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressAccessLogsChanged)
	row := onlyDeployRow(t, rows, audit.ActionIngressAccessLogsChanged)
	wantDeployRow(t, row, testActor, r.envID)
	if len(row.Details) != 1 || row.Details["state"] != "on" {
		t.Errorf("details = %v, want exactly state on", row.Details)
	}
	audittest.AssertNoSecrets(t, rows, tok.Token)
}

// issueToken rotates the environment's join token and returns the new one, the secret every proof below passes to
// AssertNoSecrets.
func (r *rig) issueToken() string {
	r.t.Helper()
	tok, err := r.svc.IssueJoinToken(testsupport.Context(r.t), testActor, testOwner, r.envID, "203.0.113.5", "test-ua")
	if err != nil {
		r.t.Fatalf("IssueJoinToken: %v", err)
	}
	return tok.Token
}

// pause pauses the environment as a restore does.
func (r *rig) pause() {
	r.t.Helper()
	if _, err := r.istore.SetPaused(testsupport.Context(r.t), r.envID, true, audit.SystemRestore.ID, nil); err != nil {
		r.t.Fatalf("SetPaused: %v", err)
	}
}

// A-01 class A: an Admin resuming a paused environment writes one ingress.deploy_resumed success row by the Admin, with no
// details; the join token is in no row.
func TestAuditProofDeployResumed(t *testing.T) {
	ctx := testsupport.Context(t)
	r := enabledRig(t, rigOptions{})
	token := r.issueToken()
	r.pause()
	if changed, err := r.svc.Resume(ctx, testActor, r.envID); err != nil || !changed {
		t.Fatalf("Resume = %v, %v; want true", changed, err)
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressDeployResumed)
	row := onlyDeployRow(t, rows, audit.ActionIngressDeployResumed)
	wantDeployRow(t, row, testActor, r.envID)
	if len(row.Details) != 0 {
		t.Errorf("details = %v, want none", row.Details)
	}
	audittest.AssertNoSecrets(t, rows, token)
}

// A-01 class A: the first ensure pass over an environment a restore paused writes one ingress.deploy_paused success row by
// system:ingress; a second pass writes no second one; the join token is in no row.
func TestAuditProofDeployPaused(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	token := r.issueToken()
	r.pause()
	r.round()
	r.round()

	rows := audittest.Rows(t, r.st, audit.ActionIngressDeployPaused)
	row := onlyDeployRow(t, rows, audit.ActionIngressDeployPaused)
	wantDeployRow(t, row, audit.SystemIngress, r.envID)
	if len(row.Details) != 0 {
		t.Errorf("details = %v, want none", row.Details)
	}
	audittest.AssertNoSecrets(t, rows, token)
}

// A-01 class A: a switch-off that cannot reach a worker's agent queues its cache volume wipe and writes one
// ingress.wipe_queued success row by the Admin who switched ingress off, with the node's name and nothing else; the join
// token is in no row.
func TestAuditProofWipeQueued(t *testing.T) {
	f := newDisableFix(t, disableOpts{worker: true, workerAway: true})
	token := f.issueToken()
	wantDisableDone(t, f.disable(seesAll))

	rows := audittest.Rows(t, f.st, audit.ActionIngressWipeQueued)
	row := onlyDeployRow(t, rows, audit.ActionIngressWipeQueued)
	wantDeployRow(t, row, testActor, f.envID)
	if len(row.Details) != 1 || row.Details["node"] != workerNode.Name {
		t.Errorf("details = %v, want exactly node %q", row.Details, workerNode.Name)
	}
	audittest.AssertNoSecrets(t, rows, token)
}

// A-01 class A: the wipe loop, once the worker's agent is back, removes the queued volume and writes one
// ingress.wipe_done success row by system:ingress with the node's name and nothing else; the join token is in no row.
func TestAuditProofWipeDone(t *testing.T) {
	f, _ := awayFix(t)
	token := f.issueToken()
	f.reconnect()
	f.svc.WipeRound(testsupport.Context(t))
	if hasVolume(t, f.worker) || f.pendingCount() != 0 {
		t.Fatal("the wipe pass did not wipe the worker's cache volume")
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressWipeDone)
	row := onlyDeployRow(t, rows, audit.ActionIngressWipeDone)
	wantDeployRow(t, row, audit.SystemIngress, f.envID)
	if len(row.Details) != 1 || row.Details["node"] != workerNode.Name {
		t.Errorf("details = %v, want exactly node %q", row.Details, workerNode.Name)
	}
	audittest.AssertNoSecrets(t, rows, token)
}
