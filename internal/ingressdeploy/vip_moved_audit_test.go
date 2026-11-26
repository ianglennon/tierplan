// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// vipTestKeyHex is the hex of a VRRP key no audit row may hold.
const vipTestKeyHex = "5a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"

// failOver gives node-b the address from node-a: node-a is MASTER and fails its probe, node-b takes over.
func (v *vipRig) failOver(t *testing.T) {
	t.Helper()
	v.a.AppendLogs("ka-a",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeA, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(1*time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateMaster, "")),
	)
	v.b.AppendLogs("ka-b",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeB, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(1*time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateBackup, "")),
	)
	v.read()
	v.a.AppendLogs("ka-a", logLine(t, vipT0.Add(10*time.Second), vipStateEvent(vipNodeA, 3, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing)))
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(11*time.Second), vipStateEvent(vipNodeB, 3, ingressbundle.VIPStateMaster, "")))
	v.read()
	v.settle(v.svc)
}

// TestAuditProofVIPMoved is the proof of D-21 and A-07: a stored move writes one ingress.vip_moved row by system:ingress, in
// the move's own transaction, with the node names and the reason and no key.
func TestAuditProofVIPMoved(t *testing.T) {
	v := newVIPRig(t)
	v.failOver(t)

	if got := v.moves(); len(got) != 1 {
		t.Fatalf("%d moves stored, want 1: %+v", len(got), got)
	}
	rows := audittest.Rows(t, v.st, audit.ActionIngressVIPMoved)
	if len(rows) != 1 {
		t.Fatalf("%d ingress.vip_moved rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.ActorName != audit.SystemIngress.ID || r.TargetType != "environment" || r.TargetID != v.envID || r.EnvID != v.envID ||
		r.Result != string(audit.ResultSuccess) {
		t.Fatalf("row = %+v, want system:ingress, environment %s, success", r, v.envID)
	}
	if r.Details["from"] != "node-a" || r.Details["to"] != "node-b" || r.Details["reason"] != ingressdeploy.VIPMoveProbeFailing {
		t.Fatalf("details = %v, want from node-a, to node-b, reason %s", r.Details, ingressdeploy.VIPMoveProbeFailing)
	}
	audittest.AssertNoSecrets(t, rows, vipTestKeyHex)
}

// TestVIPMovedAuditedOnce: a restart replays the same log and the moves already stored write no second row.
func TestVIPMovedAuditedOnce(t *testing.T) {
	v := newVIPRig(t)
	v.failOver(t)

	fresh := ingressdeploy.New(v.deps)
	fresh.ReadVIPOnce(testsupport.Context(t), v.envID)
	v.settle(fresh)
	v.settle(fresh)

	if n := len(v.moves()); n != 1 {
		t.Fatalf("%d moves after a replay, want 1", n)
	}
	if n := len(audittest.Rows(t, v.st, audit.ActionIngressVIPMoved)); n != 1 {
		t.Fatalf("%d ingress.vip_moved rows after a replay, want 1", n)
	}
}

// TestVIPMoveStoredWithoutAudit: with no audit writer the move is stored and nothing is written to the log.
func TestVIPMoveStoredWithoutAudit(t *testing.T) {
	v := newVIPRig(t)
	deps := v.deps
	deps.Audit = nil
	v.svc = ingressdeploy.New(deps)
	v.failOver(t)

	got := v.moves()
	if len(got) != 1 || got[0].Reason != ingressdeploy.VIPMoveProbeFailing {
		t.Fatalf("moves = %+v, want the one move stored", got)
	}
	if n := len(audittest.Rows(t, v.st, audit.ActionIngressVIPMoved)); n != 0 {
		t.Fatalf("%d ingress.vip_moved rows with no audit writer, want 0", n)
	}
}
