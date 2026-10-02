// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// Sizes and timings of the events feed.
const (
	// ringSize is how many events the hub keeps per environment, newest last.
	// Events live in memory only: they are never written to the database
	// (D-57, Pattern 8).
	ringSize = 200
	// seenSize is how many event IDs the hub remembers to drop an event the
	// daemon sends twice. It is larger than the ring, so an event that has just
	// left the ring is still recognised when a resume replays it.
	seenSize = 1024
	// subscriberBuffer is how far a subscriber may fall behind before it is
	// dropped instead of slowing the others down.
	subscriberBuffer = 64
	// backfill is how far back the first stream of an environment looks.
	backfill = time.Hour

	defaultPoll     = time.Second
	defaultRetryMin = 500 * time.Millisecond
	defaultRetryMax = 15 * time.Second
)

// errNotOnline ends a stream because the monitor no longer calls the
// environment online.
var errNotOnline = errors.New("envs: the environment is not online")

// Hub holds one Docker events stream per environment and fans it out to every
// browser watching. It keeps the last 200 events of each environment in memory,
// fills them from the daemon's own buffer when it starts (best effort: Docker
// keeps at most 256 events and none across a daemon restart, Pitfall 7), resumes
// after a dropped stream from the last event it saw, and tells the snapshot
// cache about every event so the counts and lists refresh promptly (D-57).
//
// A hub is told which environments exist by the monitor it is built over: it
// starts a stream when an environment starts being monitored and stops it when
// monitoring ends. While an environment is not online, no stream is held. It is
// safe for concurrent use.
type Hub struct {
	mon   *Monitor
	snaps *Snapshots
	log   *slog.Logger
	now   func() time.Time

	// poll is how often a feed looks at the monitor's status, and retryMin and
	// retryMax bound the wait before it reopens a stream that ended. They are
	// fields so tests can shorten them.
	poll, retryMin, retryMax time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	feeds  map[string]*feed
	closed bool
}

// feed is the state of one environment's stream.
type feed struct {
	env    string
	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// ring is the buffered events, oldest first.
	ring []dockerx.EventRow
	// seen remembers recent event IDs; seenOrder is the same IDs oldest first,
	// used as a circular queue.
	seen      map[string]struct{}
	seenOrder []string
	seenNext  int
	// last is the time of the newest event received; a resume starts there.
	last     int64
	haveLast bool
	subs     map[*subscriber]struct{}
	closed   bool
}

// subscriber is one browser's channel.
type subscriber struct {
	ch chan dockerx.EventRow
}

// NewHub returns a hub over m. snaps may be nil, in which case no cache is
// invalidated. now defaults to time.Now; it picks the start of the first
// backfill. The hub starts a feed for each environment m starts and stops it
// when m stops it.
func NewHub(m *Monitor, snaps *Snapshots, log *slog.Logger, now func() time.Time) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		mon: m, snaps: snaps, log: log, now: now,
		poll: defaultPoll, retryMin: defaultRetryMin, retryMax: defaultRetryMax,
		ctx: ctx, cancel: cancel,
		feeds: map[string]*feed{},
	}
	m.watch(h.Start, h.Stop)
	return h
}

// Start begins the stream of envID. It does nothing when one already exists, so
// there is exactly one per environment however often it is called, and nothing
// once the hub is closed. The monitor calls it; callers need not.
func (h *Hub) Start(envID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if _, ok := h.feeds[envID]; ok {
		return
	}
	ctx, cancel := context.WithCancel(h.ctx)
	f := &feed{
		env: envID, cancel: cancel, done: make(chan struct{}),
		seen: map[string]struct{}{}, subs: map[*subscriber]struct{}{},
	}
	h.feeds[envID] = f
	h.wg.Go(func() {
		defer close(f.done)
		h.run(ctx, f)
	})
}

// Stop ends the stream of envID, closes its subscribers and forgets its
// events. The monitor calls it when an environment is removed.
func (h *Hub) Stop(envID string) {
	h.mu.Lock()
	f := h.feeds[envID]
	delete(h.feeds, envID)
	h.mu.Unlock()
	if f == nil {
		return
	}
	f.cancel()
	<-f.done
	f.closeSubscribers()
}

// Close ends every stream and closes every subscriber. A subscriber that
// arrives afterwards gets a closed channel. The server calls it at shutdown so
// open event streams do not hold the graceful shutdown up. It is safe to call
// twice.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	feeds := make([]*feed, 0, len(h.feeds))
	for id, f := range h.feeds {
		feeds = append(feeds, f)
		delete(h.feeds, id)
	}
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	for _, f := range feeds {
		f.closeSubscribers()
	}
}

// Subscribe returns the buffered events of envID, oldest first, and a channel
// of the events that follow, with no gap and no overlap between the two. The
// channel is closed when the environment is removed, the hub closes, the
// subscriber falls more than 64 events behind, the context ends, or cancel is
// called (it may be called more than once). For an environment the hub does not
// know the backlog is empty and the channel already closed.
func (h *Hub) Subscribe(ctx context.Context, envID string) (backlog []dockerx.EventRow, ch <-chan dockerx.EventRow, cancel func()) {
	h.mu.Lock()
	f := h.feeds[envID]
	h.mu.Unlock()
	if f == nil {
		closed := make(chan dockerx.EventRow)
		close(closed)
		return nil, closed, func() {}
	}
	backlog, ch, unsub := f.subscribe()
	stop := context.AfterFunc(ctx, unsub)
	return backlog, ch, func() {
		stop()
		unsub()
	}
}

