// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"sync"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// statsMemberSpec says what one member of an aggregate stats stream is: the container to
// sample on a node, or nothing to sample because no agent reaches the node (silent).
type statsMemberSpec struct {
	// Node is the node ID the hub is asked on (the ID OnNode takes); Container the full
	// container ID. Both are empty for a silent member.
	Node, Container string
	// Silent marks a member whose node nothing reaches now: it is never sampled and counts
	// as not reporting.
	Silent bool
	// Group is what the stream sums the member into: a swarm node ID, a service ID or the
	// member's own ID, as the stream chooses.
	Group string
	// Label is a name the stream carries along (the swarm node's host name of a task).
	Label string
}

// statsMember is one container an aggregate stream watches, with what it last learned.
type statsMember struct {
	id   string
	spec statsMemberSpec
	// state is the kind of the last update: empty while the member has not answered yet.
	state StatsKind
	point dockerx.StatsPoint
	// fresh is set when the member answered since the last round was taken.
	fresh bool
	leave func()
}

// statsView is a member as a round reports it.
type statsView struct {
	ID    string
	Spec  statsMemberSpec
	State StatsKind
	Point dockerx.StatsPoint
}

// feedMsg is one update of one member, or the news that the hub closed its channel.
type feedMsg struct {
	m      *statsMember
	u      StatsUpdate
	closed bool
}

// statsFeed merges the samples of many containers into rounds, for the streams that report
// a total (a service, a list, a node) rather than one container. It watches each member
// through the shared hub, so a container two surfaces watch is sampled once. A round is
// complete when every member that can answer has answered since the last one, which is
// once per tick; a member that is slow does not hold the others back for longer than the
// wait. It is used by one goroutine, the stream's handler.
type statsFeed struct {
	hub    *StatsHub
	env    string
	ctx    context.Context
	cancel context.CancelFunc
	in     chan feedMsg
	wg     sync.WaitGroup

	members map[string]*statsMember
	// dirty is true when something changed since the last round was taken.
	dirty bool
	// hubClosed is set when the hub closed a member's channel.
	hubClosed bool
}

func newStatsFeed(parent context.Context, hub *StatsHub, env string) *statsFeed {
	ctx, cancel := context.WithCancel(parent)
	return &statsFeed{hub: hub, env: env, ctx: ctx, cancel: cancel, in: make(chan feedMsg, 64), members: map[string]*statsMember{}, dirty: true}
}

// sync makes the watched set the given one: members that are gone are let go, new ones are
// watched, and one whose spec changed is watched anew.
func (f *statsFeed) sync(want map[string]statsMemberSpec) {
	for id, m := range f.members {
		if s, ok := want[id]; !ok || s != m.spec {
			f.drop(id, m)
		}
	}
	for id, spec := range want {
		if _, ok := f.members[id]; ok {
			continue
		}
		m := &statsMember{id: id, spec: spec}
		f.members[id] = m
		f.dirty = true
		if spec.Silent {
			m.state = StatsKindNotReporting
			continue
		}
		ch, leave := f.hub.Subscribe(f.env, spec.Node, spec.Container)
		m.leave = leave
		f.wg.Add(1)
		go f.forward(m, ch)
	}
}

func (f *statsFeed) drop(id string, m *statsMember) {
	delete(f.members, id)
	f.dirty = true
	if m.leave != nil {
		m.leave()
	}
}

// forward passes the updates of one subscription to the feed until its channel closes.
func (f *statsFeed) forward(m *statsMember, ch <-chan StatsUpdate) {
	defer f.wg.Done()
	for u := range ch {
		select {
		case f.in <- feedMsg{m: m, u: u}:
		case <-f.ctx.Done():
			return
		}
	}
	select {
	case f.in <- feedMsg{m: m, closed: true}:
	case <-f.ctx.Done():
	}
}

// apply takes one message. A message of a member that was let go is ignored.
func (f *statsFeed) apply(msg feedMsg) {
	if f.members[msg.m.id] != msg.m {
		return
	}
	if msg.closed {
		f.hubClosed = true
		return
	}
	m := msg.m
	m.state = msg.u.Kind
	if msg.u.Kind == StatsKindSample {
		m.point = msg.u.Point
	}
	m.fresh = true
	f.dirty = true
}

// complete reports whether a round can be taken: something changed and every member that
// can answer has.
func (f *statsFeed) complete() bool {
	if !f.dirty {
		return false
	}
	for _, m := range f.members {
		if m.state == StatsKindNotReporting || m.state == StatsKindStopped {
			continue
		}
		if !m.fresh {
			return false
		}
	}
	return true
}

// take returns the members as they stand and starts the next round.
func (f *statsFeed) take() []statsView {
	out := make([]statsView, 0, len(f.members))
	for id, m := range f.members {
		out = append(out, statsView{ID: id, Spec: m.spec, State: m.state, Point: m.point})
		m.fresh = false
	}
	f.dirty = false
	return out
}

// close lets every member go and waits for the forwarders.
func (f *statsFeed) close() {
	f.cancel()
	for id, m := range f.members {
		f.drop(id, m)
	}
	f.wg.Wait()
}

// statsSum is the total of the members of one group.
type statsSum struct {
	// CPU is the percent of one core, summed; nil while no member has one yet.
	CPU *float64
	Mem uint64
	// The rates are bytes per second, summed; nil while no member has one yet.
	NetRx, NetTx, BlkRead, BlkWrite *float64
	// Counted are the members that gave a sample, Silent those on a node that is not
	// reporting. A member that has not answered yet, or has stopped, is neither.
	Counted, Silent int
}

// sumViews adds the samples of the given members together.
func sumViews(vs []statsView) statsSum {
	var s statsSum
	add := func(dst **float64, v float64) {
		if *dst == nil {
			z := 0.0
			*dst = &z
		}
		**dst += v
	}
	for _, v := range vs {
		switch v.State {
		case StatsKindNotReporting:
			s.Silent++
		case StatsKindSample:
			s.Counted++
			s.Mem += v.Point.MemBytes
			if v.Point.CPUPercent != nil {
				add(&s.CPU, *v.Point.CPUPercent)
			}
			if v.Point.HasRates {
				add(&s.NetRx, v.Point.NetRxRate)
				add(&s.NetTx, v.Point.NetTxRate)
				add(&s.BlkRead, v.Point.BlkReadRate)
				add(&s.BlkWrite, v.Point.BlkWriteRate)
			}
		}
	}
	return s
}

// statsRoundWait is the longest a round waits for a member that is slow, when a stream is
// given no other: a little over a tick, so one node that answers late does not hold the
// totals of the others back for more than one more tick.
const statsRoundWait = 3 * time.Second
