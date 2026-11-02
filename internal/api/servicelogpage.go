// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// Earlier lines and the merged download of a service's log (plan 06-31, OBS-02, D-35). Both
// read the service's log from one manager without following it, once and to its end: the
// engine ignores until on a service (RESEARCH E2) and applies a tail to each task (E1), so a
// window of time is read by since and cut at until here.

const (
	// maxServerDownloads is how many downloads of service logs run at once on the server,
	// whoever asks (T-06-91).
	maxServerDownloads = 4
	// downloadReadTimeout bounds how long reading the range of one download may take.
	downloadReadTimeout = 15 * time.Minute
	// pageTrimSlack is how many lines beyond twice the page a page reads before it sorts and
	// drops the ones it cannot need.
	pageTrimSlack = 1024
	// downloadCutNotice starts the first line of a download that left lines out.
	downloadCutNotice = "TierPlan: the oldest lines of this range were left out, so this file starts later than asked. It holds the newest "
)

// ServiceLogsInput is the path and query of a page of earlier lines of a service's log.
type ServiceLogsInput struct {
	EnvironmentPathInput
	ServiceID string `path:"serviceId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The service ID or name"`
	SID       string `query:"sid" required:"true" pattern:"^[A-Za-z0-9_-]{16,64}$" doc:"The viewer's ID: 16 to 64 letters, digits, dashes or underscores"`
	Until     string `query:"until" maxLength:"40" doc:"Only lines written before this time: an RFC 3339 time or Unix seconds. Left out, the newest lines"`
	Since     string `query:"since" maxLength:"40" doc:"Do not look further back than this time: an RFC 3339 time or Unix seconds"`
	Tail      int    `query:"tail" default:"500" minimum:"1" maximum:"2000" doc:"How many lines at most; 500 by default"`
	Task      string `query:"task" maxLength:"8300" doc:"Only these tasks: comma-separated task IDs, or the first eight or more characters of one"`
	Node      string `query:"node" maxLength:"8300" doc:"Only tasks on these nodes: comma-separated host names or node IDs"`
	Stopped   bool   `query:"stopped" doc:"Also the lines of tasks that have ended"`
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// ServiceLogsBody is a page of a service's log lines.
type ServiceLogsBody struct {
	Lines       []ServiceLogLine `json:"lines" doc:"The lines, oldest first, merged across tasks. t is the engine's timestamp (UTC, nine digits), s is o for stdout and e for stderr, m is the text as written, task is the task that wrote it, slot its replica number (0 for a global service) and node its host"`
	StartOfLogs bool             `json:"startOfLogs" doc:"True when there are no earlier lines than these"`
}

// ServiceLogsOutput wraps ServiceLogsBody for huma.
type ServiceLogsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceLogsBody
}

// registerServiceLogPages registers the page and the download of a service's log. They share
// the audit memory of the stream, so a viewer is audited once however it reads.
func registerServiceLogPages(r *httpx.Router, d ServiceLogsDeps, need rbac.Requirement, mem *logAuditMemory) {
	httpx.Register(r, huma.Operation{
		OperationID: "getServiceLogs", Method: http.MethodGet, Path: "/api/env/{envId}/services/{serviceId}/logs",
		Summary: "A page of earlier lines of a service's log",
		Description: "Up to tail lines of the service's log, merged across its tasks in time order and oldest first, that were written before until, for \"Load earlier lines\". It does not follow the log. " +
			"until is an RFC 3339 time or Unix seconds and is exclusive; leave it out for the newest lines. since, when given, is the earliest time to look back to. " +
			"startOfLogs is true when fewer than tail lines came back because the log has no more, and false when more may be earlier (including when the page was cut to a size limit, newest lines kept). " +
			"To page without losing a line that shares its timestamp with the one before, ask for until one nanosecond after the oldest line held and drop the lines already held by timestamp, task and text. " +
			"task and node narrow the page to those tasks and nodes as the live stream does. Tasks that have already ended are left out unless stopped is set. " +
			"sid is the viewer's ID, the one its live stream uses: a viewer is audited once however many pages it reads. " +
			"Lines are returned as the application wrote them and are never masked. Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *ServiceLogsInput) (*ServiceLogsOutput, error) {
		return getServiceLogs(ctx, d, mem, in)
	})

	user := &streamLimiter{max: maxLogDownloads, open: map[string]int{}}
	server := &streamLimiter{max: d.MaxServerDownloads, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/services/{serviceId}/logs/download", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveServiceLogDownload(w, req, d, user, server)
	}))
}