// Recent returns up to n of the newest buffered events of envID, newest first.
func (h *Hub) Recent(envID string, n int) []dockerx.EventRow {
	h.mu.Lock()
	f := h.feeds[envID]
	h.mu.Unlock()
	if f == nil || n <= 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n = min(n, len(f.ring))
	out := make([]dockerx.EventRow, 0, n)
	for i := len(f.ring) - 1; i >= len(f.ring)-n; i-- {
		out = append(out, f.ring[i])
	}
	return out
}

// run is the one loop of an environment: while the monitor says online, hold a
// stream; when it ends, wait and reopen from where it stopped.
func (h *Hub) run(ctx context.Context, f *feed) {
	var since time.Time
	delay := h.retryMin
	failing := false
	for ctx.Err() == nil {
		cli, _, err := h.mon.Client(f.env)
		if err != nil {
			// Offline, unsupported or not probed yet: no stream is held.
			failing = false
			delay = h.retryMin
			h.pause(ctx, h.poll)
			continue
		}
		start := since
		if n, ok := f.lastSeen(); ok {
			start = time.Unix(0, n).UTC()
		} else if since.IsZero() {
			// The first stream of the environment. Unix seconds are what the
			// daemon wants for a plain backfill.
			since = h.now().Add(-backfill).Truncate(time.Second)
			start = since
		}

		got, err := h.stream(ctx, f, cli, start)
		if ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, errNotOnline):
			failing = false
		case !failing:
			failing = true
			h.log.Warn("the events stream of an environment ended; reopening", "environment", f.env, "error", err)
		default:
			h.log.Debug("the events stream of an environment ended again", "environment", f.env, "error", err)
		}
		if got {
			delay = h.retryMin
			failing = false
		}
		h.pause(ctx, delay)
		delay = min(delay*2, h.retryMax)
	}
}

// pause waits for d or until ctx ends.
func (h *Hub) pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// stream holds one daemon stream until it ends, the environment stops being
// online, or ctx ends. got reports whether it delivered any event.
func (h *Hub) stream(ctx context.Context, f *feed, cli *dockerx.Client, since time.Time) (got bool, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows, errc := cli.Events(sctx, since)
	// Whatever way this returns, wait for the stream's goroutine to finish, so
	// none outlives the hub.
	defer func() {
		cancel()
		for range rows {
			// draining, so the stream's goroutine can finish
		}
	}()

	tick := time.NewTicker(h.poll)
	defer tick.Stop()
	for {
		select {
		case row, ok := <-rows:
			if !ok {
				return got, <-errc
			}
			got = true
			f.ingest(h.snaps, row)
		case <-tick.C:
			if h.mon.Status(f.env).State != StateOnline {
				return got, errNotOnline
			}
		case <-ctx.Done():
			return got, ctx.Err()
		}
	}
}

func (f *feed) lastSeen() (nano int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.haveLast
}

// ingest records one event, unless it was already seen, drops the cached
// snapshot of the environment, and hands the event to every subscriber. All of
// it happens under one lock, so a subscriber that arrives meanwhile gets the
// event either in its backlog or on its channel, never both and never neither.
// The snapshot is invalidated before the event is delivered, so a browser that
// refetches on seeing it reads fresh data.
func (f *feed) ingest(snaps *Snapshots, ev dockerx.EventRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.seen[ev.ID]; dup {
		return
	}
	f.remember(ev.ID)
	if !f.haveLast || ev.TimeNano > f.last {
		f.last, f.haveLast = ev.TimeNano, true
	}
	f.ring = append(f.ring, ev)
	if len(f.ring) > ringSize {
		n := copy(f.ring, f.ring[len(f.ring)-ringSize:])
		clear(f.ring[n:])
		f.ring = f.ring[:n]
	}
	if snaps != nil {
		snaps.Invalidate(f.env)
	}
	for s := range f.subs {
		select {
		case s.ch <- ev:
		default:
			// Too far behind. Closing its channel tells the stream handler to
			// end, and the browser reconnects with Last-Event-ID.
			close(s.ch)
			delete(f.subs, s)
		}
	}
}

// remember adds id to the bounded seen set, forgetting the oldest when full.
// The caller holds f.mu.
func (f *feed) remember(id string) {
	if len(f.seenOrder) < seenSize {
		f.seenOrder = append(f.seenOrder, id)
	} else {
		delete(f.seen, f.seenOrder[f.seenNext])
		f.seenOrder[f.seenNext] = id
		f.seenNext = (f.seenNext + 1) % seenSize
	}
	f.seen[id] = struct{}{}
}

func (f *feed) subscribe() (backlog []dockerx.EventRow, ch <-chan dockerx.EventRow, unsub func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &subscriber{ch: make(chan dockerx.EventRow, subscriberBuffer)}
	if f.closed {
		close(s.ch)
		return nil, s.ch, func() {}
	}
	backlog = append([]dockerx.EventRow(nil), f.ring...)
	f.subs[s] = struct{}{}
	return backlog, s.ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[s]; ok {
			delete(f.subs, s)
			close(s.ch)
		}
	}
}

// closeSubscribers closes every subscriber channel and refuses new ones.
func (f *feed) closeSubscribers() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for s := range f.subs {
		close(s.ch)
		delete(f.subs, s)
	}
}
