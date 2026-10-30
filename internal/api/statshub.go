// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// Limits of the stats sampler (RESEARCH Pattern 7, A9).
const (
	// defaultStatsInterval is how often a watched container is sampled.
	defaultStatsInterval = 2 * time.Second
	// defaultStatsPerNode is the most sample requests in flight on one node, so a page
	// of fifty containers cannot take every stream of a node's agent.
	defaultStatsPerNode = 8
	// statsRequestTimeout bounds one sample; a node that is slower is not reporting.
	statsRequestTimeout = 10 * time.Second
	// statsSubscriberBuffer is how many updates a slow viewer may be behind before the
	// newest are dropped for it: a chart tolerates a gap, a sampler must not wait.
	statsSubscriberBuffer = 16
)

// StatsKind says what one StatsUpdate is.
type StatsKind string

const (
	// StatsKindSample carries a point.
	StatsKindSample StatsKind = "sample"
	// StatsKindNotReporting says the node of the container could not be asked.
	StatsKindNotReporting StatsKind = "not_reporting"
	// StatsKindStopped says the container is gone or is not running.
	StatsKindStopped StatsKind = "stopped"
)

// StatsUpdate is what a subscriber to a container's stats receives. The two states are
// sent when they begin, not on every tick that finds them still true; a subscriber that
// joins while one holds is told it first.
type StatsUpdate struct {
	Kind StatsKind
	// Point is the sample; it is set for StatsKindSample only.
	Point dockerx.StatsPoint
}

// statsNodes is what the hub needs of the aggregator: to run a call on one node's engine.
type statsNodes interface {
	OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
}

// StatsHubOptions tune a StatsHub. The zero value is the production setting.
type StatsHubOptions struct {
	// Interval is how often a container is sampled. Zero means two seconds.
	Interval time.Duration
	// PerNode is the most sample requests in flight on one node. Zero means eight.
	PerNode int
	// NewTicker makes the ticker of one sampler. Nil means time.NewTicker. A test passes
	// its own, so that no test waits for a clock.
	NewTicker func(d time.Duration) (ticks <-chan time.Time, stop func())
	// Log receives server-side failures, never a sample. Nil means slog.Default().
	Log *slog.Logger
}

// statsKey names one watched container.
type statsKey struct{ env, node, container string }

// statsNode names one node whose in-flight requests are capped.
type statsNode struct{ env, node string }

// statsSub is one viewer of one container.
type statsSub struct {
	ch     chan StatsUpdate
	closed bool
}

// statsSampler is the goroutine that samples one container for every viewer of it. Its
// fields are guarded by the hub's mutex.
type statsSampler struct {
	cancel context.CancelFunc
	subs   map[*statsSub]struct{}
	// state is the kind of the last update, so a state is sent once.
	state StatsKind
}

// StatsHub samples the containers that browsers are watching (D-29 to D-31). A container
// has one sampler however many viewers it has, so two people on one container cost one
// request per tick. A sampler lives from its first viewer to its last. Each sample is one
// one-shot stats request through the agent of the container's node, never repeated while
// the last is pending and capped per node. The hub keeps the last raw sample of each
// container it watches, in memory and nowhere else: nothing is stored (D-30).
type StatsHub struct {
	src       statsNodes
	interval  time.Duration
	perNode   int
	newTicker func(time.Duration) (<-chan time.Time, func())
	log       *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	samplers map[statsKey]*statsSampler
	slots    map[statsNode]chan struct{}

	// changed is closed and replaced whenever a sampler starts or stops. Its own mutex is a
	// leaf, so notify may be called with or without mu held.
	changeMu sync.Mutex
	changed  chan struct{}
}

// notify wakes every Await.
func (h *StatsHub) notify() {
	h.changeMu.Lock()
	defer h.changeMu.Unlock()
	if h.changed != nil {
		close(h.changed)
		h.changed = nil
	}
}

