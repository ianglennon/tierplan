// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// Limits and problem types of the log stream.
const (
	// defaultLogTail is how many of the last lines a stream starts with, and
	// maxLogTail the most it may ask for (tail=all means this many).
	defaultLogTail = 500
	maxLogTail     = 50_000
	// logOpenTimeout bounds how long opening the engine's log may take.
	logOpenTimeout = 30 * time.Second

	problemStreamBusy = "stream_busy"

	logTargetType = "container"
)

var (
	// logSID is the shape of the viewer ID a page makes for itself: 16 to 64 URL-safe
	// characters.
	logSID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	// logUnixTime is a Unix time in seconds with an optional fraction, one of the
	// forms of since and until.
	logUnixTime = regexp.MustCompile(`^\d{1,12}(\.\d{1,9})?$`)
)

// logTimeLayout is the engine's own layout for the stamp of a line: UTC with nine
// digits, so stamps sort as text.
const logTimeLayout = "2006-01-02T15:04:05.000000000Z"

// LogsSource is what the log stream reads: the aggregate of an environment, to find
// the node of a container, and that node's engine. *envs.Aggregator implements it.
type LogsSource interface {
	snapshotReader
	// OnNode runs fn against the engine of a node; see envs.Aggregator.OnNode.
	OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
}

// LogsDeps are the dependencies of the log stream route.
type LogsDeps struct {
	Aggregator LogsSource
	// Audit records that a stream was opened, never what it showed.
	Audit *audit.Writer
	// Heartbeat is how often an idle stream gets a comment line and has its access
	// checked again. Zero means 15 seconds; tests shorten it.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent log streams per user. Zero means 8.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour; tests
	// shorten it.
	MaxLifetime time.Duration
	// Now is the clock of the audit memory. Nil means the wall clock.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default(). It is never given a
	// line of a container's log (D-23).
	Log *slog.Logger
}

// logParams are the validated query of a log stream request.
type logParams struct {
	sid, nodeID string
	// since, until and tail are what the engine is asked for. After is the stamp of the
	// last line the client has, normalised to the engine's layout: the stream resumes
	// there and sends nothing at or before it.
	since, until, tail, after string
}

// RegisterLogs registers the live log stream of a container under /api/env/{envId}/.
// It is mounted through Router.Guarded because the contract cannot describe a stream
// (D-47), and needs containers.read on the environment.
func RegisterLogs(r *httpx.Router, d LogsDeps) {
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
	need := rbac.Requirement{Perm: rbac.PermContainersRead, Scope: rbac.ScopeEnvironment}
	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	mem := newLogAuditMemory(logAuditTTL, logAuditMax, d.Now)
	r.Guarded(http.MethodGet, "/api/env/{envId}/containers/{containerId}/logs/stream", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveLogStream(w, req, d, lim, mem)
	}))
}

// parseLogParams reads and checks the query. The error is a sentence for the 422.
func parseLogParams(req *http.Request) (logParams, error) {
	q := req.URL.Query()
	var p logParams
	p.sid = q.Get("sid")
	if !logSID.MatchString(p.sid) {
		return p, errors.New("sid must be 16 to 64 letters, digits, dashes or underscores")
	}
	p.nodeID = q.Get("nodeId")
	if len(p.nodeID) > 128 {
		return p, errors.New("nodeId is too long")
	}
	for name, dst := range map[string]*string{"since": &p.since, "until": &p.until} {
		v := q.Get(name)
		if v == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, v); err != nil && !logUnixTime.MatchString(v) {
			return p, fmt.Errorf("%s must be an RFC 3339 time or Unix seconds", name)
		}
		*dst = v
	}
	p.tail = strconv.Itoa(defaultLogTail)
	if v := q.Get("tail"); v != "" {
		if v == "all" {
			p.tail = strconv.Itoa(maxLogTail)
		} else {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return p, errors.New("tail must be a number of lines or all")
			}
			p.tail = strconv.Itoa(min(n, maxLogTail))
		}
	}
	after := q.Get("after")
	if after == "" {
		after = req.Header.Get("Last-Event-ID")
	}
	if after != "" {
		t, err := time.Parse(time.RFC3339Nano, after)
		if err != nil {
			return p, errors.New("after must be an RFC 3339 time")
		}
		// A resume asks for everything since the last line, not the last lines.
		p.after = t.UTC().Format(logTimeLayout)
		p.since, p.tail = p.after, ""
	}
	return p, nil
}

// openLogs opens the engine's log for the container on the node and maps what can go
// wrong to the problem to answer before the stream starts.
func openLogs(ctx context.Context, d LogsDeps, envID, nodeID, containerID string, p logParams) (rc io.ReadCloser, tty bool, err error) {
	opts := dockerx.LogOptions{Since: p.since, Until: p.until, Tail: p.tail, Follow: true}
	err = d.Aggregator.OnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		var e error
		rc, tty, e = c.ContainerLogs(ctx, containerID, opts)
		return e
	})
	switch {
	case err == nil:
		return rc, tty, nil
	case errors.Is(err, tunnel.ErrBusy):
		return nil, false, httpx.Problem(http.StatusServiceUnavailable, problemStreamBusy,
			"This host's agent is serving as many logs and shells as it can. Close one and try again.")
	case dockerx.IsNotFound(err):
		return nil, false, httpx.Problem(http.StatusNotFound, problemContainerNotFound, "This container does not exist on that node. It may have been removed.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		return nil, false, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Bayplan.")
	default:
		return nil, false, snapshotProblem(d.Log, envID, err)
	}
}

