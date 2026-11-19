// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// /ingress/connect answers anyone until the welcome, so everything that bounds what an anonymous caller can
// cost lives here: how many handshakes run at once overall and from one address, how often one address may
// start one, and how often one address may try to enrol, which is the one handshake that costs a hash
// compare and a database read. The gate is an instance of its own: nothing is shared with the agent hub's
// limits (D-10), so a flood of one channel cannot starve the other. internal/tunnel/limits.go is the model,
// with the constants of this channel.

const (
	// maxHandshakes is how many unauthenticated handshakes may be in flight.
	maxHandshakes = 32
	// maxHandshakesPerAddr is how many of them one client address, or one IPv6 /64, may hold at once. It
	// leaves 24 slots for everyone else, so one address that opens handshakes and goes silent cannot take
	// them all.
	maxHandshakesPerAddr = 8
	// firstFrameTimeout is how long a node has, from the challenge, to send its auth. A real node sends it
	// as soon as it reads the challenge, so a few seconds is generous; a silent client gives its slot back
	// after this and not after the whole handshake timeout.
	firstFrameTimeout = 3 * time.Second
	// connectBurst is how many handshakes one address may start at once. A whole swarm behind one NAT
	// reconnects together after a server restart, so it is sized for that and not for a person.
	connectBurst = 64
	// connectRefill is how often an address earns one more handshake.
	connectRefill = 100 * time.Millisecond
	// enrolBurst and enrolRefill bound enrolment attempts per address: ten at once, then one every six
	// seconds. A refused enrolment is the guess of a token, so it is far scarcer than a reconnect.
	enrolBurst  = 10
	enrolRefill = 6 * time.Second
	// limiterTTL is how long an idle address is remembered.
	limiterTTL = 10 * time.Minute
	// connectRetryAfter and enrolRetryAfter are what a refused node is told to wait.
	connectRetryAfter = time.Second
	enrolRetryAfter   = 30 * time.Second
)

// gate is the admission control of the handshake: the in-flight caps, the connect bucket and the enrolment
// bucket.
type gate struct {
	connect *auth.IPLimiter
	enrol   *auth.IPLimiter
	slots   chan struct{}
	perAddr *addrInflight
}

// newGate builds the gate on now, which must not be nil (NewHub guarantees it).
func newGate(now func() time.Time) *gate {
	return &gate{
		connect: auth.NewIPLimiter(connectRefill, connectBurst, limiterTTL, now),
		enrol:   auth.NewIPLimiter(enrolRefill, enrolBurst, limiterTTL, now),
		slots:   make(chan struct{}, maxHandshakes),
		perAddr: newAddrInflight(),
	}
}

// admit decides whether one more handshake may start. Three bounds apply, in this order: the address may have
// fewer than maxHandshakesPerAddr in flight (checked first, so a refusal for concurrency does not spend its
// attempt budget), it must be within its budget of attempts, and fewer than maxHandshakes may be in flight in
// all. It returns release, which gives the slots back and is safe to call more than once, or the refusal to
// send.
func (g *gate) admit(ip string) (release func(), refuse *ingresswire.Refuse) {
	limited := &ingresswire.Refuse{
		T: ingresswire.TypeRefuse, Code: ingresswire.RefuseRateLimited,
		RetryAfterMs: int(connectRetryAfter / time.Millisecond),
		Detail:       "too many connection attempts; the node retries by itself",
	}
	key := auth.AddrKey(ip)
	if !g.perAddr.take(key, maxHandshakesPerAddr) {
		return nil, limited
	}
	if !g.connect.Allow(ip) {
		g.perAddr.give(key)
		return nil, limited
	}
	select {
	case g.slots <- struct{}{}:
	default:
		g.perAddr.give(key)
		return nil, limited
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-g.slots
			g.perAddr.give(key)
		})
	}, nil
}

// allowEnrol spends one of the address's enrolment attempts and reports whether it had one.
func (g *gate) allowEnrol(ip string) bool { return g.enrol.Allow(ip) }

// addrInflight counts the handshakes in flight per client address, keyed like the attempt budget
// (auth.AddrKey), so an IPv6 /64 is one address. An entry exists only while its address has a handshake in
// flight, so the map cannot outgrow the number of slots. It is a copy of internal/tunnel/limits.go's, kept
// separate so the two channels share no state (D-10).
type addrInflight struct {
	mu sync.Mutex
	n  map[string]int
	// changed is closed and cleared whenever a count changes, to wake Hub.AwaitHandshakes waiters; a waiter
	// makes it.
	changed chan struct{}
}

func newAddrInflight() *addrInflight { return &addrInflight{n: map[string]int{}} }

// notifyLocked wakes every waiter. a.mu must be held.
func (a *addrInflight) notifyLocked() {
	if a.changed != nil {
		close(a.changed)
		a.changed = nil
	}
}

// take counts one more handshake for key, or reports false when key already has max in flight.
func (a *addrInflight) take(key string, limit int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n[key] >= limit {
		return false
	}
	a.n[key]++
	a.notifyLocked()
	return true
}

// give takes one back.
func (a *addrInflight) give(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.notifyLocked()
	if a.n[key] <= 1 {
		delete(a.n, key)
		return
	}
	a.n[key]--
}

// AwaitHandshakes returns the first count of handshakes in flight, over all addresses, for which cond is true.
// It evaluates cond now and again whenever a handshake starts or ends, and never polls. When ctx ends first it
// returns the last count together with the context's error.
func (h *Hub) AwaitHandshakes(ctx context.Context, cond func(inFlight int) bool) (int, error) {
	a := h.gate.perAddr
	for {
		// Take the channel in the same critical section as the read, so a change between the read and the
		// wait is not missed.
		a.mu.Lock()
		if a.changed == nil {
			a.changed = make(chan struct{})
		}
		ch, total := a.changed, 0
		for _, n := range a.n {
			total += n
		}
		a.mu.Unlock()
		if cond(total) {
			return total, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return total, ctx.Err()
		}
	}
}
