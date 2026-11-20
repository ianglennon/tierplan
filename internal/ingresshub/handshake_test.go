// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// enrolIntoSwarm starts a swarm environment with one listed node and enrols a new identity into it.
func enrolIntoSwarm(t *testing.T, srv *ingresstest.Server) (ingresstest.Environment, *ingresstest.Identity, *ingresstest.Conn) {
	t.Helper()
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "swarmnode1")
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1"})
	return env, id, c
}

// leaveAndWait closes the node's end and waits until the hub has dropped its session, so a later registration
// of the same node is the only one the hub holds.
func leaveAndWait(t *testing.T, srv *ingresstest.Server, id *ingresstest.Identity, c *ingresstest.Conn) {
	t.Helper()
	_ = c.WS.Close(websocket.StatusNormalClosure, "")
	if _, err := srv.Hub.AwaitSession(testsupport.Context(t), id.NodeID, func(s *ingresshub.Session) bool { return s == nil }); err != nil {
		t.Fatalf("the hub kept the session of a node that left: %v", err)
	}
}

// refusedRow is one ingress.node_enrol_refused row.
type refusedRow struct {
	ActorType, IP, Reason, Details string
	EnvID                          sql.NullString
}

func refusedRows(t *testing.T, srv *ingresstest.Server) []refusedRow {
	t.Helper()
	rows, err := srv.Store.Read.Query(`SELECT actor_type, env_id, ip, reason, details FROM audit_log WHERE action = ? ORDER BY seq`,
		audit.ActionIngressNodeEnrolRefused)
	if err != nil {
		t.Fatalf("query the refusals: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []refusedRow
	for rows.Next() {
		var r refusedRow
		if err := rows.Scan(&r.ActorType, &r.EnvID, &r.IP, &r.Reason, &r.Details); err != nil {
			t.Fatalf("scan a refusal: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("refusal rows: %v", err)
	}
	return out
}

func nodeCount(t *testing.T, srv *ingresstest.Server) int {
	t.Helper()
	var n int
	if err := srv.Store.Read.QueryRow(`SELECT COUNT(*) FROM ingress_nodes`).Scan(&n); err != nil {
		t.Fatalf("count the nodes: %v", err)
	}
	return n
}

func detailsOf(t *testing.T, r refusedRow) map[string]string {
	t.Helper()
	var d map[string]string
	if err := json.Unmarshal([]byte(r.Details), &d); err != nil {
		t.Fatalf("details %q: %v", r.Details, err)
	}
	return d
}

// trustLoopback puts the hub behind the client-address middleware with 127.0.0.0/8 as a trusted proxy, so the
// test client counts as a TLS-terminating proxy in front of TierPlan.
func trustLoopback(h http.Handler) http.Handler {
	return httpx.ClientIPMiddleware([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})(h)
}

// TestHubConnectWithTheStoredKey proves D-06: a node that enrolled reconnects in mode connect with its stored
// key, with no token, and is welcomed for the same node ID and instance. A hello gets the bundle again only
// when the bundle the node runs is not the current one.
func TestHubConnectWithTheStoredKey(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env, id, first := enrolIntoSwarm(t, srv)
	want := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, want)
	nodeID, instanceID := id.NodeID, id.InstanceID
	leaveAndWait(t, srv, id, first)

	// A node that runs the current bundle is not sent it again. The hub's decision is waited for first, so a
	// resend would be seen as a sent event rather than race the close below (WR-C-4). The next thing it reads
	// is then the close that follows a message the channel does not accept.
	second := srv.Connect(t, id, ingresstest.Dial{})
	if second.Welcome.NodeID != nodeID || second.Welcome.InstanceID != instanceID || second.Welcome.EnvID != env.ID {
		t.Fatalf("connect welcome = %+v, want node %s instance %s env %s", second.Welcome, nodeID, instanceID, env.ID)
	}
	before := len(srv.Pushes().Events())
	second.Hello(t, []int{1}, want.ID)
	evs := srv.Pushes().Await(t, func(evs []ingresshub.PushEvent) bool {
		return dvCount(evs[before:], nodeID, ingresshub.PushSame, want.ID)+dvCount(evs[before:], nodeID, ingresshub.PushSent, "") > 0
	})
	if n := dvCount(evs[before:], nodeID, ingresshub.PushSent, ""); n != 0 {
		t.Fatalf("a node that runs the current bundle was sent it (%d sent events)", n)
	}
	second.Send(t, map[string]string{"t": "nope"})
	if _, _, err := second.WS.Read(testsupport.Context(t)); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("a node that runs the current bundle read %v, want the close of the next message", err)
	}
	leaveAndWait(t, srv, id, second)

	// One that runs something else is.
	third := srv.Connect(t, id, ingresstest.Dial{})
	third.Hello(t, []int{1}, "another-bundle")
	if got := third.NextBundle(t); got.ID != want.ID {
		t.Errorf("bundle ID = %q, want %q", got.ID, want.ID)
	}
	if n := nodeCount(t, srv); n != 1 {
		t.Errorf("%d ingress_nodes rows after two reconnects, want 1", n)
	}
}

// TestHubPreflightRefusals proves the order of the checks before the upgrade (D-36): Origin first, then TLS,
// then the upgrade. Plain HTTP is 403 insecure_transport whether or not it carries upgrade headers, so it
// never reaches the 426 path.
//
// Only the TLS request without upgrade headers is an upgrade failure, which the proxy observer hears of with the
// client address (WR-B-2).
func TestHubPreflightRefusals(t *testing.T) {
	var mu sync.Mutex
	var failures []string
	srv := ingresstest.Start(t, ingresstest.Options{Configure: func(h *ingresshub.Hub) *ingresshub.Hub {
		return h.OnUpgradeFailure(func(peer string) {
			mu.Lock()
			failures = append(failures, peer)
			mu.Unlock()
		})
	}})
	upgrade := http.Header{
		"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
		"Sec-WebSocket-Version": {"13"}, "Sec-WebSocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
	}
	tlsRequest := func(t *testing.T, h http.Header) (int, string) {
		t.Helper()
		ctx := testsupport.Context(t)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.HTTP.URL+httpx.IngressConnectPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range h {
			req.Header[k] = v
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	plainRequest := func(t *testing.T, h http.Header) (int, string) {
		t.Helper()
		req := httptest.NewRequestWithContext(testsupport.Context(t), http.MethodGet, httpx.IngressConnectPath, nil)
		for k, v := range h {
			req.Header[k] = v
		}
		rec := httptest.NewRecorder()
		srv.Hub.Handler().ServeHTTP(rec, req) // r.TLS is nil
		return rec.Code, rec.Body.String()
	}
	withOrigin := upgrade.Clone()
	withOrigin.Set("Origin", "https://evil.example")

	for _, tc := range []struct {
		name   string
		do     func(*testing.T, http.Header) (int, string)
		header http.Header
		status int
		prefix string
	}{
		{"an Origin header", tlsRequest, withOrigin, http.StatusForbidden, "forbidden"},
		{"plain HTTP without upgrade headers", plainRequest, nil, http.StatusForbidden, "insecure_transport"},
		{"plain HTTP with upgrade headers, never 426", plainRequest, upgrade, http.StatusForbidden, "insecure_transport"},
		{"TLS without upgrade headers", tlsRequest, nil, http.StatusUpgradeRequired, "a WebSocket upgrade is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := tc.do(t, tc.header)
			if status != tc.status || !strings.HasPrefix(body, tc.prefix) {
				t.Errorf("got %d %q, want %d starting %q", status, body, tc.status, tc.prefix)
			}
		})
	}
	// The hook runs before the answer is written, so every request above has been heard by now.
	mu.Lock()
	defer mu.Unlock()
	if len(failures) != 1 || failures[0] != "127.0.0.1" {
		t.Errorf("upgrade failures = %v, want one from 127.0.0.1 (the TLS request without upgrade headers)", failures)
	}
}

// TestHubSubprotocolIsRequired proves a node that offers the agent's subprotocol, or none, is closed 1008 and
// never gets a challenge.
func TestHubSubprotocolIsRequired(t *testing.T) {
	for name, d := range map[string]ingresstest.Dial{
		"the agent subprotocol": {Subprotocol: "tierplan.agent.v1"},
		"no subprotocol":        {NoSubprotocol: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := ingresstest.Start(t, ingresstest.Options{})
			c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(), d)
			if c.Welcome != nil || c.Refuse != nil || websocket.CloseStatus(c.Err) != websocket.StatusPolicyViolation {
				t.Fatalf("welcome %+v refuse %+v err %v, want the close 1008", c.Welcome, c.Refuse, c.Err)
			}
			if n := nodeCount(t, srv); n != 0 {
				t.Errorf("%d ingress_nodes rows, want 0", n)
			}
		})
	}
}

// TestHubHandshakeRefusals proves each cheap check of the first frame refuses auth_rejected, then closes 1008
// with the code as the reason, and writes nothing to ingress_nodes.
func TestHubHandshakeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dial   func(env ingresstest.Environment) ingresstest.Dial
		reason string
	}{
		{"an audience that is another host", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{Token: e.Token, Aud: "other.example:443"}
		}, "audience"},
		{"an exporter value that is not this connection's", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{Token: e.Token, EKM: bytes.Repeat([]byte{7}, 32)}
		}, "channel_binding"},
		{"no exporter value", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{Token: e.Token, NoEKM: true}
		}, "channel_binding"},
		{"a binary first frame", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{Token: e.Token, BinaryFirstFrame: true}
		}, "not_text"},
		{"a first frame of the wrong type", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{RawFirstFrame: []byte(`{"t":"nope"}`)}
		}, "malformed"},
		{"first frame that is not JSON", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{RawFirstFrame: []byte(`{`)}
		}, "malformed"},
		{"protocol version 2", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{Token: e.Token, Proto: 2}
		}, "proto"},
		{"a mode that is neither enrol nor connect", func(e ingresstest.Environment) ingresstest.Dial {
			return ingresstest.Dial{Token: e.Token, Mode: "hello"}
		}, "mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := ingresstest.Start(t, ingresstest.Options{AuditRefusals: true})
			env := srv.NewEnvironment(t, "swarm")
			srv.Nodes.Set(env.ID, "swarmnode1")
			c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(), tc.dial(env))
			if c.Refuse == nil || c.Refuse.Code != ingresswire.RefuseAuthRejected || c.Welcome != nil {
				t.Fatalf("welcome %+v refuse %+v err %v, want auth_rejected", c.Welcome, c.Refuse, c.Err)
			}
			if code, reason := c.AwaitClose(t); code != websocket.StatusPolicyViolation || reason != ingresswire.RefuseAuthRejected {
				t.Errorf("closed %d %q, want 1008 auth_rejected", code, reason)
			}
			if n := nodeCount(t, srv); n != 0 {
				t.Errorf("%d ingress_nodes rows, want 0", n)
			}
			rows := refusedRows(t, srv)
			if len(rows) != 1 || rows[0].Reason != tc.reason {
				t.Errorf("refusal rows = %+v, want one with reason %q", rows, tc.reason)
			}
		})
	}
}

