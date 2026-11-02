// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/proto/wire"
)

const (
	// DefaultOfflineAfter is how long a session may be silent before it is closed
	// (D-41).
	DefaultOfflineAfter = 30 * time.Second

	// kindTimeout bounds the wait for the kind byte of an agent-opened stream.
	kindTimeout = 10 * time.Second
	// maxAgentStreams caps the agent-opened streams being looked at at once. Only
	// one is ever needed, the control stream; the rest is room for a late one.
	maxAgentStreams = 8
	// touchTimeout bounds one last-seen write.
	touchTimeout = 5 * time.Second
	// controlWriteTimeout bounds one write to an agent's control stream, so an
	// agent that stopped reading cannot hold up whoever is writing to it.
	controlWriteTimeout = 10 * time.Second
	// renewTimeout bounds one renewal: a certificate signed and one transaction.
	renewTimeout = 30 * time.Second
)

// errNoControlStream means the agent has not opened its control stream yet, or the
// session has ended.
var errNoControlStream = errors.New("tunnel: the agent has no control stream")

// sendControl writes one message, as a line of JSON, to the agent's control
// stream. It fails when the agent has not opened one (it is a message for a
// session that is already up) or when the write does not finish in time.
func (s *Session) sendControl(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	if s.ctl == nil {
		return errNoControlStream
	}
	_ = s.ctl.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
	_, err = s.ctl.Write(append(b, '\n'))
	return err
}

// LastSeenRecorder is told about every sign of life from an agent, so that the
// database can keep a "last seen" without a write per heartbeat: the hub calls
// TouchLastSeen on every heartbeat and the recorder decides how often to write
// (agents.Service.TouchLastSeen writes at most once a minute per agent).
type LastSeenRecorder interface {
	TouchLastSeen(ctx context.Context, agentID string)
}

// SessionStatus is what the agent last reported, held in memory only: heartbeats
// every few seconds must not become database writes (RESEARCH Pitfall 8).
type SessionStatus struct {
	// LastHeartbeat is when the server last received a heartbeat, on its own
	// clock. It is zero until the first one.
	LastHeartbeat time.Time
	// EngineReachable is what the last heartbeat said about the Docker engine.
	EngineReachable bool
	// ClockMs is the agent's clock in the last heartbeat, in Unix milliseconds.
	ClockMs int64
	// ClockOffset is how far the agent's clock is ahead of the server's (negative
	// when behind), measured in the handshake, and ClockWarning is set when it is
	// beyond ClockWarnOffset. They hold for the whole session (D-45).
	ClockOffset  time.Duration
	ClockWarning bool
	// LastDenial is the last request the agent refused by policy, nil when it has
	// refused none (D-29).
	LastDenial *Denial
}

// Denial is a request an agent refused by policy.
type Denial struct {
	Method, Endpoint, Reason string
	At                       time.Time
}

// Status is the last heartbeat. It reads memory and never the database.
func (s *Session) Status() SessionStatus {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	return s.status
}

// ErrSessionEnded is what AwaitStatus returns when the session ended before its
// status satisfied the condition.
var ErrSessionEnded = errors.New("tunnel: the session ended")

