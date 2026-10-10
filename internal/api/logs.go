// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bufio"
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

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
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

	// maxLogDownloads is how many downloads of logs one user may run at once; each
	// holds a stream open to the engine for as long as the log takes to copy.
	maxLogDownloads = 2
	// downloadBuffer is the size of the buffer a download is written with, so a log of
	// any size crosses in writes this big and the handler holds no more than this.
	downloadBuffer = 32 << 10

	problemStreamBusy     = "stream_busy"
	problemLogsUnreadable = "logs_unreadable"

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
	// Events subscribes to the merged event feed of an environment; the stream watches
	// it for the start of the container it follows. See envs.Aggregator.Events.
	Events(ctx context.Context, envID string) (backlog []envs.NodeEvent, ch <-chan envs.NodeEvent, cancel func())
}

// LogsDeps are the dependencies of the log stream route.
type LogsDeps struct {
	Aggregator LogsSource
	// Audit records that a stream was opened, never what it showed.
	Audit *audit.Writer
	// Checker decides who may see the values of a log driver's options (secrets.reveal).
	Checker *rbac.Checker
	// Heartbeat is how often an idle stream gets a comment line and has its access
	// checked again. Zero means 15 seconds; tests shorten it.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent log streams per user. Zero means 8.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour; tests
	// shorten it.
	MaxLifetime time.Duration
	// MaxPageBytes is the most one page of earlier lines holds, in bytes of log text.
	// Zero means 8 MiB; tests shorten it.
	MaxPageBytes int
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
	// follow keeps the engine's log open for the lines written from now on.
	follow bool
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

	dl := &streamLimiter{max: maxLogDownloads, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/containers/{containerId}/logs/download", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveLogDownload(w, req, d, dl)
	}))

	httpx.Register(r, huma.Operation{
		OperationID: "getContainerLogs", Method: http.MethodGet, Path: "/api/env/{envId}/containers/{containerId}/logs",
		Summary: "A page of earlier log lines",
		Description: "Up to tail lines of the container's log that were written before until, oldest first, for \"Load earlier lines\". It does not follow the log. " +
			"until is an RFC 3339 time or Unix seconds and is exclusive; leave it out for the newest lines. since, when given, is the earliest time to look back to. " +
			"startOfLogs is true when fewer than tail lines came back because the log has no more, and false when more may be earlier (including when the page was cut to a size limit, newest lines kept). " +
			"To page without losing a line that shares its timestamp with the one before, ask for until one nanosecond after the oldest line held and drop the lines already held by timestamp and text. " +
			"The container lives on one node, named by nodeId, with the same rules and problems as the container detail route (422 node_required, 404 container_not_found, 503 node_unavailable). " +
			"sid is the viewer's ID, the one its live stream uses: a viewer is audited once however many pages it reads. " +
			"Lines are returned as the application wrote them and are never masked. Needs containers.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *ContainerLogsInput) (*ContainerLogsOutput, error) {
		return getContainerLogs(ctx, d, mem, in)
	})
}

// ContainerLogsInput is the path and query of a page of earlier log lines.
type ContainerLogsInput struct {
	EnvironmentPathInput
	ContainerID string `path:"containerId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The container ID or name"`
	NodeID      string `query:"nodeId" maxLength:"128" doc:"The node the container is on; see the container detail route"`
	SID         string `query:"sid" required:"true" pattern:"^[A-Za-z0-9_-]{16,64}$" doc:"The viewer's ID: 16 to 64 letters, digits, dashes or underscores"`
	Until       string `query:"until" maxLength:"40" doc:"Only lines written before this time: an RFC 3339 time or Unix seconds. Left out, the newest lines"`
	Since       string `query:"since" maxLength:"40" doc:"Do not look further back than this time: an RFC 3339 time or Unix seconds"`
	Tail        int    `query:"tail" default:"500" minimum:"1" maximum:"2000" doc:"How many lines at most; 500 by default"`
	UserAgent   string `header:"User-Agent" hidden:"true"`
}

// ContainerLogsBody is a page of log lines.
type ContainerLogsBody struct {
	Lines       []LogLine `json:"lines" doc:"The lines, oldest first. t is the engine's timestamp (UTC, nine digits), s is o for stdout and e for stderr, m is the text as written"`
	StartOfLogs bool      `json:"startOfLogs" doc:"True when there are no earlier lines than these"`
}

