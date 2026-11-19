// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"encoding/json"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/instance"
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
