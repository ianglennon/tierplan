// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
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

// The merged log stream of one swarm service (plan 06-29, OBS-02, D-35, D-37). One
// ServiceLogs stream is opened on one manager and followed there; its records name their task,
// the burst it opens with is sorted as a whole and the live lines are put in time order.

const (
	// burstQuiet is how long the stream waits without a record before it takes the burst the
	// engine sent as complete (the engine marks no end), and burstMax the longest it waits for
	// that whatever arrives.
	burstQuiet = 300 * time.Millisecond
	burstMax   = 5 * time.Second
	// maxBurstRecords and maxBurstBytes bound what a burst holds while it is collected; a
	// burst that reaches either is taken as complete there.
	maxBurstRecords = 100_000
	maxBurstBytes   = 32 << 20
	// windowTick is how often the reorder window is looked at for lines that are due.
	windowTick = 50 * time.Millisecond
	// maxLogFilters is the most task or node names a stream may be narrowed to.
	maxLogFilters = 64
	// minTaskPrefix is the fewest characters of a task ID that name it.
	minTaskPrefix = 8

	serviceLogTargetType = "service"
	problemTaskNotFound  = "task_not_found"
)

// ServiceLogsDeps are the dependencies of the service log stream route.
type ServiceLogsDeps struct {
	// Clusters reads the service and its tasks, and runs the log follow once on the manager
	// it chooses (StreamOnManager).
	Clusters *envs.Clusters
	// Audit records that a stream was opened, never what it showed.
	Audit *audit.Writer
	// Checker is the policy the route is registered with.
	Checker *rbac.Checker
	// Heartbeat is how often an idle stream gets a comment line and has its access checked
	// again. Zero means 15 seconds; tests shorten it.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent log streams per user. Zero means 8.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour.
	MaxLifetime time.Duration
	// TaskRefresh is how often the service's tasks and the state of their nodes are read again,
	// to announce tasks that join, mark a task that was replaced and tell of a node that cannot
	// be reached (D-34, D-37). Zero means 3 seconds; tests shorten it.
	TaskRefresh time.Duration
	// MaxPageBytes is the most one page of earlier lines holds, in bytes of log text. Zero means
	// 8 MiB; tests shorten it.
	MaxPageBytes int
	// MaxDownloadBytes is the most one merged download holds while it is merged. Zero means 32
	// MiB; tests shorten it.
	MaxDownloadBytes int
	// MaxServerDownloads caps the downloads of service logs running at once on the server, whoever
	// asks. Zero means 4; tests shorten it.
	MaxServerDownloads int
	// Now is the clock of the audit memory and of a download's file name. Nil means the wall
	// clock. The reorder window and the task cache measure real time and do not use it.
	Now func() time.Time
	// BurstEnded, when set, is called each time a stream takes the burst the engine opened with
	// as complete, from the stream's own goroutine, once the live window has taken over. It
	// exists so a test can tell that what follows is live without waiting out the quiet period;
	// production leaves it nil.
	BurstEnded func()
	// Log receives server-side failures. Nil means slog.Default(). It is never given a line of
	// a service's log.
	Log *slog.Logger
}

// serviceLogParams are the validated query of a service log stream request.
type serviceLogParams struct {
	logParams
	// stopped keeps the tasks that had ended when the stream opened.
	stopped bool
	// tasks and nodes narrow the stream to those tasks (ID, or the start of one of at least
	// eight characters) and those nodes (host name or ID); empty means all.
	tasks, nodes []string
}

// RegisterServiceLogs registers the live log stream of a service under /api/env/{envId}/. It
// is mounted through Router.Guarded because the contract cannot describe a stream (D-47), and
// needs services.read on the environment.
func RegisterServiceLogs(r *httpx.Router, d ServiceLogsDeps) {
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
	if d.TaskRefresh <= 0 {
		d.TaskRefresh = taskMetaTTL
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.MaxServerDownloads <= 0 {
		d.MaxServerDownloads = maxServerDownloads
	}
	need := rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment}
	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	mem := newLogAuditMemory(logAuditTTL, logAuditMax, d.Now)
	r.Guarded(http.MethodGet, "/api/env/{envId}/services/{serviceId}/logs/stream", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveServiceLogStream(w, req, d, lim, mem)
	}))
	registerServiceLogPages(r, d, need, mem)
}