// ContainerLogsOutput wraps ContainerLogsBody for huma.
type ContainerLogsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ContainerLogsBody
}

const (
	// defaultPageBytes is the most one page of earlier lines holds, in bytes of log
	// text (the timestamp and the line).
	defaultPageBytes = 8 << 20
	// pageTimeout bounds one page, however many windows it takes.
	pageTimeout = 30 * time.Second
	// pageFirstWindow is how far back the first read of a page looks; each read that
	// finds too few lines looks pageWindowGrowth times further, until a read with no
	// lower bound at all. An engine applies a tail before an until, so a page cannot
	// ask for "the last N lines before T": it reads a window of time instead, and a
	// quiet log costs a few small reads while a busy one costs one.
	pageFirstWindow  = 10 * time.Minute
	pageWindowGrowth = 8
	pageMaxWindow    = 400 * 24 * time.Hour
)

// parseLogBound reads a since or until: an RFC 3339 time or Unix seconds with an
// optional fraction.
func parseLogBound(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC(), nil
	}
	if !logUnixTime.MatchString(v) {
		return time.Time{}, errors.New("not a time")
	}
	sec, frac, _ := strings.Cut(v, ".")
	secs, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	var nanos int64
	if frac != "" {
		nanos, _ = strconv.ParseInt((frac + "000000000")[:9], 10, 64)
	}
	return time.Unix(secs, nanos).UTC(), nil
}

// logPageRing keeps the newest lines offered, up to a count and a size.
type logPageRing struct {
	maxLines, maxBytes int
	lines              []LogLine
	size               int
	// seen counts every line offered; cut says lines were dropped for the size limit.
	seen int
	cut  bool
}

func (r *logPageRing) add(l LogLine) {
	r.seen++
	r.lines = append(r.lines, l)
	r.size += len(l.T) + len(l.M)
	for len(r.lines) > r.maxLines {
		r.drop()
	}
	for r.size > r.maxBytes && len(r.lines) > 1 {
		r.drop()
		r.cut = true
	}
}

func (r *logPageRing) drop() {
	r.size -= len(r.lines[0].T) + len(r.lines[0].M)
	r.lines[0] = LogLine{}
	r.lines = r.lines[1:]
}

// getContainerLogs answers a page of earlier lines (see the route's description).
func getContainerLogs(ctx context.Context, d LogsDeps, mem *logAuditMemory, in *ContainerLogsInput) (*ContainerLogsOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	var until, since time.Time
	var hasUntil, hasSince bool
	var err error
	if in.Until != "" {
		if until, err = parseLogBound(in.Until); err != nil {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValidationFailed, "until must be an RFC 3339 time or Unix seconds")
		}
		hasUntil = true
	}
	if in.Since != "" {
		if since, err = parseLogBound(in.Since); err != nil {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValidationFailed, "since must be an RFC 3339 time or Unix seconds")
		}
		hasSince = true
	}
	agg, nodeID, _, err := resolveContainerNode(ctx, d.Aggregator, d.Log, env.ID, in.NodeID)
	if err != nil {
		return nil, err
	}
	budget := d.MaxPageBytes
	if budget <= 0 {
		budget = defaultPageBytes
	}
	untilStamp := ""
	top := time.Now().UTC()
	if hasUntil {
		untilStamp, top = until.Format(logTimeLayout), until
	}

	ctx, cancel := context.WithTimeout(ctx, pageTimeout)
	defer cancel()
	var ring *logPageRing
	final := false
	opened := false
	for w := pageFirstWindow; ; w *= pageWindowGrowth {
		params := logParams{sid: in.SID, nodeID: nodeID}
		if hasUntil {
			params.until = untilStamp
		}
		low := top.Add(-w)
		switch {
		case hasSince && !low.After(since):
			low, final = since, true
		case w > pageMaxWindow:
			final = true
		}
		if !final || hasSince {
			params.since = low.Format(logTimeLayout)
		}
		ring = &logPageRing{maxLines: in.Tail, maxBytes: budget}
		rc, tty, err := openLogs(ctx, d, env.ID, nodeID, in.ContainerID, params)
		if errors.Is(err, dockerx.ErrLogsUnreadable) {
			return nil, httpx.Problem(http.StatusConflict, problemLogsUnreadable, "Docker can't read this container's logs back.")
		}
		if err != nil {
			return nil, err
		}
		if !opened {
			opened = true
			fullID, name := logContainerRow(agg, nodeID, in.ContainerID)
			auditLogsViewed(ctx, d, mem, p, env.ID, fullID, name, nodeID, logParams{sid: in.SID, since: in.Since, tail: strconv.Itoa(in.Tail)}, in.UserAgent)
		}
		err = pumpLogs(ctx, rc, tty, func(l LogLine) {
			if untilStamp != "" && l.T >= untilStamp {
				return
			}
			ring.add(l)
		})
		_ = rc.Close()
		if err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		if final || ring.seen >= in.Tail || ring.cut {
			break
		}
	}
	lines := ring.lines
	if lines == nil {
		lines = []LogLine{}
	}
	return &ContainerLogsOutput{CacheControl: "no-store", Body: ContainerLogsBody{
		Lines:       lines,
		StartOfLogs: final && !ring.cut && ring.seen < in.Tail,
	}}, nil
}