// notifyStatusLocked wakes every AwaitStatus waiter. s.hbMu must be held. The
// channel is made by the first waiter, so a session nobody waits on allocates
// nothing.
func (s *Session) notifyStatusLocked() {
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// AwaitStatus returns the first status of the session for which cond is true. It
// evaluates cond now and again after every heartbeat and every denial, and never
// polls. When ctx ends first it returns the last status together with the
// context's error, and when the session ends first the last status together with
// ErrSessionEnded (cond is evaluated once more on that last status, so a
// condition made true by the final change still returns). The context only
// guards against a hang.
func (s *Session) AwaitStatus(ctx context.Context, cond func(SessionStatus) bool) (SessionStatus, error) {
	for {
		// Take the channel in the same critical section as the read, so a change
		// between the read and the wait is not missed.
		s.hbMu.Lock()
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch, st := s.changed, s.status
		s.hbMu.Unlock()
		if cond(st) {
			return st, nil
		}
		select {
		case <-ch:
		case <-s.done:
			if st = s.Status(); cond(st) {
				return st, nil
			}
			return st, ErrSessionEnded
		case <-ctx.Done():
			return st, ctx.Err()
		}
	}
}

// lastSignOfLife is when the session last proved it was alive: its last
// heartbeat, or when it connected if it has sent none yet, so a new session has
// the whole window to send its first.
func (s *Session) lastSignOfLife() time.Time {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	if s.status.LastHeartbeat.After(s.ConnectedAt) {
		return s.status.LastHeartbeat
	}
	return s.ConnectedAt
}

// WithTiming sets how often agents are told to heartbeat (the welcome carries
// it) and how long a session may be silent before it is closed with the reason
// timeout. A zero or negative value keeps the default. Call it before the hub
// serves.
func (h *Hub) WithTiming(heartbeat, offlineAfter time.Duration) *Hub {
	if heartbeat > 0 {
		h.heartbeat = heartbeat
	}
	if offlineAfter > 0 {
		h.offlineAfter = offlineAfter
	}
	return h
}

// WithWatchInterval sets how often the hub looks for silent sessions. The
// default is a sixth of the offline window, so a silent session is closed within
// a sixth of the window after it should be. Tests shorten it. Call it before the
// hub serves.
func (h *Hub) WithWatchInterval(d time.Duration) *Hub {
	if d > 0 {
		h.watchEvery = d
	}
	return h
}

// WithMuxTimeouts sets how often the multiplexer pings the agent and how long a
// ping or a write may wait before the session is taken for dead. A zero or
// negative value keeps the default of 10 s. Tests that move tens of MiB under
// -race on a loaded machine raise them, where a stall of the whole process would
// otherwise read as a dead agent. Call it before the hub serves.
func (h *Hub) WithMuxTimeouts(keepAlive, write time.Duration) *Hub {
	if keepAlive > 0 {
		h.muxKeepAlive = keepAlive
	}
	if write > 0 {
		h.muxWriteTimeout = write
	}
	return h
}

// WithLastSeen sets where heartbeats and connects are recorded. Nil records
// nothing. Call it before the hub serves.
func (h *Hub) WithLastSeen(r LastSeenRecorder) *Hub {
	h.lastSeen = r
	return h
}

// WithWatchTicks replaces the watchdog's time.Ticker with ticks: every value
// received is one look for silent sessions. A test sends on an unbuffered channel,
// so a send that completes means the watchdog took the tick, and the next send
// completing means it has finished evaluating the first. Nil, the default, keeps
// the ticker. Call it before the hub serves.
func (h *Hub) WithWatchTicks(ticks <-chan time.Time) *Hub {
	h.watchTicks = ticks
	return h
}

func (h *Hub) watchInterval() time.Duration {
	if h.watchEvery > 0 {
		return h.watchEvery
	}
	return max(h.offlineAfter/6, 10*time.Millisecond)
}

// runSession starts what a registered session needs: the loop that accepts the
// agent's streams (the control stream among them) and the watchdog that closes a
// session that has gone quiet. Both end with the session or ctx.
func (h *Hub) runSession(ctx context.Context, s *Session) {
	go h.acceptAgentStreams(ctx, s)
	go h.watch(ctx, s)
}

// watch closes a session whose last sign of life is older than the offline
// window. The close is immediate (no close handshake): the peer has stopped
// answering, and waiting for it would be the hang this exists to prevent.
func (h *Hub) watch(ctx context.Context, s *Session) {
	ticks := h.watchTicks
	if ticks == nil {
		t := time.NewTicker(h.watchInterval())
		defer t.Stop()
		ticks = t.C
	}
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if silent := h.now().Sub(s.lastSignOfLife()); silent > h.offlineAfter {
			h.log.Warn("agent session closed: no heartbeat within the offline window",
				"agent_id", s.AgentID, "environment_id", s.EnvID, "silent_for", silent.Round(time.Second).String())
			s.endWith(DisconnectTimeout)
			s.closeNow()
			return
		}
	}
}

