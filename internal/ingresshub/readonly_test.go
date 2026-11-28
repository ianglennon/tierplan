// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// roRow is the node's row as the database holds it now.
func roRow(t *testing.T, srv *ingresstest.Server, nodeID string) sqlc.IngressNode {
	t.Helper()
	row, err := srv.Store.RQ.GetIngressNode(testsupport.Context(t), nodeID)
	if err != nil {
		t.Fatalf("read the node row: %v", err)
	}
	return row
}

// roAwaitEvents waits until the hub has reported n push events. Every accepted node message wakes the node's
// writer exactly once and the writer reports every look, so a test that waits for one event before it sends the
// next message knows the hub has finished the previous message, row write included.
func roAwaitEvents(t *testing.T, srv *ingresstest.Server, n int) []ingresshub.PushEvent {
	t.Helper()
	return srv.Pushes().Await(t, func(evs []ingresshub.PushEvent) bool { return len(evs) >= n })
}

// roHello enrols a node of a swarm env that lists swarmID, says hello for the current schema and waits for the hub's
// first look at its environment.
func roHello(t *testing.T, srv *ingresstest.Server, env ingresstest.Environment, swarmID string, events int) (*ingresstest.Identity, *ingresstest.Conn) {
	t.Helper()
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-" + swarmID, SwarmNodeID: swarmID})
	c.Hello(t, ingresstest.CurrentSchemas(), "")
	roAwaitEvents(t, srv, events)
	return id, c
}

// TestReadOnlyChannelUnknownTypeCloses: after the hello, a message type the node may not send, a request for
// another environment's bundle among them, ends the session with 1008 protocol_error. There is no request
// message at all: a node cannot ask for anything.
func TestReadOnlyChannelUnknownTypeCloses(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	other := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2", "n3")
	srv.Bundles.Set(other.ID, ingresstest.MakeBundle(t, 1, "other.example.com"))

	cases := []struct{ name, msg string }{
		{"request_bundle", `{"t":"request_bundle","envId":"` + other.ID + `"}`},
		{"welcome", `{"t":"welcome"}`},
		{"bundle", `{"t":"bundle"}`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, c := roHello(t, srv, env, []string{"n1", "n2", "n3"}[i], i+1)
			before := roRow(t, srv, id.NodeID)
			c.SendText(t, []byte(tc.msg))
			requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
			if after := roRow(t, srv, id.NodeID); !reflect.DeepEqual(before, after) {
				t.Errorf("the row changed:\n before %+v\n after  %+v", before, after)
			}
		})
	}
}

// TestReadOnlyChannelMessageBeforeHelloCloses: applied or health as the first message is a protocol error.
func TestReadOnlyChannelMessageBeforeHelloCloses(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2", "n3")

	first := map[string]func(*testing.T, *ingresstest.Conn){
		"applied":  func(t *testing.T, c *ingresstest.Conn) { c.Applied(t, "bundle-1") },
		"rejected": func(t *testing.T, c *ingresstest.Conn) { c.Rejected(t, "bundle-1", "bad") },
		"health":   func(t *testing.T, c *ingresstest.Conn) { c.Health(t, true) },
	}
	swarm := map[string]string{"applied": "n1", "rejected": "n2", "health": "n3"}
	for name, send := range first {
		t.Run(name, func(t *testing.T) {
			id := ingresstest.NewIdentity()
			c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: name, SwarmNodeID: swarm[name]})
			before := roRow(t, srv, id.NodeID)
			send(t, c)
			requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
			if after := roRow(t, srv, id.NodeID); !reflect.DeepEqual(before, after) {
				t.Errorf("the row changed:\n before %+v\n after  %+v", before, after)
			}
		})
	}
}

// TestReadOnlyChannelSecondHelloCloses: a node says hello once.
func TestReadOnlyChannelSecondHelloCloses(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	_, c := roHello(t, srv, env, "n1", 1)
	c.Hello(t, ingresstest.CurrentSchemas(), "")
	requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
}

