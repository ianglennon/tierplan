// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Limits of the events routes.
const (
	// defaultHeartbeat keeps an idle stream from being closed by a proxy that
	// times out silent connections (Pitfall 8).
	defaultHeartbeat = 15 * time.Second
	// defaultMaxStreams is how many event streams one user may hold open at
	// once, however many tabs they have (T-01-87).
	defaultMaxStreams = 8
	// defaultMaxStreamLifetime is the longest one stream lives. Whatever the
	// re-checks missed, the browser's reconnect then goes through the full
	// policy (WR-02).
	defaultMaxStreamLifetime = time.Hour
	// recentEventsLimit is how many events the polling fallback returns.
	recentEventsLimit = 50
)

// EventsSource is what the events routes read: the events of an environment,
// whichever target they come from. *envs.Aggregator implements it.
type EventsSource interface {
	// Events returns the buffered events, oldest first, and the channel of the
	// ones that follow; see envs.Aggregator.Events.
	Events(ctx context.Context, envID string) (backlog []dockerx.EventRow, ch <-chan dockerx.EventRow, cancel func())
	// Recent returns up to n buffered events, newest first.
	Recent(ctx context.Context, envID string, n int) []dockerx.EventRow
}

// EventsDeps are the dependencies of the events routes.
type EventsDeps struct {
	Aggregator EventsSource
	// Heartbeat is how often an idle stream gets a comment line. Zero means 15
	// seconds; tests shorten it.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent streams per user. Zero means 8.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour;
	// tests shorten it.
	MaxLifetime time.Duration
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// EventDTO is one Docker event as the feed shows it. It is the data of every
// stream frame and the item of the recent-events list. It carries no labels or
// environment: only name, image and exit code survive from Docker's attributes
// (T-01-88).
type EventDTO struct {
	ID         string            `json:"id" doc:"A stable ID for the event; the SSE frame id, and what a repeated event is recognised by"`
	Time       string            `json:"time" format:"date-time" doc:"When it happened, in UTC, with sub-second precision"`
	Type       string            `json:"type" enum:"container,image,network,volume" doc:"What kind of object it is about"`
	Action     string            `json:"action" doc:"What happened, for example start, die, pull, connect or create"`
	ActorID    string            `json:"actorId" doc:"The ID of the container, image, network or volume"`
	Name       string            `json:"name" doc:"The container, image, network or volume name; empty when Docker gave none"`
	Attributes map[string]string `json:"attributes" doc:"Only name, image and exitCode, when the event had them"`
}

// EventListBody is the recent events of an environment.
type EventListBody struct {
	Items []EventDTO `json:"items" doc:"At most 50 events, newest first"`
}

// EventListOutput wraps EventListBody for huma.
type EventListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EventListBody
}

func eventDTO(r dockerx.EventRow) EventDTO {
	attrs := r.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	return EventDTO{
		ID: r.ID, Time: r.Time.UTC().Format(time.RFC3339Nano), Type: r.Type, Action: r.Action,
		ActorID: r.ActorID, Name: r.Name, Attributes: attrs,
	}
}

// streamLimiter counts the open streams of each user.
type streamLimiter struct {
	max int

	mu   sync.Mutex
	open map[string]int
}

// acquire takes a slot for key. It reports false, and takes nothing, when key
// already holds the maximum. release gives the slot back; calling it twice is
// harmless.
func (l *streamLimiter) acquire(key string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[key] >= l.max {
		return nil, false
	}
	l.open[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.open[key]--; l.open[key] <= 0 {
				delete(l.open, key)
			}
		})
	}, true
}

