// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/proto/wire"
)

// /agent/connect is the one endpoint that answers anyone, so everything that
// bounds what an anonymous caller can cost lives here: who may reach it (no
// browser, no plain HTTP), how many handshakes run at once overall and from one
// address, how often one address may start one, and what a refused attempt costs
// the single SQLite writer (RESEARCH Pattern 2, Pitfall 7, Pitfall 8, T-02-24).
// A handshake is admitted only when all three bounds allow it, checked in this
// order: the address's in-flight cap, its attempt budget, the global slots. The
// same file holds the two caps that stop an authenticated but untrusted agent
// from taking the server down with it (Pattern 1).

const (
	// MaxSessionStreams is how many server-opened streams one session carries at
	// once. A request beyond it fails with ErrBusy and is never queued.
	MaxSessionStreams = 64
	// MaxResponseBytes caps one Docker response that is not the events feed, so a
	// hostile agent cannot exhaust server memory with a huge JSON list.
	MaxResponseBytes = 64 << 20

	// maxHandshakes is how many unauthenticated handshakes may be in flight.
	maxHandshakes = 64
	// maxHandshakesPerAddr is how many of them one client address, or one IPv6 /64,
	// may hold at once. A client that opens a handshake and never answers holds its
	// slot, so without this one address could take all 64 and every other agent's
	// reconnect would be refused (T-02-24). It is sized above what a NAT'd swarm
	// needs, because a handshake takes milliseconds and an agent that is refused
	// retries after busyRetryAfter, and it leaves 48 slots for everyone else.
	maxHandshakesPerAddr = 16
	// firstFrameTimeout is how long an agent has, from the challenge, to send its
	// auth. That is the only message it sends before the welcome, and a real agent
	// sends it as soon as it reads the challenge (it collects its facts before it
	// dials), so a few seconds is generous. A client that stays silent gives its
	// slot back after this, not after the whole handshake timeout (T-02-24).
	firstFrameTimeout = 3 * time.Second
	// DefaultConnectBurst is how many connection attempts one client address may
	// make at once. A whole swarm behind one NAT reconnects together after a server
	// restart, so it is sized for that and not for a person (Pitfall 7).
	DefaultConnectBurst = 200
	// connectRefill is how often an address earns one more attempt.
	connectRefill = 100 * time.Millisecond
	// connectLimiterTTL is how long an idle address is remembered.
	connectLimiterTTL = 10 * time.Minute
	// busyRetryAfter is what a refused agent is told to wait before it tries again.
	busyRetryAfter = time.Second
	// recordTimeout bounds the audit write of one refusal.
	recordTimeout = 5 * time.Second
	// maxAuditNode is how much of an agent-supplied hostname an audit entry keeps.
	maxAuditNode = 64
)

var (
	// ErrBusy means the session already has MaxSessionStreams requests in flight.
	// It is not ErrAgentGone: the agent is fine, there is only too much asked of
	// it, so the caller must not mark it offline.
	ErrBusy = errors.New("tunnel: the agent has too many requests in flight")
	// ErrTooLarge means a Docker response passed the size cap. The stream is
	// closed and the session stays up; like ErrBusy it is not ErrAgentGone.
	ErrTooLarge = errors.New("tunnel: the agent's answer is larger than the limit")
)

// Reasons an attempt is recorded as refused before the handshake starts. The
// first two are not wire codes: the caller is not an agent, so there is nobody to
// tell.
const (
	refusedOrigin     = "origin"
	refusedNotUpgrade = "not_upgrade"
)

// WithAllowInsecure lets agents connect over plain ws:// although the public URL
// is https. It is for development only (BAYPLAN_AGENT_ALLOW_INSECURE): the join
// token and every Docker answer then cross the network in the clear. Call it
// before the hub serves.
func (h *Hub) WithAllowInsecure(allow bool) *Hub {
	h.allowInsecure = allow
	if allow {
		h.log.Warn("BAYPLAN_AGENT_ALLOW_INSECURE is set: agents may connect over plain ws://, so join tokens and Docker answers can be read on the network. Use it for development only")
	}
	return h
}

