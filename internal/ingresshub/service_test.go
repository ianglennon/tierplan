// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// TestEnrolTracer is the tracer of the phase: a token issued for a swarm environment, a node the manager's list
// holds, a signed enrolment, and the node row and the audit row read back from the database.
func TestEnrolTracer(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	const swarmNode = "xyz1abc2def3ghi4jkl5mno6p"
	f.freshList(envID, "other-node", swarmNode)
	tok := f.issue(t, envID)
	if tok.Generation != 1 || tok.Rotated {
		t.Fatalf("first token = %+v, want generation 1 and not rotated", tok)
	}

	pub, key := newKey(t)
	res, err := f.svc.Enrol(ctx, enrolRequest(t, key, tok.Token, "edge-a", swarmNode))
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if id, err := uuid.Parse(res.NodeID); err != nil || id.Version() != 7 {
		t.Fatalf("node ID %q is not a UUIDv7 (%v)", res.NodeID, err)
	}
	if res.EnvID != envID || res.Name != "edge-a" || res.SwarmNodeID != swarmNode {
		t.Errorf("result = %+v", res)
	}

	// The row, read from the database.
	row, err := sqlc.New(f.st.Read).GetIngressNode(ctx, res.NodeID)
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}
	if row.Status != "active" || row.EnvID != envID || row.Name != "edge-a" {
		t.Errorf("row = %+v", row)
	}
	if !row.SwarmNodeID.Valid || row.SwarmNodeID.String != swarmNode {
		t.Errorf("swarm_node_id = %+v, want %q", row.SwarmNodeID, swarmNode)
	}
	if !bytes.Equal(row.PublicKey, pub) {
		t.Errorf("public key = %x, want %x", row.PublicKey, []byte(pub))
	}
	if row.CreatedAt != f.clock.Now().UnixNano() {
		t.Errorf("created_at = %d, want the clock %d", row.CreatedAt, f.clock.Now().UnixNano())
	}

	// Exactly one audit row, with the actor, target, environment and details of the plan.
	rows := f.auditRows(t, audit.ActionIngressNodeEnrolled)
	if len(rows) != 1 {
		t.Fatalf("%d %s rows, want 1", len(rows), audit.ActionIngressNodeEnrolled)
	}
	r := rows[0]
	if r.ActorID != "system:ingress" || r.TargetType != "ingress_node" || r.TargetID != res.NodeID || r.EnvID != envID {
		t.Errorf("audit row = %+v", r)
	}
	if want := `{"node":"edge-a","swarm_node":"` + swarmNode + `"}`; r.Details != want {
		t.Errorf("audit details = %s, want %s", r.Details, want)
	}
}

// TestEnrolRollsBackWhenTheAuditFails shows the node row and its audit row are one transaction: a failing audit
// insert leaves no node.
func TestEnrolRollsBackWhenTheAuditFails(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "node-1")
	tok := f.issue(t, envID)

	if _, err := f.st.Write.ExecContext(ctx, `CREATE TRIGGER fail_enrol_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'ingress.node_enrolled' BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	_, key := newKey(t)
	_, err := f.svc.Enrol(ctx, enrolRequest(t, key, tok.Token, "edge-a", "node-1"))
	if err == nil {
		t.Fatal("Enrol succeeded with a failing audit")
	}
	var re *ingresshub.RefuseError
	if errors.As(err, &re) {
		t.Fatalf("a failed audit is an internal error, not the refusal %v", re)
	}

	var nodes int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_nodes WHERE env_id = ?`, envID).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if nodes != 0 {
		t.Errorf("%d ingress_nodes rows remain after the rolled back enrolment", nodes)
	}
	if rows := f.auditRows(t, audit.ActionIngressNodeEnrolled); len(rows) != 0 {
		t.Errorf("%d audit rows remain", len(rows))
	}
}
