// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// The ingress node routes (Phase 22, PRX-01): the join token, the node list and the revoke of one node.

const (
	nodesSwarmNodeA = "xyz1abc2def3ghi4jkl5mno6p"
	nodesSwarmNodeB = "bbb1abc2def3ghi4jkl5mno6p"
	nodesAudience   = "tierplan.test:7480"
)

// nodeListsStub is the swarm node list the service checks an enrolment against: every node it holds is fresh.
type nodeListsStub struct {
	ids []string
	now func() time.Time
}

func (s *nodeListsStub) SwarmNodeIDs(string) ([]string, time.Time, bool) {
	return s.ids, s.now(), true
}

func (s *nodeListsStub) RefreshSwarmNodeIDs(_ context.Context, _ string, _ time.Duration) ([]string, time.Time, bool) {
	return s.ids, s.now(), true
}

// nodesPath is a route of one environment's ingress join token or nodes.
func nodesPath(env string, rest ...string) string {
	return "/api/env/" + env + "/ingress/" + strings.Join(rest, "/")
}

// ingressTokenResp is the answer of the join token POST.
type ingressTokenResp struct {
	Token      string `json:"token"`
	Generation int    `json:"generation"`
	Rotated    bool   `json:"rotated"`
}

// ingressTokenStatusResp is the answer of the join token GET.
type ingressTokenStatusResp struct {
	Exists     bool   `json:"exists"`
	Generation int    `json:"generation"`
	CreatedAt  string `json:"createdAt"`
	CreatedBy  string `json:"createdBy"`
	Disabled   bool   `json:"disabled"`
}

// ingressNodeResp is the part of a listed node the tests read.
type ingressNodeResp struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	SwarmNodeID   string   `json:"swarmNodeId"`
	Status        string   `json:"status"`
	HeldHostnames []string `json:"heldHostnames"`
	Connected     bool     `json:"connected"`
	Sync          string   `json:"sync"`
	RevokedBy     string   `json:"revokedBy"`
	RevokeReason  string   `json:"revokeReason"`
	Reenrol       bool     `json:"reenrolBlocked"`
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

// makeSwarm turns an environment into an agent swarm environment, the kind an ingress node enrols into, and makes the
// service see the given swarm nodes.
func (f *ingressFix) makeSwarm(t *testing.T, env string, swarmNodes ...string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`UPDATE environments SET kind = 'agent', env_type = 'swarm' WHERE id = ?`, env); err != nil {
		t.Fatalf("make %s a swarm: %v", env, err)
	}
	f.ingressNodes.WithNodeLists(&nodeListsStub{ids: swarmNodes, now: f.clk.now})
}

// enrolNode enrols a node through the service with a token-signed request, as the hub would hand it over.
func (f *ingressFix) enrolNode(t *testing.T, token, name, swarmNodeID string) (ingresshub.EnrolResult, error) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverNonce := []byte("server-nonce-0123456789abcdef012")
	nodeNonce := []byte("node-nonce-0123456789abcdef01234")
	ekm := []byte("ekm-0123456789abcdef0123456789ab")
	transcript := authn.IngressTranscript(ingresswire.ModeEnrol, ingresswire.ProtoVersion, serverNonce, nodeNonce, nodesAudience, pub, ekm)
	return f.ingressNodes.Enrol(context.Background(), ingresshub.EnrolRequest{
		Token: token, Pub: pub, Sig: ed25519.Sign(key, transcript), Transcript: transcript,
		Name: name, SwarmNodeID: swarmNodeID, Version: "0.1.0", IP: "192.0.2.10",
	})
}

// issueJoinToken posts the join token as u and requires 200.
func (f *ingressFix) issueJoinToken(t *testing.T, u credentialUser, env string) ingressTokenResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodPost, nodesPath(env, "join-token"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("issue the join token of %s: %d %s", env, rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("issue the join token: Cache-Control = %q, want no-store", cc)
	}
	return decodeBody[ingressTokenResp](t, rec)
}

// listNodes lists the ingress nodes of env as u and requires 200.
func (f *ingressFix) listNodes(t *testing.T, u credentialUser, env string) []ingressNodeResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, nodesPath(env, "nodes"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list the nodes of %s: %d %s", env, rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("list the nodes: Cache-Control = %q, want no-store", cc)
	}
	return decodeBody[struct {
		Items []ingressNodeResp `json:"items"`
	}](t, rec).Items
}

func TestIngressNodesJoinTokenTracer(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	// An Admin of environment A makes the join token: it is in the answer, once, for this environment.
	tok := f.issueJoinToken(t, f.adminA, f.envA)
	if !strings.HasPrefix(tok.Token, "tpit_"+f.envA+"_") {
		t.Fatalf("token = %q, want the tpit_%s_ prefix", tok.Token, f.envA)
	}
	if tok.Generation != 1 || tok.Rotated {
		t.Fatalf("token answer = generation %d rotated %v, want generation 1 and not rotated", tok.Generation, tok.Rotated)
	}

	// Its status says it exists and never carries the token.
	rec := f.credCall(t, f.adminA, http.MethodGet, nodesPath(f.envA, "join-token"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("status: Cache-Control = %q, want no-store", cc)
	}
	if strings.Contains(rec.Body.String(), tok.Token) || strings.Contains(rec.Body.String(), "tpit_") {
		t.Fatalf("status carries the token: %s", rec.Body)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if _, has := keys["token"]; has {
		t.Fatalf("status body has a token field: %s", rec.Body)
	}
	st := decodeBody[ingressTokenStatusResp](t, rec)
	if !st.Exists || st.Generation != 1 || st.Disabled || st.CreatedAt == "" || st.CreatedBy == "" {
		t.Fatalf("status = %+v, want an existing, enabled generation 1 with createdAt and createdBy", st)
	}

	// A node that enrols through the service with that token is listed.
	f.makeSwarm(t, f.envA, nodesSwarmNodeA)
	if _, err := f.enrolNode(t, tok.Token, "edge-a", nodesSwarmNodeA); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	items := f.listNodes(t, f.adminA, f.envA)
	if len(items) != 1 {
		t.Fatalf("listed %d nodes, want 1: %+v", len(items), items)
	}
	n := items[0]
	if n.Status != "active" || n.Name != "edge-a" || n.SwarmNodeID != nodesSwarmNodeA || n.Sync != "unknown" || n.Connected {
		t.Fatalf("node = %+v, want active edge-a on %s, sync unknown, not connected", n, nodesSwarmNodeA)
	}
}