// WithConnectLimit sets how many connection attempts one client address may make
// at once; it earns one more every 100 ms. A burst below one keeps the default
// (BAYPLAN_AGENT_CONNECT_BURST). Call it before the hub serves.
func (h *Hub) WithConnectLimit(burst int) *Hub {
	if burst > 0 {
		h.connect = auth.NewIPLimiter(connectRefill, burst, connectLimiterTTL, h.now)
	}
	return h
}

// WithAudit sets where what an authenticated agent does is recorded, such as the
// requests it refused by policy. It is the audit writer itself: those agents are
// authenticated, and the per-session limit on denials is what protects the writer.
// Nil records nothing. Call it before the hub serves.
func (h *Hub) WithAudit(r auth.AuditRecorder) *Hub {
	h.audit = r
	return h
}

// WithRefusalAudit sets where refused attempts are recorded as agent.enrol_refused.
// It must be an auth.AuditThrottle, or something as careful: the actor is
// anonymous, so without a per-address budget one caller could fill the audit log
// and starve the single database writer (Pitfall 8). Nil records nothing. Call it
// before the hub serves.
func (h *Hub) WithRefusalAudit(r auth.AuditRecorder) *Hub {
	h.refusals = r
	return h
}

// OnUpgradeFailure sets a hook that runs, with the client address, for every
// request to /agent/connect that is not a WebSocket upgrade. A proxy that strips
// the Upgrade headers produces exactly this, so the proxy observer reads it as a
// misconfiguration signal. The hook must not block.
func (h *Hub) OnUpgradeFailure(f func(peer string)) *Hub {
	h.onUpgradeFailure = f
	return h
}

// WithResponseLimit sets the cap, in bytes, on a Docker response that is not the
// events feed. A value below one keeps MaxResponseBytes. Tests shrink it; nothing
// else should. Call it before the hub serves.
func (h *Hub) WithResponseLimit(n int64) *Hub {
	if n > 0 {
		h.maxResponse = n
	}
	return h
}

// WithFirstFrameTimeout sets how long an agent has, from the challenge, to send its
// auth. A value below one keeps firstFrameTimeout. Tests change it; nothing else
// should. Call it before the hub serves.
func (h *Hub) WithFirstFrameTimeout(d time.Duration) *Hub {
	if d > 0 {
		h.firstFrame = d
	}
	return h
}

// preflightRefusal is a request turned away before the WebSocket is accepted.
type preflightRefusal struct {
	status int
	// code is what the audit entry records, and for a wire code what the body says.
	code string
	msg  string
	// audit is false for a refusal that is the server's own state, not a caller's
	// fault: no public URL.
	audit bool
}

// preflight runs the checks that need nothing but the request. It returns the
// audience the handshake must be addressed to.
func (h *Hub) preflight(r *http.Request, ip string) (aud string, ref *preflightRefusal) {
	// Browsers always send Origin and agents never do: a request with one is a web
	// page trying to use this endpoint.
	if r.Header.Get("Origin") != "" {
		return "", &preflightRefusal{status: http.StatusForbidden, code: refusedOrigin, msg: "forbidden", audit: true}
	}
	if !isWebSocketUpgrade(r) {
		// A proxy that strips the Upgrade headers lands here.
		h.log.Warn("agent endpoint reached without a WebSocket upgrade; a reverse proxy may be dropping the Upgrade headers", "ip", ip)
		if h.onUpgradeFailure != nil {
			h.onUpgradeFailure(ip)
		}
		return "", &preflightRefusal{status: http.StatusUpgradeRequired, code: refusedNotUpgrade, msg: "a WebSocket upgrade is required", audit: true}
	}
	aud, plainOK, err := h.audience()
	if err != nil {
		h.log.Error("agent connection refused: Bayplan has no usable public URL", "error", err)
		return "", &preflightRefusal{status: http.StatusServiceUnavailable, msg: "this server has no public URL, so it cannot accept agents"}
	}
	if !secureTransport(r) {
		if !plainOK && !h.allowInsecure {
			h.log.Warn("agent connection refused: it did not arrive over TLS", "ip", ip)
			return "", &preflightRefusal{
				status: http.StatusForbidden, code: wire.RefuseInsecureTransport, audit: true,
				msg: wire.RefuseInsecureTransport + ": agents must connect over wss://",
			}
		}
		h.log.Warn("an agent connected over plain HTTP, so the join token and the Docker answers are not encrypted", "ip", ip)
	}
	return aud, nil
}

