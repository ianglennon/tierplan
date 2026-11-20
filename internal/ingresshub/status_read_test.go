// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// hideNodes makes every read of ingress_nodes fail, as a database fault would, by renaming the table, and puts
// it back when the test ends or when the returned function is called.
func hideNodes(t *testing.T, st *store.Store) (restore func()) {
	t.Helper()
	if _, err := st.Write.Exec(`ALTER TABLE ingress_nodes RENAME TO ingress_nodes_hidden`); err != nil {
		t.Fatalf("hide the node table: %v", err)
	}
	var once atomic.Bool
	restore = func() {
		if once.Swap(true) {
			return
		}
		if _, err := st.Write.Exec(`ALTER TABLE ingress_nodes_hidden RENAME TO ingress_nodes`); err != nil {
			t.Errorf("restore the node table: %v", err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// TestStillActiveReadErrorIsNotRevoked is WR-A-01 at the service: a read that fails is an error, not "revoked".
func TestStillActiveReadErrorIsNotRevoked(t *testing.T) {
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	restore := hideNodes(t, f.st)
	active, err := f.svc.StillActive(a.res.NodeID)
	if err == nil {
		t.Errorf("StillActive with a failing read = %v and no error, want an error", active)
	}
	restore()
	if active, err := f.svc.StillActive(a.res.NodeID); err != nil || !active {
		t.Errorf("StillActive after the read works again = %v, %v, want true", active, err)
	}
}

// TestRegisterStatusReadErrorClosesRetryable is WR-A-01 at registration: a node whose status cannot be read as
// its session registers is closed with internal-error status, which it retries, and never with the reason
// revoked, which a supervisor may take as final.
func TestRegisterStatusReadErrorClosesRetryable(t *testing.T) {
	var hide atomic.Bool
	var srv *ingresstest.Server
	srv = ingresstest.Start(t, ingresstest.Options{Hold: func(string) {
		if hide.Load() {
			hideNodes(t, srv.Store)
		}
	}})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	id := ingresstest.NewIdentity()

	hide.Store(true)
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	code, reason := c.AwaitClose(t)
	if code != websocket.StatusInternalError || reason != ingresswire.RefuseInternal {
		t.Errorf("close = %d %q, want %d %q", code, reason, websocket.StatusInternalError, ingresswire.RefuseInternal)
	}
	if srv.Hub.Session(id.NodeID) != nil {
		t.Error("a session whose status could not be read was registered")
	}
}

// TestSendStatusReadErrorEndsTheSession is WR-A-01 in the writer: a status read that fails before a bundle is sent
// ends the session as a fault of the server's, rather than leaving it to wait for the next wake.
func TestSendStatusReadErrorEndsTheSession(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	lcAwaitRegistered(t, srv, id.NodeID)
	c.Hello(t, []int{1}, "")
	// The hello has been handled when the writer has looked at the (empty) environment.
	dvAwaitCount(t, srv, id.NodeID, ingresshub.PushNone, "", 1)

	hideNodes(t, srv.Store)
	srv.Bundles.Set(env.ID, ingresstest.MakeBundle(t, 1, "a.example.com"))
	code, reason := c.AwaitClose(t)
	if code != websocket.StatusInternalError || reason != "internal_error" {
		t.Errorf("close = %d %q, want %d internal_error", code, reason, websocket.StatusInternalError)
	}
	if n := dvCount(srv.Pushes().Events(), id.NodeID, ingresshub.PushSent, ""); n != 0 {
		t.Errorf("%d bundles sent while the node's status was unknown, want 0", n)
	}
}