// readServiceLog reads the log of a service from one manager, once and to its end, without
// following it, and hands every record to fn in the order the engine gave them (task by task).
// since, when not empty, is a time the engine reads from. A failure to open it, or one on the
// way, is returned as it came.
func (d ServiceLogsDeps) readServiceLog(ctx context.Context, envID, serviceID, since string, fn func(svcRec)) error {
	return d.Clusters.StreamOnManager(ctx, envID, func(c *dockerx.Client) error {
		rc, tty, err := c.ServiceLogs(ctx, serviceID, dockerx.ServiceLogOptions{Since: since})
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		// Closing the reader is what interrupts a read that is waiting on the engine.
		stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
		defer stop()
		var seq uint64
		err = dockerx.ServiceLogFrames(rc, tty, func(stream string, b []byte) {
			if rec, ok := parseSvcRec(stream, b); ok {
				seq++
				rec.seq = seq
				fn(rec)
			}
		})
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	})
}

// pageRec is a record on a page with what sorts it.
type pageRec struct {
	rec  svcRec
	slot int
	node string
}

// svcPageRing keeps the newest lines offered, up to a count and a size. Lines come task by
// task, so it holds some more than it needs and sorts and trims when it has twice as many.
type svcPageRing struct {
	maxLines, maxBytes int
	recs               []pageRec
	size               int
	// seen counts every line offered; cut says lines were dropped for the size limit.
	seen int
	cut  bool
}

func comparePageRec(a, b pageRec) int {
	return cmp.Or(cmp.Compare(a.rec.T, b.rec.T), cmp.Compare(a.slot, b.slot), cmp.Compare(a.rec.seq, b.rec.seq))
}

func (r *svcPageRing) add(p pageRec) {
	r.seen++
	r.recs = append(r.recs, p)
	r.size += recSize(p.rec)
	if len(r.recs) > 2*r.maxLines+pageTrimSlack || r.size > 2*r.maxBytes {
		r.trim()
	}
}

// trim sorts what is held and drops the oldest lines past the count and the size.
func (r *svcPageRing) trim() {
	slices.SortStableFunc(r.recs, comparePageRec)
	drop := 0
	if len(r.recs) > r.maxLines {
		drop = len(r.recs) - r.maxLines
	}
	for _, p := range r.recs[:drop] {
		r.size -= recSize(p.rec)
	}
	for drop < len(r.recs)-1 && r.size > r.maxBytes {
		r.size -= recSize(r.recs[drop].rec)
		drop++
		r.cut = true
	}
	r.recs = slices.Clone(r.recs[drop:])
}

