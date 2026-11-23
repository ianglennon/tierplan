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

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
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

// nodeCall is one route of this file as a caller sees it.
type nodeCall struct {
	op, method, path, body string
}

// nodeCalls is the five operations against environment env and node id.
func nodeCalls(env, node string) []nodeCall {
	return []nodeCall{
		{"getIngressJoinToken", http.MethodGet, nodesPath(env, "join-token"), ""},
		{"issueIngressJoinToken", http.MethodPost, nodesPath(env, "join-token"), ""},
		{"disableIngressJoinToken", http.MethodDelete, nodesPath(env, "join-token"), ""},
		{"listIngressNodes", http.MethodGet, nodesPath(env, "nodes"), ""},
		{"revokeIngressNode", http.MethodPost, nodesPath(env, "nodes", node, "revoke"), `{}`},
	}
}

// enrolled makes env a swarm, issues its token as u and enrols one node per name on the matching swarm node, returning
// the listed nodes in enrolment order.
func (f *ingressFix) enrolled(t *testing.T, u credentialUser, env string, swarmNodes []string, names ...string) []ingressNodeResp {
	t.Helper()
	f.makeSwarm(t, env, swarmNodes...)
	tok := f.issueJoinToken(t, u, env)
	for i, name := range names {
		if _, err := f.enrolNode(t, tok.Token, name, swarmNodes[i]); err != nil {
			t.Fatalf("enrol %s: %v", name, err)
		}
	}
	items := f.listNodes(t, u, env)
	if len(items) != len(names) {
		t.Fatalf("listed %d nodes, want %d: %+v", len(items), len(names), items)
	}
	return items
}

func TestIngressNodesRevokeRoute(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	swarms := []string{nodesSwarmNodeA, nodesSwarmNodeB}
	// Both environments are swarms and the one node list holds every swarm node.
	f.makeSwarm(t, f.envB, swarms...)
	other := f.enrolled(t, f.adminB, f.envB, swarms, "edge-b")
	items := f.enrolled(t, f.adminA, f.envA, swarms, "edge-a", "edge-a2")
	node, second := items[0], items[1]

	// The hostnames the node was ever sent are what the answer lists.
	if _, err := f.st.Write.Exec(`UPDATE ingress_nodes SET held_hostnames = ? WHERE id = ?`, `["a.example.com","b.example.com"]`, node.ID); err != nil {
		t.Fatal(err)
	}

	// An unknown node and a node of another environment are the same 404.
	for _, id := range []string{"no-such-node", other[0].ID} {
		rec := f.credCall(t, f.adminA, http.MethodPost, nodesPath(f.envA, "nodes", id, "revoke"), `{}`)
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "ingress_node_not_found" {
			t.Fatalf("revoke %s on env A: %d %s, want 404 ingress_node_not_found", id, rec.Code, rec.Body)
		}
	}
	// A reason of more than 256 characters is refused by the schema.
	rec := f.credCall(t, f.adminA, http.MethodPost, nodesPath(f.envA, "nodes", node.ID, "revoke"), `{"reason":"`+strings.Repeat("x", 257)+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("revoke with a 257 character reason: %d %s, want 422", rec.Code, rec.Body)
	}

	// The Admin revokes the node: the answer lists the hostnames to re-issue and recommends rotating the token.
	rec = f.credCall(t, f.adminA, http.MethodPost, nodesPath(f.envA, "nodes", node.ID, "revoke"), `{"reason":"decommissioned"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("revoke: Cache-Control = %q, want no-store", cc)
	}
	got := decodeBody[struct {
		HeldHostnames          []string `json:"heldHostnames"`
		RotateTokenRecommended bool     `json:"rotateTokenRecommended"`
		RevokedAt              string   `json:"revokedAt"`
	}](t, rec)
	if strings.Join(got.HeldHostnames, ",") != "a.example.com,b.example.com" || !got.RotateTokenRecommended || got.RevokedAt == "" {
		t.Fatalf("revoke answer = %+v, want both hostnames, rotateTokenRecommended and revokedAt", got)
	}

	// Only that node is revoked, with who, why and the block on enrolling again.
	after := f.listNodes(t, f.adminA, f.envA)
	if after[0].Status != "revoked" || after[0].RevokeReason != "decommissioned" || after[0].RevokedBy == "" || !after[0].Reenrol {
		t.Fatalf("revoked node = %+v, want revoked with its reason, revoker and the re-enrol block", after[0])
	}
	if after[1].ID != second.ID || after[1].Status != "active" {
		t.Fatalf("the other node = %+v, want it untouched and active", after[1])
	}

	// Revoking it again is a conflict.
	rec = f.credCall(t, f.adminA, http.MethodPost, nodesPath(f.envA, "nodes", node.ID, "revoke"), `{}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_node_already_revoked" {
		t.Fatalf("revoke again: %d %s, want 409 ingress_node_already_revoked", rec.Code, rec.Body)
	}

	// allowReenrol leaves the host free to enrol again, and an empty list of hostnames is [] and not null.
	rec = f.credCall(t, f.adminA, http.MethodPost, nodesPath(f.envA, "nodes", second.ID, "revoke"), `{"allowReenrol":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"heldHostnames":[]`) {
		t.Fatalf("revoke with allowReenrol: %d %s, want 200 and an empty heldHostnames", rec.Code, rec.Body)
	}
	var blocked int
	if err := f.st.Write.QueryRow(`SELECT reenrol_blocked FROM ingress_nodes WHERE id = ?`, second.ID).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if blocked != 0 {
		t.Fatalf("reenrol_blocked = %d after allowReenrol, want 0", blocked)
	}
}