// TestReadOnlyChannelBinaryFrameCloses: a node's messages are text frames; a binary frame, before or after the
// hello, is a protocol error.
func TestReadOnlyChannelBinaryFrameCloses(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")

	t.Run("before hello", func(t *testing.T) {
		c := srv.Enrol(t, ingresstest.NewIdentity(), ingresstest.Dial{Token: env.Token, Name: "a", SwarmNodeID: "n1"})
		c.SendBinary(t, []byte(`{"t":"hello","version":"1","schemas":[1]}`))
		requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
	})
	t.Run("after hello", func(t *testing.T) {
		_, c := roHello(t, srv, env, "n2", 1)
		c.SendBinary(t, []byte(`{"t":"health","traefikOk":true}`))
		requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
	})
}

// TestReadOnlyChannelAppliedForAnUnsentIDCloses: applied and rejected are accepted only for a bundle this
// session sent. Any other ID ends the session, and neither the node's row nor its held hostnames take the
// node's value: it cannot write arbitrary text into its own row.
func TestReadOnlyChannelAppliedForAnUnsentIDCloses(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	srv.Bundles.Set(env.ID, ingresstest.MakeBundle(t, 1, "a.example.com"))

	for i, tc := range []struct {
		name string
		send func(*testing.T, *ingresstest.Conn)
	}{
		{"applied", func(t *testing.T, c *ingresstest.Conn) { c.Applied(t, "never-sent-id") }},
		{"rejected", func(t *testing.T, c *ingresstest.Conn) { c.Rejected(t, "never-sent-id", "because") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, c := roHello(t, srv, env, []string{"n1", "n2"}[i], i+1)
			c.NextBundle(t)
			before := roRow(t, srv, id.NodeID)
			if !strings.Contains(before.HeldHostnames, "a.example.com") {
				t.Fatalf("the sent bundle's hostname is not held: %s", before.HeldHostnames)
			}
			tc.send(t, c)
			requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
			after := roRow(t, srv, id.NodeID)
			if after.AppliedBundleID != "" || after.RejectedBundleID != "" || after.RejectedReason != "" {
				t.Errorf("the node's value reached its row: applied %q rejected %q reason %q", after.AppliedBundleID, after.RejectedBundleID, after.RejectedReason)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("the row changed:\n before %+v\n after  %+v", before, after)
			}
		})
	}
}

// TestReadOnlyChannelOversizeMessageCloses: a node message is at most 16 KiB. A longer one is cut off by the
// WebSocket's own limit, which closes the connection with message too big, and nothing is written.
func TestReadOnlyChannelOversizeMessageCloses(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	id, c := roHello(t, srv, env, "n1", 1)
	before := roRow(t, srv, id.NodeID)

	c.SendText(t, []byte(`{"t":"health","traefikOk":false,"pad":"`+strings.Repeat("x", 17<<10)+`"}`))
	code, _ := c.AwaitClose(t)
	if code != websocket.StatusMessageTooBig {
		t.Errorf("close status = %d, want %d (message too big)", code, websocket.StatusMessageTooBig)
	}
	if after := roRow(t, srv, id.NodeID); !reflect.DeepEqual(before, after) {
		t.Errorf("the row changed:\n before %+v\n after  %+v", before, after)
	}
}

