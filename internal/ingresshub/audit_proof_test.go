// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// The node lifecycle and join token proofs of the audit table test (A-01 class P, D-23): each drives the real hub or
// service to the action, finds the one row of it with its documented detail keys, and shows no row holds the join token
// or its secret.

// proofOnly returns the only row of rows and fails when there is not exactly one.
func proofOnly(t *testing.T, rows []audit.Row, action string) audit.Row {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%d %s rows, want 1", len(rows), action)
	}
	return rows[0]
}

// proofDetails fails unless row's details are exactly want.
func proofDetails(t *testing.T, row audit.Row, want map[string]string) {
	t.Helper()
	if len(row.Details) != len(want) {
		t.Errorf("details = %v, want exactly %v", row.Details, want)
		return
	}
	for k, v := range want {
		if row.Details[k] != v {
			t.Errorf("details[%s] = %q, want %q (all: %v)", k, row.Details[k], v, row.Details)
		}
	}
}

// proofRow fails unless row is by actorType/actorName with result, on target of targetType in env.
func proofRow(t *testing.T, row audit.Row, actorType, actorName string, result audit.Result, targetType, target, env string) {
	t.Helper()
	if row.ActorType != actorType || row.ActorName != actorName {
		t.Errorf("actor = %s/%s, want %s/%s", row.ActorType, row.ActorName, actorType, actorName)
	}
	if row.Result != string(result) || row.TargetType != targetType || row.TargetID != target || row.EnvID != env {
		t.Errorf("row = result %q target %s/%q env %q, want %s on %s/%q in %q",
			row.Result, row.TargetType, row.TargetID, row.EnvID, result, targetType, target, env)
	}
}

// A-01 class P tracer: a node that enrols with a join token writes one success row by system:ingress with the node's
// name and swarm node; the token and its secret are in no row.
func TestAuditProofNodeEnrolled(t *testing.T) {
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	n := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	rows := audittest.Rows(t, f.st, audit.ActionIngressNodeEnrolled)
	row := proofOnly(t, rows, audit.ActionIngressNodeEnrolled)
	proofRow(t, row, string(audit.ActorSystem), audit.SystemIngress.Name, audit.ResultSuccess, "ingress_node", n.res.NodeID, envID)
	proofDetails(t, row, map[string]string{"node": "edge-a", "swarm_node": "node-1"})
	audittest.AssertNoSecrets(t, rows, tok.Token, secretOf(t, tok.Token))
}

// A node that presents a join token the hub does not accept writes one denied row: anonymous, no environment (the token
// names one of the caller's choosing), the refusal code and the mode, and never the token.
func TestAuditProofNodeEnrolRefused(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "swarmnode1")
	bad := env.Token + "x"

	c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(),
		ingresstest.Dial{Token: bad, Name: "node-1", SwarmNodeID: "swarmnode1"})
	if c.Refuse == nil || c.Welcome != nil {
		t.Fatalf("welcome %+v refuse %+v err %v, want a refusal", c.Welcome, c.Refuse, c.Err)
	}

	rows := audittest.Rows(t, srv.Store, audit.ActionIngressNodeEnrolRefused)
	row := proofOnly(t, rows, audit.ActionIngressNodeEnrolRefused)
	if row.ActorType != string(audit.ActorAnonymous) || row.Result != string(audit.ResultDenied) || row.EnvID != "" {
		t.Errorf("row = actor %q result %q env %q, want anonymous, denied and no environment", row.ActorType, row.Result, row.EnvID)
	}
	if row.Reason == "" || row.Details["code"] != ingresswire.RefuseAuthRejected || row.Details["mode"] != ingresswire.ModeEnrol {
		t.Errorf("row = reason %q details %v, want a reason, code %s and mode %s", row.Reason, row.Details, ingresswire.RefuseAuthRejected, ingresswire.ModeEnrol)
	}
	audittest.AssertNoSecrets(t, rows, bad, env.Token)
}

