// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"io"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Clock is the time source of the helper: the rate limit reads Now, and the discovery rescan, the FIFO retry
// and the stop window wait on After. It is an interface so a test can move time by hand.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

// Now returns the current time.
func (RealClock) Now() time.Time { return time.Now() }

// After waits for d.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// MaxLinesPerSecond is the most fault, foreign_router and clock_skew lines the helper writes in one second
// (Pitfall 15). State and start lines are never limited: they are bounded by what keepalived does, and the
// server's view of the holder depends on every one of them.
const MaxLinesPerSecond = 20

// Emitter writes the helper's event lines to one writer. It fills in what is the same on every line (the node,
// its name and the router ID) and a sequence number, and it is the only thing that writes to the report
// channel, so nothing but a validated ingressbundle.VIPEvent ever reaches the container log (T-26-29).
type Emitter struct {
	w     io.Writer
	cfg   Config
	clock Clock

	mu        sync.Mutex
	seq       uint64
	tokens    float64
	last      time.Time
	lastState string
	dropped   uint64
	refused   uint64
}

// NewEmitter returns an Emitter that writes to w for the node cfg describes.
func NewEmitter(w io.Writer, cfg Config, clock Clock) *Emitter {
	return &Emitter{w: w, cfg: cfg, clock: clock, tokens: MaxLinesPerSecond, last: clock.Now()}
}

// limited reports whether the rate limit applies to an event.
func limited(event string) bool {
	return event != ingressbundle.VIPEventState && event != ingressbundle.VIPEventStart
}

// Emit writes one event line and reports whether it was written. The event's Node, Name, Router and Seq are
// overwritten. Every call takes the next sequence number, written or not, so a line the limit dropped is a gap
// in the sequence the server can see.
//
// A fault, foreign_router or clock_skew line over MaxLinesPerSecond is not written and only counts toward
// Dropped. An event the format refuses is not written and counts toward Refused.
func (e *Emitter) Emit(ev ingressbundle.VIPEvent) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.seq++
	if limited(ev.Event) {
		now := e.clock.Now()
		if el := now.Sub(e.last); el > 0 {
			e.tokens = min(MaxLinesPerSecond, e.tokens+el.Seconds()*MaxLinesPerSecond)
			e.last = now
		}
		if e.tokens < 1 {
			e.dropped++
			return false
		}
		e.tokens--
	}
	ev.Node, ev.Name, ev.Router, ev.Seq = e.cfg.NodeID, e.cfg.NodeName, e.cfg.RouterID, e.seq
	line, err := ev.MarshalLine()
	if err != nil {
		e.refused++
		return false
	}
	_, _ = e.w.Write(append(line, '\n'))
	if ev.Event == ingressbundle.VIPEventState {
		e.lastState = ev.State
	}
	return true
}

// LastState is the state of the last state line written, or empty when none was.
func (e *Emitter) LastState() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastState
}

// Dropped is how many lines the rate limit has dropped.
func (e *Emitter) Dropped() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

// Refused is how many events the line format refused.
func (e *Emitter) Refused() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refused
}