// parseServiceLogParams reads the query of the container log stream (sid, since, tail, after)
// and what a service adds: stopped, task and node. The error is a sentence for the 422.
func parseServiceLogParams(req *http.Request) (serviceLogParams, error) {
	lp, err := parseLogParams(req)
	if err != nil {
		return serviceLogParams{}, err
	}
	p := serviceLogParams{logParams: lp}
	q := req.URL.Query()
	switch q.Get("stopped") {
	case "", "0", "false":
	case "1", "true":
		p.stopped = true
	default:
		return p, errors.New("stopped must be 1 or 0")
	}
	for name, dst := range map[string]*[]string{"task": &p.tasks, "node": &p.nodes} {
		names, err := splitLogFilter(name, q.Get(name))
		if err != nil {
			return p, err
		}
		*dst = names
	}
	return p, nil
}

// splitLogFilter reads a comma list of task or node names (what is the filter's kind, for the
// sentence of the 422).
func splitLogFilter(what, raw string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if len(part) > 128 {
			return nil, fmt.Errorf("a %s name is too long", what)
		}
		out = append(out, part)
	}
	if len(out) > maxLogFilters {
		return nil, fmt.Errorf("at most %d %s names may be given", maxLogFilters, what)
	}
	return out, nil
}

// ServiceLogTask is one task of the service the stream follows, as the ready and tasks events
// carry it.
type ServiceLogTask struct {
	ID    string `json:"id"`
	Slot  int    `json:"slot"`
	Node  string `json:"node"`
	State string `json:"state"`
}

// serviceLogTasks is the body of the ready and tasks events.
type serviceLogTasks struct {
	TTY   bool             `json:"tty"`
	Tasks []ServiceLogTask `json:"tasks"`
}

// ServiceLogLine is one line of a service's merged log: LogLine and the task that wrote it.
type ServiceLogLine struct {
	T    string `json:"t"`
	S    string `json:"s"`
	M    string `json:"m"`
	Task string `json:"task"`
	Slot int    `json:"slot"`
	Node string `json:"node"`
}

// svcBatch collects lines for one log event, as batcher does for a container's.
type svcBatch struct {
	lines []ServiceLogLine
	size  int
}

func (b *svcBatch) add(l ServiceLogLine) bool {
	b.lines = append(b.lines, l)
	b.size += len(l.M) + len(l.T) + len(l.Node) + len(l.Task) + 16
	return len(b.lines) >= batchLines || b.size >= batchBytes
}

func (b *svcBatch) take() []ServiceLogLine {
	out := b.lines
	b.lines, b.size = nil, 0
	return out
}

// matches says whether a task passes the task and node filters.
func (p serviceLogParams) matches(ti taskInfo) bool {
	if len(p.tasks) > 0 && !slices.ContainsFunc(p.tasks, func(f string) bool {
		return ti.ID == f || (len(f) >= minTaskPrefix && strings.HasPrefix(ti.ID, f))
	}) {
		return false
	}
	if len(p.nodes) > 0 && !slices.ContainsFunc(p.nodes, func(f string) bool { return ti.Node == f || ti.NodeID == f }) {
		return false
	}
	return true
}

// matchesMarker says whether a marker concerns a task the stream is narrowed to: its old task
// or the one that took over.
func (p serviceLogParams) matchesMarker(m slotMarker, byID map[string]taskInfo) bool {
	if old, ok := byID[m.Old]; ok && p.matches(old) {
		return true
	}
	if next, ok := byID[m.New]; ok && m.New != "" && p.matches(next) {
		return true
	}
	return false
}

