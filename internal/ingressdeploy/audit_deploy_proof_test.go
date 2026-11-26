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
