// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// Limits of the global environments stream.
const (
	// defaultEnvStreams is how many of these streams one user may hold open: one per
	// tab, a few tabs (T-02-47).
	defaultEnvStreams = 4
	// envStreamPing is how often an idle stream gets a comment line, and how often
	// its authorisation and the caller's readable set are worked out again (D-54).
	envStreamPing = defaultHeartbeat
)

// HealthSource is what the global stream reads: the health broker.
type HealthSource interface {
	// Subscribe returns the ring and the live channel; see envs.Broker.Subscribe.
	Subscribe(ctx context.Context) (backlog []envs.HealthEvent, ch <-chan envs.HealthEvent, cancel func())
	// SnapshotAll returns every environment with its health now.
	SnapshotAll(ctx context.Context) ([]envs.Environment, error)
}

// EnvStreamDeps are the dependencies of GET /api/environments/stream.
type EnvStreamDeps struct {
	Broker HealthSource
	// Checker works out which environments the caller may read.
	Checker *rbac.Checker
	// Heartbeat is how often an idle stream gets a ping. Zero means 15 seconds;
	// tests shorten it.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent streams per user. Zero means 4.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour.
	MaxLifetime time.Duration
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// EnvStreamEventBody is the data of every frame of the stream but the first. The
// frame's event name is the type of the event and its id is the broker's, a number
// that strictly increases; a browser that reconnects sends the last one it saw in
// Last-Event-ID and is sent what is newer.
//
//   - env.status: Environment is the environment as GET /api/environments lists it
//   - agent.connected: AgentID, Node, Type (manager, worker or standalone), Version
//   - agent.disconnected: AgentID, Node, Reason (closed, timeout, replaced or shutdown)
//   - agent.refused: Code, Mode, Node, IP
//   - failover: FromAgentID, FromNode, ToAgentID, ToNode. A read of the cluster
//     stopped being answered by a manager and went to another; the environment shows
//     degraded until the first is back (D-43)
//   - proxy.warning: Kind (untrusted_forwarded, url_mismatch or websocket_failing),
//     with an empty EnvID. A kind of reverse proxy problem has appeared; only a
//     caller who holds settings.read is sent it, and the bell reads
//     GET /api/warnings again (D-53)
//
// The first frame is named snapshot, has no id and carries {"items": [...]} with
// every environment the caller may read.
type EnvStreamEventBody struct {
	EnvID       string           `json:"envId"`
	At          string           `json:"at"`
	Environment *EnvironmentBody `json:"environment,omitempty"`
	AgentID     string           `json:"agentId,omitempty"`
	Node        string           `json:"node,omitempty"`
	Type        string           `json:"type,omitempty"`
	Version     string           `json:"version,omitempty"`
	Reason      string           `json:"reason,omitempty"`
	Code        string           `json:"code,omitempty"`
	Mode        string           `json:"mode,omitempty"`
	IP          string           `json:"ip,omitempty"`
	FromAgentID string           `json:"fromAgentId,omitempty"`
	FromNode    string           `json:"fromNode,omitempty"`
	ToAgentID   string           `json:"toAgentId,omitempty"`
	ToNode      string           `json:"toNode,omitempty"`
	Kind        string           `json:"kind,omitempty"`
}

// envStreamEventBody maps a broker event to what the stream sends, or reports
// false for one it does not know how to send.
func envStreamEventBody(ev envs.HealthEvent) (EnvStreamEventBody, bool) {
	body := EnvStreamEventBody{EnvID: ev.EnvID, At: ev.At.UTC().Format(time.RFC3339Nano)}
	switch d := ev.Data.(type) {
	case envs.Environment:
		e := environmentBody(d)
		body.Environment = &e
	case envs.AgentConnectedData:
		body.AgentID, body.Node, body.Type, body.Version = d.AgentID, d.Node, d.Type, d.Version
	case envs.AgentDisconnectedData:
		body.AgentID, body.Node, body.Reason = d.AgentID, d.Node, d.Reason
	case envs.AgentRefusedData:
		body.Code, body.Mode, body.Node, body.IP = d.Code, d.Mode, d.Node, d.IP
	case envs.FailoverData:
		body.FromAgentID, body.FromNode, body.ToAgentID, body.ToNode = d.FromAgentID, d.FromNode, d.ToAgentID, d.ToNode
	case envs.ProxyWarningData:
		body.Kind = d.Kind
	default:
		return EnvStreamEventBody{}, false
	}
	return body, true
}

// RegisterEnvStream registers GET /api/environments/stream: one server-sent events
// stream per browser tab that carries the health of every environment the caller
// may read (D-36, D-37). Any signed-in user may open it; what they receive is
// filtered by environments.read per environment, as the environment list is, so
// it reveals nothing the list would not. It is mounted through Router.Guarded
// because the OpenAPI contract cannot describe a stream (D-47).
func RegisterEnvStream(r *httpx.Router, d EnvStreamDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Heartbeat <= 0 {
		d.Heartbeat = envStreamPing
	}
	if d.MaxStreamsPerUser <= 0 {
		d.MaxStreamsPerUser = defaultEnvStreams
	}
	if d.MaxLifetime <= 0 {
		d.MaxLifetime = defaultMaxStreamLifetime
	}
	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/environments/stream", rbac.SelfService, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveEnvStream(w, req, d, lim)
	}))
}