// getServiceLogs answers a page of earlier lines (see the route's description).
func getServiceLogs(ctx context.Context, d ServiceLogsDeps, mem *logAuditMemory, in *ServiceLogsInput) (*ServiceLogsOutput, error) {
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
	params := serviceLogParams{stopped: in.Stopped}
	if params.tasks, err = splitLogFilter("task", in.Task); err != nil {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValidationFailed, err.Error())
	}
	if params.nodes, err = splitLogFilter("node", in.Node); err != nil {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValidationFailed, err.Error())
	}
	if err := requireSwarm(ctx, ServicesDeps{Clusters: d.Clusters, Log: d.Log}, env.ID); err != nil {
		return nil, err
	}
	serviceID, serviceName, err := d.resolveService(ctx, env.ID, in.ServiceID)
	if err != nil {
		return nil, serviceLogProblem(d.Log, env.ID, err)
	}
	budget := cmp.Or(d.MaxPageBytes, defaultPageBytes)
	untilStamp := ""
	top := time.Now().UTC()
	if hasUntil {
		untilStamp, top = until.Format(logTimeLayout), until
	}

	ctx, cancel := context.WithTimeout(ctx, pageTimeout)
	defer cancel()
	meta := newTaskMeta(d.loadTasks(env.ID, serviceID), time.Now)
	meta.ttl = d.TaskRefresh
	// Tasks that have ended are left out unless asked for, as a stream leaves out the ones that
	// had ended when it opened.
	hidden := map[string]bool{}
	if !params.stopped {
		for _, ti := range meta.all(ctx) {
			if ti.Ended {
				hidden[ti.ID] = true
			}
		}
	}
	resolved := map[string]taskInfo{}
	taskOf := func(r svcRec) taskInfo {
		if ti, ok := resolved[r.TaskID]; ok {
			return ti
		}
		ti, known := meta.lookup(ctx, r.TaskID)
		if !known {
			ti = taskInfo{ID: r.TaskID, NodeID: r.NodeID, Node: cleanTaskText(r.NodeID)}
		}
		resolved[r.TaskID] = ti
		return ti
	}

	var ring *svcPageRing
	final := false
	opened := false
	for w := pageFirstWindow; ; w *= pageWindowGrowth {
		low := top.Add(-w)
		switch {
		case hasSince && !low.After(since):
			low, final = since, true
		case w > pageMaxWindow:
			final = true
		}
		sinceStamp := ""
		if !final || hasSince {
			sinceStamp = low.Format(logTimeLayout)
		}
		ring = &svcPageRing{maxLines: in.Tail, maxBytes: budget}
		err := d.readServiceLog(ctx, env.ID, serviceID, sinceStamp, func(r svcRec) {
			if untilStamp != "" && r.T >= untilStamp {
				return
			}
			if hidden[r.TaskID] {
				return
			}
			ti := taskOf(r)
			if !params.matches(ti) {
				return
			}
			ring.add(pageRec{rec: r, slot: ti.Slot, node: cmp.Or(ti.Node, r.NodeID)})
		})
		if err != nil {
			return nil, serviceLogProblem(d.Log, env.ID, err)
		}
		if !opened {
			opened = true
			auditServiceLogsViewed(ctx, d, mem, p, env.ID, serviceID, serviceName,
				serviceLogParams{logParams: logParams{sid: in.SID, since: in.Since, tail: strconv.Itoa(in.Tail)}, tasks: params.tasks, nodes: params.nodes}, in.UserAgent)
		}
		if final || ring.seen >= in.Tail || ring.cut {
			break
		}
	}
	ring.trim()
	lines := make([]ServiceLogLine, 0, len(ring.recs))
	for _, pr := range ring.recs {
		lines = append(lines, ServiceLogLine{T: pr.rec.T, S: pr.rec.S, M: pr.rec.M, Task: pr.rec.TaskID, Slot: pr.slot, Node: pr.node})
	}
	return &ServiceLogsOutput{CacheControl: "no-store", Body: ServiceLogsBody{
		Lines:       lines,
		StartOfLogs: final && !ring.cut && ring.seen < in.Tail,
	}}, nil
}

// serviceLogDownloadParams are the validated query of a download of a service's log.
type serviceLogDownloadParams struct {
	since, until       time.Time
	hasSince, hasUntil bool
}

func parseServiceLogDownloadParams(req *http.Request) (serviceLogDownloadParams, error) {
	var p serviceLogDownloadParams
	q := req.URL.Query()
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
	return p, nil
}

// downloadPrefix is the name a line of a download starts with: the task as the viewer shows it
// beside the service's name, web.2@node3, or web@node3 for a task of a global service.
func downloadPrefix(service string, r svcRec, ti taskInfo, known bool) string {
	if !known {
		short := cleanTaskText(r.TaskID)
		if len(short) > minTaskPrefix {
			short = short[:minTaskPrefix]
		}
		return service + "." + short
	}
	name := service
	if ti.Slot != 0 {
		name += "." + strconv.Itoa(ti.Slot)
	}
	if ti.Node != "" {
		name += "@" + ti.Node
	}
	return name
}

// noticeSize says a size the way the notice of a cut download does.
func noticeSize(n int) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return strconv.Itoa(n>>20) + " MiB"
	}
	return strconv.Itoa(n) + " bytes"
}

