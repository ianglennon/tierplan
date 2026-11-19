// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// rvActor is the administrator who revokes in these tests.
var rvActor = audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}

// rvHeld reads the hostnames a node's row says it was ever sent.
func rvHeld(t *testing.T, srv *ingresstest.Server, nodeID string) []string {
	t.Helper()
	row, err := sqlc.New(srv.Store.Read).GetIngressNode(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}
	var held []string
	if err := json.Unmarshal([]byte(row.HeldHostnames), &held); err != nil {
		t.Fatalf("decode held_hostnames %q: %v", row.HeldHostnames, err)
	}
	return held
}

// rvTwoNodes enrols two nodes of one swarm environment, and returns them once each holds bundle x, which names
// a.example and b.example, and has its session registered.
func rvTwoNodes(t *testing.T, srv *ingresstest.Server) (env ingresstest.Environment, a, b *ingresstest.Identity, ca, cb *ingresstest.Conn) {
	t.Helper()
	env = srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	x := ingresstest.MakeBundle(t, 1, "a.example", "b.example")
	srv.Bundles.Set(env.ID, x)

	// Neither node runs a bundle yet, so each is sent x after its hello.
	a, ca = dvNode(t, srv, env, "n1", "")
	b, cb = dvNode(t, srv, env, "n2", "")
	if got := ca.NextBundle(t); got.ID != x.ID {
		t.Fatalf("node a received %s, want %s", got.ID, x.ID)
	}
	if got := cb.NextBundle(t); got.ID != x.ID {
		t.Fatalf("node b received %s, want %s", got.ID, x.ID)
	}
	ca.Applied(t, x.ID)
	cb.Applied(t, x.ID)
	lcAwaitRegistered(t, srv, a.NodeID)
	lcAwaitRegistered(t, srv, b.NodeID)
	return env, a, b, ca, cb
}

// TestRevokeClosesTheSessionAtOnceAndOthersKeepWorking: an Admin revokes node A. A's session is closed with the
// reason revoked, A is refused when it connects again, and B, of the same environment, keeps the very session it
// had and is sent the next bundle (SC 2, D-16, PROH-22-02).
func TestRevokeClosesTheSessionAtOnceAndOthersKeepWorking(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env, a, b, ca, cb := rvTwoNodes(t, srv)
	sessionB := srv.Hub.Session(b.NodeID)
	if sessionB == nil {
		t.Fatal("node b has no session")
	}
	beforeB := rvHeld(t, srv, b.NodeID)

	got, err := srv.Service.Revoke(context.Background(), rvActor, env.ID, a.NodeID, ingresshub.RevokeOptions{Reason: "lost the host"}, "192.0.2.1", "e2e")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if want := []string{"a.example", "b.example"}; !slices.Equal(got.HeldHostnames, want) {
		t.Errorf("HeldHostnames = %v, want %v", got.HeldHostnames, want)
	}
	if !got.RotateTokenRecommended {
		t.Error("the revoke does not recommend rotating the join token")
	}

	code, reason := ca.AwaitClose(t)
	if code != websocket.StatusPolicyViolation || reason != ingresswire.CloseRevoked {
		t.Errorf("node a was closed with %d %q, want 1008 %q", code, reason, ingresswire.CloseRevoked)
	}
	lcAwaitGone(t, srv, a.NodeID)

	// A reconnects with its stored key and is refused as revoked.
	re := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), a, ingresstest.Dial{Mode: ingresswire.ModeConnect})
	if re.Welcome != nil || re.Refuse == nil || re.Refuse.Code != ingresswire.RefuseRevoked {
		t.Fatalf("a's connect: welcome %+v refuse %+v err %v, want refuse %q", re.Welcome, re.Refuse, re.Err, ingresswire.RefuseRevoked)
	}

	// B has the same session value it had, and the next bundle the source publishes reaches it.
	if now := srv.Hub.Session(b.NodeID); now != sessionB {
		t.Errorf("node b's session changed across the revoke of node a: %p, was %p", now, sessionB)
	}
	y := ingresstest.MakeBundle(t, 2, "a.example", "b.example", "c.example")
	srv.Bundles.Set(env.ID, y)
	if got := cb.NextBundle(t); got.ID != y.ID {
		t.Fatalf("node b received %s after the revoke, want %s", got.ID, y.ID)
	}
	if now := srv.Hub.Session(b.NodeID); now != sessionB {
		t.Errorf("node b's session changed after the next bundle: %p, was %p", now, sessionB)
	}

	// Y was published after the revoke committed, so A was sent nothing of it, and B's row is the only one that grew.
	if held := rvHeld(t, srv, a.NodeID); !slices.Equal(held, got.HeldHostnames) {
		t.Errorf("node a's held hostnames became %v after the revoke, want %v", held, got.HeldHostnames)
	}
	if held, want := rvHeld(t, srv, b.NodeID), []string{"a.example", "b.example", "c.example"}; !slices.Equal(held, want) || slices.Equal(held, beforeB) {
		t.Errorf("node b's held hostnames = %v, want %v (was %v)", held, want, beforeB)
	}
	if row, err := sqlc.New(srv.Store.Read).GetIngressNode(context.Background(), b.NodeID); err != nil || row.Status == "revoked" || row.ReenrolBlocked != 0 {
		t.Errorf("node b's row after the revoke of a: status %q blocked %d err %v", row.Status, row.ReenrolBlocked, err)
	}
}