// acceptAgentStreams takes the streams the agent opens. The first whose kind byte
// is wire.StreamKindControl is the control stream; any other is closed.
func (h *Hub) acceptAgentStreams(ctx context.Context, s *Session) {
	var inFlight atomic.Int32
	for {
		st, err := s.ym.AcceptStreamWithContext(ctx)
		if err != nil {
			return
		}
		if inFlight.Add(1) > maxAgentStreams {
			inFlight.Add(-1)
			_ = st.Close()
			continue
		}
		go func() {
			defer inFlight.Add(-1)
			h.agentStream(s, st)
		}()
	}
}

// agentStream reads the kind byte of one agent-opened stream and serves it.
func (h *Hub) agentStream(s *Session, st *yamux.Stream) {
	defer func() { _ = st.Close() }()
	_ = st.SetReadDeadline(time.Now().Add(kindTimeout))
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	if kind[0] != wire.StreamKindControl || !s.control.CompareAndSwap(false, true) {
		h.log.Warn("closed an agent-opened stream that is not the control stream", "agent_id", s.AgentID, "kind", kind[0])
		return
	}
	s.ctlMu.Lock()
	s.ctl = st
	s.ctlMu.Unlock()
	// An address that changed after this agent's welcome, and before it opened its
	// control stream, was pushed to sessions that had a control stream and so not to
	// this one: say it now. Both ends merge a repeated list into the same state.
	if cur := h.URLs(); len(cur) > 0 && !slices.Equal(cur, s.welcomeURLs) {
		if err := s.sendControl(wire.URLUpdate{T: wire.TypeURLs, URLs: cur, Pins: h.Pins()}); err != nil {
			h.log.Warn("could not tell an agent the new server address", "agent_id", s.AgentID, "error", err)
		}
	}
	if h.updates != nil {
		h.updates.ControlOpened(s)
	}
	h.readControl(s, st)
}

// readControl reads the agent's newline-delimited JSON messages. The input is
// authenticated but not trusted: a line is at most wire.MaxControlLine bytes, a
// line that is not JSON or has a type this server does not know is ignored, and
// nothing here touches the database except the rate-limited last-seen and the
// audit entry of a policy denial, which is rate-limited too.
func (h *Hub) readControl(s *Session, st io.Reader) {
	sc := bufio.NewScanner(st)
	sc.Buffer(make([]byte, 0, 4096), wire.MaxControlLine)
	for sc.Scan() {
		var head struct {
			T string `json:"t"`
		}
		if json.Unmarshal(sc.Bytes(), &head) != nil {
			continue
		}
		switch head.T {
		case wire.TypeHeartbeat:
			var hb wire.Heartbeat
			if json.Unmarshal(sc.Bytes(), &hb) == nil {
				h.beat(s, hb)
			}
		case wire.TypeDenied:
			var d wire.Denied
			if json.Unmarshal(sc.Bytes(), &d) == nil {
				h.denied(s, d)
			}
		case wire.TypeRenew:
			h.renew(s)
		case wire.TypeUpdateResult:
			var res wire.UpdateResult
			if json.Unmarshal(sc.Bytes(), &res) == nil {
				h.updateResult(s, res)
			}
		}
	}
	if errors.Is(sc.Err(), bufio.ErrTooLong) {
		h.log.Warn("agent session closed: a control line was longer than the limit", "agent_id", s.AgentID, "limit", wire.MaxControlLine)
		s.close(websocket.StatusMessageTooBig, "control line too long")
	}
}

