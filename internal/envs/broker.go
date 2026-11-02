// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/tunnel"
)

// Types of the events on the health stream (D-36, D-37). proxy.warning belongs to
// no environment (D-53).
const (
	EventEnvStatus         = "env.status"
	EventAgentConnected    = "agent.connected"
	EventAgentDisconnected = "agent.disconnected"
	EventAgentRefused      = "agent.refused"
	EventFailover          = "failover"
	EventProxyWarning      = "proxy.warning"
)

// Sizes and timings of the broker.
const (
	// brokerRing is how many events the broker keeps for a reconnecting tab.
	brokerRing = 1024
	// brokerTick is how often every environment's health is recomputed even when
	// nothing was announced: the 30 s presence window and the 60 s grace end with no
	// event from the hub.
	brokerTick = 5 * time.Second
	// healthSubscriberBuffer is how far one subscriber may fall behind before it is
	// dropped instead of slowing the others down. The browser reconnects with
	// Last-Event-ID and the ring fills the gap.
	healthSubscriberBuffer = 128
	// refusalEvery is how often one address may make one environment publish a
	// refusal. A guesser who knows an environment ID must not fill the stream.
	refusalEvery = 10 * time.Second
	// maxRefusalKeys bounds the memory of the throttle.
	maxRefusalKeys = 1024
	// knownTimeout bounds the lookup that tells a refusal's environment exists.
	knownTimeout = 2 * time.Second
	// maxEventText caps every piece of text an agent supplied.
	maxEventText = 64
)

// HealthEvent is one event of the health stream. ID is assigned by Publish, from 1,
// and strictly increases. Data is an Environment for env.status and one of the
// Agent*Data types for the agent events.
type HealthEvent struct {
	ID    uint64
	Type  string
	EnvID string
	At    time.Time
	Data  any
}

// AgentConnectedData is the data of an agent.connected event.
type AgentConnectedData struct {
	AgentID string
	// Node is the host name the agent reported, cleaned and cut to 64 characters.
	Node string
	// Type is the node's role: manager, worker or standalone.
	Type    string
	Version string
}

// AgentDisconnectedData is the data of an agent.disconnected event.
type AgentDisconnectedData struct {
	AgentID, Node string
	// Reason is a tunnel Disconnect code: closed, timeout, replaced or shutdown.
	Reason string
}

// AgentRefusedData is the data of an agent.refused event.
type AgentRefusedData struct {
	// Code is the refusal code the agent was given, for example auth_rejected.
	Code string
	// Mode is enrol or hello as the agent sent it, cleaned and cut.
	Mode string
	// Node is the host name the agent reported, cleaned and cut.
	Node string
	// IP is the address the attempt came from.
	IP string
}

// FailoverData is the data of a failover event: the manager that stopped answering
// and the one that took over.
type FailoverData struct {
	FromAgentID, FromNode string
	ToAgentID, ToNode     string
}

// ProxyWarningData is the data of a proxy.warning event: a kind of reverse proxy
// problem (the httpx.Signal kinds) that has just appeared. The event belongs to no
// environment; the stream shows it to whoever holds settings.read.
type ProxyWarningData struct {
	Kind string
}

// PublishProxyWarning announces that a kind of proxy problem has appeared, so an
// open bell reads the warnings again at once. kind is cleaned and cut like any
// other text on the stream.
func (b *Broker) PublishProxyWarning(kind string) {
	b.Publish(HealthEvent{Type: EventProxyWarning, Data: ProxyWarningData{Kind: cleanText(kind, maxEventText)}})
}

// healthSub is one subscriber's channel.
type healthSub struct{ ch chan HealthEvent }

// Broker is the health broker: it turns what the tunnel hub reports and what the
// rollups of every environment say into one ordered stream of events with a ring
// of the newest 1024 for a tab that reconnects (D-36, D-37). It detects change by
// recomputing every environment's health when the hub announces something and on a
// 5 s tick, and publishes env.status only when an environment's state, reason or
// issues changed. It is safe for concurrent use.
type Broker struct {
	svc   *Service
	now   func() time.Time
	every time.Duration
	log   *slog.Logger

	// pass makes one recompute at a time, so the order of a pass and the events it
	// publishes is the order it saw.
	pass sync.Mutex

	mu     sync.Mutex
	seq    uint64
	ring   []HealthEvent
	subs   map[*healthSub]struct{}
	closed bool
	// last is what was published for each environment, as healthKey; seeded is true
	// once a full pass has learned the environments there are at the start.
	last   map[string]string
	seeded bool
	// pending are the environments the hub announced something about since the last
	// recompute of them.
	pending  map[string]struct{}
	refusals map[string]time.Time

	wake chan struct{}
	done chan struct{}
}