// Switching ingress off removes the active nodes: one success row per node, by the Admin, with the node's name only.
func TestAuditProofNodeRemoved(t *testing.T) {
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	n := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	if _, err := f.svc.RemoveEnvNodes(context.Background(), testAdmin, envID); err != nil {
		t.Fatalf("RemoveEnvNodes: %v", err)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressNodeRemoved)
	row := proofOnly(t, rows, audit.ActionIngressNodeRemoved)
	proofRow(t, row, string(audit.ActorUser), testAdmin.Name, audit.ResultSuccess, "ingress_node", n.res.NodeID, envID)
	proofDetails(t, row, map[string]string{"node": "edge-a"})
	audittest.AssertNoSecrets(t, rows, tok.Token, secretOf(t, tok.Token))
}

// A revoke writes one success row with the node, the swarm node, how many hostnames it held, whether re-enrolment is
// blocked and the Admin's reason.
func TestAuditProofNodeRevoked(t *testing.T) {
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	n := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	if _, err := f.svc.Revoke(context.Background(), testAdmin, envID, n.res.NodeID,
		ingresshub.RevokeOptions{Reason: "decommissioned"}, "192.0.2.1", "ua"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressNodeRevoked)
	row := proofOnly(t, rows, audit.ActionIngressNodeRevoked)
	proofRow(t, row, string(audit.ActorUser), testAdmin.Name, audit.ResultSuccess, "ingress_node", n.res.NodeID, envID)
	proofDetails(t, row, map[string]string{
		"node": "edge-a", "swarm_node": "node-1", "held": "0", "reenrol": "blocked", "reason": "decommissioned",
	})
	audittest.AssertNoSecrets(t, rows, tok.Token, secretOf(t, tok.Token))
}

// The first join token of an environment writes one success row on the environment with generation 1 and never the token.
func TestAuditProofJoinTokenCreated(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "node-1")
	tok := f.issue(t, envID)

	rows := audittest.Rows(t, f.st, audit.ActionIngressJoinTokenCreated)
	row := proofOnly(t, rows, audit.ActionIngressJoinTokenCreated)
	proofRow(t, row, string(audit.ActorUser), testAdmin.Name, audit.ResultSuccess, "environment", envID, envID)
	proofDetails(t, row, map[string]string{"generation": "1"})
	audittest.AssertNoSecrets(t, rows, tok.Token, secretOf(t, tok.Token))
}

// Issuing again rotates: one success row with generation 2, and neither the old nor the new token in it.
func TestAuditProofJoinTokenRotated(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "node-1")
	first := f.issue(t, envID)
	second := f.issue(t, envID)
	if !second.Rotated || second.Generation != 2 {
		t.Fatalf("second = %+v, want a rotation to generation 2", second)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressJoinTokenRotated)
	row := proofOnly(t, rows, audit.ActionIngressJoinTokenRotated)
	proofRow(t, row, string(audit.ActorUser), testAdmin.Name, audit.ResultSuccess, "environment", envID, envID)
	proofDetails(t, row, map[string]string{"generation": "2"})
	audittest.AssertNoSecrets(t, rows, first.Token, secretOf(t, first.Token), second.Token, secretOf(t, second.Token))
}

// Disabling the current token writes one success row with the generation that was switched off and never the token.
func TestAuditProofJoinTokenDisabled(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "node-1")
	tok := f.issue(t, envID)
	if err := f.svc.DisableToken(context.Background(), testAdmin, envID, "10.0.0.1", "test"); err != nil {
		t.Fatalf("DisableToken: %v", err)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressJoinTokenDisabled)
	row := proofOnly(t, rows, audit.ActionIngressJoinTokenDisabled)
	proofRow(t, row, string(audit.ActorUser), testAdmin.Name, audit.ResultSuccess, "environment", envID, envID)
	proofDetails(t, row, map[string]string{"generation": "1"})
	audittest.AssertNoSecrets(t, rows, tok.Token, secretOf(t, tok.Token))
}