// renew answers an agent's request for a new certificate (D-04). The work, a
// signature and a transaction, runs off the control reader so heartbeats keep being
// read, and only one renewal per session runs at a time: a line that arrives while
// one is in progress is dropped, since the agent asks once and waits.
//
// A revoked agent is told so and its session is closed (T-02-83). Any other refusal
// is only said: the session stays, and the agent asks again later.
func (h *Hub) renew(s *Session) {
	if !s.renewing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.renewing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), renewTimeout)
		defer cancel()
		res, err := h.v.Renew(ctx, s.AgentID, s.IP)
		if err != nil {
			code := wire.RenewInternal
			var re *RefuseError
			if errors.As(err, &re) {
				code = re.Code
			} else {
				h.log.Warn("could not renew an agent's certificate", "agent_id", s.AgentID, "environment_id", s.EnvID, "error", err)
			}
			if werr := s.sendControl(wire.RenewRefused{T: wire.TypeRenewRefused, Code: code}); werr != nil {
				h.log.Debug("could not tell an agent its renewal was refused", "agent_id", s.AgentID, "error", werr)
			}
			if code == wire.ReasonRevoked {
				h.log.Warn("a revoked agent asked for a new certificate; its session is closed", "agent_id", s.AgentID, "environment_id", s.EnvID)
				s.endWith(DisconnectRevoked)
				s.kick(wire.ReasonRevoked)
			}
			return
		}
		if err := s.sendControl(wire.Renewed{T: wire.TypeRenewed, Cert: res.Cert}); err != nil {
			// The new certificate is recorded and the old one still works until it
			// expires: the agent asks again, and is told it is not due.
			h.log.Warn("could not send an agent its renewed certificate", "agent_id", s.AgentID, "environment_id", s.EnvID, "error", err)
		}
	}()
}

// beat records a heartbeat in memory and tells the last-seen recorder.
func (h *Hub) beat(s *Session, hb wire.Heartbeat) {
	s.hbMu.Lock()
	s.status.LastHeartbeat, s.status.EngineReachable, s.status.ClockMs = h.now(), hb.EngineReachable, hb.ClockMs
	s.notifyStatusLocked()
	s.hbMu.Unlock()
	h.touch(s)
}

// touch tells the last-seen recorder that the agent was heard from.
func (h *Hub) touch(s *Session) {
	if h.lastSeen == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
	defer cancel()
	h.lastSeen.TouchLastSeen(ctx, s.AgentID)
}

const (
	// maxDenialMethod, maxDenialEndpoint and maxDenialReason cap what an agent
	// says about a refusal: it is authenticated but not trusted, and the text ends
	// up in the audit log and the UI (T-02-30).
	maxDenialMethod   = 16
	maxDenialEndpoint = 256
	maxDenialReason   = 32
	// denyBurst audit rows may be written at once for one session, and then one
	// every denyRefill. A compromised agent can send denied lines as fast as it
	// likes; each audited one is a write on the single database writer.
	denyBurst  = 20
	denyRefill = time.Second
)

// agentHub is the principal under which what the hub notices about an agent is
// audited (D-13, P1 D-51).
const agentHub = "system:agent-hub"

// denied handles an agent telling the server that its policy refused a request
// (D-29): it means a bug in TierPlan or a server that asks for more than it
// should, so it is remembered on the session for the node list and audited. Only
// the audit is limited; the session always shows the latest denial.
func (h *Hub) denied(s *Session, d wire.Denied) {
	method, endpoint, reason := clip(d.Method, maxDenialMethod), clip(d.Path, maxDenialEndpoint), clip(d.Reason, maxDenialReason)
	now := h.now()
	s.hbMu.Lock()
	s.status.LastDenial = &Denial{Method: method, Endpoint: endpoint, Reason: reason, At: now}
	s.notifyStatusLocked()
	s.hbMu.Unlock()

	if s.denyLimit != nil && !s.denyLimit.AllowN(now, 1) {
		s.suppressed.Add(1)
		return
	}
	h.log.Warn("an agent refused a request by policy", "agent_id", s.AgentID, "environment_id", s.EnvID,
		"method", method, "endpoint", endpoint, "reason", reason)
	if h.audit == nil {
		return
	}
	details := map[string]string{
		"node": clip(s.Facts.Hostname, maxAuditNode), "method": method, "endpoint": endpoint, "reason": reason,
	}
	if n := s.suppressed.Swap(0); n > 0 {
		details["suppressed"] = strconv.FormatInt(n, 10)
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	err := h.audit.Record(ctx, audit.Entry{
		Actor: rbac.System(agentHub).AuditActor(), Action: audit.ActionAgentPolicyDenied,
		TargetType: "agent", TargetID: s.AgentID, EnvID: s.EnvID, Result: audit.ResultDenied,
		Reason: reason, IP: s.IP, Details: details,
	})
	if err != nil {
		h.log.Warn("could not record a policy denial", "agent_id", s.AgentID, "error", err)
	}
}