// followedNodes are the nodes that run a task the stream follows: one it announced and that has
// not ended, since a task that ended writes nothing more, whatever its node does.
func followedNodes(tasks []taskInfo, announced map[string]ServiceLogTask) []nodeInfo {
	seen := map[string]bool{}
	var out []nodeInfo
	for _, ti := range tasks {
		if _, ok := announced[ti.ID]; !ok || ti.Ended || ti.NodeID == "" || seen[ti.NodeID] {
			continue
		}
		seen[ti.NodeID] = true
		out = append(out, nodeInfo{ID: ti.NodeID, Name: ti.Node, State: ti.NodeState})
	}
	return out
}

// dueMarker is a marker waiting for the reorder window to pass, so the lines of the task it
// ends are sent before it.
type dueMarker struct {
	m   slotMarker
	due time.Time
}

// loadTasks reads the tasks of a service and the nodes they are on in one cluster read and
// the cluster reader's last node list.
func (d ServiceLogsDeps) loadTasks(envID, serviceID string) func(context.Context) ([]taskInfo, error) {
	return func(ctx context.Context) ([]taskInfo, error) {
		return readTaskInfos(ctx, d.Clusters, envID, []string{serviceID}, nil)
	}
}

// readTaskInfos reads the tasks of the given services and the nodes they are on in one cluster
// read and the cluster reader's last node list. names says by service ID what each task carries
// as the name of its service; a service it does not name leaves the task's empty.
func readTaskInfos(ctx context.Context, clusters *envs.Clusters, envID string, serviceIDs []string, names map[string]string) ([]taskInfo, error) {
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	var rows []dockerx.TaskRow
	err := clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		rows, e = c.Tasks(readCtx, dockerx.TaskFilter{ServiceIDs: serviceIDs})
		return e
	})
	if err != nil {
		return nil, err
	}
	nodes := map[string]dockerx.NodeRow{}
	if st, err := clusters.State(readCtx, envID); err == nil {
		for _, n := range st.Read.Nodes {
			nodes[n.ID] = n
		}
	}
	out := make([]taskInfo, 0, len(rows))
	for _, r := range rows {
		n := nodes[r.NodeID]
		host := cleanTaskText(n.Hostname)
		if host == "" {
			host = r.NodeID
		}
		out = append(out, taskInfo{
			ID: r.ID, Service: names[r.ServiceID], Slot: r.Slot, NodeID: r.NodeID, Node: host,
			State: effectiveTaskState(r, n.State), Ended: taskEnded(r.State),
			NodeState: n.State, Created: r.CreatedAt, Since: cmp.Or(r.StateSince, r.UpdatedAt), ExitCode: r.ExitCode,
		})
	}
	return out, nil
}

// resolveService finds the service a route was asked about, in the same cluster read that
// confirms the environment has a manager to ask.
func (d ServiceLogsDeps) resolveService(ctx context.Context, envID, ref string) (id, name string, err error) {
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err = d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		detail, _, e := c.ServiceInspect(readCtx, ref)
		id, name = detail.ID, detail.Name
		return e
	})
	return id, name, err
}

// serviceLogProblem turns the error of opening or reading a service log into the problem to
// answer before the stream starts.
func serviceLogProblem(log *slog.Logger, envID string, err error) error {
	switch {
	case errors.Is(err, tunnel.ErrBusy):
		return httpx.Problem(http.StatusServiceUnavailable, problemStreamBusy,
			"This swarm's manager agent is serving as many logs and shells as it can. Close one and try again.")
	case errors.Is(err, dockerx.ErrTaskNotFound):
		return httpx.Problem(http.StatusNotFound, problemTaskNotFound, "This task does not exist. It may have been removed.")
	}
	return serviceReadProblem(ServicesDeps{Log: log}, envID, err)
}

// svcRun is one read of a service's log on one manager.
type svcRun struct {
	cancel context.CancelFunc
	// records carries the records read, oldest first as the engine gave them, and is closed
	// when the read ends; err is why, and is only valid after that. nil means a clean end.
	records chan svcRec
	err     error
}

// stop ends the read and releases the engine's follow.
func (r *svcRun) stop() { r.cancel() }