// serveLogStream is the SSE handler of a container's log. The policy has already run
// when it is called, so the principal and the environment are on the context.
//
// The stream starts with a ready event ({tty}), then sends log events, each a batch of
// lines {t, s, m} with the stamp of the last line as the event ID, so a reconnect of
// the browser resumes from there. When the engine ends the log (the container
// stopped) it sends a state event and keeps the stream for the heartbeats. Every
// heartbeat re-checks access; a user who lost it is told with a closed event, and the
// stream never outlives MaxLifetime.
//
// No line of the log is stored, audited or written to Bayplan's own log; only the
// fact that a stream was opened is recorded (D-23, D-24).
func serveLogStream(w http.ResponseWriter, req *http.Request, d LogsDeps, lim *streamLimiter, mem *logAuditMemory) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	params, err := parseLogParams(req)
	if err != nil {
		writeJSONProblem(w, http.StatusUnprocessableEntity, problemValidationFailed, err.Error())
		return
	}
	containerID := chi.URLParam(req, "containerId")
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemRateLimited,
			fmt.Sprintf("You already have %d log streams open. Close one and try again.", lim.max))
		return
	}
	defer release()

	agg, nodeID, _, err := resolveContainerNode(ctx, d.Aggregator, d.Log, env.ID, params.nodeID)
	if err != nil {
		writeProblemError(w, err)
		return
	}

	// The engine request lives as long as the stream; it is cancelled when the handler
	// returns, which also closes the log reader. Opening it gets its own time limit.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	openTimer := time.AfterFunc(logOpenTimeout, cancel)
	logs, tty, err := openLogs(streamCtx, d, env.ID, nodeID, containerID, params)
	openTimer.Stop()
	if err != nil {
		writeProblemError(w, err)
		return
	}
	defer func() { _ = logs.Close() }()

	fullID, name := containerID, containerID
	for _, row := range agg.Containers {
		if row.NodeID == nodeID && (row.ID == containerID || (len(containerID) >= minSelfIDLen && strings.HasPrefix(row.ID, containerID)) || row.Name == containerID) {
			fullID, name = row.ID, row.Name
			break
		}
	}
	auditLogsViewed(ctx, d, mem, p, env.ID, fullID, name, nodeID, params, req.UserAgent())

	// A stream lives for hours, so the server's other timeouts must not apply to it.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Tells nginx not to buffer the response (Pitfall 8).
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	send := func(event, id string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			d.Log.Error("encode a log event", "event", event, "error", err)
			return false
		}
		if id != "" {
			if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
				return false
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("ready", "", map[string]bool{"tty": tty}) {
		return
	}

	// The pump reads the engine into a small channel, so a slow browser slows the
	// read instead of growing a buffer.
	lines := make(chan LogLine, 256)
	var pumpErr error
	go func() {
		defer close(lines)
		pumpErr = pumpLogs(streamCtx, logs, tty, func(l LogLine) {
			if params.after != "" && l.T != "" && l.T <= params.after {
				return
			}
			select {
			case lines <- l:
			case <-streamCtx.Done():
			}
		})
	}()

	var b batcher
	flush := func() bool {
		if b.empty() {
			return true
		}
		batch := b.take()
		return send("log", batch[len(batch)-1].T, map[string][]LogLine{"lines": batch})
	}
	flushTick := time.NewTicker(batchInterval)
	defer flushTick.Stop()
	beat := time.NewTicker(d.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(d.MaxLifetime)
	defer life.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			// The browser reconnects through the whole policy and resumes.
			flush()
			return
		case l, open := <-lines:
			if !open {
				if !flush() {
					return
				}
				if pumpErr != nil {
					// The connection to the engine broke: end the response, so the
					// browser reconnects and resumes from the last line it has.
					d.Log.Warn("a container log stream ended with an error", "container", fullID, "error", pumpErr)
					return
				}
				if !send("state", "", map[string]string{"state": "stopped", "at": time.Now().UTC().Format(time.RFC3339Nano)}) {
					return
				}
				lines = nil
				continue
			}
			if b.add(l) && !flush() {
				return
			}
		case <-flushTick.C:
			if !flush() {
				return
			}
		case <-beat.C:
			if !httpx.StillAuthorized(ctx) {
				flush()
				send("closed", "", map[string]string{"reason": "access_lost"})
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// auditLogsViewed writes the logs.viewed row of a stream, once per viewer: a
// reconnect that carries the same sid writes nothing (D-24).
func auditLogsViewed(ctx context.Context, d LogsDeps, mem *logAuditMemory, p *rbac.Principal, envID, containerID, name, nodeID string, params logParams, userAgent string) {
	if d.Audit == nil || mem.seen(logAuditKey{user: string(p.Type) + ":" + p.ID, container: containerID, sid: params.sid}) {
		return
	}
	details := map[string]string{"container": name, "node": nodeID, "sid": params.sid, "tail": params.tail}
	if params.since != "" {
		details["since"] = params.since
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionLogsViewed, TargetType: logTargetType, TargetID: containerID,
		EnvID: envID, Result: audit.ResultSuccess,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		// The viewer must still get the log; the lost row is the server's fault.
		d.Log.Error("could not audit a log stream", "container", containerID, "error", err)
	}
}