// logContainerRow finds the full ID and the name of the container a route was asked
// about in the aggregate; without a row both are what was asked.
func logContainerRow(agg envs.Aggregate, nodeID, containerID string) (fullID, name string) {
	for _, row := range agg.Containers {
		if row.NodeID == nodeID && (row.ID == containerID || (len(containerID) >= minSelfIDLen && strings.HasPrefix(row.ID, containerID)) || row.Name == containerID) {
			return row.ID, row.Name
		}
	}
	return containerID, containerID
}

// parseLogParams reads and checks the query. The error is a sentence for the 422.
func parseLogParams(req *http.Request) (logParams, error) {
	q := req.URL.Query()
	p := logParams{follow: true}
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
	// The stamp to resume after comes from the query (after) and from the header a
	// browser sends when it reconnects an event stream (Last-Event-ID). When both are
	// given the later one wins: whichever the client has seen further is where it is.
	// A header that is not a time is ignored, since it is not the page's to get wrong.
	var after time.Time
	if v := q.Get("after"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return p, errors.New("after must be an RFC 3339 time")
		}
		after = t.UTC()
	}
	if v := req.Header.Get("Last-Event-ID"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil && t.After(after) {
			after = t.UTC()
		}
	}
	if !after.IsZero() {
		// A resume asks for everything since the last line, not the last lines.
		p.after = after.Format(logTimeLayout)
		p.since, p.tail = p.after, ""
	}
	return p, nil
}

