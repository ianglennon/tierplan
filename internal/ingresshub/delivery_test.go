// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// syncBuffer is a log sink a test reads while the hub writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// dvNode enrols a node of env, which lists swarmID, and says hello for schema 1 with the bundle it runs.
func dvNode(t *testing.T, srv *ingresstest.Server, env ingresstest.Environment, swarmID, applied string) (*ingresstest.Identity, *ingresstest.Conn) {
	t.Helper()
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-" + swarmID, SwarmNodeID: swarmID})
	c.Hello(t, []int{1}, applied)
	return id, c
}

// dvCount is how many of the events are of the node, with the outcome (and the bundle, when it is not "").
func dvCount(evs []ingresshub.PushEvent, nodeID, outcome, bundleID string) int {
	n := 0
	for _, e := range evs {
		if e.NodeID == nodeID && e.Outcome == outcome && (bundleID == "" || e.BundleID == bundleID) {
			n++
		}
	}
	return n
}

// dvAwaitCount waits until the node has at least n events of the outcome, and returns all events.
func dvAwaitCount(t *testing.T, srv *ingresstest.Server, nodeID, outcome, bundleID string, n int) []ingresshub.PushEvent {
	t.Helper()
	return srv.Pushes().Await(t, func(evs []ingresshub.PushEvent) bool { return dvCount(evs, nodeID, outcome, bundleID) >= n })
}

