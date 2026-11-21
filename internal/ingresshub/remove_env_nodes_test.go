// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
)

// TestRemoveEnvNodesAuditsEach: every row of the environment, active or revoked, is deleted and each one is audited
// as ingress.node_removed under its own ID in the same transaction; another environment's rows stay, and a second call
// removes and records nothing.
func TestRemoveEnvNodesAuditsEach(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1", "node-2")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	b := f.enrolNode(t, envID, tok.Token, "edge-b", "node-2")
	f.markRevoked(t, b.res.NodeID)
	other, otherTok := f.swarmEnv(t, "node-9")
	c := f.enrolNode(t, other, otherTok.Token, "edge-c", "node-9")

	actor := audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}
	got, err := f.svc.RemoveEnvNodes(ctx, actor, envID)
	if err != nil {
		t.Fatalf("RemoveEnvNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("removed %d nodes, want 2: %+v", len(got), got)
	}
	ids := map[string]string{}
	for _, n := range got {
		ids[n.ID] = n.Name
	}
	if ids[a.res.NodeID] != "edge-a" || ids[b.res.NodeID] != "edge-b" {
		t.Errorf("removed = %v, want edge-a and edge-b", ids)
	}

	list, err := f.svc.ListNodes(ctx, envID)
	if err != nil || len(list) != 0 {
		t.Fatalf("ListNodes after removal = %+v, %v; want none", list, err)
	}
	if kept, err := f.svc.ListNodes(ctx, other); err != nil || len(kept) != 1 || kept[0].ID != c.res.NodeID {
		t.Fatalf("the other environment's nodes = %+v, %v; want its one node kept", kept, err)
	}

	rows := f.auditRows(t, audit.ActionIngressNodeRemoved)
	if len(rows) != 2 {
		t.Fatalf("ingress.node_removed rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.TargetType != "ingress_node" || r.EnvID != envID || ids[r.TargetID] == "" || r.ActorID != "u-admin" {
			t.Errorf("audit row = %+v", r)
		}
	}

	again, err := f.svc.RemoveEnvNodes(ctx, actor, envID)
	if err != nil || len(again) != 0 {
		t.Fatalf("second RemoveEnvNodes = %+v, %v; want nothing", again, err)
	}
	if rows := f.auditRows(t, audit.ActionIngressNodeRemoved); len(rows) != 2 {
		t.Errorf("a repeat recorded %d rows in all, want still 2", len(rows))
	}
}
