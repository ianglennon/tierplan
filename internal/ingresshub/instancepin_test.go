// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

// The instance pin (D-14). A node pins the instance ID it was welcomed by when it enrols, and the server puts
// that ID in every welcome and in every bundle envelope. A node that finds another ID refuses the bundle, so a
// node whose address now answers with a different TierPlan, or with a backup restored from an older day, takes
// nothing from it.
//
// D-25 is the limit of this: the pin guards against a misdirected or stale TierPlan. It does not authenticate the
// server. A server that knows the instance ID could say it, and what proves the server is TLS with the SPKI
// pins the node was given. These tests prove only what the pin is for.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// ipEnvelope reads the next message off the socket as raw bytes and decodes it as a bundle envelope, which is
// how a node sees the instance ID field.
func ipEnvelope(t *testing.T, c *ingresstest.Conn) ingresswire.BundleEnvelope {
	t.Helper()
	typ, data := c.NextRaw(t)
	if typ != websocket.MessageBinary {
		t.Fatalf("the next message is a text frame, not a bundle: %s", data)
	}
	var env ingresswire.BundleEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	if env.T != ingresswire.TypeBundle {
		t.Fatalf("the envelope's type is %q, want %q", env.T, ingresswire.TypeBundle)
	}
	return env
}

// ipAppliedRow reads the bundle ID the node's row says it runs.
func ipAppliedRow(t *testing.T, srv *ingresstest.Server, nodeID string) string {
	t.Helper()
	row, err := sqlc.New(srv.Store.Read).GetIngressNode(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}
	return row.AppliedBundleID
}

// TestInstancePinInWelcomeAndEveryBundle: the welcome and every bundle envelope carry the instance ID of the
// settings row.
func TestInstancePinInWelcomeAndEveryBundle(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	stored, err := instance.InstanceID(context.Background(), srv.Store)
	if err != nil || stored == "" {
		t.Fatalf("the settings row has no instance ID: %q, %v", stored, err)
	}
	x := ingresstest.MakeBundle(t, 1, "a.example")
	srv.Bundles.Set(env.ID, x)

	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	if c.Welcome.InstanceID != stored {
		t.Errorf("the welcome's instance ID is %q, want %q", c.Welcome.InstanceID, stored)
	}
	if id.InstanceID != stored {
		t.Errorf("the node pinned %q, want %q", id.InstanceID, stored)
	}
	c.Hello(t, []int{1}, "")

	first := ipEnvelope(t, c)
	if first.InstanceID != stored || first.Bundle.ID != x.ID {
		t.Errorf("the first envelope carries instance %q bundle %s, want %q and %s", first.InstanceID, first.Bundle.ID, stored, x.ID)
	}
	c.Applied(t, x.ID)

	y := ingresstest.MakeBundle(t, 2, "a.example", "b.example")
	srv.Bundles.Set(env.ID, y)
	second := ipEnvelope(t, c)
	if second.InstanceID != stored || second.Bundle.ID != y.ID {
		t.Errorf("the second envelope carries instance %q bundle %s, want %q and %s", second.InstanceID, second.Bundle.ID, stored, y.ID)
	}
}

// TestInstancePinMismatchIsRefusedByTheNode: after the settings row holds another instance ID, as when a different
// TierPlan answers at the same address, the next bundle fails the node's check with ErrInstanceMismatch and a
// reconnect's welcome fails CheckWelcome against the pin.
func TestInstancePinMismatchIsRefusedByTheNode(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	x := ingresstest.MakeBundle(t, 1, "a.example")
	srv.Bundles.Set(env.ID, x)

	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	c.Hello(t, []int{1}, "")
	if got := c.NextBundle(t); got.ID != x.ID {
		t.Fatalf("the first bundle is %s, want %s", got.ID, x.ID)
	}
	c.Applied(t, x.ID)
	pinned := id.InstanceID

	other, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("make an instance ID: %v", err)
	}
	if other.String() == pinned {
		t.Fatal("the new instance ID equals the pinned one")
	}
	srv.SetInstanceID(t, other.String())

	srv.Bundles.Set(env.ID, ingresstest.MakeBundle(t, 2, "a.example", "b.example"))
	if _, err := c.TryNextBundle(t); !errors.Is(err, ingresswire.ErrInstanceMismatch) {
		t.Fatalf("the bundle of another instance: error %v, want ErrInstanceMismatch", err)
	}

	// A reconnect: the server welcomes the node with the other instance ID, and the node's check refuses it. The
	// node keeps its pin.
	re := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), id, ingresstest.Dial{Mode: ingresswire.ModeConnect})
	if re.Welcome == nil || re.Welcome.InstanceID != other.String() {
		t.Fatalf("the reconnect's welcome = %+v, want one carrying %q", re.Welcome, other.String())
	}
	if !errors.Is(re.Err, ingresswire.ErrInstanceMismatch) {
		t.Errorf("the reconnect's check error = %v, want ErrInstanceMismatch", re.Err)
	}
	if id.InstanceID != pinned {
		t.Errorf("the node's pin became %q, want it to stay %q", id.InstanceID, pinned)
	}
	if _, err := ingresswire.CheckWelcome(*re.Welcome, pinned); !errors.Is(err, ingresswire.ErrInstanceMismatch) {
		t.Errorf("CheckWelcome with the old pin: %v, want ErrInstanceMismatch", err)
	}
}