// openServiceLogRun opens the service's log (or one task's) on a manager and starts reading
// it into a small channel, so a slow browser slows the read instead of growing a buffer. It
// returns once the engine has answered, with whether the service has a TTY. The run lives as
// long as ctx, or until stop.
func openServiceLogRun(ctx context.Context, d ServiceLogsDeps, envID, serviceID, taskID string, opts dockerx.ServiceLogOptions) (*svcRun, bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	r := &svcRun{cancel: cancel, records: make(chan svcRec, 256)}
	type opened struct {
		tty bool
		err error
	}
	answer := make(chan opened, 1)
	timer := time.AfterFunc(logOpenTimeout, cancel)
	go func() {
		defer close(r.records)
		sent := false
		err := d.Clusters.StreamOnManager(runCtx, envID, func(c *dockerx.Client) error {
			var (
				rc  io.ReadCloser
				tty bool
				e   error
			)
			if taskID != "" {
				rc, tty, e = c.TaskLogs(runCtx, taskID, opts)
			} else {
				rc, tty, e = c.ServiceLogs(runCtx, serviceID, opts)
			}
			if e != nil {
				return e
			}
			defer func() { _ = rc.Close() }()
			stop := context.AfterFunc(runCtx, func() { _ = rc.Close() })
			defer stop()
			sent = true
			answer <- opened{tty: tty}
			var seq uint64
			return dockerx.ServiceLogFrames(rc, tty, func(stream string, b []byte) {
				rec, ok := parseSvcRec(stream, b)
				if !ok {
					return
				}
				seq++
				rec.seq = seq
				select {
				case r.records <- rec:
				case <-runCtx.Done():
				}
			})
		})
		if !sent {
			if err == nil {
				err = errors.New("the service log was not opened")
			}
			answer <- opened{err: err}
			return
		}
		r.err = err
	}()
	res := <-answer
	timer.Stop()
	if res.err != nil {
		cancel()
		return nil, false, res.err
	}
	return r, res.tty, nil
}

// parseSvcRec reads one frame of a service log into a record. A frame that is not a record of
// a service log may hold application text, so it is dropped without being logged.
func parseSvcRec(stream string, b []byte) (svcRec, bool) {
	rec, err := dockerx.ParseServiceLogRecord(string(b))
	if err != nil {
		return svcRec{}, false
	}
	s := streamOut
	if stream == "stderr" {
		s = streamErr
	}
	return svcRec{T: rec.T.Format(logTimeLayout), S: s, M: rec.Message, TaskID: rec.TaskID, NodeID: rec.NodeID}, true
}

