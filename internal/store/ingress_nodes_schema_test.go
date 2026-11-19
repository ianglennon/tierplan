// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"reflect"
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