// addrInflight counts the handshakes in flight per client address, keyed like the
// attempt budget (auth.AddrKey), so an IPv6 /64 is one address. An entry exists
// only while its address has a handshake in flight, so the map cannot outgrow the
// number of slots.
type addrInflight struct {
	mu sync.Mutex
	n  map[string]int
}

func newAddrInflight() *addrInflight { return &addrInflight{n: map[string]int{}} }

// take counts one more handshake for key, or reports false when key already has
// max in flight.
func (a *addrInflight) take(key string, max int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n[key] >= max {
		return false
	}
	a.n[key]++
	return true
}

// give takes one back.
func (a *addrInflight) give(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n[key] <= 1 {
		delete(a.n, key)
		return
	}
	a.n[key]--
}

// admit decides whether one more handshake may start. Three bounds apply, in this
// order: the address may have fewer than maxHandshakesPerAddr in flight (checked
// first, so a refusal for concurrency does not spend its attempt budget), it must
// be within its budget of attempts, and fewer than maxHandshakes may be in flight
// in all. It returns release, which gives the slots back and is safe to call more
// than once, or the refusal to send.
func (h *Hub) admit(ip string) (release func(), refuse *wire.Refuse) {
	limited := &wire.Refuse{
		T: wire.TypeRefuse, Code: wire.RefuseRateLimited, RetryAfterMs: int(busyRetryAfter / time.Millisecond),
		Detail: "too many connection attempts; the agent retries by itself",
	}
	key := auth.AddrKey(ip)
	if !h.perAddr.take(key, maxHandshakesPerAddr) {
		return nil, limited
	}
	if !h.connect.Allow(ip) {
		h.perAddr.give(key)
		return nil, limited
	}
	select {
	case h.handshakes <- struct{}{}:
	default:
		h.perAddr.give(key)
		return nil, limited
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-h.handshakes
			h.perAddr.give(key)
		})
	}, nil
}

// clientAddr is the address of the caller: the one the client-address middleware
// recorded, else the TCP peer.
func clientAddr(r *http.Request) string {
	if ip := httpx.ClientIP(r.Context()); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recordRefusal writes one agent.enrol_refused entry for an anonymous caller.
// The refusals recorder is a throttle, so a caller who keeps being refused costs
// the writer a bounded number of rows and the rest are counted. The details hold
// the code, the mode and the node's name, never anything that could be a secret.
func (h *Hub) recordRefusal(ctx context.Context, ip, code, mode, node, reason string) {
	if h.refusals == nil {
		return
	}
	d := map[string]string{"code": code}
	if mode != "" {
		d["mode"] = clip(mode, 16)
	}
	if node != "" {
		d["node"] = clip(node, maxAuditNode)
	}
	if reason == "" {
		reason = code
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	err := h.refusals.Record(ctx, audit.Entry{
		Actor: audit.Actor{Type: audit.ActorAnonymous}, Action: audit.ActionAgentEnrolRefused,
		TargetType: "agent", Result: audit.ResultDenied, Reason: reason, IP: ip, Details: d,
	})
	if err != nil {
		h.log.Warn("could not record a refused agent connection", "code", code, "error", err)
	}
}

// acquire takes one of the session's stream slots, or reports false when all
// MaxSessionStreams are in use.
func (s *Session) acquire() bool {
	if s.streams == nil {
		return true
	}
	select {
	case s.streams <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseStream gives a slot back.
func (s *Session) releaseStream() {
	if s.streams == nil {
		return
	}
	select {
	case <-s.streams:
	default:
	}
}

// requestLine matches the first line of an HTTP request, the start of one
// exchange on a stream.
var requestLine = regexp.MustCompile(`^[A-Z]+ (\S+) HTTP/1\.[01]\r\n`)

// startsEventsRequest reports whether p begins an HTTP request for the events
// feed, which is a stream that is meant to run for hours and is not capped. It
// reports ok false when p is not the start of a request at all, for example the
// next piece of a body.
func startsEventsRequest(p []byte) (events, ok bool) {
	head := p
	if len(head) > 1024 {
		head = head[:1024]
	}
	m := requestLine.FindSubmatch(head)
	if m == nil {
		return false, false
	}
	target, _, _ := strings.Cut(string(m[1]), "?")
	return strings.HasSuffix(target, "/events"), true
}
