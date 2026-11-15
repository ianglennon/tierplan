// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"slices"
	"testing"
)

// signalled reports whether a wake-up is waiting on the subscription, without waiting for one.
// Notify sends before it returns, so the answer is exact.
func signalled(s *Subscription) bool {
	select {
	case <-s.C():
		return true
	default:
		return false
	}
}

func TestNotifierCoalescesAndSortsPendingEnvironments(t *testing.T) {
	n := NewNotifier()
	sub := n.Subscribe()
	defer sub.Close()

	n.Notify("a")
	n.Notify("a")
	n.Notify("b")

	if !signalled(sub) {
		t.Fatal("no wake-up after Notify")
	}
	if signalled(sub) {
		t.Fatal("three notifications gave more than one wake-up")
	}
	if got := sub.Pending(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Pending = %v, want [a b]", got)
	}
	if got := sub.Pending(); len(got) != 0 {
		t.Fatalf("second Pending = %v, want it drained", got)
	}

	n.Notify("c")
	if !signalled(sub) {
		t.Fatal("no wake-up after the pending set was drained")
	}
	if got := sub.Pending(); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("Pending = %v, want [c]", got)
	}
}

func TestNotifierNeverBlocksAWriter(t *testing.T) {
	n := NewNotifier()
	n.Notify("no-subscriber") // no reader at all

	slow := n.Subscribe() // a reader that never reads
	defer slow.Close()
	for range 1000 {
		n.Notify("env") // would deadlock if the send blocked on a full channel
	}
	if got := slow.Pending(); !slices.Equal(got, []string{"env"}) {
		t.Fatalf("Pending = %v, want [env] once", got)
	}
}

func TestNotifierEverySubscriptionHearsEveryEnvironment(t *testing.T) {
	n := NewNotifier()
	one, two := n.Subscribe(), n.Subscribe()
	defer one.Close()
	defer two.Close()

	n.Notify("a")

	for name, s := range map[string]*Subscription{"one": one, "two": two} {
		if !signalled(s) {
			t.Errorf("subscription %s heard nothing", name)
		}
		if got := s.Pending(); !slices.Equal(got, []string{"a"}) {
			t.Errorf("subscription %s Pending = %v, want [a]", name, got)
		}
	}
}

func TestNotifierCloseStopsDelivery(t *testing.T) {
	n := NewNotifier()
	closed, open := n.Subscribe(), n.Subscribe()
	defer open.Close()

	closed.Close()
	closed.Close() // closing twice is harmless
	n.Notify("a")

	if signalled(closed) {
		t.Error("a closed subscription was woken")
	}
	if got := closed.Pending(); len(got) != 0 {
		t.Errorf("a closed subscription has pending %v", got)
	}
	if !signalled(open) {
		t.Error("the open subscription was not woken")
	}
}

func TestNotifierNilIsSilent(t *testing.T) {
	var n *Notifier
	n.Notify("a") // a store built without a notifier must not panic
}