// serveEnvStream is the handler: the snapshot, what a reconnecting tab missed, then
// live events, with a ping every Heartbeat, until the client leaves, the broker
// closes or drops the subscription, a write fails, the stream has lived MaxLifetime
// or a ping finds it no longer authorised.
//
// The policy ran once, when the stream opened. Every ping runs it again through
// httpx.StillAuthorized and works out again which environments the caller may read,
// so a sign-out, a disabled user and a withdrawn grant end or narrow the stream
// within one ping (WR-02, T-02-46). An event for an environment the caller cannot
// read is never written, and an event with no environment is sent only when it
// is a proxy warning and the caller holds settings.read, the permission that lets
// them read the same warnings from GET /api/warnings.
func serveEnvStream(w http.ResponseWriter, req *http.Request, d EnvStreamDeps, lim *streamLimiter) {
	ctx := req.Context()
	p := httpx.PrincipalFrom(ctx)
	if p == nil || d.Broker == nil || d.Checker == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemRateLimited,
			fmt.Sprintf("You already have %d environment streams open. Close one and try again.", lim.max))
		return
	}
	defer release()

	// readable is what the caller may read now: per environment, and whether they
	// hold settings.read, which is what a proxy warning needs.
	var settingsRead bool
	readable := func() (map[string][]rbac.Perm, error) {
		global, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
		settingsRead = slices.Contains(global, rbac.PermSettingsRead)
		return perEnv, err
	}
	perEnv, err := readable()
	if err != nil {
		d.Log.Error("work out what a stream may show", "error", err)
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	// Subscribed before the snapshot is taken, so nothing that happens in between
	// is lost; an event the snapshot already shows arrives again and is harmless.
	backlog, ch, cancel := d.Broker.Subscribe(ctx)
	defer cancel()
	all, err := d.Broker.SnapshotAll(ctx)
	if err != nil {
		d.Log.Error("snapshot the environments for a stream", "error", err)
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}

	// A stream lives for hours, so the server's other timeouts must not apply to
	// it; a writer that cannot clear the deadline (a test recorder) is not an error.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}

	write := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	items := make([]EnvironmentBody, 0, len(all))
	for _, e := range all {
		if canRead(perEnv, e.ID) {
			items = append(items, environmentBody(e))
		}
	}
	data, err := json.Marshal(EnvironmentListBody{Items: items})
	if err != nil {
		d.Log.Error("encode a snapshot", "error", err)
		return
	}
	// The snapshot has no id: it is not an event, and a tab that drops before the
	// first event simply asks for a new one.
	if !write("event: snapshot\ndata: %s\n\n", data) {
		return
	}

	send := func(ev envs.HealthEvent) bool {
		switch {
		case ev.EnvID == "" && ev.Type == envs.EventProxyWarning:
			if !settingsRead {
				return true
			}
		case ev.EnvID == "" || !canRead(perEnv, ev.EnvID):
			return true
		}
		body, ok := envStreamEventBody(ev)
		if !ok {
			d.Log.Warn("an environment stream event has data it cannot send", "type", ev.Type)
			return true
		}
		data, err := json.Marshal(body)
		if err != nil {
			d.Log.Error("encode a stream event", "error", err)
			return false
		}
		return write("id: %s\nevent: %s\ndata: %s\n\n", strconv.FormatUint(ev.ID, 10), ev.Type, data)
	}
	// Only a tab that says what it last saw is replayed anything; a new one has the
	// snapshot. An ID the ring no longer holds replays the whole ring.
	if last := req.Header.Get("Last-Event-ID"); last != "" {
		for _, ev := range envs.EventsAfter(backlog, last) {
			if !send(ev) {
				return
			}
		}
	}

	beat := time.NewTicker(d.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(d.MaxLifetime)
	defer life.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if !send(ev) {
				return
			}
		case <-beat.C:
			if !httpx.StillAuthorized(ctx) {
				return
			}
			next, err := readable()
			if err != nil {
				// Fails closed: without knowing what the caller may read, nothing more is sent.
				d.Log.Warn("work out what a stream may show", "error", err)
				return
			}
			perEnv = next
			if !write(": ping\n\n") {
				return
			}
		}
	}
}
