// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
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

// Inject adds an event of Bayplan's own to the feed of a target, so it shows with
// the Docker events of the environment that target serves. It reports false when
// the target has no feed, which is when it is not being monitored. The event keeps
// its ID, so injecting the same one twice shows it once.
func (h *Hub) Inject(key string, ev dockerx.EventRow) bool {
	h.mu.Lock()
	f := h.feeds[key]
	h.mu.Unlock()
	if f == nil {
		return false
	}
	f.inject(ev)
	return true
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
		cli, st, err := h.mon.Client(f.env)
		if err != nil {
			// Offline, unsupported or not probed yet: no stream is held.
			failing = false
			delay = h.retryMin
			if st.State == StateUnknown {
				// Just added: wait for the first probe instead of for the next
				// poll, so the feed starts as soon as the engine answers.
				h.mon.AwaitProbe(ctx, f.env)
				if _, st, _ = h.mon.Client(f.env); st.State != StateUnknown {
					continue
				}
			}
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
//
// An invalidate-only row (a health_status change) does all of that except enter
// the ring: it is remembered, so a replay does not repeat it, and delivered, so
// every open list refreshes, but Recent and a new subscriber's backlog never see
// it.
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
	if !ev.InvalidateOnly {
		f.addLocked(ev)
	}
	if snaps != nil {
		snaps.Invalidate(f.env)
	}
	f.fanOutLocked(ev)
}

// inject records an event of Bayplan's own. Unlike ingest it leaves the resume
// point alone, which is the time of the newest event the daemon sent: moving it
// would make a reopened stream skip what the daemon said in between. The caller
// does not hold f.mu.
func (f *feed) inject(ev dockerx.EventRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.seen[ev.ID]; dup {
		return
	}
	f.remember(ev.ID)
	f.addLocked(ev)
	f.fanOutLocked(ev)
}

// addLocked puts ev in the ring, dropping the oldest when it is full. The caller
// holds f.mu.
func (f *feed) addLocked(ev dockerx.EventRow) {
	f.ring = append(f.ring, ev)
	if len(f.ring) > ringSize {
		n := copy(f.ring, f.ring[len(f.ring)-ringSize:])
		clear(f.ring[n:])
		f.ring = f.ring[:n]
	}
}

// fanOutLocked hands ev to every subscriber. The caller holds f.mu.
func (f *feed) fanOutLocked(ev dockerx.EventRow) {
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

// Timing of the merged feed of an environment.
const (
	// defaultFeedPoll is how often a merged feed looks for targets that came or went
	// and for a change of the manager in use.
	defaultFeedPoll = time.Second
	// feedIdleAfter is how long a merged feed that nobody asks for is kept.
	feedIdleAfter = 5 * time.Minute
	// swarmDupWindow is how far apart two managers' clocks may stamp one cluster event
	// for it to still be recognised as the same event.
	swarmDupWindow = 2 * time.Second
)

// NodeEvent is a row of an environment's feed with the node it came from: the node
// the Docker event happened on, or for a row of Bayplan's own the node it concerns.
type NodeEvent struct {
	dockerx.EventRow
	Node string
	// from is the target the row was read from, to tell one manager's report of a
	// cluster event from another's.
	from string
}

// feedTarget is one target a merged feed reads and the name its rows carry.
type feedTarget struct{ key, node string }

// mergedFeed is the one live feed of an environment: the rows of every present
// target, each with its node, in the order they arrived. It holds the ring that
// Recent and a Last-Event-ID replay read, so they agree with the stream.
//
// A cluster event, one with scope swarm, is reported by every manager, each with
// its own timestamp. The feed keeps it from the designated manager only, the one
// serving the environment's cluster reads, so it appears once (Pitfall 18). When the
// designated manager changes, the new one's ring is read for the cluster events it
// saw while it was not designated, so none is lost, and what the old one already
// showed is recognised and not shown again.
type mergedFeed struct {
	agg    *Aggregator
	env    string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wg     sync.WaitGroup

	mu        sync.Mutex
	ring      []NodeEvent
	seen      map[string]struct{}
	seenOrder []string
	seenNext  int
	subs      map[*mergedSub]struct{}
	closed    bool
	// attached are the targets being read, by key.
	attached map[string]*attachment
	// present is whether any target could be read at the last look.
	present bool
	// designated is the target whose cluster events are kept; swarmHigh is the newest
	// cluster event shown.
	designated string
	swarmHigh  int64
	// lastUse is when a caller last asked for the feed.
	lastUse time.Time
}

// attachment is the subscription to one target's feed.
type attachment struct{ cancel func() }

// mergedSub is one subscriber's channel.
type mergedSub struct{ ch chan NodeEvent }

func (a *Aggregator) newFeed(env string) *mergedFeed {
	ctx, cancel := context.WithCancel(a.ctx)
	return &mergedFeed{
		agg: a, env: env, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		seen: map[string]struct{}{}, subs: map[*mergedSub]struct{}{}, attached: map[string]*attachment{},
		lastUse: time.Now(),
	}
}

// feedFor is the merged feed of env, started if it is new. It is nil for an
// environment that does not exist, is closed over, or has nothing to read now, which
// is what an offline environment gets: no events, rather than a stale ring.
func (a *Aggregator) feedFor(ctx context.Context, env string) *mergedFeed {
	a.fmu.Lock()
	defer a.fmu.Unlock()
	if a.closed {
		return nil
	}
	f := a.feeds[env]
	if f == nil {
		f = a.newFeed(env)
		if err := f.refresh(ctx); err != nil {
			f.cancel()
			f.wg.Wait()
			return nil
		}
		a.feeds[env] = f
		go f.run()
	}
	f.mu.Lock()
	f.lastUse = time.Now()
	present := f.present
	f.mu.Unlock()
	if !present {
		return nil
	}
	return f
}

// drop forgets f, which has ended.
func (a *Aggregator) drop(f *mergedFeed) {
	a.fmu.Lock()
	if a.feeds[f.env] == f {
		delete(a.feeds, f.env)
	}
	a.fmu.Unlock()
}

// Close ends every merged feed and closes its subscribers, so open event streams do
// not hold the server's shutdown up. A subscriber that arrives afterwards gets a
// closed channel. It is safe to call twice.
func (a *Aggregator) Close() {
	a.fmu.Lock()
	a.closed = true
	feeds := make([]*mergedFeed, 0, len(a.feeds))
	for id, f := range a.feeds {
		feeds = append(feeds, f)
		delete(a.feeds, id)
	}
	a.fmu.Unlock()
	a.cancel()
	for _, f := range feeds {
		f.cancel()
		<-f.done
	}
}

// WithEventPoll sets how often an environment's merged feed looks for targets that
// came or went; the default is one second. It returns a for chaining.
func (a *Aggregator) WithEventPoll(d time.Duration) *Aggregator {
	a.feedPoll = d
	return a
}

// Events subscribes to the merged feed of an environment: the buffered rows, oldest
// first, and a channel of the ones that follow, with no gap and no overlap between
// the two. The channel is closed when the feed ends, the aggregator closes, the
// subscriber falls more than 64 events behind, the context ends or cancel is called.
// An environment with nothing to read gets an empty backlog and a channel that is
// already closed.
func (a *Aggregator) Events(ctx context.Context, envID string) (backlog []NodeEvent, ch <-chan NodeEvent, cancel func()) {
	f := a.feedFor(ctx, envID)
	if f == nil {
		closed := make(chan NodeEvent)
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

// Recent returns up to n of the newest buffered rows of an environment's merged
// feed, newest first.
func (a *Aggregator) Recent(ctx context.Context, envID string, n int) []NodeEvent {
	f := a.feedFor(ctx, envID)
	if f == nil || n <= 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n = min(n, len(f.ring))
	out := make([]NodeEvent, 0, n)
	for i := len(f.ring) - 1; i >= len(f.ring)-n; i-- {
		out = append(out, f.ring[i])
	}
	return out
}

// run keeps the feed attached to the environment's targets until the aggregator or
// the events hub closes, the environment is removed, or nobody has asked for it for
// a while.
func (f *mergedFeed) run() {
	defer func() {
		f.cancel()
		f.wg.Wait()
		f.closeSubscribers()
		f.agg.drop(f)
		close(f.done)
	}()
	poll := f.agg.feedPoll
	if poll <= 0 {
		poll = defaultFeedPoll
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-f.agg.events.ctx.Done():
			// The hub is closing: its streams end, and so does this one.
			return
		case <-t.C:
			if err := f.refresh(f.ctx); err != nil {
				if f.ctx.Err() == nil && !errors.Is(err, ErrNotFound) {
					f.agg.svc.log.Debug("refresh the merged event feed", "environment", f.env, "error", err)
					continue
				}
				return
			}
			f.mu.Lock()
			idle := len(f.subs) == 0 && time.Since(f.lastUse) > feedIdleAfter
			f.mu.Unlock()
			if idle {
				return
			}
		}
	}
}

// targets are the targets an environment's events are read from, with their node
// names: the local socket, or each connected agent.
func (f *mergedFeed) targets(in RollupInput) []feedTarget {
	if in.Kind == KindLocal {
		return []feedTarget{{in.EnvID, nodeOf(in, in.EnvID, in.Local).Name}}
	}
	var out []feedTarget
	for _, a := range connectedAgents(in) {
		out = append(out, feedTarget{a.TargetKey(), nodeName(a)})
	}
	return out
}

// refresh attaches the targets that have appeared, forgets the ones that are gone
// and follows the manager in use. It returns ErrNotFound for an environment that no
// longer exists.
func (f *mergedFeed) refresh(ctx context.Context) error {
	in, err := f.agg.svc.Input(ctx, f.env, f.agg.now())
	if err != nil {
		return err
	}
	targets := f.targets(in)
	inUse, _ := f.agg.svc.primary(in)

	type fresh struct {
		t       feedTarget
		att     *attachment
		backlog []dockerx.EventRow
		ch      <-chan dockerx.EventRow
	}
	var added []fresh
	want := make(map[string]bool, len(targets))
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return context.Canceled
	}
	for _, t := range targets {
		want[t.key] = true
		f.mu.Lock()
		_, have := f.attached[t.key]
		f.mu.Unlock()
		if have {
			continue
		}
		backlog, ch, cancel := f.agg.events.Subscribe(f.ctx, t.key)
		added = append(added, fresh{t: t, att: &attachment{cancel: cancel}, backlog: backlog, ch: ch})
	}

	f.mu.Lock()
	for key, att := range f.attached {
		if !want[key] {
			att.cancel()
			delete(f.attached, key)
		}
	}
	prev := f.designated
	f.designated = inUse
	f.present = len(targets) > 0
	for _, a := range added {
		f.attached[a.t.key] = a.att
	}
	f.mu.Unlock()

	// What the new targets already hold goes in as one stretch in time order, so the
	// ring reads as one history however many targets joined at once.
	var pending []NodeEvent
	for _, a := range added {
		for _, row := range a.backlog {
			pending = append(pending, NodeEvent{EventRow: row, Node: a.t.node, from: a.t.key})
		}
	}
	slices.SortStableFunc(pending, func(x, y NodeEvent) int { return cmp.Compare(x.TimeNano, y.TimeNano) })
	for _, ev := range pending {
		f.ingest(ev)
	}
	for _, a := range added {
		f.wg.Go(func() { f.pump(a.t, a.att, a.ch) })
	}
	if prev != "" && inUse != prev {
		f.catchUp(inUse, targets)
	}
	return nil
}

// pump feeds one target's rows into the merged feed until its subscription ends,
// then lets the next refresh attach it again.
func (f *mergedFeed) pump(t feedTarget, att *attachment, ch <-chan dockerx.EventRow) {
	for row := range ch {
		f.ingest(NodeEvent{EventRow: row, Node: t.node, from: t.key})
	}
	att.cancel()
	f.mu.Lock()
	if f.attached[t.key] == att {
		delete(f.attached, t.key)
	}
	f.mu.Unlock()
}

// catchUp reads the cluster events the manager that is now designated saw while it
// was not: they were not kept then, and its ring still holds them.
func (f *mergedFeed) catchUp(key string, targets []feedTarget) {
	var node string
	for _, t := range targets {
		if t.key == key {
			node = t.node
		}
	}
	rows := f.agg.events.Recent(key, ringSize) // newest first
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Scope == swarmScope {
			f.ingest(NodeEvent{EventRow: rows[i], Node: node, from: key})
		}
	}
}

// ingest adds one row unless it is a repeat or a cluster event this manager is not
// the one to report, and hands it to every subscriber.
func (f *mergedFeed) ingest(ev NodeEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	if ev.Scope == swarmScope && !f.keepSwarmLocked(ev) {
		return
	}
	if _, dup := f.seen[ev.ID]; dup {
		return
	}
	f.remember(ev.ID)
	if !ev.InvalidateOnly {
		f.ring = append(f.ring, ev)
		if len(f.ring) > ringSize {
			n := copy(f.ring, f.ring[len(f.ring)-ringSize:])
			clear(f.ring[n:])
			f.ring = f.ring[:n]
		}
	}
	for s := range f.subs {
		select {
		case s.ch <- ev:
		default:
			// Too far behind: the stream handler ends, and the browser reconnects
			// with Last-Event-ID.
			close(s.ch)
			delete(f.subs, s)
		}
	}
}

// keepSwarmLocked decides whether a cluster event is shown: only from the designated
// manager, not when it is older than what was already shown, and not when another
// manager's report of the same event is already in the ring. The caller holds f.mu.
func (f *mergedFeed) keepSwarmLocked(ev NodeEvent) bool {
	if ev.from != f.designated {
		return false
	}
	window := int64(swarmDupWindow)
	if ev.TimeNano < f.swarmHigh-window {
		return false
	}
	for i := len(f.ring) - 1; i >= 0; i-- {
		r := f.ring[i]
		if r.Scope != swarmScope || r.from == ev.from || r.Type != ev.Type || r.Action != ev.Action || r.ActorID != ev.ActorID {
			continue
		}
		if d := r.TimeNano - ev.TimeNano; d <= window && d >= -window {
			return false
		}
	}
	f.swarmHigh = max(f.swarmHigh, ev.TimeNano)
	return true
}

// remember adds id to the bounded seen set, forgetting the oldest when full. The
// caller holds f.mu.
func (f *mergedFeed) remember(id string) {
	if len(f.seenOrder) < seenSize {
		f.seenOrder = append(f.seenOrder, id)
	} else {
		delete(f.seen, f.seenOrder[f.seenNext])
		f.seenOrder[f.seenNext] = id
		f.seenNext = (f.seenNext + 1) % seenSize
	}
	f.seen[id] = struct{}{}
}

func (f *mergedFeed) subscribe() (backlog []NodeEvent, ch <-chan NodeEvent, unsub func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &mergedSub{ch: make(chan NodeEvent, subscriberBuffer)}
	if f.closed {
		close(s.ch)
		return nil, s.ch, func() {}
	}
	backlog = append([]NodeEvent(nil), f.ring...)
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
func (f *mergedFeed) closeSubscribers() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for s := range f.subs {
		close(s.ch)
		delete(f.subs, s)
	}
}