// serveServiceLogStream is the SSE handler of a service's merged log. The policy has already
// run when it is called, so the principal and the environment are on the context.
//
// The stream starts with a ready event ({tty, tasks}), then sends log events, each a batch of
// lines {t, s, m, task, slot, node} with the stamp of the last line as the event ID, so a
// reconnect of the browser resumes from there. A tasks event follows when a task is first seen
// after the stream opened. The first lines are the engine's burst, which it gives task by task:
// they are sorted as a whole and the newest tail kept. Live lines are held for 500 ms and sent
// in time order. When the engine ends the log it sends a state event and keeps the stream for
// the heartbeats. Every heartbeat re-checks access; a user who lost it is told with a closed
// event, and the stream never outlives MaxLifetime.
//
// No line of the log is stored, audited or written to Bayplan's own log; only the fact that a
// stream was opened is recorded (D-35).
func serveServiceLogStream(w http.ResponseWriter, req *http.Request, d ServiceLogsDeps, lim *streamLimiter, mem *logAuditMemory) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	params, err := parseServiceLogParams(req)
	if err != nil {
		writeJSONProblem(w, http.StatusUnprocessableEntity, problemValidationFailed, err.Error())
		return
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemRateLimited,
			fmt.Sprintf("You already have %d log streams open. Close one and try again.", lim.max))
		return
	}
	defer release()

	if d.Clusters == nil {
		d.Log.Error("the service log route has no cluster reader")
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if err := requireSwarm(ctx, ServicesDeps{Clusters: d.Clusters, Log: d.Log}, env.ID); err != nil {
		writeProblemError(w, err)
		return
	}
	serviceID, serviceName, err := d.resolveService(ctx, env.ID, chi.URLParam(req, "serviceId"))
	if err != nil {
		writeProblemError(w, serviceLogProblem(d.Log, env.ID, err))
		return
	}

	meta := newTaskMeta(d.loadTasks(env.ID, serviceID), time.Now)
	meta.ttl = d.TaskRefresh
	initial := meta.all(ctx)
	// Tasks that had ended when the stream opened are left out, unless asked for; tasks
	// first seen later are kept (D-33).
	hidden := map[string]bool{}
	if !params.stopped {
		for _, ti := range initial {
			if ti.Ended {
				hidden[ti.ID] = true
			}
		}
	}
	announced := map[string]ServiceLogTask{}
	for _, ti := range initial {
		if !hidden[ti.ID] && params.matches(ti) {
			announced[ti.ID] = ServiceLogTask{ID: ti.ID, Slot: ti.Slot, Node: ti.Node, State: ti.State}
		}
	}
	tasksList := func() []ServiceLogTask {
		out := make([]ServiceLogTask, 0, len(announced))
		for _, t := range announced {
			out = append(out, t)
		}
		slices.SortFunc(out, func(a, b ServiceLogTask) int {
			return cmp.Or(cmp.Compare(a.Slot, b.Slot), cmp.Compare(a.Node, b.Node), cmp.Compare(a.ID, b.ID))
		})
		return out
	}

	// The engine request lives as long as the stream; it is cancelled when the handler
	// returns, which also closes the log reader. With one task named, the task's own log is
	// read.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	taskID := ""
	if len(params.tasks) == 1 {
		for _, ti := range initial {
			if params.matches(ti) {
				taskID = ti.ID
				break
			}
		}
	}
	run, tty, err := openServiceLogRun(streamCtx, d, env.ID, serviceID, taskID, dockerx.ServiceLogOptions{Follow: true, Since: params.since, Tail: params.tail})
	if err != nil {
		writeProblemError(w, serviceLogProblem(d.Log, env.ID, err))
		return
	}
	defer run.stop()

	auditServiceLogsViewed(ctx, d, mem, p, env.ID, serviceID, serviceName, params, req.UserAgent())

	rc, send := startSSE(w, d.Log)
	if rc == nil {
		return
	}
	if !send("ready", "", serviceLogTasks{TTY: tty, Tasks: tasksList()}) {
		return
	}

	keepN := maxLogTail
	if params.after == "" {
		keepN, _ = strconv.Atoi(params.tail)
	}
	var (
		b       svcBatch
		burst   []svcRec
		bytes   int
		window  = newReorderWindow(reorderHold, time.Now, func(id string) int { return slotOrLast(meta.byIDMap(), id) })
		records = run.records
		// bursting is true until the engine's first answer is complete.
		bursting = true
	)
	// prevTasks and prevNodes are what the markers and notices were last worked out from.
	var (
		prevTasks = initial
		prevNodes []nodeInfo
		pending   []dueMarker
	)
	flush := func() bool {
		if len(b.lines) == 0 {
			return true
		}
		batch := b.take()
		return send("log", batch[len(batch)-1].T, map[string][]ServiceLogLine{"lines": batch})
	}
	// unresolved are the tasks announced from a record alone, before the cluster listed them.
	unresolved := map[string]bool{}
	// emit adds a record to the batch, announcing its task when it is the first of it, or when
	// the cluster has listed it since it was announced from the record alone.
	emit := func(r svcRec) bool {
		ti, known := meta.lookup(streamCtx, r.TaskID)
		_, seen := announced[r.TaskID]
		if !seen || (unresolved[r.TaskID] && known) {
			if !known {
				ti = taskInfo{ID: r.TaskID, NodeID: r.NodeID, Node: r.NodeID}
				unresolved[r.TaskID] = true
			} else {
				delete(unresolved, r.TaskID)
			}
			announced[r.TaskID] = ServiceLogTask{ID: ti.ID, Slot: ti.Slot, Node: ti.Node, State: ti.State}
			if !flush() || !send("tasks", "", serviceLogTasks{TTY: tty, Tasks: tasksList()}) {
				return false
			}
		}
		node := ti.Node
		if node == "" {
			node = r.NodeID
		}
		line := ServiceLogLine{T: r.T, S: r.S, M: r.M, Task: r.TaskID, Slot: ti.Slot, Node: node}
		return !b.add(line) || flush()
	}
	// keep says whether a record is one the stream shows: after the resume point, of a task
	// that was not hidden and passes the filters.
	keep := func(r svcRec) bool {
		if params.after != "" && r.T <= params.after {
			return false
		}
		if hidden[r.TaskID] {
			return false
		}
		ti, known := meta.lookup(streamCtx, r.TaskID)
		if !known {
			ti = taskInfo{ID: r.TaskID, NodeID: r.NodeID, Node: r.NodeID}
		}
		return params.matches(ti)
	}
	endBurst := func() bool {
		bursting = false
		if d.BurstEnded != nil {
			defer d.BurstEnded()
		}
		sorted := mergeBurst(burst, meta.byIDMap(), keepN)
		burst = nil
		for _, r := range sorted {
			if !emit(r) {
				return false
			}
		}
		if n := len(sorted); n > 0 {
			window.seed(sorted[n-1].T)
		}
		if !flush() {
			return false
		}
		// What happened before the stream opened is told after its lines: the markers of the
		// tasks that had ended, when they are asked for and fall in what is shown (a marker
		// older than the first line has nothing to sit beside), and the nodes that could not
		// be reached already (D-34, D-37).
		if params.stopped && len(sorted) > 0 {
			byID := byTaskID(initial)
			for _, m := range slotMarkers(nil, initial) {
				if m.At == "" || m.At < sorted[0].T || !params.matchesMarker(m, byID) {
					continue
				}
				if !send("marker", "", m) {
					return false
				}
			}
		}
		prevNodes = followedNodes(initial, announced)
		for _, n := range nodeNotices(nil, prevNodes, time.Now().UTC().Format(logTimeLayout)) {
			if !send("node", "", n) {
				return false
			}
		}
		return true
	}

	// refreshTasks reads the tasks again and tells what changed: tasks that joined or changed
	// state (a tasks event), slots whose task was replaced (a marker, once the lines of the old
	// task have had their window) and nodes that stopped reporting or came back (a node event).
	refreshTasks := func() bool {
		cur, ok := meta.fresh(streamCtx)
		if !ok {
			return true
		}
		byID := byTaskID(cur)
		changed := false
		for _, ti := range cur {
			if hidden[ti.ID] || !params.matches(ti) {
				continue
			}
			st := ServiceLogTask{ID: ti.ID, Slot: ti.Slot, Node: ti.Node, State: ti.State}
			if announced[ti.ID] != st {
				announced[ti.ID] = st
				delete(unresolved, ti.ID)
				changed = true
			}
		}
		if changed && (!flush() || !send("tasks", "", serviceLogTasks{TTY: tty, Tasks: tasksList()})) {
			return false
		}
		for _, m := range slotMarkers(prevTasks, cur) {
			if !params.matchesMarker(m, byID) {
				continue
			}
			if m.At == "" {
				m.At = time.Now().UTC().Format(logTimeLayout)
			}
			pending = append(pending, dueMarker{m: m, due: time.Now().Add(reorderHold)})
		}
		nodes := followedNodes(cur, announced)
		for _, n := range nodeNotices(prevNodes, nodes, time.Now().UTC().Format(logTimeLayout)) {
			if !flush() || !send("node", "", n) {
				return false
			}
		}
		prevTasks, prevNodes = cur, nodes
		return true
	}
	// releaseMarkers sends the markers whose window has passed, after the lines before them.
	releaseMarkers := func() bool {
		n := 0
		for n < len(pending) && !pending[n].due.After(time.Now()) {
			n++
		}
		if n == 0 {
			return true
		}
		if !flush() {
			return false
		}
		for _, p := range pending[:n] {
			if !send("marker", "", p.m) {
				return false
			}
		}
		pending = pending[n:]
		return true
	}

	quiet := time.NewTimer(burstQuiet)
	defer quiet.Stop()
	maxWait := time.NewTimer(burstMax)
	defer maxWait.Stop()
	winTick := time.NewTicker(windowTick)
	defer winTick.Stop()
	metaTick := time.NewTicker(d.TaskRefresh)
	defer metaTick.Stop()
	flushTick := time.NewTicker(batchInterval)
	defer flushTick.Stop()
	beat := time.NewTicker(d.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(d.MaxLifetime)
	defer life.Stop()
	for {
		var burstQuietC, burstMaxC <-chan time.Time
		if bursting {
			burstQuietC, burstMaxC = quiet.C, maxWait.C
		}
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			// The browser reconnects through the whole policy and resumes.
			flush()
			return
		case r, open := <-records:
			if !open {
				records = nil
				if bursting && !endBurst() {
					return
				}
				for _, r := range window.drain() {
					if !emit(r) {
						return
					}
				}
				if !flush() {
					return
				}
				if run.err != nil && !errors.Is(run.err, context.Canceled) {
					// The connection to the manager broke: end the response, so the browser
					// reconnects and resumes from the last line it has.
					d.Log.Warn("a service log stream ended with an error", "service", serviceID, "error", run.err)
					return
				}
				if !send("state", "", map[string]string{"state": "stopped", "at": time.Now().UTC().Format(time.RFC3339Nano)}) {
					return
				}
				continue
			}
			if bursting {
				quiet.Reset(burstQuiet)
			}
			if !keep(r) {
				continue
			}
			if !bursting {
				window.add(r)
				continue
			}
			burst = append(burst, r)
			bytes += len(r.M) + len(r.T)
			if (len(burst) >= maxBurstRecords || bytes >= maxBurstBytes) && !endBurst() {
				return
			}
		case <-burstQuietC:
			if !endBurst() {
				return
			}
		case <-burstMaxC:
			if !endBurst() {
				return
			}
		case <-winTick.C:
			for _, r := range window.release() {
				if !emit(r) {
					return
				}
			}
			if !releaseMarkers() {
				return
			}
		case <-metaTick.C:
			// The burst is told first; what changed meanwhile is worked out against the tasks
			// the burst was merged with, so nothing is missed by waiting.
			if !bursting && !refreshTasks() {
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

// auditServiceLogsViewed writes the logs.viewed row of a stream, once per viewer: a reconnect
// that carries the same sid writes nothing. The target is the service; the details are what
// was asked for, never what the log held.
func auditServiceLogsViewed(ctx context.Context, d ServiceLogsDeps, mem *logAuditMemory, p *rbac.Principal, envID, serviceID, name string, params serviceLogParams, userAgent string) {
	key := logAuditKey{user: string(p.Type) + ":" + p.ID, container: serviceID, sid: params.sid}
	if d.Audit == nil || mem.seen(key) {
		return
	}
	details := map[string]string{"service": name, "sid": params.sid, "tail": params.tail}
	if params.since != "" {
		details["since"] = params.since
	}
	if len(params.tasks) > 0 {
		details["task"] = strings.Join(params.tasks, ",")
	}
	if len(params.nodes) > 0 {
		details["node"] = strings.Join(params.nodes, ",")
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionLogsViewed, TargetType: serviceLogTargetType, TargetID: serviceID,
		EnvID: envID, Result: audit.ResultSuccess,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		// The viewer must still get the log; the lost row is the server's fault. The viewer is
		// forgotten, so the next stream of it tries the row again.
		mem.forget(key)
		d.Log.Error("could not audit a service log stream", "service", serviceID, "error", err)
	}
}

// byTaskID indexes tasks by their ID.
func byTaskID(tasks []taskInfo) map[string]taskInfo {
	out := make(map[string]taskInfo, len(tasks))
	for _, ti := range tasks {
		out[ti.ID] = ti
	}
	return out
}