// Await returns nil once cond is true, and the context's error if ctx ends first. cond is
// evaluated now and again after every change to the set of samplers (one started, one
// stopped). It never polls, so the deadline in ctx only guards against a hang. cond may call
// Samplers but not Await.
func (h *StatsHub) Await(ctx context.Context, cond func() bool) error {
	for {
		// Taken before cond is evaluated, so a change in between is not missed.
		h.changeMu.Lock()
		if h.changed == nil {
			h.changed = make(chan struct{})
		}
		ch := h.changed
		h.changeMu.Unlock()
		if cond() {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// NewStatsHub builds a hub that samples through src.
func NewStatsHub(src statsNodes, o StatsHubOptions) *StatsHub {
	if o.Interval <= 0 {
		o.Interval = defaultStatsInterval
	}
	if o.PerNode <= 0 {
		o.PerNode = defaultStatsPerNode
	}
	if o.NewTicker == nil {
		o.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &StatsHub{
		src: src, interval: o.Interval, perNode: o.PerNode, newTicker: o.NewTicker, log: o.Log,
		ctx: ctx, cancel: cancel,
		samplers: map[statsKey]*statsSampler{}, slots: map[statsNode]chan struct{}{},
	}
}

// Subscribe starts watching a container, or joins the sampler that already does. The
// channel carries the updates; leaving is the returned function, which closes the channel
// and, for the last viewer, stops the sampler. It may be called more than once. A hub that
// is closed answers with a closed channel.
func (h *StatsHub) Subscribe(envID, nodeID, containerID string) (<-chan StatsUpdate, func()) {
	sub := &statsSub{ch: make(chan StatsUpdate, statsSubscriberBuffer)}
	key := statsKey{envID, nodeID, containerID}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		sub.closed = true
		close(sub.ch)
		return sub.ch, func() {}
	}
	s := h.samplers[key]
	if s == nil {
		ctx, cancel := context.WithCancel(h.ctx)
		s = &statsSampler{cancel: cancel, subs: map[*statsSub]struct{}{}}
		h.samplers[key] = s
		h.wg.Add(1)
		go h.run(ctx, key, s)
		defer h.notify()
	}
	s.subs[sub] = struct{}{}
	if s.state == StatsKindNotReporting || s.state == StatsKindStopped {
		sub.ch <- StatsUpdate{Kind: s.state}
	}
	h.mu.Unlock()

	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := s.subs[sub]; !ok {
				return // Close already ended it
			}
			delete(s.subs, sub)
			sub.closed = true
			close(sub.ch)
			if len(s.subs) == 0 {
				s.cancel()
				if h.samplers[key] == s {
					delete(h.samplers, key)
				}
				h.notify()
			}
		})
	}
}

// Ticker makes a ticker the way the samplers do. A stream that merges the samples of many
// containers uses it for its own clock, so a test that supplies the hub's ticks drives the
// merge and the refresh of the stream's tasks as well.
func (h *StatsHub) Ticker(d time.Duration) (<-chan time.Time, func()) { return h.newTicker(d) }

// Interval is how often a watched container is sampled.
func (h *StatsHub) Interval() time.Duration { return h.interval }

// Samplers is how many containers are being sampled now.
func (h *StatsHub) Samplers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.samplers)
}

// Close stops every sampler, closes every subscriber's channel and waits for the
// samplers to return. It may be called more than once.
func (h *StatsHub) Close() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		h.cancel()
		for key, s := range h.samplers {
			for sub := range s.subs {
				if !sub.closed {
					sub.closed = true
					close(sub.ch)
				}
				delete(s.subs, sub)
			}
			delete(h.samplers, key)
		}
		h.notify()
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// slot is the in-flight counter of a node: a channel with room for perNode requests.
func (h *StatsHub) slot(env, node string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := statsNode{env, node}
	c := h.slots[k]
	if c == nil {
		c = make(chan struct{}, h.perNode)
		h.slots[k] = c
	}
	return c
}

// run is one sampler: a sample at once, then one on every tick. It reads one container
// at a time, so a sample that is still pending is never asked for again; a tick that
// came while it was busy waits (one at most) and is served when the request is done.
func (h *StatsHub) run(ctx context.Context, key statsKey, s *statsSampler) {
	defer h.wg.Done()
	ticks, stop := h.newTicker(h.interval)
	defer stop()
	var prev *dockerx.StatsSample
	for {
		h.sample(ctx, key, s, &prev)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// sample takes one reading and tells the viewers what it made.
func (h *StatsHub) sample(ctx context.Context, key statsKey, s *statsSampler, prev **dockerx.StatsSample) {
	slot := h.slot(key.env, key.node)
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-slot }()

	callCtx, cancel := context.WithTimeout(ctx, statsRequestTimeout)
	defer cancel()
	var cur dockerx.StatsSample
	err := h.src.OnNode(callCtx, key.env, key.node, func(c *dockerx.Client) error {
		var e error
		cur, e = c.ContainerStatsOnce(callCtx, key.container)
		return e
	})
	if ctx.Err() != nil {
		return // the last viewer left or the hub closed
	}
	switch {
	case err == nil && !cur.Read.IsZero():
		point := dockerx.DeriveStats(*prev, cur)
		*prev = &cur
		h.publish(s, StatsUpdate{Kind: StatsKindSample, Point: point})
	case err == nil || dockerx.IsNotFound(err):
		// The engine answers a container that is not running with an empty document. Its
		// counters start again when it does, so the next reading has nothing to compare.
		*prev = nil
		h.publish(s, StatsUpdate{Kind: StatsKindStopped})
	default:
		if h.publish(s, StatsUpdate{Kind: StatsKindNotReporting}) {
			// The failure and the node, never a sample.
			h.log.Warn("a container's stats could not be read", "environment", key.env, "node", key.node, "container", key.container, "error", err)
		}
	}
}

// publish gives an update to every viewer without waiting for any of them. A state is
// sent once, when it begins; it reports whether this one was sent.
func (h *StatsHub) publish(s *statsSampler, u StatsUpdate) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u.Kind != StatsKindSample && s.state == u.Kind {
		return false
	}
	s.state = u.Kind
	for sub := range s.subs {
		select {
		case sub.ch <- u:
		default: // a viewer too far behind loses this one
		}
	}
	return true
}
