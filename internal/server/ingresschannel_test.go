// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/server/servertest"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// icNode is a node that enrolled and said hello through a running server.
type icNode struct {
	id   *ingresstest.Identity
	conn *ingresstest.Conn
}

// icServer is a running server with one standalone environment that has an ingress model to render.
type icServer struct {
	srv   *servertest.Server
	envID string
}

// icStart starts a real server and gives it an agent environment of a standalone host whose ingress model holds one
// claimed hostname and one route, so the server's own reconciler renders a bundle for it.
func icStart(t *testing.T) *icServer {
	t.Helper()
	srv := servertest.Start(t, servertest.Options{Host: "127.0.0.1"})
	env := srv.CreateAgentEnvironment("Ingress env")
	ctx := testsupport.Context(t)
	w := srv.Core().Store().Write
	// Issuing is paused first, as the reconciler's own boot test does (D-32): the certificate manager would otherwise
	// start ordering for the claimed hostname against a CA.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE ingress_cert_state SET paused_reason = 'restore' WHERE id = 1`, nil},
		{`UPDATE environments SET env_type = 'standalone' WHERE id = ?`, []any{env.ID}},
		// Ingress starts off: a standalone environment's first join token is issued before the proxy exists, and the rotation rule
		// (D-26) refuses one while ingress is on and no proxy has enrolled. joinToken switches it on once the token is issued.
		{`INSERT INTO ingress_env (env_id, enabled, config_version, updated_at, updated_by) VALUES (?, 0, 1, 1, 'tester')`, []any{env.ID}},
		{`INSERT INTO ingress_domains (id, env_id, suffix, issuer, dns_provider, created_by, created_at, updated_at)
		  VALUES ('dom-ic', ?, 'apps.example.com', 'staging', 'cloudflare', 'tester', 1, 1)`, []any{env.ID}},
		{`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-ic', ?, 'shop', 'standalone', 'tester', 1)`, []any{env.ID}},
		{`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at)
		  VALUES ('web.apps.example.com', ?, 'dom-ic', 'stack', 'stack-ic', 'tester', 1)`, []any{env.ID}},
		{`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, scheme, strip, created_at, updated_at)
		  VALUES ('route-ic', ?, 'web.apps.example.com', 'web', '/', 'web', 8080, 'http', 0, 1, 1)`, []any{env.ID}},
	} {
		if _, err := w.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed %q: %v", q.sql, err)
		}
	}
	return &icServer{srv: srv, envID: env.ID}
}

// switchIngress sets the environment's ingress switch, as the enable job's last step leaves it.
func (s *icServer) switchIngress(t *testing.T, on bool) {
	t.Helper()
	v := 0
	if on {
		v = 1
	}
	if _, err := s.srv.Core().Store().Write.ExecContext(testsupport.Context(t), `UPDATE ingress_env SET enabled = ? WHERE env_id = ?`, v, s.envID); err != nil {
		t.Fatalf("switch ingress: %v", err)
	}
}

// joinToken issues the environment's ingress join token through the API, as the signed-in admin, while ingress is off as in
// the real flow, and then switches ingress on so the node that enrols with it is sent its bundle.
func (s *icServer) joinToken(t *testing.T) string {
	t.Helper()
	defer s.switchIngress(t, true)
	code, body := s.srv.Do(http.MethodPost, "/api/env/"+s.envID+"/ingress/join-token", nil)
	if code != http.StatusOK {
		t.Fatalf("POST ingress/join-token: %d %s", code, body)
	}
	var tok struct {
		Token      string `json:"token"`
		Generation int    `json:"generation"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" || tok.Generation != 1 {
		t.Fatalf("join token answer: %v: generation %d, token present %t", err, tok.Generation, tok.Token != "")
	}
	return tok.Token
}

// dial runs a handshake against the running server over wss, addressed to the host and port of its public URL, the
// audience a node is told to use.
func (s *icServer) dial(t *testing.T, id *ingresstest.Identity, d ingresstest.Dial) *ingresstest.Conn {
	t.Helper()
	aud := strings.TrimPrefix(s.srv.URL, "https://")
	return ingresstest.DialNode(t, "wss://"+aud+"/ingress/connect", s.srv.Client(), aud, id, d)
}

// enrol enrols a new node with the environment's join token and says hello for schema 1.
func (s *icServer) enrol(t *testing.T, token, name string) icNode {
	t.Helper()
	id := ingresstest.NewIdentity()
	c := s.dial(t, id, ingresstest.Dial{Token: token, Name: name})
	if c.Welcome == nil || c.Err != nil {
		t.Fatalf("enrol was not welcomed: refuse %+v, err %v", c.Refuse, c.Err)
	}
	c.Hello(t, []int{1}, "")
	return icNode{id: id, conn: c}
}

// nodes reads the environment's ingress nodes through the API.
func (s *icServer) nodes(t *testing.T) []struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Connected bool   `json:"connected"`
} {
	t.Helper()
	code, body := s.srv.Do(http.MethodGet, "/api/env/"+s.envID+"/ingress/nodes", nil)
	if code != http.StatusOK {
		t.Fatalf("GET ingress/nodes: %d %s", code, body)
	}
	var list struct {
		Items []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Status    string `json:"status"`
			Connected bool   `json:"connected"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode the node list: %v: %s", err, body)
	}
	return list.Items
}