// TestDeliveryOnChange: when the environment's bundle changes, every node of the environment is sent the new
// bundle once, and the hub's log never holds the bytes of its files.
func TestDeliveryOnChange(t *testing.T) {
	logs := &syncBuffer{}
	srv := ingresstest.Start(t, ingresstest.Options{Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	y := ingresstest.MakeBundle(t, 2, "a.example.com", "b.example.com")
	srv.Bundles.Set(env.ID, x)

	a, ca := dvNode(t, srv, env, "n1", x.ID)
	b, cb := dvNode(t, srv, env, "n2", x.ID)
	dvAwaitCount(t, srv, a.NodeID, ingresshub.PushSame, x.ID, 1)
	dvAwaitCount(t, srv, b.NodeID, ingresshub.PushSame, x.ID, 1)

	srv.Bundles.Set(env.ID, y)
	if got := ca.NextBundle(t); got.ID != y.ID {
		t.Errorf("node a received %s, want %s", got.ID, y.ID)
	}
	if got := cb.NextBundle(t); got.ID != y.ID {
		t.Errorf("node b received %s, want %s", got.ID, y.ID)
	}
	dvAwaitCount(t, srv, b.NodeID, ingresshub.PushSent, y.ID, 1)
	evs := dvAwaitCount(t, srv, a.NodeID, ingresshub.PushSent, y.ID, 1)
	if n := dvCount(evs, a.NodeID, ingresshub.PushSent, "") + dvCount(evs, b.NodeID, ingresshub.PushSent, ""); n != 2 {
		t.Errorf("%d sent events, want 2", n)
	}

	// A wake with Y still current finds both nodes sent it already: no second frame.
	srv.Bundles.Set(env.ID, y)
	dvAwaitCount(t, srv, a.NodeID, ingresshub.PushSame, y.ID, 1)
	evs = dvAwaitCount(t, srv, b.NodeID, ingresshub.PushSame, y.ID, 1)
	if n := dvCount(evs, a.NodeID, ingresshub.PushSent, "") + dvCount(evs, b.NodeID, ingresshub.PushSent, ""); n != 2 {
		t.Errorf("%d sent events after a repeated wake, want 2", n)
	}

	// The log line of a send is there (so this test would catch a leak) and holds the paths and no data.
	out := logs.String()
	if !strings.Contains(out, "bundle sent to an ingress node") {
		t.Fatalf("the hub logged no send:\n%s", out)
	}
	for _, f := range y.Files {
		if strings.Contains(out, string(f.Data)) || strings.Contains(out, base64.StdEncoding.EncodeToString(f.Data)) {
			t.Errorf("the hub's log holds the data of %s", f.Path)
		}
	}
	if !strings.Contains(out, ingressbundle.DynamicFile) {
		t.Errorf("the send was not logged through the envelope's redacted form:\n%s", out)
	}
}

// TestDeliveryNothingWhenEqual: a node that says it runs the current bundle is sent nothing, and the next change
// is the first frame it gets.
func TestDeliveryNothingWhenEqual(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, x)

	id, c := dvNode(t, srv, env, "n1", x.ID)
	evs := dvAwaitCount(t, srv, id.NodeID, ingresshub.PushSame, x.ID, 1)
	if n := dvCount(evs, id.NodeID, ingresshub.PushSent, ""); n != 0 {
		t.Fatalf("a node running the current bundle was sent it (%d sent events)", n)
	}

	y := ingresstest.MakeBundle(t, 2, "a.example.com")
	srv.Bundles.Set(env.ID, y)
	if got := c.NextBundle(t); got.ID != y.ID {
		t.Errorf("the first frame is %s, want %s", got.ID, y.ID)
	}
}

// TestDeliveryNeverResentAfterRejected: a bundle the node refused is not sent again on this connection, however
// often the environment wakes the writer, and a reconnect starts again.
func TestDeliveryNeverResentAfterRejected(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	y := ingresstest.MakeBundle(t, 1, "a.example.com")
	z := ingresstest.MakeBundle(t, 2, "a.example.com")
	srv.Bundles.Set(env.ID, y)

	id, c := dvNode(t, srv, env, "n1", "")
	if got := c.NextBundle(t); got.ID != y.ID {
		t.Fatalf("first frame = %s, want %s", got.ID, y.ID)
	}
	c.Rejected(t, y.ID, "bad_certificate")
	dvAwaitCount(t, srv, id.NodeID, ingresshub.PushRejectedBefore, y.ID, 1)

	// The environment wakes with Y still current: refused before, no frame.
	srv.Bundles.Set(env.ID, y)
	evs := dvAwaitCount(t, srv, id.NodeID, ingresshub.PushRejectedBefore, y.ID, 2)
	if n := dvCount(evs, id.NodeID, ingresshub.PushSent, ""); n != 1 {
		t.Fatalf("%d bundles sent, want only the first", n)
	}

	// A reconnect while Y is still current sends it again.
	_ = c.WS.CloseNow()
	lcAwaitGone(t, srv, id.NodeID)
	c2 := srv.Connect(t, id, ingresstest.Dial{})
	c2.Hello(t, []int{1}, "")
	if got := c2.NextBundle(t); got.ID != y.ID {
		t.Fatalf("after a reconnect the frame is %s, want %s", got.ID, y.ID)
	}

	// Rejected again, and then a new bundle is published: it arrives, and it is the first frame after Y.
	c2.Rejected(t, y.ID, "bad_certificate")
	dvAwaitCount(t, srv, id.NodeID, ingresshub.PushRejectedBefore, y.ID, 3)
	srv.Bundles.Set(env.ID, z)
	if got := c2.NextBundle(t); got.ID != z.ID {
		t.Errorf("the next frame is %s, want %s", got.ID, z.ID)
	}
}

// TestDeliveryIncompatibleSchemaMarksTheNode: a node whose schemas share none with this server's is sent nothing
// and the node list says why.
func TestDeliveryIncompatibleSchemaMarksTheNode(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, x)

	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "old", SwarmNodeID: "n1"})
	c.Hello(t, []int{7}, "")
	evs := dvAwaitCount(t, srv, id.NodeID, ingresshub.PushIncompatible, x.ID, 1)
	if n := dvCount(evs, id.NodeID, ingresshub.PushSent, ""); n != 0 {
		t.Errorf("an incompatible node was sent a bundle (%d sent events)", n)
	}

	nodes, err := srv.Service.ListNodes(testsupport.Context(t), env.ID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 || !nodes[0].Incompatible || nodes[0].IncompatibleReason != ingressrender.CodeSchemaUnsupported {
		t.Errorf("node list = %+v, want one incompatible node with reason %s", nodes, ingressrender.CodeSchemaUnsupported)
	}

	// A change does not reach it either.
	srv.Bundles.Set(env.ID, ingresstest.MakeBundle(t, 2, "a.example.com"))
	evs = dvAwaitCount(t, srv, id.NodeID, ingresshub.PushIncompatible, "", 2)
	if n := dvCount(evs, id.NodeID, ingresshub.PushSent, ""); n != 0 {
		t.Errorf("an incompatible node was sent a changed bundle (%d sent events)", n)
	}
}

