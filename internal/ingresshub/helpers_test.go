// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// testAudience and the nonces are what a hub would have handed the node; the service only sees the transcript.
const testAudience = "tierplan.test:7480"

var (
	testServerNonce = []byte("server-nonce-0123456789abcdef012")
	testNodeNonce   = []byte("node-nonce-0123456789abcdef01234")
	testEKM         = []byte("ekm-0123456789abcdef0123456789ab")
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// nodeList is what the fake provider holds for one environment.
type nodeList struct {
	ids []string
	at  time.Time
	ok  bool
}

// fakeNodeLists is an agents.NodeLists with lists a test sets. A refresh returns the held list unchanged.
type fakeNodeLists struct {
	mu    sync.Mutex
	lists map[string]nodeList
}

func (f *fakeNodeLists) set(envID string, l nodeList) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lists == nil {
		f.lists = map[string]nodeList{}
	}
	f.lists[envID] = l
}

func (f *fakeNodeLists) SwarmNodeIDs(envID string) ([]string, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lists[envID]
	return l.ids, l.at, l.ok
}

func (f *fakeNodeLists) RefreshSwarmNodeIDs(_ context.Context, envID string, _ time.Duration) ([]string, time.Time, bool) {
	return f.SwarmNodeIDs(envID)
}

// fixture is a real migrated store, a vault, an audit writer and the service under test.
type fixture struct {
	st    *store.Store
	clock *clock
	nodes *fakeNodeLists
	svc   *ingresshub.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	st := storetest.New(t)
	vlt, err := vault.LoadOrCreate("", filepath.Dir(st.Path), log)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	clk := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	aud := audit.NewWriter(st, vlt, log, clk.Now)
	nodes := &fakeNodeLists{}
	return &fixture{st: st, clock: clk, nodes: nodes, svc: ingresshub.NewService(st, aud, clk.Now).WithNodeLists(nodes)}
}

// insertEnv adds an agent environment with a fresh UUID and the given env_type ("" leaves it NULL, as for an
// agent that has not reported yet) and returns its ID.
func (f *fixture) insertEnv(t *testing.T, envType string) string {
	t.Helper()
	return f.insertEnvKind(t, "agent", envType)
}

func (f *fixture) insertEnvKind(t *testing.T, kind, envType string) string {
	t.Helper()
	id := uuid.NewString()
	var et sql.NullString
	if envType != "" {
		et = sql.NullString{String: envType, Valid: true}
	}
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		VALUES (?, ?, ?, '', 1, 1, 1, ?)`, id, kind, id, et); err != nil {
		t.Fatalf("insert environment: %v", err)
	}
	return id
}

// freshList holds the given node IDs for envID, read at the test clock.
func (f *fixture) freshList(envID string, ids ...string) {
	f.nodes.set(envID, nodeList{ids: ids, at: f.clock.Now(), ok: true})
}

// issue issues the environment's join token and returns it.
func (f *fixture) issue(t *testing.T, envID string) ingresshub.Token {
	t.Helper()
	tok, err := f.svc.IssueToken(context.Background(), audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}, envID, "10.0.0.1", "test")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	return tok
}

// newKey is a fresh Ed25519 key pair.
func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

// signEnrol returns an enrolment request whose proof of possession is a valid signature over the transcript the
// hub would rebuild for the key. The caller sets Token, Name and SwarmNodeID.
func signEnrol(t *testing.T, key ed25519.PrivateKey, serverNonce []byte, aud string, ekm []byte) ingresshub.EnrolRequest {
	t.Helper()
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("public key of an ed25519 private key is a %T", key.Public())
	}
	transcript := authn.IngressTranscript(ingresswire.ModeEnrol, ingresswire.ProtoVersion, serverNonce, testNodeNonce, aud, pub, ekm)
	return ingresshub.EnrolRequest{
		Pub: pub, Sig: ed25519.Sign(key, transcript), Transcript: transcript,
		Version: "0.1.0", IP: "192.0.2.10",
	}
}

// enrolRequest is a signed request carrying token, name and swarmNodeID with the standard nonces.
func enrolRequest(t *testing.T, key ed25519.PrivateKey, token, name, swarmNodeID string) ingresshub.EnrolRequest {
	t.Helper()
	req := signEnrol(t, key, testServerNonce, testAudience, testEKM)
	req.Token, req.Name, req.SwarmNodeID = token, name, swarmNodeID
	return req
}

// auditRows returns the details of every audit row of the action, oldest first.
func (f *fixture) auditRows(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_id, target_type, target_id, COALESCE(env_id, ''), details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.ActorID, &r.TargetType, &r.TargetID, &r.EnvID, &r.Details); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

type auditRow struct {
	ActorID, TargetType, TargetID, EnvID, Details string
}
