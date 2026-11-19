// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/store/storetest"
)

// ingressNodesFixture is a migrated store with one agent environment, "env-1", the environment every
// ingress node and token test below hangs its rows on.
func ingressNodesFixture(t *testing.T) (*store.Store, *sqlc.Queries) {
	t.Helper()
	s := storetest.New(t)
	addAgentEnvironment(t, s, "env-1")
	return s, sqlc.New(s.Write)
}

func addAgentEnvironment(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if _, err := s.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		VALUES (?, 'agent', ?, '', 1, 1, 1, 'swarm')`, id, id); err != nil {
		t.Fatalf("insert environment %s: %v", id, err)
	}
}

// key32 is a 32-byte public key whose bytes are all b, so two nodes' keys differ by one argument.
func key32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func swarmNode(id string) sql.NullString { return sql.NullString{String: id, Valid: true} }

func insertIngressNode(t *testing.T, q *sqlc.Queries, id, envID string, swarmNodeID sql.NullString, created int64) error {
	t.Helper()
	return q.InsertIngressNode(context.Background(), sqlc.InsertIngressNodeParams{
		ID: id, EnvID: envID, Name: "proxy-" + id, SwarmNodeID: swarmNodeID,
		PublicKey: key32(1), Status: "active", CreatedAt: created,
	})
}

func TestIngressNodesTablesRoundTripThroughSqlc(t *testing.T) {
	ctx := context.Background()
	_, q := ingressNodesFixture(t)

	hash := bytes.Repeat([]byte{0xab}, 32)
	if err := q.InsertIngressJoinToken(ctx, sqlc.InsertIngressJoinTokenParams{
		ID: "tok-1", EnvID: "env-1", TokenHash: hash, Generation: 1, CreatedAt: 1234, CreatedBy: "u-1",
	}); err != nil {
		t.Fatalf("InsertIngressJoinToken: %v", err)
	}
	tok, err := q.IngressTokenStatus(ctx, "env-1")
	if err != nil {
		t.Fatalf("IngressTokenStatus: %v", err)
	}
	wantTok := sqlc.IngressJoinToken{
		ID: "tok-1", EnvID: "env-1", TokenHash: hash, Generation: 1, CreatedAt: 1234, CreatedBy: "u-1",
		SupersededAt: 0, DisabledAt: 0,
	}
	if !bytes.Equal(tok.TokenHash, wantTok.TokenHash) {
		t.Errorf("token hash = %x, want %x", tok.TokenHash, wantTok.TokenHash)
	}
	tok.TokenHash, wantTok.TokenHash = nil, nil
	if !reflect.DeepEqual(tok, wantTok) {
		t.Errorf("token = %+v, want %+v", tok, wantTok)
	}

	key := key32(7)
	if err := q.InsertIngressNode(ctx, sqlc.InsertIngressNodeParams{
		ID: "node-1", EnvID: "env-1", Name: "edge-a", SwarmNodeID: swarmNode("swarm-node-1"),
		PublicKey: key, Status: "active", CreatedAt: 5678,
	}); err != nil {
		t.Fatalf("InsertIngressNode: %v", err)
	}
	got, err := q.GetIngressNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}
	if !bytes.Equal(got.PublicKey, key) {
		t.Errorf("public key = %x, want %x", got.PublicKey, key)
	}
	got.PublicKey = nil
	want := sqlc.IngressNode{
		ID: "node-1", EnvID: "env-1", Name: "edge-a", SwarmNodeID: swarmNode("swarm-node-1"),
		Status: "active", Version: "", Schemas: "[]", AppliedBundleID: "", AppliedAt: 0,
		RejectedBundleID: "", RejectedReason: "", RejectedAt: 0, Health: "", HealthAt: 0,
		HeldHostnames: "[]", LastSeenAt: 0, CreatedAt: 5678, RevokedAt: 0, RevokedBy: "",
		RevokeReason: "", ReenrolBlocked: 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("node = %+v, want %+v", got, want)
	}

	listed, err := q.ListIngressNodes(ctx, "env-1")
	if err != nil || len(listed) != 1 || listed[0].ID != "node-1" {
		t.Errorf("ListIngressNodes = %+v, %v", listed, err)
	}
}

// wantConstraint fails the test unless err is a SQLite constraint failure of the given kind
// ("UNIQUE" or "CHECK").
func wantConstraint(t *testing.T, err error, kind, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want a %s constraint failure, got no error", what, kind)
	}
	if want := kind + " constraint failed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: error %q does not contain %q", what, err, want)
	}
}

func revokeIngressNode(t *testing.T, q *sqlc.Queries, id, envID string, blocked int64) {
	t.Helper()
	n, err := q.RevokeIngressNode(context.Background(), sqlc.RevokeIngressNodeParams{
		RevokedAt: 99, RevokedBy: "u-1", RevokeReason: "decommissioned", ReenrolBlocked: blocked, ID: id, EnvID: envID,
	})
	if err != nil || n != 1 {
		t.Fatalf("RevokeIngressNode %s = %d, %v", id, n, err)
	}
}

func TestIngressNodesOneActiveRowPerSwarmNode(t *testing.T) {
	s, q := ingressNodesFixture(t)
	addAgentEnvironment(t, s, "env-2")

	if err := insertIngressNode(t, q, "n-1", "env-1", swarmNode("sw-1"), 1); err != nil {
		t.Fatal(err)
	}
	wantConstraint(t, insertIngressNode(t, q, "n-2", "env-1", swarmNode("sw-1"), 2), "UNIQUE", "second active row, same swarm node")

	// A different swarm node of the same environment is a separate enrolment, and the same swarm node ID
	// in another environment does not collide either.
	if err := insertIngressNode(t, q, "n-3", "env-1", swarmNode("sw-2"), 3); err != nil {
		t.Fatalf("a second swarm node of one environment must insert: %v", err)
	}
	if err := insertIngressNode(t, q, "n-4", "env-2", swarmNode("sw-1"), 4); err != nil {
		t.Fatalf("the same swarm node ID in another environment must insert: %v", err)
	}

	// Once the first row is revoked, the host may enrol again.
	revokeIngressNode(t, q, "n-1", "env-1", 0)
	if err := insertIngressNode(t, q, "n-5", "env-1", swarmNode("sw-1"), 5); err != nil {
		t.Fatalf("insert after the first row was revoked: %v", err)
	}
	n, err := q.CountActiveIngressSwarmNode(context.Background(), sqlc.CountActiveIngressSwarmNodeParams{EnvID: "env-1", SwarmNodeID: swarmNode("sw-1")})
	if err != nil || n != 1 {
		t.Fatalf("CountActiveIngressSwarmNode = %d, %v, want 1", n, err)
	}
}

func TestIngressNodesOneActiveStandaloneRowPerEnvironment(t *testing.T) {
	s, q := ingressNodesFixture(t)
	addAgentEnvironment(t, s, "env-2")
	none := sql.NullString{}

	if err := insertIngressNode(t, q, "n-1", "env-1", none, 1); err != nil {
		t.Fatal(err)
	}
	wantConstraint(t, insertIngressNode(t, q, "n-2", "env-1", none, 2), "UNIQUE", "second active standalone row")
	if err := insertIngressNode(t, q, "n-3", "env-2", none, 3); err != nil {
		t.Fatalf("another environment's standalone row must insert: %v", err)
	}

	revokeIngressNode(t, q, "n-1", "env-1", 0)
	if err := insertIngressNode(t, q, "n-4", "env-1", none, 4); err != nil {
		t.Fatalf("insert after the first standalone row was revoked: %v", err)
	}
	n, err := q.CountActiveIngressStandalone(context.Background(), "env-1")
	if err != nil || n != 1 {
		t.Fatalf("CountActiveIngressStandalone = %d, %v, want 1", n, err)
	}
}

func TestIngressNodesBlockedRowsAreCounted(t *testing.T) {
	ctx := context.Background()
	_, q := ingressNodesFixture(t)
	none := sql.NullString{}

	for i, id := range []string{"sw-blocked", "sw-allowed", "sw-live"} {
		if err := insertIngressNode(t, q, "n-"+id, "env-1", swarmNode(id), int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertIngressNode(t, q, "n-sa-blocked", "env-1", none, 10); err != nil {
		t.Fatal(err)
	}
	revokeIngressNode(t, q, "n-sw-blocked", "env-1", 1)
	revokeIngressNode(t, q, "n-sw-allowed", "env-1", 0)
	revokeIngressNode(t, q, "n-sa-blocked", "env-1", 1)

	for _, c := range []struct {
		node string
		want int64
	}{{"sw-blocked", 1}, {"sw-allowed", 0}, {"sw-live", 0}, {"sw-unknown", 0}} {
		got, err := q.CountBlockedIngressSwarmNode(ctx, sqlc.CountBlockedIngressSwarmNodeParams{EnvID: "env-1", SwarmNodeID: swarmNode(c.node)})
		if err != nil || got != c.want {
			t.Errorf("CountBlockedIngressSwarmNode(%s) = %d, %v, want %d", c.node, got, err, c.want)
		}
	}
	if got, err := q.CountBlockedIngressStandalone(ctx, "env-1"); err != nil || got != 1 {
		t.Errorf("CountBlockedIngressStandalone = %d, %v, want 1", got, err)
	}

	// The revoke records who, when and why, and a second revoke of a revoked row changes nothing.
	row, err := q.GetIngressNode(ctx, "n-sw-blocked")
	if err != nil || row.Status != "revoked" || row.RevokedAt != 99 || row.RevokedBy != "u-1" || row.RevokeReason != "decommissioned" || row.ReenrolBlocked != 1 {
		t.Errorf("revoked row = %+v, %v", row, err)
	}
	again, err := q.RevokeIngressNode(ctx, sqlc.RevokeIngressNodeParams{RevokedAt: 100, RevokedBy: "u-2", RevokeReason: "x", ReenrolBlocked: 0, ID: "n-sw-blocked", EnvID: "env-1"})
	if err != nil || again != 0 {
		t.Errorf("second revoke changed %d rows, %v, want 0", again, err)
	}
}

func TestIngressNodesGoWithTheirEnvironment(t *testing.T) {
	ctx := context.Background()
	s, q := ingressNodesFixture(t)
	addAgentEnvironment(t, s, "env-2")

	if err := q.InsertIngressJoinToken(ctx, sqlc.InsertIngressJoinTokenParams{ID: "t-1", EnvID: "env-1", TokenHash: key32(2), Generation: 1, CreatedAt: 1, CreatedBy: "u-1"}); err != nil {
		t.Fatal(err)
	}
	if err := insertIngressNode(t, q, "n-1", "env-1", swarmNode("sw-1"), 1); err != nil {
		t.Fatal(err)
	}
	if err := insertIngressNode(t, q, "n-2", "env-2", swarmNode("sw-1"), 1); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Write.Exec(`DELETE FROM environments WHERE id = 'env-1'`); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"ingress_join_tokens": 0, "ingress_nodes": 1} {
		var n int
		if err := s.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s has %d rows (%v) after its environment was removed, want %d", table, n, err, want)
		}
	}
	var left string
	if err := s.Read.QueryRow(`SELECT env_id FROM ingress_nodes`).Scan(&left); err != nil || left != "env-2" {
		t.Errorf("the surviving node belongs to %q (%v), want env-2", left, err)
	}
}

func TestIngressNodesPublicKeyIs32Bytes(t *testing.T) {
	_, q := ingressNodesFixture(t)
	for _, n := range []int{0, 31, 33} {
		err := q.InsertIngressNode(context.Background(), sqlc.InsertIngressNodeParams{
			ID: "n-short", EnvID: "env-1", SwarmNodeID: swarmNode("sw-1"), PublicKey: make([]byte, n), Status: "active", CreatedAt: 1,
		})
		wantConstraint(t, err, "CHECK", "a public key of the wrong length")
	}
	if err := insertIngressNode(t, q, "n-ok", "env-1", swarmNode("sw-1"), 1); err != nil {
		t.Fatalf("a 32-byte key must insert: %v", err)
	}
}

func TestIngressJoinTokensOneCurrentPerEnvironment(t *testing.T) {
	ctx := context.Background()
	s, q := ingressNodesFixture(t)
	addAgentEnvironment(t, s, "env-2")
	add := func(id, env string, gen int64) error {
		return q.InsertIngressJoinToken(ctx, sqlc.InsertIngressJoinTokenParams{ID: id, EnvID: env, TokenHash: key32(3), Generation: gen, CreatedAt: gen, CreatedBy: "u-1"})
	}

	if err := add("t-1", "env-1", 1); err != nil {
		t.Fatal(err)
	}
	wantConstraint(t, add("t-2", "env-1", 2), "UNIQUE", "a second current token")
	if err := add("t-3", "env-2", 1); err != nil {
		t.Fatalf("another environment's current token must insert: %v", err)
	}

	next, err := q.NextIngressTokenGeneration(ctx, "env-1")
	if err != nil || next != 2 {
		t.Fatalf("NextIngressTokenGeneration = %d, %v, want 2", next, err)
	}
	if n, err := q.SupersedeCurrentIngressToken(ctx, sqlc.SupersedeCurrentIngressTokenParams{SupersededAt: 50, EnvID: "env-1"}); err != nil || n != 1 {
		t.Fatalf("SupersedeCurrentIngressToken = %d, %v, want 1", n, err)
	}
	if err := add("t-2", "env-1", next); err != nil {
		t.Fatalf("insert after the first token was superseded: %v", err)
	}
	if n, err := q.DisableCurrentIngressToken(ctx, sqlc.DisableCurrentIngressTokenParams{DisabledAt: 60, EnvID: "env-1"}); err != nil || n != 1 {
		t.Fatalf("DisableCurrentIngressToken = %d, %v, want 1", n, err)
	}

	all, err := q.ListIngressJoinTokens(ctx, "env-1")
	if err != nil || len(all) != 2 || all[0].ID != "t-2" || all[0].DisabledAt != 60 || all[1].ID != "t-1" || all[1].SupersededAt != 50 {
		t.Errorf("ListIngressJoinTokens = %+v, %v", all, err)
	}
	cur, err := q.IngressTokenStatus(ctx, "env-1")
	if err != nil || cur.ID != "t-2" {
		t.Errorf("IngressTokenStatus = %+v, %v, want t-2", cur, err)
	}
}