// TestReadOnlyChannelReportsReachTheRow: hello, applied for the bundle sent, rejected for a later one, and
// health each reach the node's row, and saying the same thing again leaves the row as it was, down to the
// instants (the store has one writer and a node repeats itself every 30 s).
func TestReadOnlyChannelReportsReachTheRow(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	y := ingresstest.MakeBundle(t, 2, "a.example.com", "b.example.com")
	srv.Bundles.Set(env.ID, x)

	// hello: event 1 (X is sent)
	id, c := roHello(t, srv, env, "n1", 1)
	if got := c.NextBundle(t); got.ID != x.ID {
		t.Fatalf("first bundle = %s, want %s", got.ID, x.ID)
	}
	row := roRow(t, srv, id.NodeID)
	if row.Version != "0.1.0" || row.Schemas != "[2]" {
		t.Errorf("hello: version %q schemas %q", row.Version, row.Schemas)
	}

	// applied X: event 2
	c.Applied(t, x.ID)
	roAwaitEvents(t, srv, 2)
	row = roRow(t, srv, id.NodeID)
	if row.AppliedBundleID != x.ID || row.AppliedAt != srv.Clock.Now().UnixNano() {
		t.Errorf("applied: %q at %d", row.AppliedBundleID, row.AppliedAt)
	}

	// Y is published and sent: event 3
	srv.Bundles.Set(env.ID, y)
	roAwaitEvents(t, srv, 3)
	if got := c.NextBundle(t); got.ID != y.ID {
		t.Fatalf("second bundle = %s, want %s", got.ID, y.ID)
	}

	// rejected Y: event 4
	srv.Clock.Advance(time.Second)
	c.Rejected(t, y.ID, "bad_certificate")
	roAwaitEvents(t, srv, 4)
	row = roRow(t, srv, id.NodeID)
	if row.RejectedBundleID != y.ID || row.RejectedReason != "bad_certificate" || row.RejectedAt != srv.Clock.Now().UnixNano() {
		t.Errorf("rejected: %q %q at %d", row.RejectedBundleID, row.RejectedReason, row.RejectedAt)
	}

	// health ok, then not ok: events 5 and 6
	srv.Clock.Advance(time.Second)
	c.Health(t, true)
	roAwaitEvents(t, srv, 5)
	if row = roRow(t, srv, id.NodeID); row.Health != "ok" {
		t.Errorf("health = %q, want ok", row.Health)
	}
	// A change of health is written at most once per HealthWriteEvery.
	srv.Clock.Advance(ingresshub.HealthWriteEvery)
	c.Health(t, false)
	roAwaitEvents(t, srv, 6)
	row = roRow(t, srv, id.NodeID)
	if row.Health != "unhealthy" || row.HealthAt != srv.Clock.Now().UnixNano() {
		t.Errorf("health = %q at %d, want unhealthy now", row.Health, row.HealthAt)
	}
	if got := row.HeldHostnames; got != `["a.example.com","b.example.com"]` {
		t.Errorf("held hostnames = %s", got)
	}
	settled := row

	// The same reports again, later: events 7 to 9. Nothing in the row moves, not even last_seen_at.
	srv.Clock.Advance(time.Second)
	c.Rejected(t, y.ID, "bad_certificate")
	roAwaitEvents(t, srv, 7)
	c.Health(t, false)
	roAwaitEvents(t, srv, 8)
	c.Applied(t, x.ID)
	roAwaitEvents(t, srv, 9)
	if after := roRow(t, srv, id.NodeID); !reflect.DeepEqual(settled, after) {
		t.Errorf("a repeated report changed the row:\n before %+v\n after  %+v", settled, after)
	}
}

// TestReadOnlyChannelMessageFloodCloses is WR-A-03: a session may send ReportBurst messages at once and one a
// second after that. The server's clock does not move here, so the message after the burst ends the session as
// a protocol error.
func TestReadOnlyChannelMessageFloodCloses(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})

	c.Hello(t, ingresstest.CurrentSchemas(), "")
	for range ingresshub.ReportBurst {
		c.Health(t, true)
	}
	requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
}

// TestReadOnlyChannelRepeatedRejectionLoggedOnce is WR-A-03: a node that keeps rejecting one bundle is logged once
// on the session, however often and with whatever reason it repeats it.
func TestReadOnlyChannelRepeatedRejectionLoggedOnce(t *testing.T) {
	logs := &syncBuffer{}
	srv := ingresstest.Start(t, ingresstest.Options{Log: slog.New(slog.NewJSONHandler(logs, nil))})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, x)

	id, c := roHello(t, srv, env, "n1", 1)
	c.NextBundle(t)
	for _, reason := range []string{"first", "second", "third"} {
		c.Rejected(t, x.ID, reason)
	}
	// Messages are handled in order on one goroutine: once a message the channel does not accept has closed
	// the session, every rejection before it has been handled and logged.
	c.Send(t, map[string]string{"t": "nope"})
	requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
	if row := roRow(t, srv, id.NodeID); row.RejectedReason != "third" {
		t.Errorf("rejected reason = %q, want the last one", row.RejectedReason)
	}
	if n := strings.Count(logs.String(), "an ingress node rejected a bundle"); n != 1 {
		t.Errorf("%d log lines for one rejected bundle, want 1", n)
	}
}