func TestIngressNodesPermissions(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	items := f.enrolled(t, f.adminA, f.envA, []string{nodesSwarmNodeA}, "edge-a")
	node := items[0].ID

	for _, c := range nodeCalls(f.envA, node) {
		// No session: 401.
		if rec := f.call(t, authReq{method: c.method, path: c.path, body: c.body}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d %s, want 401", c.op, rec.Code, rec.Body)
		}
		// An Operator of the environment may not manage ingress: 403.
		if rec := f.credCall(t, f.operatorA, c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s as an Operator of the environment: %d %s, want 403", c.op, rec.Code, rec.Body)
		}
		// An Admin of another environment does not learn this one exists.
		if rec := f.credCall(t, f.adminB, c.method, c.path, c.body); rec.Code != http.StatusNotFound || problemType(t, rec) != "environment_not_found" {
			t.Errorf("%s as an Admin of another environment: %d %s, want 404 environment_not_found", c.op, rec.Code, rec.Body)
		}
	}
	// None of those refusals changed anything: the node is active and the token is not disabled.
	if after := f.listNodes(t, f.adminA, f.envA); after[0].Status != "active" {
		t.Fatalf("node after the refused calls = %+v, want active", after[0])
	}
	rec := f.credCall(t, f.adminA, http.MethodGet, nodesPath(f.envA, "join-token"), "")
	if st := decodeBody[ingressTokenStatusResp](t, rec); st.Generation != 1 || st.Disabled {
		t.Fatalf("token status after the refused calls = %+v, want generation 1, enabled", st)
	}
	// The instance admin may do everything an environment Admin may.
	if rec := f.credCall(t, f.admin, http.MethodGet, nodesPath(f.envA, "nodes"), ""); rec.Code != http.StatusOK {
		t.Errorf("list as the instance admin: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestIngressNodesTokenRotateAndDisable(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.makeSwarm(t, f.envA, nodesSwarmNodeA, nodesSwarmNodeB)

	// Before any token the status says so, and is still no-store.
	rec := f.credCall(t, f.adminA, http.MethodGet, nodesPath(f.envA, "join-token"), "")
	if st := decodeBody[ingressTokenStatusResp](t, rec); rec.Code != http.StatusOK || st.Exists || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status with no token: %d %s, want 200, exists false, no-store", rec.Code, rec.Body)
	}

	first := f.issueJoinToken(t, f.adminA, f.envA)
	second := f.issueJoinToken(t, f.adminA, f.envA)
	if first.Generation != 1 || first.Rotated || second.Generation != 2 || !second.Rotated || first.Token == second.Token {
		t.Fatalf("tokens = %+v then %+v, want generation 1 then a rotated 2 with another token", first, second)
	}
	// The first token no longer enrols, and the second does.
	if _, err := f.enrolNode(t, first.Token, "edge-a", nodesSwarmNodeA); err == nil {
		t.Fatal("the rotated-away token enrolled a node")
	}
	if _, err := f.enrolNode(t, second.Token, "edge-a", nodesSwarmNodeA); err != nil {
		t.Fatalf("the current token did not enrol: %v", err)
	}

	// The audit rows carry the generation and never a token.
	for action, gen := range map[string]string{"ingress.join_token_created": "1", "ingress.join_token_rotated": "2"} {
		rows := f.ingressAudit(t, action)
		if len(rows) != 1 || rows[0].Details["generation"] != gen {
			t.Fatalf("%s audit rows = %+v, want one with generation %s", action, rows, gen)
		}
		for _, v := range rows[0].Details {
			if strings.Contains(v, "tpit_") {
				t.Fatalf("%s audit details carry a token: %v", action, rows[0].Details)
			}
		}
	}

	// Disabling answers 204, shows in the status, stops enrolment, and leaves the enrolled node alone.
	rec = f.credCall(t, f.adminA, http.MethodDelete, nodesPath(f.envA, "join-token"), "")
	if rec.Code != http.StatusNoContent || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("disable: %d %s, Cache-Control %q, want 204 and no-store", rec.Code, rec.Body, rec.Header().Get("Cache-Control"))
	}
	rec = f.credCall(t, f.adminA, http.MethodGet, nodesPath(f.envA, "join-token"), "")
	if st := decodeBody[ingressTokenStatusResp](t, rec); !st.Exists || st.Generation != 2 || !st.Disabled {
		t.Fatalf("status after disable = %+v, want generation 2, disabled", st)
	}
	if _, err := f.enrolNode(t, second.Token, "edge-b", nodesSwarmNodeB); err == nil {
		t.Fatal("a disabled token enrolled a node")
	}
	if rec := f.credCall(t, f.adminA, http.MethodDelete, nodesPath(f.envA, "join-token"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable again: %d %s, want 204", rec.Code, rec.Body)
	}
	if n := f.listNodes(t, f.adminA, f.envA); len(n) != 1 || n[0].Status != "active" {
		t.Fatalf("nodes after disable = %+v, want the one enrolled node, active", n)
	}
	// A rotation enables enrolment again.
	third := f.issueJoinToken(t, f.adminA, f.envA)
	if third.Generation != 3 {
		t.Fatalf("token after disable = %+v, want generation 3", third)
	}
	if _, err := f.enrolNode(t, third.Token, "edge-b", nodesSwarmNodeB); err != nil {
		t.Fatalf("the new token did not enrol: %v", err)
	}
}

func TestIngressNodesListShape(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	swarms := []string{nodesSwarmNodeA, nodesSwarmNodeB, "ccc1abc2def3ghi4jkl5mno6p", "ddd1abc2def3ghi4jkl5mno6p"}
	items := f.enrolled(t, f.adminA, f.envA, swarms, "n-current", "n-pending", "n-incompatible", "n-revoked")
	for _, n := range items {
		if n.Sync != "unknown" || n.Connected {
			t.Fatalf("%s without a hub or a bundle: sync %q connected %v, want unknown and not connected", n.Name, n.Sync, n.Connected)
		}
	}

	// The first node applied the current bundle, the second an older one, the third cannot apply any schema this server
	// writes, and the fourth is revoked.
	for _, q := range []struct{ sql, id string }{
		{`UPDATE ingress_nodes SET applied_bundle_id = 'b-2' WHERE id = ?`, items[0].ID},
		{`UPDATE ingress_nodes SET applied_bundle_id = 'b-1' WHERE id = ?`, items[1].ID},
		{`UPDATE ingress_nodes SET schemas = '[999]', applied_bundle_id = 'b-2' WHERE id = ?`, items[2].ID},
	} {
		if _, err := f.st.Write.Exec(q.sql, q.id); err != nil {
			t.Fatal(err)
		}
	}
	if rec := f.credCall(t, f.adminA, http.MethodPost, nodesPath(f.envA, "nodes", items[3].ID, "revoke"), `{}`); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}

	// The hub and the bundle store are read at request time once the routes have them.
	f.ingressLive = func(id string) bool { return id == items[0].ID || id == items[3].ID }
	f.ingressCurrentBundle = func(env string) (string, bool) { return "b-2", env == f.envA }
	f.h = f.build(t)

	rec := f.credCall(t, f.adminA, http.MethodGet, nodesPath(f.envA, "nodes"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	listed := decodeBody[struct {
		Items []ingressNodeResp `json:"items"`
	}](t, rec).Items
	want := []struct {
		sync      string
		connected bool
	}{{"current", true}, {"pending", false}, {"incompatible", false}, {"unknown", false}}
	for i, w := range want {
		if listed[i].Sync != w.sync || listed[i].Connected != w.connected {
			t.Errorf("%s: sync %q connected %v, want %q and %v", listed[i].Name, listed[i].Sync, listed[i].Connected, w.sync, w.connected)
		}
	}

	// Each item has exactly the documented keys: the instants are left out when they never happened, the rest are always there.
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	always := []string{"id", "name", "swarmNodeId", "status", "version", "schemas", "appliedBundleId", "rejectedBundleId", "rejectedReason",
		"health", "revokedBy", "revokeReason", "reenrolBlocked", "heldHostnames", "incompatible", "connected", "sync"}
	instants := []string{"appliedAt", "lastSeenAt", "createdAt", "revokedAt"}
	allowed := map[string]bool{}
	for _, k := range append(append([]string{}, always...), instants...) {
		allowed[k] = true
	}
	for _, item := range raw.Items {
		for _, k := range always {
			if _, ok := item[k]; !ok {
				t.Errorf("item has no %q key: %s", k, rec.Body)
			}
		}
		for k := range item {
			if !allowed[k] {
				t.Errorf("item has the undocumented key %q", k)
			}
		}
	}
	if _, ok := raw.Items[3]["revokedAt"]; !ok {
		t.Errorf("the revoked node has no revokedAt")
	}
	if _, ok := raw.Items[0]["revokedAt"]; ok {
		t.Errorf("an active node has a revokedAt")
	}
	if listed[2].Sync != "incompatible" {
		t.Errorf("incompatible node sync = %q", listed[2].Sync)
	}
}

// notEnrolledIssuer answers every rotation as a standalone host whose proxy has not enrolled yet would (D-26).
type notEnrolledIssuer struct{}

func (notEnrolledIssuer) IssueJoinToken(context.Context, audit.Actor, string, string, string, string) (ingresshub.Token, error) {
	return ingresshub.Token{}, ingressdeploy.ErrTokenNotEnrolled
}

// A rotation the deployer refuses because the proxy on a standalone host has not enrolled with the token it was given is
// 409 ingress_token_not_enrolled, with the sentence that says nothing was issued, and no token in the body (D-26).
func TestIngressJoinTokenRotationNotEnrolled(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.ingressIssuer = notEnrolledIssuer{}
	f.h = f.build(t)

	rec := f.credCall(t, f.adminA, http.MethodPost, nodesPath(f.envA, "join-token"), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("rotate before the proxy enrolled: %d %s, want 409", rec.Code, rec.Body)
	}
	typ, detail := deployProblem(t, rec.Body.Bytes())
	if typ != ingressdeploy.CodeTokenNotEnrolled {
		t.Errorf("problem type = %q, want %s", typ, ingressdeploy.CodeTokenNotEnrolled)
	}
	for _, want := range []string{"has not enrolled yet", "nothing was issued", "Rotate after it has enrolled"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not say %q", detail, want)
		}
	}
	if strings.Contains(rec.Body.String(), "tpit_") || strings.Contains(rec.Body.String(), `"token"`) {
		t.Errorf("the refusal carries a token: %s", rec.Body)
	}
}