// openLogs opens the engine's log for the container on the node and maps what can go
// wrong to the problem to answer before the stream starts.
func openLogs(ctx context.Context, d LogsDeps, envID, nodeID, containerID string, p logParams) (rc io.ReadCloser, tty bool, err error) {
	opts := dockerx.LogOptions{Since: p.since, Until: p.until, Tail: p.tail, Follow: p.follow}
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
	case errors.Is(err, dockerx.ErrLogsUnreadable):
		// Not a failure: the caller says so in its own way.
		return nil, false, err
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
	// returns, which also closes the log reader.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The container's own events are subscribed before the log is opened, so a start
	// between the two is not missed (D-20).
	_, events, cancelEvents := d.Aggregator.Events(streamCtx, env.ID)
	defer cancelEvents()
	run, tty, err := openLogRun(streamCtx, d, env.ID, nodeID, containerID, params)
	if errors.Is(err, dockerx.ErrLogsUnreadable) {
		serveUnreadable(streamCtx, w, d, p, env.ID, nodeID, containerID)
		return
	}
	if err != nil {
		writeProblemError(w, err)
		return
	}
	defer func() { run.stop() }()

	fullID, name := logContainerRow(agg, nodeID, containerID)
	auditLogsViewed(ctx, d, mem, p, env.ID, fullID, name, nodeID, params, req.UserAgent())

	rc, send := startSSE(w, d.Log)
	if rc == nil {
		return
	}
	if !send("ready", "", map[string]bool{"tty": tty}) {
		return
	}

	var (
		b     batcher
		track logTrack
		// seam is set while the lines of a reopened log may still repeat ones already
		// sent; see logSeam.
		seam *logSeam
		// lines is the current run's channel; nil once the run has ended.
		lines = run.lines
	)
	flush := func() bool {
		if b.empty() {
			return true
		}
		batch := b.take()
		return send("log", batch[len(batch)-1].T, map[string][]LogLine{"lines": batch})
	}
	// follow reopens the log after the container started again: from the last line
	// sent, with no repeat of it, and tells the viewer (D-20). False means the stream
	// should end; the browser then reconnects through the whole policy.
	follow := func(at time.Time) bool {
		if !flush() {
			return false
		}
		run.stop()
		again := params
		again.after, again.tail = "", ""
		if track.stamp != "" {
			again.since = track.stamp
		}
		next, _, err := openLogRun(streamCtx, d, env.ID, nodeID, containerID, again)
		if err != nil {
			d.Log.Warn("could not follow a container's log across its restart", "container", fullID, "error", err)
			return false
		}
		run, lines = next, next.lines
		seam = track.seam()
		if at.IsZero() {
			at = time.Now()
		}
		return send("state", "", map[string]string{"state": "restarted", "at": at.UTC().Format(time.RFC3339Nano)})
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
		case ev, open := <-events:
			if !open {
				events = nil
				continue
			}
			// Only a start of this container matters: a restart is a stop (the log
			// ends and the viewer is told) and a start. A "restart" event follows its
			// start, so acting on it too would reopen twice.
			if ev.Type != "container" || ev.Action != "start" || !eventIsFor(ev.ActorID, fullID, containerID) {
				continue
			}
			if !follow(ev.Time) {
				return
			}
		case l, open := <-lines:
			if !open {
				if !flush() {
					return
				}
				if run.err != nil && !errors.Is(run.err, context.Canceled) {
					// The connection to the engine broke: end the response, so the
					// browser reconnects and resumes from the last line it has.
					d.Log.Warn("a container log stream ended with an error", "container", fullID, "error", run.err)
					return
				}
				if !send("state", "", map[string]string{"state": "stopped", "at": time.Now().UTC().Format(time.RFC3339Nano)}) {
					return
				}
				lines = nil
				continue
			}
			if params.after != "" && l.T != "" && l.T <= params.after {
				continue
			}
			if seam.skip(l) {
				continue
			}
			track.note(l)
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

// logFileName is the name a downloaded log is saved under: the container's name and
// the minute of the download in UTC. Only letters, digits, underscore, dot and dash
// are kept, so the name can neither leave its quotes nor name a path (T-04-71).
func logFileName(container string, at time.Time) string {
	var b strings.Builder
	for _, r := range container {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 100 {
			break
		}
	}
	name := strings.Trim(b.String(), ".")
	if name == "" {
		name = "container"
	}
	return name + "-" + at.UTC().Format("20060102-1504") + ".log"
}

// logDownloadParams are the validated query of a download.
type logDownloadParams struct {
	nodeID             string
	since, until       time.Time
	hasSince, hasUntil bool
	timestamps         bool
}

func parseLogDownloadParams(req *http.Request) (logDownloadParams, error) {
	q := req.URL.Query()
	var p logDownloadParams
	p.nodeID = q.Get("nodeId")
	if len(p.nodeID) > 128 {
		return p, errors.New("nodeId is too long")
	}
	for name, dst := range map[string]struct {
		t   *time.Time
		has *bool
	}{"since": {&p.since, &p.hasSince}, "until": {&p.until, &p.hasUntil}} {
		v := q.Get(name)
		if v == "" {
			continue
		}
		t, err := parseLogBound(v)
		if err != nil {
			return p, fmt.Errorf("%s must be an RFC 3339 time or Unix seconds", name)
		}
		*dst.t, *dst.has = t, true
	}
	if p.hasSince && p.hasUntil && !p.until.After(p.since) {
		return p, errors.New("until must be after since")
	}
	switch q.Get("timestamps") {
	case "", "0", "false":
	case "1", "true":
		p.timestamps = true
	default:
		return p, errors.New("timestamps must be 1 or 0")
	}
	return p, nil
}

// serveLogDownload streams a container's log for a range as a text file (D-18). The
// engine's answer is copied through fixed buffers line by line as it arrives, so a log
// of any size costs the handler no more than one buffer and one line, and the browser
// saves it to disk as it comes. It never follows the log: the range ends at until, or
// now. Each line is the text as written, with the engine's own RFC 3339 UTC timestamp
// in front when timestamps are asked for; stdout and stderr share the file in the order
// the engine gave them.
//
// One logs.downloaded row is written when the response starts; the text of the log is
// never audited or logged (D-23, D-24). A user runs at most two at once.
func serveLogDownload(w http.ResponseWriter, req *http.Request, d LogsDeps, lim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	dp, err := parseLogDownloadParams(req)
	if err != nil {
		writeJSONProblem(w, http.StatusUnprocessableEntity, problemValidationFailed, err.Error())
		return
	}
	containerID := chi.URLParam(req, "containerId")
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemRateLimited,
			fmt.Sprintf("You already have %d log downloads running. Wait for one to finish and try again.", lim.max))
		return
	}
	defer release()

	agg, nodeID, _, err := resolveContainerNode(ctx, d.Aggregator, d.Log, env.ID, dp.nodeID)
	if err != nil {
		writeProblemError(w, err)
		return
	}
	params := logParams{nodeID: nodeID}
	if dp.hasSince {
		params.since = dp.since.Format(logTimeLayout)
	}
	if dp.hasUntil {
		params.until = dp.until.Format(logTimeLayout)
	}
	// The read ends with the request, and closing the reader is what interrupts a copy
	// that is waiting on the engine.
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	rc, tty, err := openLogs(dlCtx, d, env.ID, nodeID, containerID, params)
	if errors.Is(err, dockerx.ErrLogsUnreadable) {
		writeJSONProblem(w, http.StatusConflict, problemLogsUnreadable, "Docker can't read this container's logs back.")
		return
	}
	if err != nil {
		writeProblemError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	stop := context.AfterFunc(dlCtx, func() { _ = rc.Close() })
	defer stop()

	fullID, name := logContainerRow(agg, nodeID, containerID)
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	until := now.UTC()
	if dp.hasUntil {
		until = dp.until
	}
	auditLogsDownloaded(ctx, d, p, env.ID, fullID, name, nodeID, dp, until, req.UserAgent())

	// A download lasts as long as the log takes to copy, so the server's other timeouts
	// must not cut it.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+logFileName(name, now)+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	out := bufio.NewWriterSize(w, downloadBuffer)
	var werr error
	_ = pumpLogs(dlCtx, rc, tty, func(l LogLine) {
		if werr != nil {
			return
		}
		if dp.timestamps && l.T != "" {
			_, werr = out.WriteString(l.T)
			if werr == nil {
				werr = out.WriteByte(' ')
			}
		}
		if werr == nil {
			_, werr = out.WriteString(l.M)
		}
		if werr == nil {
			werr = out.WriteByte('\n')
		}
		if werr != nil {
			// The browser left: stop reading the engine.
			cancel()
		}
	})
	if werr == nil {
		_ = out.Flush()
	}
}

// eventIsFor says whether the actor of a container event is the container a stream
// follows: its full ID, or the unambiguous prefix the route was asked with.
func eventIsFor(actor, fullID, asked string) bool {
	return actor == fullID || (len(asked) >= minSelfIDLen && strings.HasPrefix(actor, asked))
}

// logRun is one read of the engine's log. The engine ends it when the container
// stops; a container that starts again needs a new one.
type logRun struct {
	cancel context.CancelFunc
	rc     io.ReadCloser
	// lines carries the lines read, oldest first, and is closed when the read ends;
	// err is why, and is only valid after that. nil means a clean end.
	lines chan LogLine
	err   error
}

// openLogRun opens the engine's log and starts reading it into a small channel, so a
// slow browser slows the read instead of growing a buffer. The run lives as long as
// ctx, or until stop.
func openLogRun(ctx context.Context, d LogsDeps, envID, nodeID, containerID string, p logParams) (*logRun, bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	// Opening gets its own time limit.
	openTimer := time.AfterFunc(logOpenTimeout, cancel)
	rc, tty, err := openLogs(runCtx, d, envID, nodeID, containerID, p)
	openTimer.Stop()
	if err != nil {
		cancel()
		return nil, false, err
	}
	r := &logRun{cancel: cancel, rc: rc, lines: make(chan LogLine, 256)}
	go func() {
		defer close(r.lines)
		r.err = pumpLogs(runCtx, rc, tty, func(l LogLine) {
			select {
			case r.lines <- l:
			case <-runCtx.Done():
			}
		})
	}()
	return r, tty, nil
}

// stop ends the read and releases the engine's follow.
func (r *logRun) stop() {
	r.cancel()
	_ = r.rc.Close()
}

// logTrack remembers the newest stamp sent and the lines sent with exactly that stamp,
// so a reopened log can skip them.
type logTrack struct {
	stamp string
	// at holds the key of every line sent with stamp.
	at []string
}

func logLineKey(l LogLine) string { return l.S + "\x00" + l.M }

func (t *logTrack) note(l LogLine) {
	if l.T != t.stamp {
		t.stamp, t.at = l.T, t.at[:0]
	}
	t.at = append(t.at, logLineKey(l))
}

// seam is what skip needs to tell the lines a reopened log repeats from new ones.
func (t *logTrack) seam() *logSeam {
	if t.stamp == "" {
		return nil
	}
	s := &logSeam{stamp: t.stamp, counts: make(map[string]int, len(t.at))}
	for _, k := range t.at {
		s.counts[k]++
	}
	return s
}

// logSeam drops from a reopened log the lines already sent: everything before the
// newest stamp, and at that stamp as many lines of each text as were sent. Comparing
// the text as well as the stamp is what keeps a second line written at the very same
// nanosecond. A nil seam skips nothing.
type logSeam struct {
	stamp  string
	counts map[string]int
}

func (s *logSeam) skip(l LogLine) bool {
	if s == nil || l.T == "" {
		return false
	}
	switch {
	case l.T < s.stamp:
		return true
	case l.T == s.stamp:
		k := logLineKey(l)
		if s.counts[k] > 0 {
			s.counts[k]--
			return true
		}
	}
	return false
}

// startSSE begins a server-sent event response and returns what writes to it: the
// response controller, to flush, and send, which writes one event (with an id when it is
// not empty) and flushes. A stream lives for hours, so the server's other timeouts must
// not apply to it. A nil controller means the response could not be started.
func startSSE(w http.ResponseWriter, log *slog.Logger) (*http.ResponseController, func(event, id string, v any) bool) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Tells nginx not to buffer the response (Pitfall 8).
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return nil, nil
	}
	send := func(event, id string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			log.Error("encode a log event", "event", event, "error", err)
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
	return rc, send
}