// TestHubRefusalsAreAuditedWithoutAnEnvironment proves each refusal writes one ingress.node_enrol_refused row
// with an anonymous actor, the specific reason and the caller's address, and no environment, even when the
// token named a real one; a refused connect is written under the same action with details mode connect.
func TestHubRefusalsAreAuditedWithoutAnEnvironment(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "swarm")

	// An enrolment with a real token for a real environment, addressed to another host.
	c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(),
		ingresstest.Dial{Token: env.Token, Aud: "other.example:443"})
	if c.Refuse == nil {
		t.Fatalf("the enrolment was not refused: %+v %v", c.Welcome, c.Err)
	}
	// A connect for a node ID that has no row.
	ghost := ingresstest.NewIdentity()
	ghost.NodeID = "018f0000-0000-7000-8000-0000000000aa"
	c = ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ghost, ingresstest.Dial{})
	if c.Refuse == nil || c.Refuse.Code != ingresswire.RefuseAuthRejected {
		t.Fatalf("the connect was not refused auth_rejected: %+v %v", c.Welcome, c.Err)
	}
	// A browser.
	c = ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(), ingresstest.Dial{Origin: "https://evil.example"})
	if c.Status != http.StatusForbidden {
		t.Fatalf("the browser got status %d (%v), want 403", c.Status, c.Err)
	}

	rows := refusedRows(t, srv)
	want := []struct{ reason, mode string }{{"audience", ingresswire.ModeEnrol}, {"unknown_node", ingresswire.ModeConnect}, {"origin", ""}}
	if len(rows) != len(want) {
		t.Fatalf("%d refusal rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.ActorType != string(audit.ActorAnonymous) || r.EnvID.Valid || r.IP != "127.0.0.1" || r.Reason != w.reason {
			t.Errorf("row %d = %+v, want anonymous, no environment, 127.0.0.1, reason %q", i, r, w.reason)
		}
		d := detailsOf(t, r)
		if d["mode"] != w.mode {
			t.Errorf("row %d details %v, want mode %q", i, d, w.mode)
		}
		if strings.Contains(r.Details, env.Token) {
			t.Errorf("row %d details carry the join token", i)
		}
	}
	if d := detailsOf(t, rows[0]); d["code"] != ingresswire.RefuseAuthRejected {
		t.Errorf("row 0 details %v, want code auth_rejected", d)
	}
	if d := detailsOf(t, rows[2]); d["code"] != "origin" {
		t.Errorf("row 2 details %v, want code origin", d)
	}
}

