// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
)

// switches is the delivery gate a test turns by hand, per environment.
type switches struct {
	mu sync.Mutex
	on map[string]bool
}

func (s *switches) set(envID string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on == nil {
		s.on = map[string]bool{}
	}
	s.on[envID] = on
}

func (s *switches) enabled(_ context.Context, envID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on[envID]
}

// TestDeliveryGatedOnEnabled: a node of an environment whose ingress is off completes its hello and is sent no bundle
// (outcome disabled); switching on and telling the environment's notifier delivers the current bundle at once; a node
// of an enabled environment is unaffected; and a hub with no gate sends as it always did (D-12).
func TestDeliveryGatedOnEnabled(t *testing.T) {
	gate := &switches{}
	changes := ingress.NewNotifier()
	srv := ingresstest.Start(t, ingresstest.Options{Configure: func(h *ingresshub.Hub) *ingresshub.Hub {
		return h.WithDeliveryGate(gate.enabled).WithEnvChanges(changes)
	}})
	off := srv.NewEnvironment(t, "swarm")
	on := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(off.ID, "n1")
	srv.Nodes.Set(on.ID, "n2")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(off.ID, x)
	srv.Bundles.Set(on.ID, x)
	gate.set(on.ID, true)

	idOff, cOff := dvNode(t, srv, off, "n1", "")
	idOn, cOn := dvNode(t, srv, on, "n2", "")

	// The enabled environment's node gets the bundle; the other's is held back and the event says why.
	if got := cOn.NextBundle(t); got.ID != x.ID {
		t.Errorf("the enabled environment's node received %s, want %s", got.ID, x.ID)
	}
	dvAwaitCount(t, srv, idOn.NodeID, ingresshub.PushSent, x.ID, 1)
	evs := dvAwaitCount(t, srv, idOff.NodeID, ingresshub.PushDisabled, "", 1)
	if n := dvCount(evs, idOff.NodeID, ingresshub.PushSent, ""); n != 0 {
		t.Fatalf("the node of a disabled environment was sent %d bundles", n)
	}
	if ingresshub.PushDisabled != "disabled" {
		t.Errorf("outcome = %q", ingresshub.PushDisabled)
	}

	// Switching on, and the store's notification, delivers the current bundle.
	gate.set(off.ID, true)
	changes.Notify(off.ID)
	if got := cOff.NextBundle(t); got.ID != x.ID {
		t.Errorf("after the switch the node received %s, want %s", got.ID, x.ID)
	}
	dvAwaitCount(t, srv, idOff.NodeID, ingresshub.PushSent, x.ID, 1)

	// A notification for one environment does not wake the other: the enabled node saw one frame only.
	if n := dvCount(srv.Pushes().Events(), idOn.NodeID, ingresshub.PushSent, ""); n != 1 {
		t.Errorf("the enabled environment's node was sent %d bundles, want 1", n)
	}
}

// TestDeliveryWithoutGate: a hub with no delivery gate sends to every environment, as before the gate existed.
func TestDeliveryWithoutGate(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	x := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, x)
	_, c := dvNode(t, srv, env, "n1", "")
	if got := c.NextBundle(t); got.ID != x.ID {
		t.Errorf("received %s, want %s", got.ID, x.ID)
	}
}