// NewBroker returns a broker over svc. now defaults to time.Now; it stamps events
// and spaces the refusal throttle. With a nil svc it still publishes and replays
// events, and recomputes nothing.
func NewBroker(svc *Service, now func() time.Time) *Broker {
	if now == nil {
		now = time.Now
	}
	return &Broker{
		svc: svc, now: now, every: brokerTick, log: slog.Default(),
		subs: map[*healthSub]struct{}{}, last: map[string]string{},
		pending: map[string]struct{}{}, refusals: map[string]time.Time{},
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

// WithInterval sets how often Run recomputes without being told to; a value of
// zero or less keeps the default of 5 s. Call it before Run.
func (b *Broker) WithInterval(d time.Duration) *Broker {
	if d > 0 {
		b.every = d
	}
	return b
}

// Publish adds an event to the stream: it gets the next ID, and the current time
// unless it has one, goes into the ring and is sent to every subscriber. A
// subscriber that has fallen a full buffer behind is dropped, not waited for. It
// returns the event as stored.
func (b *Broker) Publish(ev HealthEvent) HealthEvent {
	if ev.At.IsZero() {
		ev.At = b.now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev.ID = b.seq
	b.ring = append(b.ring, ev)
	if len(b.ring) >= 2*brokerRing {
		// Trimmed in bulk, so a publish costs a copy once per brokerRing events.
		b.ring = append([]HealthEvent(nil), b.ring[len(b.ring)-brokerRing:]...)
	}
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			delete(b.subs, s)
			close(s.ch)
		}
	}
	return ev
}

// Subscribe registers a subscriber. backlog is the whole ring, oldest first, taken
// in the same step as the registration, so no event is missed or repeated between
// it and the channel. The channel is closed when the subscriber falls behind, when
// the broker closes, and when cancel is called or ctx ends. A subscriber that wants
// only what is newer than an ID it saw trims the backlog with EventsAfter.
func (b *Broker) Subscribe(ctx context.Context) (backlog []HealthEvent, ch <-chan HealthEvent, cancel func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		closed := make(chan HealthEvent)
		close(closed)
		return nil, closed, func() {}
	}
	view := b.ring
	if len(view) > brokerRing {
		view = view[len(view)-brokerRing:]
	}
	backlog = slices.Clone(view)
	s := &healthSub{ch: make(chan HealthEvent, healthSubscriberBuffer)}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	end := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.subs[s]; ok {
				delete(b.subs, s)
				close(s.ch)
			}
		})
	}
	stop := context.AfterFunc(ctx, end)
	return backlog, s.ch, func() {
		stop()
		end()
	}
}

// EventsAfter is the part of backlog a client that last saw the event lastID has
// not seen. An ID the backlog does not hold, whether it is older than the ring,
// newer than anything published (the server restarted) or not a number, gives the
// whole backlog: a repeat is harmless, a gap is not.
func EventsAfter(backlog []HealthEvent, lastID string) []HealthEvent {
	id, err := strconv.ParseUint(lastID, 10, 64)
	if err != nil {
		return backlog
	}
	for i := range backlog {
		if backlog[i].ID == id {
			return backlog[i+1:]
		}
	}
	return backlog
}

// SnapshotAll is every environment with its health as of now, for the first frame
// of a stream. The caller filters it by what the user may read.
func (b *Broker) SnapshotAll(ctx context.Context) ([]Environment, error) {
	if b.svc == nil {
		return nil, nil
	}
	return b.svc.List(ctx)
}

// Notify asks for the health of one environment to be recomputed now; Run does it
// at once. It never blocks, and repeated calls before Run gets to it are one.
func (b *Broker) Notify(envID string) {
	b.mu.Lock()
	b.pending[envID] = struct{}{}
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Recompute looks at every environment once and publishes env.status for each
// whose state, reason or issues are not what was last published. The first call
// only learns what is there, so a server start announces nothing; an environment
// that appears later is published once.
func (b *Broker) Recompute(ctx context.Context) {
	if b.svc == nil {
		return
	}
	b.pass.Lock()
	defer b.pass.Unlock()
	list, err := b.svc.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("recompute environment health", "error", err)
		}
		return
	}
	b.observe(list)
	keep := make(map[string]struct{}, len(list))
	for _, e := range list {
		keep[e.ID] = struct{}{}
	}
	b.mu.Lock()
	for id := range b.last {
		if _, ok := keep[id]; !ok {
			delete(b.last, id)
		}
	}
	b.seeded = true
	b.mu.Unlock()
}

// recomputeOne is Recompute for the environments the hub announced.
func (b *Broker) recomputeOne(ctx context.Context, envID string) {
	b.pass.Lock()
	defer b.pass.Unlock()
	e, err := b.svc.Get(ctx, envID)
	if errors.Is(err, ErrNotFound) {
		b.mu.Lock()
		delete(b.last, envID)
		b.mu.Unlock()
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("recompute environment health", "environment", envID, "error", err)
		}
		return
	}
	b.observe([]Environment{e})
}