// TestInstancePinLowerVersionIsApplied: a bundle whose version is lower than the one the node applied before is
// received, decoded and applied, as after a restore of an older backup of the same TierPlan. Ordering by
// version is not the node's job.
func TestInstancePinLowerVersionIsApplied(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	v5 := ingresstest.MakeBundle(t, 5, "a.example", "b.example")
	srv.Bundles.Set(env.ID, v5)

	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	c.Hello(t, []int{1}, "")
	got := c.NextBundle(t)
	if got.ID != v5.ID || got.Version.Config != 5 {
		t.Fatalf("the first bundle is %s at version %d, want %s at 5", got.ID, got.Version.Config, v5.ID)
	}
	c.Applied(t, v5.ID)
	// The hub finished the applied message, row write included, when the writer's look at it is reported.
	srv.Pushes().Await(t, func(evs []ingresshub.PushEvent) bool { return dvCount(evs, id.NodeID, ingresshub.PushSame, v5.ID) >= 1 })
	if row := ipAppliedRow(t, srv, id.NodeID); row != v5.ID {
		t.Fatalf("the row's applied bundle is %q, want %s", row, v5.ID)
	}

	// The restored source is at version 2 with other content, hence another ID.
	v2 := ingresstest.MakeBundle(t, 2, "restored.example")
	if v2.ID == v5.ID {
		t.Fatal("the restored bundle has the ID of the later one")
	}
	srv.Bundles.Set(env.ID, v2)
	got, err := c.TryNextBundle(t)
	if err != nil {
		t.Fatalf("the lower-version bundle was refused by the node's check: %v", err)
	}
	if got.ID != v2.ID || got.Version.Config != 2 {
		t.Fatalf("the restored bundle is %s at version %d, want %s at 2", got.ID, got.Version.Config, v2.ID)
	}
	c.Applied(t, v2.ID)
	srv.Pushes().Await(t, func(evs []ingresshub.PushEvent) bool { return dvCount(evs, id.NodeID, ingresshub.PushSame, v2.ID) >= 1 })
	if row := ipAppliedRow(t, srv, id.NodeID); row != v2.ID {
		t.Errorf("the row's applied bundle is %q after the restore, want %s", row, v2.ID)
	}
}

// TestInstancePinLargeBundleIsReceived: a bundle whose envelope is far over the WebSocket library's default read
// limit of 32768 bytes reaches a node that called ingresswire.PrepareNodeConn whole (D-21), and a node that did
// not is cut off.
func TestInstancePinLargeBundleIsReceived(t *testing.T) {
	const size = 200 << 10
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	big := ingresstest.MakeLargeBundle(t, size)
	srv.Bundles.Set(env.ID, big)

	// The bundle as marshalled is over the default limit, asserted here and not assumed.
	payload, err := json.Marshal(ingresswire.BundleEnvelope{T: ingresswire.TypeBundle, InstanceID: "x", Bundle: big})
	if err != nil {
		t.Fatalf("marshal the bundle: %v", err)
	}
	if len(payload) <= 32768 {
		t.Fatalf("the marshalled bundle is %d bytes, not over the default read limit of 32768", len(payload))
	}

	// DialNode calls PrepareNodeConn after the welcome, as the supervisor does.
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	c.Hello(t, []int{1}, "")
	typ, data := c.NextRaw(t)
	if typ != websocket.MessageBinary || len(data) <= 32768 {
		t.Fatalf("the message is %v of %d bytes, want a binary frame over 32768", typ, len(data))
	}
	got, err := ingresswire.DecodeBundle(data, id.InstanceID)
	if err != nil {
		t.Fatalf("decode the large bundle: %v", err)
	}
	if got.ID != big.ID || len(got.Files) != 1 || len(got.Files[0].Data) != size {
		t.Errorf("the node received bundle %s with %d files, want %s whole (%d bytes of data)", got.ID, len(got.Files), big.ID, size)
	}

	// A node that kept the library's default limit cannot read it: this is what PrepareNodeConn is for.
	id2 := ingresstest.NewIdentity()
	c2 := srv.Enrol(t, id2, ingresstest.Dial{Token: env.Token, Name: "node-n2", SwarmNodeID: "n2"})
	c2.WS.SetReadLimit(32768)
	c2.Hello(t, []int{1}, "")
	// The read must fail on the limit itself, not on the test's own deadline: a hub that never sent the frame
	// would otherwise pass after a stall (WR-C-2).
	ctx := testsupport.Context(t)
	if _, _, err := c2.WS.Read(ctx); err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "read limited") {
		t.Errorf("a node with the default read limit: err = %v (test context: %v), want a read-limit error", err, ctx.Err())
	}
}