// RegisterEvents registers the events routes under /api/env/{envId}/. Both
// need environments.read on the environment (D-48): the recent-events list the
// SPA polls as a fallback, which is in the OpenAPI contract and so gives the
// frontend its event type, and the server-sent events stream, which is mounted
// through Router.Guarded because the contract cannot describe a stream (D-47).
func RegisterEvents(r *httpx.Router, d EventsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Heartbeat <= 0 {
		d.Heartbeat = defaultHeartbeat
	}
	if d.MaxStreamsPerUser <= 0 {
		d.MaxStreamsPerUser = defaultMaxStreams
	}
	if d.MaxLifetime <= 0 {
		d.MaxLifetime = defaultMaxStreamLifetime
	}
	need := rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment}

	httpx.Register(r, huma.Operation{
		OperationID: "listRecentEvents", Method: http.MethodGet, Path: "/api/env/{envId}/events",
		Summary: "The recent Docker events of an environment",
		Description: "The newest 50 container, image, network and volume events Bayplan has seen on this environment, " +
			"newest first, for clients that cannot hold a stream open. Events are kept in memory only: the list is " +
			"empty after a restart until Docker reports something, apart from what the daemon still remembers from " +
			"the last hour. The live version of this list is a server-sent events stream with the same event data.",
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *EnvironmentPathInput) (*EventListOutput, error) {
		rows := d.Aggregator.Recent(ctx, in.EnvID, recentEventsLimit)
		items := make([]EventDTO, 0, len(rows))
		for _, row := range rows {
			items = append(items, eventDTO(row))
		}
		return &EventListOutput{CacheControl: "no-store", Body: EventListBody{Items: items}}, nil
	})

	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/events/stream", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveEventStream(w, req, d, lim)
	}))
}

// serveEventStream is the SSE handler: the buffered backlog, then live events,
// with a heartbeat while nothing happens, until the client leaves, the hub
// closes the subscription, a write fails, the stream has lived MaxLifetime or
// a heartbeat finds it no longer authorised. The policy has already run, so the
// principal and environment are on the context.
//
// The policy ran once, when the stream opened, so every heartbeat runs it again
// through httpx.StillAuthorized: a sign-out, a password change that ends other
// sessions, idle expiry, a disabled user, a removed or disabled environment and
// a withdrawn permission all end the stream within one heartbeat (WR-02). The
// browser's EventSource then reconnects through the full policy and a revoked
// session gets a 401, which the SPA treats as a signed-out session.
func serveEventStream(w http.ResponseWriter, req *http.Request, d EventsDeps, lim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemRateLimited,
			fmt.Sprintf("You already have %d event streams open. Close one and try again.", lim.max))
		return
	}
	defer release()

	backlog, ch, cancel := d.Aggregator.Events(ctx, env.ID)
	defer cancel()
	if last := req.Header.Get("Last-Event-ID"); last != "" {
		// Only what the browser has not seen. An ID the buffer no longer holds
		// sends everything: a repeat is harmless, a gap is not.
		for i := range backlog {
			if backlog[i].ID == last {
				backlog = backlog[i+1:]
				break
			}
		}
	}

	// A stream lives for hours, so the server's other timeouts must not apply
	// to it. There is no global write timeout (server.go); this clears any a
	// proxy-aware wrapper or a future setting adds. A writer that cannot do it
	// (a test recorder) is not an error.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Tells nginx not to buffer the response, which would hold events back until
	// a buffer fills (Pitfall 8). Nothing in Bayplan compresses this response.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}

	send := func(row dockerx.EventRow) bool {
		data, err := json.Marshal(eventDTO(row))
		if err != nil {
			d.Log.Error("encode an event", "error", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: docker\ndata: %s\n\n", row.ID, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	for _, row := range backlog {
		if !send(row) {
			return
		}
	}

	beat := time.NewTicker(d.Heartbeat)
	defer beat.Stop()
	// A stream never outlives MaxLifetime: ending it is how the browser comes
	// back through the whole policy even if a re-check was somehow missed.
	life := time.NewTimer(d.MaxLifetime)
	defer life.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			return
		case row, open := <-ch:
			if !open {
				return
			}
			if !send(row) {
				return
			}
		case <-beat.C:
			if !httpx.StillAuthorized(ctx) {
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// writeJSONProblem answers a plain route with an RFC 9457 body, the shape huma
// and httpx use for their own refusals.
func writeJSONProblem(w http.ResponseWriter, status int, ptype, detail string) {
	body, _ := json.Marshal(&huma.ErrorModel{Type: ptype, Title: http.StatusText(status), Status: status, Detail: detail})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
