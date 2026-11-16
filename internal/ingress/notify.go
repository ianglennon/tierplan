// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"sort"
	"sync"
)

// Notifier tells the reconciler that an environment's ingress configuration changed. The store calls
// Notify after a change commits; a subscriber wakes, asks Pending which environments, and rebuilds
// them. It never blocks the caller: a subscriber that is busy, slow or gone cannot hold up a write.
// Wake-ups coalesce, so a burst of changes is one wake-up and the set of environments is kept
// apart from the signal. A Notifier starts no goroutine.
type Notifier struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// NewNotifier returns a Notifier with no subscribers.
func NewNotifier() *Notifier {
	return &Notifier{subs: map[*Subscription]struct{}{}}
}

// Subscription is one reader of a Notifier.
type Subscription struct {
	n       *Notifier
	mu      sync.Mutex
	pending map[string]struct{}
	wake    chan struct{}
}

// Subscribe adds a reader. Close it when done.
func (n *Notifier) Subscribe() *Subscription {
	s := &Subscription{n: n, pending: map[string]struct{}{}, wake: make(chan struct{}, 1)}
	n.mu.Lock()
	n.subs[s] = struct{}{}
	n.mu.Unlock()
	return s
}

// Notify records that envID changed and wakes every subscriber. It returns at once whatever the
// subscribers are doing, and does nothing on a nil Notifier, so a store built without one works.
func (n *Notifier) Notify(envID string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	subs := make([]*Subscription, 0, len(n.subs))
	for s := range n.subs {
		subs = append(subs, s)
	}
	n.mu.Unlock()
	for _, s := range subs {
		s.mu.Lock()
		s.pending[envID] = struct{}{}
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default: // a wake-up is already waiting; the environment is in the pending set
		}
	}
}

// C is signalled when at least one environment is pending. A signal can find nothing pending when
// an earlier Pending call already took the environments; the reader treats that as a no-op.
func (s *Subscription) C() <-chan struct{} { return s.wake }

// Pending returns the environments that changed since the last call, sorted, and forgets them.
func (s *Subscription) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.pending))
	for id := range s.pending {
		out = append(out, id)
	}
	clear(s.pending)
	sort.Strings(out)
	return out
}

// Close stops delivery to the subscription and forgets what was pending. It is safe to call twice.
func (s *Subscription) Close() {
	s.n.mu.Lock()
	delete(s.n.subs, s)
	s.n.mu.Unlock()
	s.mu.Lock()
	clear(s.pending)
	s.mu.Unlock()
	select { // drop a wake-up that was already waiting
	case <-s.wake:
	default:
	}
}