// TestDeliveryNoBundleWhenTheEnvironmentHasNone: an environment with no current bundle sends nothing.
func TestDeliveryNoBundleWhenTheEnvironmentHasNone(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	id, _ := dvNode(t, srv, env, "n1", "")
	evs := dvAwaitCount(t, srv, id.NodeID, ingresshub.PushNone, "", 1)
	if n := dvCount(evs, id.NodeID, ingresshub.PushSent, ""); n != 0 {
		t.Errorf("a bundle was sent although the environment has none (%d sent events)", n)
	}
}

// TestDeliveryHeldHostnamesRecordedBeforeSend: the hostnames of the routes of a bundle are in the node's row before
// the frame is written (D-24). The row is read in the hub's before-write hook, at the instant the frame is about to
// leave, so a regression that wrote the frame first and recorded the names after would read [] here on every run.
func TestDeliveryHeldHostnamesRecordedBeforeSend(t *testing.T) {
	var srvRef atomic.Pointer[ingresstest.Server]
	held := make(chan string, 4)
	srv := ingresstest.Start(t, ingresstest.Options{Configure: func(h *ingresshub.Hub) *ingresshub.Hub {
		return h.WithBeforeWrite(func(nodeID string) {
			row, err := srvRef.Load().Store.RQ.GetIngressNode(context.Background(), nodeID)
			if err != nil {
				held <- "error: " + err.Error()
				return
			}
			held <- row.HeldHostnames
		})
	}})
	srvRef.Store(srv)
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	srv.Bundles.Set(env.ID, ingresstest.MakeBundle(t, 1, "a.example.com", "b.example.com"))

	_, c := dvNode(t, srv, env, "n1", "")
	if got := testsupport.Await(t, "the bundle frame to be about to leave", held); got != `["a.example.com","b.example.com"]` {
		t.Errorf("held hostnames just before the frame was written = %s", got)
	}
	c.NextBundle(t)
}

