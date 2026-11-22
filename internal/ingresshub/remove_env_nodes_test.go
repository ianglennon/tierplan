// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// TestRemoveEnvNodesAuditsEach: every active row of the environment is deleted and each one is audited as
// ingress.node_removed under its own ID in the same transaction; a revoked row stays, because it is the record that
// keeps a revoked host out (WR-03); another environment's rows stay, and a second call removes and records nothing.
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
	if len(got) != 1 {
		t.Fatalf("removed %d nodes, want the one active node: %+v", len(got), got)
	}
	ids := map[string]string{}
	for _, n := range got {
		ids[n.ID] = n.Name
	}
	if ids[a.res.NodeID] != "edge-a" {
		t.Errorf("removed = %v, want edge-a", ids)
	}

	list, err := f.svc.ListNodes(ctx, envID)
	if err != nil || len(list) != 1 || list[0].ID != b.res.NodeID || list[0].Status != "revoked" {
		t.Fatalf("ListNodes after removal = %+v, %v; want only the revoked edge-b", list, err)
	}
	if kept, err := f.svc.ListNodes(ctx, other); err != nil || len(kept) != 1 || kept[0].ID != c.res.NodeID {
		t.Fatalf("the other environment's nodes = %+v, %v; want its one node kept", kept, err)
	}

	rows := f.auditRows(t, audit.ActionIngressNodeRemoved)
	if len(rows) != 1 {
		t.Fatalf("ingress.node_removed rows = %d, want 1", len(rows))
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
	if rows := f.auditRows(t, audit.ActionIngressNodeRemoved); len(rows) != 1 {
		t.Errorf("a repeat recorded %d rows in all, want still 1", len(rows))
	}
}

// TestRemoveEnvNodesKeepsTheBlockOnARevokedHost: an Admin revokes a node, ingress is switched off (its rows removed) and
// on again (a new token); the revoked host's proxy, which enrols with the swarm node ID, is still refused node_revoked
// (WR-03). The revoked row is the only place the block lives.
func TestRemoveEnvNodesKeepsTheBlockOnARevokedHost(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1", "node-2")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	f.enrolNode(t, envID, tok.Token, "edge-b", "node-2")
	if _, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{Reason: "compromised"}, "", ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, err := f.svc.RemoveEnvNodes(ctx, testAdmin, envID); err != nil {
		t.Fatalf("RemoveEnvNodes: %v", err)
	}
	// Switch on again: a new token, mounted in every node's proxy container.
	next := f.issue(t, envID)

	_, err := f.svc.Enrol(ctx, enrolRequest(t, a.key, next.Token, "edge-a", "node-1"))
	var re *ingresshub.RefuseError
	if !errors.As(err, &re) || re.Code != ingresswire.RefuseNodeRevoked {
		t.Fatalf("revoked host enrolling after a switch off and on: error %v, want node_revoked", err)
	}
	// A host that was not revoked enrols as new.
	f.enrolNode(t, envID, next.Token, "edge-b", "node-2")
}
