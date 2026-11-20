// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestHubEnrolTracer is the phase's thinnest complete path: a test supervisor enrols over wss with the
// environment's ingress join token, is welcomed with the instance pin, is registered, says hello and receives
// the environment's current bundle, through the real hub, service, store and audit writer.
func TestHubEnrolTracer(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "swarmnode1")
	want := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, want)

	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1"})

	ctx := testsupport.Context(t)
	w := c.Welcome
	instanceID, err := instance.InstanceID(ctx, srv.Store)
	if err != nil {
		t.Fatalf("read the instance ID: %v", err)
	}
	if w.EnvID != env.ID {
		t.Errorf("welcome envId = %q, want %q", w.EnvID, env.ID)
	}
	if w.InstanceID != instanceID || w.InstanceID == "" {
		t.Errorf("welcome instanceId = %q, want the settings row's %q", w.InstanceID, instanceID)
	}
	if w.HealthMs != 30000 {
		t.Errorf("welcome healthMs = %d, want 30000", w.HealthMs)
	}
	if id.NodeID != w.NodeID || id.InstanceID != instanceID {
		t.Errorf("the identity holds node %q instance %q, want %q and %q", id.NodeID, id.InstanceID, w.NodeID, instanceID)
	}

	// The row is the one the welcome names, active, in the token's environment.
	var status, rowEnv, name string
	if err := srv.Store.Read.QueryRow(`SELECT status, env_id, name FROM ingress_nodes WHERE id = ?`, w.NodeID).Scan(&status, &rowEnv, &name); err != nil {
		t.Fatalf("read the node row: %v", err)
	}
	if status != "active" || rowEnv != env.ID || name != "node-1" {
		t.Errorf("node row = %q %q %q, want active in %q named node-1", status, rowEnv, name, env.ID)
	}
	var enrolled int
	if err := srv.Store.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND target_id = ?`,
		audit.ActionIngressNodeEnrolled, w.NodeID).Scan(&enrolled); err != nil || enrolled != 1 {
		t.Errorf("ingress.node_enrolled rows = %d (%v), want 1", enrolled, err)
	}

	// The hub registered the session of the new node.
	s, err := srv.Hub.AwaitSession(ctx, w.NodeID, func(s *ingresshub.Session) bool { return s != nil })
	if err != nil {
		t.Fatalf("the node was never registered: %v", err)
	}
	if s.EnvID != env.ID || s.Name != "node-1" || s.DialHost != "127.0.0.1" {
		t.Errorf("session = env %q name %q dial host %q", s.EnvID, s.Name, s.DialHost)
	}

	// After hello the node receives its environment's bundle: one binary envelope DecodeBundle accepts with
	// the pinned instance ID.
	c.Hello(t, []int{1}, "")
	got := c.NextBundle(t)
	if got.ID != want.ID {
		t.Errorf("bundle ID = %q, want %q", got.ID, want.ID)
	}

	// The hostnames were recorded before the frame left, so a revoke lists what the node could have held.
	var held string
	if err := srv.Store.Read.QueryRow(`SELECT held_hostnames FROM ingress_nodes WHERE id = ?`, w.NodeID).Scan(&held); err != nil {
		t.Fatalf("read the held hostnames: %v", err)
	}
	var names []string
	if err := json.Unmarshal([]byte(held), &names); err != nil || len(names) != 1 || names[0] != "a.example.com" {
		t.Errorf("held_hostnames = %s (%v), want [a.example.com]", held, err)
	}
	var version string
	if err := srv.Store.Read.QueryRow(`SELECT version FROM ingress_nodes WHERE id = ?`, w.NodeID).Scan(&version); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if version == "" {
		t.Error("hello did not record the node's version")
	}
}

// TestHubEnrolNothingCommittedBeforeTheInstanceID is CR-A-01 through the hub: the instance ID is read before the
// enrolment commits, so a failing read leaves no row behind, and a node that enrols again with the same key, as
// one whose welcome was lost would, is welcomed with the same node ID.
func TestHubEnrolNothingCommittedBeforeTheInstanceID(t *testing.T) {
	var failing atomic.Bool
	var srv *ingresstest.Server
	srv = ingresstest.Start(t, ingresstest.Options{Configure: func(h *ingresshub.Hub) *ingresshub.Hub {
		return h.WithInstanceID(func(ctx context.Context) (string, error) {
			if failing.Load() {
				return "", errors.New("the settings read failed")
			}
			return instance.InstanceID(ctx, srv.Store)
		})
	}})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "swarmnode1")
	id := ingresstest.NewIdentity()
	d := ingresstest.Dial{Token: env.Token, Name: "node-1", SwarmNodeID: "swarmnode1"}

	failing.Store(true)
	c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), id, d)
	if c.Refuse == nil || c.Refuse.Code != ingresswire.RefuseInternal {
		t.Fatalf("enrol with a failing instance ID read = refuse %+v, err %v, want internal_error", c.Refuse, c.Err)
	}
	var rows int
	if err := srv.Store.Read.QueryRow(`SELECT COUNT(*) FROM ingress_nodes WHERE env_id = ?`, env.ID).Scan(&rows); err != nil {
		t.Fatalf("count the node rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d node rows after an enrolment that was not welcomed, want 0", rows)
	}

	failing.Store(false)
	first := srv.Enrol(t, id, d)
	// The node lost the welcome: it still has no node ID and enrols again with the same key.
	lost := &ingresstest.Identity{Key: id.Key}
	again := srv.Enrol(t, lost, d)
	if again.Welcome.NodeID != first.Welcome.NodeID {
		t.Errorf("enrol again welcomed node %q, want the first enrolment's %q", again.Welcome.NodeID, first.Welcome.NodeID)
	}
}