// TestDeliveryOnlyTheNodesOwnEnvironment: a node is sent the bundle of the environment its credential belongs to
// and of no other, whatever is published elsewhere.
func TestDeliveryOnlyTheNodesOwnEnvironment(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env1 := srv.NewEnvironment(t, "swarm")
	env2 := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env1.ID, "n1")
	srv.Nodes.Set(env2.ID, "n2")
	x1 := ingresstest.MakeBundle(t, 1, "one.example.com")
	x2 := ingresstest.MakeBundle(t, 1, "two.example.com")
	srv.Bundles.Set(env1.ID, x1)
	srv.Bundles.Set(env2.ID, x2)

	a, ca := dvNode(t, srv, env1, "n1", "")
	b, cb := dvNode(t, srv, env2, "n2", "")
	if got := ca.NextBundle(t); got.ID != x1.ID {
		t.Fatalf("node of env 1 received %s, want %s", got.ID, x1.ID)
	}
	if got := cb.NextBundle(t); got.ID != x2.ID {
		t.Fatalf("node of env 2 received %s, want %s", got.ID, x2.ID)
	}

	// Env 2 publishes: only env 2's node is sent it.
	y2 := ingresstest.MakeBundle(t, 2, "two.example.com")
	srv.Bundles.Set(env2.ID, y2)
	if got := cb.NextBundle(t); got.ID != y2.ID {
		t.Fatalf("node of env 2 received %s, want %s", got.ID, y2.ID)
	}
	// Env 1 publishes: its node's next frame is env 1's.
	y1 := ingresstest.MakeBundle(t, 2, "one.example.com")
	srv.Bundles.Set(env1.ID, y1)
	if got := ca.NextBundle(t); got.ID != y1.ID {
		t.Fatalf("node of env 1 received %s, want %s", got.ID, y1.ID)
	}

	// The hub reports a send once the write returns, so a node can hold a frame before its sent event is
	// recorded. Wait for both nodes' second send, then check every event seen so far.
	dvAwaitCount(t, srv, a.NodeID, ingresshub.PushSent, "", 2)
	evs := dvAwaitCount(t, srv, b.NodeID, ingresshub.PushSent, "", 2)
	env1Bundles := []string{x1.ID, y1.ID}
	for _, e := range evs {
		switch e.NodeID {
		case a.NodeID:
			if e.EnvID != env1.ID || (e.BundleID != "" && !slices.Contains(env1Bundles, e.BundleID)) {
				t.Errorf("node of env 1 was looked at with %+v", e)
			}
		case b.NodeID:
			if e.EnvID != env2.ID {
				t.Errorf("node of env 2 was looked at with %+v", e)
			}
		}
	}
	for _, id := range []string{a.NodeID, b.NodeID} {
		want := map[string]int{a.NodeID: 2, b.NodeID: 2}[id]
		if n := dvCount(evs, id, ingresshub.PushSent, ""); n != want {
			t.Errorf("node %s was sent %d bundles, want %d", id, n, want)
		}
	}
}

// TestDeliveryMarshalledOncePerBundle: three nodes of an environment receive one bundle and the hub encoded it
// once.
func TestDeliveryMarshalledOncePerBundle(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2", "n3")
	y := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, y)

	conns := make([]*ingresstest.Conn, 0, 3)
	for _, sw := range []string{"n1", "n2", "n3"} {
		_, c := dvNode(t, srv, env, sw, "")
		conns = append(conns, c)
	}
	for i, c := range conns {
		if got := c.NextBundle(t); got.ID != y.ID {
			t.Errorf("node %d received %s, want %s", i, got.ID, y.ID)
		}
	}
	if n := srv.Hub.EnvelopeBuilds(); n != 1 {
		t.Errorf("the bundle was encoded %d times for 3 nodes, want 1", n)
	}
}

// TestDeliveryHelloOfTheCurrentBundleIsRecordedApplied is WR-A-02: a node that applied the current bundle but whose
// applied frame was lost says so in its next hello. It is not sent the bundle again, so the hello itself records
// it as applied. An ID the server does not hold as current never reaches the row.
func TestDeliveryHelloOfTheCurrentBundleIsRecordedApplied(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, x)

	id, _ := dvNode(t, srv, env, "n1", x.ID)
	dvAwaitCount(t, srv, id.NodeID, ingresshub.PushSame, x.ID, 1)
	if row := roRow(t, srv, id.NodeID); row.AppliedBundleID != x.ID || row.AppliedAt != srv.Clock.Now().UnixNano() {
		t.Errorf("after a hello of the current bundle: applied %q at %d, want %q now", row.AppliedBundleID, row.AppliedAt, x.ID)
	}

	other, c := dvNode(t, srv, env, "n2", "an-id-the-server-never-held")
	if got := c.NextBundle(t); got.ID != x.ID {
		t.Fatalf("bundle = %s, want %s", got.ID, x.ID)
	}
	if row := roRow(t, srv, other.NodeID); row.AppliedBundleID != "" || row.AppliedAt != 0 {
		t.Errorf("a hello of an unknown ID reached the row: applied %q at %d", row.AppliedBundleID, row.AppliedAt)
	}
}