// Phase 22, SC 1 to SC 4 (tracer): a real server issues the ingress join token, a node dials wss://<public host>/ingress/connect
// with it, is welcomed with the instance ID the server minted, says hello and is sent the bundle its own reconciler
// rendered, and the typed API then lists the node as connected.
func TestServerServesTheIngressChannel(t *testing.T) {
	s := icStart(t)
	token := s.joinToken(t)
	n := s.enrol(t, token, "node-1")

	want, err := instance.InstanceID(testsupport.Context(t), s.srv.Core().Store())
	if err != nil {
		t.Fatal(err)
	}
	if n.conn.Welcome.InstanceID != want || n.id.InstanceID != want {
		t.Errorf("welcome instanceId = %q, node pinned %q; want the settings row's %q", n.conn.Welcome.InstanceID, n.id.InstanceID, want)
	}
	if n.conn.Welcome.EnvID != s.envID {
		t.Errorf("welcome envId = %q, want %q", n.conn.Welcome.EnvID, s.envID)
	}

	// The bundle is the one the server's own reconciler rendered for the environment: schema 1 and the one dynamic file.
	// It arrives when the reconciler has rendered it, which the test waits for on the channel itself.
	b := n.conn.NextBundle(t)
	if b.Schema != ingressbundle.Schema1 || len(b.Files) != 1 || b.Files[0].Path != "dynamic/tierplan.yml" {
		t.Errorf("bundle schema %d files %v, want schema 1 and the one dynamic/tierplan.yml", b.Schema, b.Files)
	}

	var found bool
	for _, item := range s.nodes(t) {
		if item.ID == n.id.NodeID {
			found = true
			if !item.Connected || item.Status != "active" || item.Name != "node-1" {
				t.Errorf("listed node = %+v, want active, connected, named node-1", item)
			}
		}
	}
	if !found {
		t.Errorf("the node %s is not in the list", n.id.NodeID)
	}
}

// Phase 22, D-29: a server that stops closes every node's session with going-away and the reason shutdown, although
// http.Server.Shutdown ignores hijacked connections.
func TestServerShutdownClosesIngressSessions(t *testing.T) {
	s := icStart(t)
	n := s.enrol(t, s.joinToken(t), "node-shutdown")
	n.conn.NextBundle(t)

	s.srv.Restart("")
	code, reason := n.conn.AwaitClose(t)
	if code != websocket.StatusGoingAway || reason != "shutdown" {
		t.Errorf("close = %d %q, want going-away (%d) with reason shutdown", code, reason, websocket.StatusGoingAway)
	}
}

// Phase 22, D-29: removing an environment closes its nodes' sessions with a policy violation and the reason removed, and
// the node's row goes with the environment.
func TestServerRemovingAnEnvironmentKicksItsIngressNodes(t *testing.T) {
	s := icStart(t)
	n := s.enrol(t, s.joinToken(t), "node-removed")
	n.conn.NextBundle(t)

	if code, body := s.srv.Do(http.MethodDelete, "/api/environments/"+s.envID, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE the environment: %d %s", code, body)
	}
	code, reason := n.conn.AwaitClose(t)
	if code != websocket.StatusPolicyViolation || reason != "removed" {
		t.Errorf("close = %d %q, want policy violation (%d) with reason removed", code, reason, websocket.StatusPolicyViolation)
	}
	var rows int
	if err := s.srv.Core().Store().Read.QueryRowContext(testsupport.Context(t), `SELECT COUNT(*) FROM ingress_nodes WHERE id = ?`, n.id.NodeID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("the node's row is still there after its environment was removed")
	}
}

// Phase 25, D-26: with ingress on and no proxy enrolled on a standalone environment, a join-token issue answers 409
// ingress_token_not_enrolled through the real server, not 500, and issues nothing.
func TestServerRefusesAJoinTokenBeforeTheStandaloneProxyEnrolled(t *testing.T) {
	s := icStart(t)
	s.switchIngress(t, true)

	code, body := s.srv.Do(http.MethodPost, "/api/env/"+s.envID+"/ingress/join-token", nil)
	if code != http.StatusConflict {
		t.Fatalf("POST ingress/join-token with ingress on and no node: %d %s, want 409", code, body)
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Type != "ingress_token_not_enrolled" {
		t.Errorf("problem = %s (%v), want type ingress_token_not_enrolled", body, err)
	}
	if strings.Contains(string(body), "tpit_") {
		t.Errorf("the refusal carries a token: %s", body)
	}
	var tokens int
	if err := s.srv.Core().Store().Read.QueryRowContext(testsupport.Context(t), `SELECT COUNT(*) FROM ingress_join_tokens WHERE env_id = ?`, s.envID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Errorf("%d join token rows after the refusal, want none", tokens)
	}
}