// TestRevokeThenEnrolAgainWithTheSameTokenIsRefused: the join token is still mounted in the revoked node's
// container, and enrolling again with it through the endpoint is refused node_revoked (D-20).
func TestRevokeThenEnrolAgainWithTheSameTokenIsRefused(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env, a, _, ca, _ := rvTwoNodes(t, srv)

	if _, err := srv.Service.Revoke(context.Background(), rvActor, env.ID, a.NodeID, ingresshub.RevokeOptions{}, "", ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	ca.AwaitClose(t)

	again := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(),
		ingresstest.Dial{Mode: ingresswire.ModeEnrol, Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	if again.Welcome != nil || again.Refuse == nil || again.Refuse.Code != ingresswire.RefuseNodeRevoked {
		t.Fatalf("enrol again: welcome %+v refuse %+v err %v, want refuse %q", again.Welcome, again.Refuse, again.Err, ingresswire.RefuseNodeRevoked)
	}
	if n := nodeCount(t, srv); n != 2 {
		t.Errorf("%d ingress_nodes rows, want the two enrolled before the revoke", n)
	}
}

// TestRevokeAllowReenrolLetsTheNodeBack: a revoke that allows it lets the host enrol again, as a new node, with the
// token that is still valid; the revoked row stays revoked (D-20).
func TestRevokeAllowReenrolLetsTheNodeBack(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env, a, _, ca, _ := rvTwoNodes(t, srv)

	if _, err := srv.Service.Revoke(context.Background(), rvActor, env.ID, a.NodeID, ingresshub.RevokeOptions{AllowReenrol: true}, "", ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	ca.AwaitClose(t)

	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	if c.Welcome == nil || c.Welcome.NodeID == "" || c.Welcome.NodeID == a.NodeID {
		t.Fatalf("welcome = %+v, want a new node ID other than %s", c.Welcome, a.NodeID)
	}
	lcAwaitRegistered(t, srv, id.NodeID)
	if srv.Service.StillActive(a.NodeID) {
		t.Error("the revoked row is active again")
	}
	// The old identity is still refused as revoked.
	re := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), a, ingresstest.Dial{Mode: ingresswire.ModeConnect})
	if re.Refuse == nil || re.Refuse.Code != ingresswire.RefuseRevoked {
		t.Errorf("the revoked node's connect: refuse %+v err %v, want %q", re.Refuse, re.Err, ingresswire.RefuseRevoked)
	}
}