// serveServiceLogDownload streams a range of a service's log, merged across its tasks in time
// order, as a text file (D-35). The server reads the engine's one answer, which comes task by
// task, and holds each task's lines (already in order) up to a size; past it the oldest lines
// of any task are left out and the first line of the file says so. The holder is then merged
// k ways and written through one buffer. Each line is the task's name, the UTC time and the
// text as written.
//
// One logs.downloaded row is written once the range has been read and before the file starts;
// the text of the log is never audited or logged. A user runs at most two downloads at once and
// the server four (T-06-91, T-06-92).
func serveServiceLogDownload(w http.ResponseWriter, req *http.Request, d ServiceLogsDeps, userLim, serverLim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	dp, err := parseServiceLogDownloadParams(req)
	if err != nil {
		writeJSONProblem(w, http.StatusUnprocessableEntity, problemValidationFailed, err.Error())
		return
	}
	release, ok := userLim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemStreamBusy,
			fmt.Sprintf("You already have %d log downloads running. Wait for one to finish and try again.", userLim.max))
		return
	}
	defer release()
	releaseServer, ok := serverLim.acquire("server")
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemStreamBusy,
			"TierPlan is already preparing as many log downloads as it can. Try again in a moment.")
		return
	}
	defer releaseServer()

	if err := requireSwarm(ctx, ServicesDeps{Clusters: d.Clusters, Log: d.Log}, env.ID); err != nil {
		writeProblemError(w, err)
		return
	}
	serviceID, serviceName, err := d.resolveService(ctx, env.ID, chi.URLParam(req, "serviceId"))
	if err != nil {
		writeProblemError(w, serviceLogProblem(d.Log, env.ID, err))
		return
	}

	// Reading a range can take longer than the server's write timeout, which started with the
	// request, so it must not cut the response that follows.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	readCtx, cancel := context.WithTimeout(ctx, downloadReadTimeout)
	defer cancel()
	meta := newTaskMeta(d.loadTasks(env.ID, serviceID), time.Now)
	meta.ttl = d.TaskRefresh
	holder := newLineHolder(cmp.Or(d.MaxDownloadBytes, maxDownloadBytes))
	infos := map[string]taskInfo{}
	known := map[string]bool{}
	sinceStamp, untilStamp := "", ""
	if dp.hasSince {
		sinceStamp = dp.since.Format(logTimeLayout)
	}
	if dp.hasUntil {
		untilStamp = dp.until.Format(logTimeLayout)
	}
	err = d.readServiceLog(readCtx, env.ID, serviceID, sinceStamp, func(r svcRec) {
		if (sinceStamp != "" && r.T < sinceStamp) || (untilStamp != "" && r.T >= untilStamp) {
			return
		}
		ti, seen := infos[r.TaskID]
		if !seen {
			var ok bool
			if ti, ok = meta.lookup(readCtx, r.TaskID); ok {
				known[r.TaskID] = true
			}
			infos[r.TaskID] = ti
		}
		holder.add(r, ti.Slot)
	})
	if err != nil {
		writeProblemError(w, serviceLogProblem(d.Log, env.ID, err))
		return
	}

	now := d.Now()
	until := now.UTC()
	if dp.hasUntil {
		until = dp.until
	}
	auditServiceLogsDownloaded(ctx, d, p, env.ID, serviceID, serviceName, dp, until, req.UserAgent())

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+logFileName(serviceName, now)+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	out := bufio.NewWriterSize(w, downloadBuffer)
	var werr error
	put := func(s string) {
		if werr == nil {
			_, werr = io.WriteString(out, s)
		}
	}
	if holder.cut() {
		put(downloadCutNotice + noticeSize(holder.limit) + " of it.\n")
	}
	for r := range kWayMerge(holder.lists()) {
		put(downloadPrefix(serviceName, r, infos[r.TaskID], known[r.TaskID]))
		put(" ")
		put(r.T)
		put(" ")
		put(r.M)
		put("\n")
		if werr != nil {
			// The browser left: stop merging.
			break
		}
	}
	if werr == nil {
		_ = out.Flush()
	}
}

// auditServiceLogsDownloaded writes the logs.downloaded row of a download: what was asked for,
// never what the log held.
func auditServiceLogsDownloaded(ctx context.Context, d ServiceLogsDeps, p *rbac.Principal, envID, serviceID, name string, dp serviceLogDownloadParams, until time.Time, userAgent string) {
	if d.Audit == nil {
		return
	}
	details := map[string]string{
		"service": name,
		"until":   until.UTC().Format(time.RFC3339Nano),
	}
	if dp.hasSince {
		details["since"] = dp.since.UTC().Format(time.RFC3339Nano)
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionLogsDownloaded, TargetType: serviceLogTargetType, TargetID: serviceID,
		EnvID: envID, Result: audit.ResultSuccess,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		d.Log.Error("could not audit a service log download", "service", serviceID, "error", err)
	}
}