// observe publishes env.status for each environment whose health key changed. The
// caller holds b.pass.
func (b *Broker) observe(list []Environment) {
	for _, e := range list {
		key := healthKey(e.Health)
		b.mu.Lock()
		prev, known := b.last[e.ID]
		b.last[e.ID] = key
		seeded := b.seeded
		b.mu.Unlock()
		if known && prev == key || !known && !seeded {
			continue
		}
		b.Publish(HealthEvent{Type: EventEnvStatus, EnvID: e.ID, Data: e})
	}
}

// healthKey is what makes two healths the same for the stream: state, reason and
// the issues' codes and nodes. Times are left out: they say when, not what.
func healthKey(h Health) string {
	var sb strings.Builder
	sb.WriteString(string(h.State))
	sb.WriteByte(0x1f)
	sb.WriteString(h.Reason)
	for _, is := range h.Issues {
		sb.WriteByte(0x1f)
		sb.WriteString(is.Code)
		sb.WriteByte('@')
		sb.WriteString(is.Node)
	}
	return sb.String()
}

// Run recomputes every environment's health on every tick and the ones the hub
// announced as they are announced, until ctx ends or the broker closes. It learns
// the environments there are before it waits for anything.
func (b *Broker) Run(ctx context.Context) {
	if b.svc == nil {
		return
	}
	b.mu.Lock()
	seeded := b.seeded
	b.mu.Unlock()
	if !seeded {
		b.Recompute(ctx)
	}
	tick := time.NewTicker(b.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		case <-tick.C:
			b.Recompute(ctx)
		case <-b.wake:
			b.mu.Lock()
			ids := make([]string, 0, len(b.pending))
			for id := range b.pending {
				ids = append(ids, id)
			}
			clear(b.pending)
			b.mu.Unlock()
			for _, id := range ids {
				b.recomputeOne(ctx, id)
			}
		}
	}
}

// Close ends every subscription and stops Run. It is safe to call more than once.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.done)
	for s := range b.subs {
		delete(b.subs, s)
		close(s.ch)
	}
}

// AgentConnected implements tunnel.Notifier.
func (b *Broker) AgentConnected(s *tunnel.Session) {
	b.Publish(HealthEvent{Type: EventAgentConnected, EnvID: s.EnvID, Data: AgentConnectedData{
		AgentID: s.AgentID, Node: cleanText(s.Facts.Hostname, maxEventText),
		Type: roleOfFacts(s.Facts.Swarm != nil, s.Facts.Swarm != nil && s.Facts.Swarm.Control), Version: cleanText(s.AgentVersion, maxEventText),
	}})
	b.Notify(s.EnvID)
}

// AgentDisconnected implements tunnel.Notifier.
func (b *Broker) AgentDisconnected(s *tunnel.Session, code string) {
	b.Publish(HealthEvent{Type: EventAgentDisconnected, EnvID: s.EnvID, Data: AgentDisconnectedData{
		AgentID: s.AgentID, Node: cleanText(s.Facts.Hostname, maxEventText), Reason: code,
	}})
	b.Notify(s.EnvID)
}

// AgentRefused implements tunnel.Notifier. envID is what the attempt itself said
// its environment was, so it is checked against the registry first: an attempt
// that names no environment produces no event, and one that names a real one may
// make one event per address every 10 s. Nothing here changes any state.
func (b *Broker) AgentRefused(envID, code, mode, node, ip string) {
	if b.svc == nil || envID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), knownTimeout)
	known := b.svc.Known(ctx, envID)
	cancel()
	if !known {
		return
	}
	now := b.now()
	key := envID + "|" + ip
	b.mu.Lock()
	if at, ok := b.refusals[key]; ok && now.Sub(at) < refusalEvery {
		b.mu.Unlock()
		return
	}
	if len(b.refusals) >= maxRefusalKeys {
		for k, at := range b.refusals {
			if now.Sub(at) >= refusalEvery {
				delete(b.refusals, k)
			}
		}
	}
	b.refusals[key] = now
	b.mu.Unlock()
	b.Publish(HealthEvent{Type: EventAgentRefused, EnvID: envID, At: now, Data: AgentRefusedData{
		Code: cleanText(code, maxEventText), Mode: cleanText(mode, 16), Node: cleanText(node, maxEventText), IP: cleanText(ip, maxEventText),
	}})
}

var _ tunnel.Notifier = (*Broker)(nil)

// roleOfFacts is the role of a node from what its agent reported: a swarm manager,
// a swarm worker, or a standalone host, as the agents service decides it.
func roleOfFacts(inSwarm, control bool) string {
	switch {
	case !inSwarm:
		return envTypeStandalone
	case control:
		return roleManager
	default:
		return roleWorker
	}
}

// cleanText is text an agent supplied made fit to show: control characters
// dropped, surrounding space trimmed and at most n characters kept (T-02-43).
func cleanText(s string, n int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}