// TestHubTrustedProxyEnrolAndConnect proves the channel binding rule for both kinds of peer (D-06, D-07, D-32):
// behind a trusted proxy, whose TLS session with the node the server cannot see, a node that signs an exporter
// value the server cannot compute enrols and then reconnects with its key; a peer that is not trusted is held
// to the server's own exporter and refused channel_binding.
func TestHubTrustedProxyEnrolAndConnect(t *testing.T) {
	t.Run("behind a trusted proxy", func(t *testing.T) {
		srv := ingresstest.Start(t, ingresstest.Options{Wrap: trustLoopback})
		env := srv.NewEnvironment(t, "swarm")
		srv.Nodes.Set(env.ID, "swarmnode1")
		id := ingresstest.NewIdentity()
		first := srv.Enrol(t, id, ingresstest.Dial{
			Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1", EKM: bytes.Repeat([]byte{7}, 32),
		})
		nodeID := first.Welcome.NodeID
		leaveAndWait(t, srv, id, first)

		second := srv.Connect(t, id, ingresstest.Dial{EKM: bytes.Repeat([]byte{8}, 32)})
		if second.Welcome.NodeID != nodeID {
			t.Errorf("reconnect welcomed node %q, want %q", second.Welcome.NodeID, nodeID)
		}
		var status string
		var key []byte
		if err := srv.Store.Read.QueryRow(`SELECT status, public_key FROM ingress_nodes WHERE id = ?`, nodeID).Scan(&status, &key); err != nil {
			t.Fatalf("read the node row: %v", err)
		}
		if status != "active" || !bytes.Equal(key, id.Public()) {
			t.Errorf("node row = %q key %x, want active with the identity's key", status, key)
		}
	})

	t.Run("a peer that is not trusted", func(t *testing.T) {
		srv := ingresstest.Start(t, ingresstest.Options{AuditRefusals: true})
		env := srv.NewEnvironment(t, "swarm")
		srv.Nodes.Set(env.ID, "swarmnode1")
		c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(), ingresstest.Dial{
			Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1", EKM: bytes.Repeat([]byte{7}, 32),
		})
		if c.Refuse == nil || c.Refuse.Code != ingresswire.RefuseAuthRejected {
			t.Fatalf("welcome %+v refuse %+v err %v, want auth_rejected", c.Welcome, c.Refuse, c.Err)
		}
		if rows := refusedRows(t, srv); len(rows) != 1 || rows[0].Reason != "channel_binding" {
			t.Errorf("refusal rows = %+v, want one with reason channel_binding", rows)
		}
		if n := nodeCount(t, srv); n != 0 {
			t.Errorf("%d ingress_nodes rows, want 0", n)
		}
	})
}