// unreadableBody is what the viewer is told when the engine cannot read a container's
// log back (D-25): the driver, and its options with their values withheld from anyone
// who may not reveal secrets.
type unreadableBody struct {
	Driver  string             `json:"driver"`
	Options []redact.OptionVar `json:"options"`
}

// logUnreadable reads the log driver of a container for the notice.
func logUnreadable(ctx context.Context, d LogsDeps, p *rbac.Principal, envID, nodeID, containerID string) (unreadableBody, error) {
	var detail dockerx.ContainerDetail
	err := d.Aggregator.OnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		var e error
		detail, _, e = c.ContainerDetail(ctx, containerID)
		return e
	})
	if err != nil {
		return unreadableBody{}, snapshotProblem(d.Log, envID, err)
	}
	reveal := redact.CanReveal(ctx, d.Checker, p, envID)
	return unreadableBody{Driver: detail.LogConfig.Type, Options: redact.Options(detail.LogConfig.Options, reveal)}, nil
}

// serveUnreadable answers a stream for a container whose log the engine cannot read: it
// sends one unreadable event with the driver and its options and ends. Nothing was
// shown, so nothing is audited. The browser closes its connection when it gets the
// event, so it does not come back.
func serveUnreadable(ctx context.Context, w http.ResponseWriter, d LogsDeps, p *rbac.Principal, envID, nodeID, containerID string) {
	body, err := logUnreadable(ctx, d, p, envID, nodeID, containerID)
	if err != nil {
		writeProblemError(w, err)
		return
	}
	if rc, send := startSSE(w, d.Log); rc != nil {
		send("unreadable", "", body)
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

// auditLogsDownloaded writes the logs.downloaded row of a download: what was asked
// for, never what the log held.
func auditLogsDownloaded(ctx context.Context, d LogsDeps, p *rbac.Principal, envID, containerID, name, nodeID string, dp logDownloadParams, until time.Time, userAgent string) {
	if d.Audit == nil {
		return
	}
	details := map[string]string{
		"container":  name,
		"node":       nodeID,
		"until":      until.UTC().Format(time.RFC3339Nano),
		"timestamps": strconv.FormatBool(dp.timestamps),
	}
	if dp.hasSince {
		details["since"] = dp.since.UTC().Format(time.RFC3339Nano)
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionLogsDownloaded, TargetType: logTargetType, TargetID: containerID,
		EnvID: envID, Result: audit.ResultSuccess,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		d.Log.Error("could not audit a log download", "container", containerID, "error", err)
	}
}
