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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/tunnel"
)

// The merged log of a stack (plan 07-29, STK-10, D-22): every service of a swarm stack, or every
// container of a standalone one, followed at once and merged by time into one stream, with the
// stack's known secret values masked in every line. The transport, the framing, the reorder window
// and the markers are those of a service's log (plan 06-29 to 06-31); what a stack adds is the
// set of sources, which is a service on a swarm (one upstream stream carries all its tasks) and a
// container on a host, and the service each line belongs to.

const (
	// problemStackLogsMasker is the problem when the stack's secrets cannot be read to mask a log.
	// A log that cannot be masked is not streamed.
	problemStackLogsMasker = "logs_unavailable"
)

// StackLogsTuning tunes the log routes of a stack. Every zero value is the default; tests
// shorten them.
type StackLogsTuning struct {
	// Heartbeat is how often an idle stream gets a comment line and has its access checked
	// again. Zero means 15 seconds.
	Heartbeat time.Duration
	// MaxStreamsPerUser caps concurrent stack log streams per user. Zero means 8.
	MaxStreamsPerUser int
	// MaxLifetime is the longest one stream stays open. Zero means one hour.
	MaxLifetime time.Duration
	// TaskRefresh is how often the stack's tasks (or containers) and the state of their nodes
	// are read again, to announce those that join and mark those that were replaced. Zero
	// means 3 seconds.
	TaskRefresh time.Duration
	// MaxFollow is the most upstream logs one stream follows at once. Zero means the tunnel's
	// interactive stream limit, 32.
	MaxFollow int
	// MaxDownloadBytes is the most one merged download holds while it is merged. Zero means
	// 32 MiB.
	MaxDownloadBytes int
	// MaxServerDownloads caps the downloads of stack logs running at once on the server,
	// whoever asks. Zero means 4.
	MaxServerDownloads int
	// Now is the clock of the audit memory and of a download's file name. Nil means the wall clock.
	Now func() time.Time
	// BurstEnded, when set, is called each time a stream has sent the burst the engine opened
	// with and is live. It exists so a test can tell that what follows is live without waiting
	// out the quiet period; production leaves it nil.
	BurstEnded func()
}

func (t StackLogsTuning) withDefaults() StackLogsTuning {
	t.Heartbeat = cmp.Or(t.Heartbeat, defaultHeartbeat)
	t.MaxStreamsPerUser = cmp.Or(t.MaxStreamsPerUser, defaultMaxStreams)
	t.MaxLifetime = cmp.Or(t.MaxLifetime, defaultMaxStreamLifetime)
	t.TaskRefresh = cmp.Or(t.TaskRefresh, taskMetaTTL)
	t.MaxFollow = cmp.Or(t.MaxFollow, tunnel.MaxInteractiveStreams)
	t.MaxServerDownloads = cmp.Or(t.MaxServerDownloads, maxServerDownloads)
	if t.Now == nil {
		t.Now = time.Now
	}
	return t
}

// stackLogParams are the validated query of a stack log stream request: the container log's
// (sid, since, tail, after) and the narrowing a stack adds. serviceLogParams carries the stopped
// switch and the task and node lists, so a task passes the filters as it does in a service's log.
type stackLogParams struct {
	serviceLogParams
	// services and containers narrow the stream to those services (their name in the compose
	// file, or on a swarm the full name) and those containers (an ID, the start of one of at least
	// eight characters, or a name); empty means all.
	services, containers []string
}

// parseStackLogParams reads the query of the stack log stream. The error is a sentence for the 422.
func parseStackLogParams(req *http.Request) (stackLogParams, error) {
	lp, err := parseLogParams(req)
	if err != nil {
		return stackLogParams{}, err
	}
	p := stackLogParams{serviceLogParams: serviceLogParams{logParams: lp}}
	q := req.URL.Query()
	switch q.Get("stopped") {
	case "", "0", "false":
	case "1", "true":
		p.stopped = true
	default:
		return p, errors.New("stopped must be 1 or 0")
	}
	for name, dst := range map[string]*[]string{"services": &p.services, "tasks": &p.tasks, "nodes": &p.nodes, "containers": &p.containers} {
		names, err := splitLogFilter(name, q.Get(name))
		if err != nil {
			return p, err
		}
		*dst = names
	}
	return p, nil
}

// serviceWanted says whether a service passes the services filter.
func (p stackLogParams) serviceWanted(short, full string) bool {
	return len(p.services) == 0 || slices.ContainsFunc(p.services, func(f string) bool { return f == short || f == full })
}

// StackLogTask is one task (a swarm) or container (a host) the stream follows, as the ready and
// tasks events carry it.
type StackLogTask struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	// Slot is the replica number of a swarm task (0 for a task of a global service), or the
	// number of a container among its service's.
	Slot  int    `json:"slot"`
	Node  string `json:"node"`
	State string `json:"state"`
}

// stackLogTasks is the body of the ready and tasks events.
type stackLogTasks struct {
	TTY    bool           `json:"tty"`
	Target string         `json:"target"`
	Tasks  []StackLogTask `json:"tasks"`
}

// stackLogCap is the body of the cap event: the stream follows Cap of Count tasks or containers.
type stackLogCap struct {
	Count int    `json:"count"`
	Cap   int    `json:"cap"`
	Unit  string `json:"unit"`
}

// StackLogLine is one line of a stack's merged log: ServiceLogLine and the service that wrote it.
type StackLogLine struct {
	T       string `json:"t"`
	S       string `json:"s"`
	M       string `json:"m"`
	Task    string `json:"task"`
	Slot    int    `json:"slot"`
	Node    string `json:"node"`
	Service string `json:"service"`
}

// stackBatch collects lines for one log event.
type stackBatch struct {
	lines []StackLogLine
	size  int
}

func (b *stackBatch) add(l StackLogLine) bool {
	b.lines = append(b.lines, l)
	b.size += len(l.M) + len(l.T) + len(l.Node) + len(l.Task) + len(l.Service) + 16
	return len(b.lines) >= batchLines || b.size >= batchBytes
}

func (b *stackBatch) take() []StackLogLine {
	out := b.lines
	b.lines, b.size = nil, 0
	return out
}

// stackEvent is an event of the stream other than a log line. delay holds it back for the
// reorder window, so the lines of the task it concerns are sent before it.
type stackEvent struct {
	name  string
	body  any
	delay bool
}

// stackFollower is what a target (a swarm or a host) gives the stream loop: the sources to
// merge and the ways to describe a line, to keep up with changes and to stop.
type stackFollower struct {
	ready stackLogTasks
	// capped, when set, says the stack has more than the stream follows.
	capped *stackLogCap
	// sources are the upstream logs, one for each service followed.
	sources []logSource
	// tasks says where each task or container is, for ordering lines whose times are equal.
	tasks func() map[string]taskInfo
	// accept says whether a record is one the stream shows.
	accept func(svcRec) bool
	// line says the slot and the node a record is shown with, and the events its first
	// appearance calls for (a task nobody listed yet).
	line func(ctx context.Context, r svcRec) (slot int, node string, events []stackEvent)
	// afterBurst is called once the opening burst has been sent, with the stamp of the oldest
	// line in it (empty when there was none).
	afterBurst func(ctx context.Context, firstStamp string) []stackEvent
	// refresh reads what changed since the last time: tasks that joined, slots that were
	// replaced, nodes that cannot be reached. reconnect says the set of sources is out of date,
	// so the stream ends and the browser opens it again from the last line.
	refresh func(ctx context.Context) (events []stackEvent, reconnect bool)
	// feed, when set, is the engine's event feed; onEvent reads one of its events.
	feed    <-chan envs.NodeEvent
	onEvent func(ctx context.Context, ev envs.NodeEvent) (events []stackEvent, reconnect bool)
	// broken is signalled when a connection to the engine broke under one of the sources, so the
	// stream ends and the browser opens it again.
	broken <-chan struct{}
	// stop releases everything the follower opened.
	stop func()
}

// stackLogMount is what the routes of a stack's log share: the limits on how many streams and downloads one user and
// the server run, whatever the kind of stack, and the memory of who has been audited.
type stackLogMount struct {
	d            StacksDeps
	need         rbac.Requirement
	streams      *streamLimiter
	mem          *logAuditMemory
	user, server *streamLimiter
}

// registerStackLogs registers the live log stream and the download of a stack's merged log. Both
// are mounted through Router.Guarded because the contract cannot describe a stream (D-47), and
// need stacks.read on the environment. It returns what the routes of an unmanaged stack's log share with them.
func registerStackLogs(r *httpx.Router, d StacksDeps) *stackLogMount {
	t := d.Logs.withDefaults()
	d.Logs = t
	m := &stackLogMount{
		d:       d,
		need:    rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		streams: &streamLimiter{max: t.MaxStreamsPerUser, open: map[string]int{}},
		mem:     newLogAuditMemory(logAuditTTL, logAuditMax, t.Now),
		user:    &streamLimiter{max: maxLogDownloads, open: map[string]int{}},
		server:  &streamLimiter{max: t.MaxServerDownloads, open: map[string]int{}},
	}
	r.Guarded(http.MethodGet, "/api/env/{envId}/stacks/{stackId}/logs/stream", m.need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveStackLogs(w, req, d, m.streams, m.mem, managedLogSubject)
	}))
	r.Guarded(http.MethodGet, "/api/env/{envId}/stacks/{stackId}/logs/download", m.need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		downloadStackLogs(w, req, d, m.user, m.server, managedLogSubject)
	}))
	return m
}

// stackLogSubject finds the stack a log route opens and the masker that hides its known secrets. The error is the
// answer to send.
type stackLogSubject func(ctx context.Context, d StacksDeps, envID string, req *http.Request) (stackops.Stack, stacks.Masker, error)

// managedLogSubject is the stack of TierPlan's the address names by its ID, with the masker of the secrets it knows.
func managedLogSubject(ctx context.Context, d StacksDeps, envID string, req *http.Request) (stackops.Stack, stacks.Masker, error) {
	st, err := d.repo().Stack(ctx, envID, chi.URLParam(req, "stackId"))
	if err != nil {
		return stackops.Stack{}, stacks.Masker{}, stackLookupProblem(d, envID, err)
	}
	masker, err := d.logMasker(ctx, st)
	if err != nil {
		return stackops.Stack{}, stacks.Masker{}, stackLogsMaskerProblem(d, envID, err)
	}
	return st, masker, nil
}

// logSubjectID is what a stack's log is audited and limited under: the stack's ID, or for a stack TierPlan did not make,
// which has none, its environment and name.
func logSubjectID(st stackops.Stack) string {
	if st.Discovered {
		return "unmanaged:" + st.EnvID + "/" + st.Name
	}
	return st.ID
}

// logMasker builds the masker of a stack's log from the secrets TierPlan knows of it: those of the
// version that runs and those of the draft, so a value that was just changed is hidden whichever
// version a container still has. Only known values are hidden; a secret the application made up
// itself is not, and the UI never says logs are private.
func (d StacksDeps) logMasker(ctx context.Context, st stackops.Stack) (stacks.Masker, error) {
	repo := d.repo()
	var (
		vars  []stacks.Var
		files []stacks.File
	)
	rev, ok, err := repo.LatestGoodRevision(ctx, st.ID)
	if err != nil {
		return stacks.Masker{}, err
	}
	if ok {
		vars, files = append(vars, rev.Copy.Vars...), append(files, rev.Copy.Files...)
	}
	draft, err := repo.Draft(ctx, st.ID)
	switch {
	case err == nil:
		vars, files = append(vars, draft.Copy.Vars...), append(files, draft.Copy.Files...)
	case errors.Is(err, stackops.ErrNoDraft):
	default:
		return stacks.Masker{}, err
	}
	return stacks.NewMasker(vars, files), nil
}

// stackLogsMaskerProblem is the answer when the masker cannot be built.
func stackLogsMaskerProblem(d StacksDeps, envID string, err error) error {
	d.Log.Error("could not read a stack's secrets to mask its log", "environment", envID, "error", err)
	return httpx.Problem(http.StatusServiceUnavailable, problemStackLogsMasker, "TierPlan could not read this stack's secrets to hide them from its logs, so it did not open them. Try again.")
}

// serveStackLogs is the SSE handler of a stack's merged log. The policy has already run when it
// is called, so the principal and the environment are on the context.
//
// The stream starts with a ready event ({tty, target, tasks}), then a cap event when the stack has
// more upstream logs than are followed, then log events, each a batch of lines {t, s, m, task,
// slot, node, service} with the stamp of the last line as the event ID, so a reconnect of the
// browser resumes from there. A tasks event follows when a task or container is first seen after
// the stream opened; marker and node events are those of a service's log. The first lines are the
// engine's burst, sorted as a whole with the newest tail kept; live lines are held for 500 ms and
// sent in time order. Every line is masked with the stack's known secrets before it leaves. Every
// heartbeat re-checks access, and the stream never outlives MaxLifetime.
//
// No line of the log is stored, audited or written to TierPlan's own log; only the fact that a
// stream was opened is recorded (D-35, P4 D-24).
func serveStackLogs(w http.ResponseWriter, req *http.Request, d StacksDeps, lim *streamLimiter, mem *logAuditMemory, subject stackLogSubject) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	params, err := parseStackLogParams(req)
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

	st, masker, err := subject(ctx, d, env.ID, req)
	if err != nil {
		writeProblemError(w, err)
		return
	}

	// The engine requests live as long as the stream; they are cancelled when the handler
	// returns.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fol *stackFollower
	switch st.Target {
	case stacks.TargetSwarm:
		fol, err = d.swarmFollower(streamCtx, env.ID, st, params)
	case stacks.TargetStandalone:
		fol, err = d.standaloneFollower(streamCtx, env.ID, st, params)
	default:
		err = errors.New("unknown stack target " + string(st.Target))
	}
	if err != nil {
		writeProblemError(w, err)
		return
	}
	defer fol.stop()

	auditStackLogsViewed(ctx, d, mem, p, env.ID, st, params, req.UserAgent())
	rc, send := startSSE(w, d.Log)
	if rc == nil {
		return
	}
	runStackStream(ctx, streamCtx, w, rc, send, d, fol, masker, params)
}

// mergedItem is what the merging goroutine hands the loop: a record, or the end of the burst.
type mergedItem struct {
	rec      svcRec
	burstEnd bool
}

// runStackStream is the loop of the stream: it sends the ready event, takes the merged records
// from mergeSources and the changes from the follower, and writes them as events until the
// browser leaves, access is lost or the stream is at the end of its life.
func runStackStream(ctx, streamCtx context.Context, w http.ResponseWriter, rc *http.ResponseController, send func(event, id string, v any) bool, d StacksDeps, fol *stackFollower, masker stacks.Masker, params stackLogParams) {
	t := d.Logs
	if !send("ready", "", fol.ready) {
		return
	}
	if fol.capped != nil && !send("cap", "", fol.capped) {
		return
	}

	keepN := maxLogTail
	if params.after == "" {
		keepN, _ = strconv.Atoi(params.tail)
	}
	merged := make(chan mergedItem, 256)
	go func() {
		defer close(merged)
		put := func(it mergedItem) bool {
			select {
			case merged <- it:
				return true
			case <-streamCtx.Done():
				return false
			}
		}
		_ = mergeSources(streamCtx, fol.sources, reorderHold, func(r svcRec) bool { return put(mergedItem{rec: r}) },
			withBurstKeep(keepN), withTasks(fol.tasks), withAccept(fol.accept),
			withBurstEnded(func() { put(mergedItem{burstEnd: true}) }))
	}()

	var (
		b       stackBatch
		pending []dueEvent
		live    bool
		first   string
		items   = (<-chan mergedItem)(merged)
	)
	flush := func() bool {
		if len(b.lines) == 0 {
			return true
		}
		batch := b.take()
		return send("log", batch[len(batch)-1].T, map[string][]StackLogLine{"lines": batch})
	}
	sendEvents := func(evs []stackEvent) bool {
		for _, ev := range evs {
			if ev.delay {
				pending = append(pending, dueEvent{ev: ev, due: time.Now().Add(reorderHold)})
				continue
			}
			if !flush() || !send(ev.name, "", ev.body) {
				return false
			}
		}
		return true
	}
	emit := func(r svcRec) bool {
		slot, node, evs := fol.line(streamCtx, r)
		if !sendEvents(evs) {
			return false
		}
		if first == "" {
			first = r.T
		}
		line := StackLogLine{T: r.T, S: r.S, M: masker.Mask(r.M), Task: r.TaskID, Slot: slot, Node: node, Service: r.Svc}
		return !b.add(line) || flush()
	}
	releaseDue := func() bool {
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
			if !send(p.ev.name, "", p.ev.body) {
				return false
			}
		}
		pending = pending[n:]
		return true
	}

	winTick := time.NewTicker(windowTick)
	defer winTick.Stop()
	metaTick := time.NewTicker(t.TaskRefresh)
	defer metaTick.Stop()
	flushTick := time.NewTicker(batchInterval)
	defer flushTick.Stop()
	beat := time.NewTicker(t.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(t.MaxLifetime)
	defer life.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			// The browser reconnects through the whole policy and resumes.
			flush()
			return
		case it, open := <-items:
			if !open {
				items = nil
				if !flush() {
					return
				}
				if !send("state", "", map[string]string{"state": "stopped", "at": time.Now().UTC().Format(time.RFC3339Nano)}) {
					return
				}
				continue
			}
			if it.burstEnd {
				if !flush() {
					return
				}
				live = true
				if !sendEvents(fol.afterBurst(streamCtx, first)) {
					return
				}
				if t.BurstEnded != nil {
					t.BurstEnded()
				}
				continue
			}
			if !emit(it.rec) {
				return
			}
		case ev, open := <-fol.feed:
			if !open {
				// The feed dropped this subscriber: end like the events stream does, so the
				// browser reconnects, resumes from the last line and subscribes again.
				flush()
				return
			}
			evs, reconnect := fol.onEvent(streamCtx, ev)
			if !sendEvents(evs) {
				return
			}
			if reconnect {
				flush()
				return
			}
		case <-fol.broken:
			// The connection to the engine broke: end the response, so the browser reconnects and
			// resumes from the last line it has.
			flush()
			return
		case <-winTick.C:
			if !releaseDue() {
				return
			}
		case <-metaTick.C:
			if !live {
				continue
			}
			evs, reconnect := fol.refresh(streamCtx)
			if !sendEvents(evs) {
				return
			}
			if reconnect {
				flush()
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

// dueEvent is an event waiting for the reorder window to pass.
type dueEvent struct {
	ev  stackEvent
	due time.Time
}

// auditStackLogsViewed writes the logs.viewed row of a stream, once per viewer: a reconnect that
// carries the same sid writes nothing. The target is the stack; the details are what was asked
// for, never what the log held.
func auditStackLogsViewed(ctx context.Context, d StacksDeps, mem *logAuditMemory, p *rbac.Principal, envID string, st stackops.Stack, params stackLogParams, userAgent string) {
	key := logAuditKey{user: string(p.Type) + ":" + p.ID, container: logSubjectID(st), sid: params.sid}
	if d.Audit == nil || mem.seen(key) {
		return
	}
	details := map[string]string{"stack": st.Name, "sid": params.sid, "tail": params.tail}
	if st.Discovered {
		// A stack TierPlan did not make has no ID: the row names it, and says TierPlan does not manage it.
		details["managed"] = "false"
	}
	if params.since != "" {
		details["since"] = params.since
	}
	for name, list := range map[string][]string{"services": params.services, "tasks": params.tasks, "nodes": params.nodes, "containers": params.containers} {
		if len(list) > 0 {
			details[name] = strings.Join(list, ",")
		}
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionLogsViewed, TargetType: stackTargetType, TargetID: st.ID,
		EnvID: envID, Result: audit.ResultSuccess,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		// The viewer must still get the log; the lost row is the server's fault. The viewer is
		// forgotten, so the next stream of it tries the row again.
		mem.forget(key)
		d.Log.Error("could not audit a stack log stream", "stack", st.ID, "error", err)
	}
}

// ---- a swarm stack ----

// stackServiceLogs are the services of a stack a stream follows, with the cap applied.
type stackServiceLogs struct {
	followed []stackops.ServiceRow
	// total is how many tasks the services chosen have, followed or not.
	total  int
	capped bool
}

// chooseServices narrows the stack's services to those the filter names and, past the follow
// cap, keeps the newest ones (by when the swarm created the service).
func chooseServices(rows []stackops.ServiceRow, created map[string]time.Time, params stackLogParams, maxFollow int) stackServiceLogs {
	var out stackServiceLogs
	for _, row := range rows {
		if params.serviceWanted(row.Short, row.Name) {
			out.followed = append(out.followed, row)
			out.total += row.Desired
		}
	}
	if len(out.followed) > maxFollow {
		out.capped = true
		slices.SortStableFunc(out.followed, func(a, b stackops.ServiceRow) int {
			return cmp.Or(created[b.ID].Compare(created[a.ID]), cmp.Compare(a.Name, b.Name))
		})
		out.followed = out.followed[:maxFollow]
		slices.SortFunc(out.followed, func(a, b stackops.ServiceRow) int { return cmp.Compare(a.Name, b.Name) })
	}
	return out
}

// swarmFollower opens the log of each service of a swarm stack, on the manager the cluster reader
// chooses, and describes the tasks as a service's log does.
func (d StacksDeps) swarmFollower(ctx context.Context, envID string, st stackops.Stack, params stackLogParams) (*stackFollower, error) {
	if d.Clusters == nil {
		d.Log.Error("the stack log route has no cluster reader")
		return nil, httpx.Problem(http.StatusInternalServerError, "", "internal error")
	}
	var read dockerx.ClusterRead
	readCtx, cancelRead := context.WithTimeout(ctx, inspectTimeout)
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		read, e = c.Cluster(readCtx)
		return e
	})
	cancelRead()
	if err != nil {
		return nil, serviceLogProblem(d.Log, envID, err)
	}
	created := make(map[string]time.Time, len(read.Services))
	for _, s := range read.Services {
		created[s.ID] = s.CreatedAt
	}
	chosen := chooseServices(stackops.SwarmRows(st, read.Services), created, params, d.Logs.MaxFollow)

	ids := make([]string, 0, len(chosen.followed))
	names := make(map[string]string, len(chosen.followed))
	for _, row := range chosen.followed {
		ids = append(ids, row.ID)
		names[row.ID] = row.Short
	}
	meta := newTaskMeta(func(ctx context.Context) ([]taskInfo, error) {
		if len(ids) == 0 {
			return nil, nil
		}
		return readTaskInfos(ctx, d.Clusters, envID, ids, names)
	}, time.Now)
	meta.ttl = d.Logs.TaskRefresh
	initial := meta.all(ctx)
	// Tasks that had ended when the stream opened are left out, unless asked for; tasks first
	// seen later are kept (D-33).
	hidden := map[string]bool{}
	if !params.stopped {
		for _, ti := range initial {
			if ti.Ended {
				hidden[ti.ID] = true
			}
		}
	}
	announced := map[string]StackLogTask{}
	for _, ti := range initial {
		if !hidden[ti.ID] && params.matches(ti) {
			announced[ti.ID] = stackTaskOf(ti)
		}
	}
	tasksBody := func(tty bool) stackLogTasks {
		out := make([]StackLogTask, 0, len(announced))
		for _, t := range announced {
			out = append(out, t)
		}
		slices.SortFunc(out, func(a, b StackLogTask) int {
			return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Slot, b.Slot), cmp.Compare(a.Node, b.Node), cmp.Compare(a.ID, b.ID))
		})
		return stackLogTasks{TTY: tty, Target: string(stacks.TargetSwarm), Tasks: out}
	}

	fol := &stackFollower{stop: func() {}}
	sd := ServiceLogsDeps{Clusters: d.Clusters}
	opts := dockerx.ServiceLogOptions{Follow: true, Since: params.since, Tail: params.tail}
	var (
		runs   []*svcRun
		tty    bool
		broken = make(chan struct{}, 1)
	)
	fol.stop = func() {
		for _, r := range runs {
			r.stop()
		}
	}
	for _, row := range chosen.followed {
		run, rtty, err := openServiceLogRun(ctx, sd, envID, row.ID, "", opts)
		if err != nil {
			fol.stop()
			return nil, serviceLogProblem(d.Log, envID, err)
		}
		runs = append(runs, run)
		tty = tty || rtty
		fol.sources = append(fol.sources, logSource{Name: row.Short, Records: watchRun(ctx, run, broken)})
	}
	fol.broken = broken
	fol.ready = tasksBody(tty)
	if chosen.capped {
		fol.capped = &stackLogCap{Count: chosen.total, Cap: d.Logs.MaxFollow, Unit: "tasks"}
	}
	fol.tasks = meta.byIDMap
	fol.accept = func(r svcRec) bool {
		if params.after != "" && r.T <= params.after {
			return false
		}
		if hidden[r.TaskID] {
			return false
		}
		ti, known := meta.lookup(ctx, r.TaskID)
		if !known {
			ti = taskInfo{ID: r.TaskID, NodeID: r.NodeID, Node: r.NodeID}
		}
		return params.matches(ti)
	}

	// unresolved are the tasks announced from a record alone, before the cluster listed them.
	unresolved := map[string]bool{}
	fol.line = func(ctx context.Context, r svcRec) (int, string, []stackEvent) {
		ti, known := meta.lookup(ctx, r.TaskID)
		_, seen := announced[r.TaskID]
		var evs []stackEvent
		if !seen || (unresolved[r.TaskID] && known) {
			if !known {
				ti = taskInfo{ID: r.TaskID, Service: r.Svc, NodeID: r.NodeID, Node: r.NodeID}
				unresolved[r.TaskID] = true
			} else {
				delete(unresolved, r.TaskID)
			}
			announced[r.TaskID] = stackTaskOf(ti)
			evs = append(evs, stackEvent{name: "tasks", body: tasksBody(tty)})
		}
		return ti.Slot, cmp.Or(ti.Node, r.NodeID), evs
	}

	// prevTasks and prevNodes are what the markers and notices were last worked out from.
	prevTasks := initial
	var prevNodes []nodeInfo
	fol.afterBurst = func(_ context.Context, firstStamp string) []stackEvent {
		var evs []stackEvent
		// What happened before the stream opened is told after its lines: the markers of the
		// tasks that had ended, when they are asked for and fall in what is shown (a marker older
		// than the first line has nothing to sit beside), and the nodes that could not be
		// reached already (D-34, D-37).
		if params.stopped && firstStamp != "" {
			byID := byTaskID(initial)
			for _, m := range slotMarkers(nil, initial) {
				if m.At == "" || m.At < firstStamp || !params.matchesMarker(m, byID) {
					continue
				}
				evs = append(evs, stackEvent{name: "marker", body: m})
			}
		}
		prevNodes = followedNodes(initial, taskIDs(announced))
		for _, n := range nodeNotices(nil, prevNodes, time.Now().UTC().Format(logTimeLayout)) {
			evs = append(evs, stackEvent{name: "node", body: n})
		}
		return evs
	}

	followedIDs := make(map[string]bool, len(chosen.followed))
	for _, row := range chosen.followed {
		followedIDs[row.ID] = true
	}
	fol.refresh = func(ctx context.Context) ([]stackEvent, bool) {
		var evs []stackEvent
		cur, ok := meta.fresh(ctx)
		if ok {
			byID := byTaskID(cur)
			changed := false
			for _, ti := range cur {
				if hidden[ti.ID] || !params.matches(ti) {
					continue
				}
				st := stackTaskOf(ti)
				if announced[ti.ID] != st {
					announced[ti.ID] = st
					delete(unresolved, ti.ID)
					changed = true
				}
			}
			if changed {
				evs = append(evs, stackEvent{name: "tasks", body: tasksBody(tty)})
			}
			for _, m := range slotMarkers(prevTasks, cur) {
				if !params.matchesMarker(m, byID) {
					continue
				}
				if m.At == "" {
					m.At = time.Now().UTC().Format(logTimeLayout)
				}
				evs = append(evs, stackEvent{name: "marker", body: m, delay: true})
			}
			nodes := followedNodes(cur, taskIDs(announced))
			for _, n := range nodeNotices(prevNodes, nodes, time.Now().UTC().Format(logTimeLayout)) {
				evs = append(evs, stackEvent{name: "node", body: n})
			}
			prevTasks, prevNodes = cur, nodes
		}
		// A service the stack gained since the stream opened is not followed by it: the stream
		// ends and the browser opens it again from the last line, with the service in it.
		if len(followedIDs) < d.Logs.MaxFollow {
			if state, err := d.Clusters.State(ctx, envID); err == nil && state.Known() {
				for _, row := range stackops.SwarmRows(st, state.Read.Services) {
					if !followedIDs[row.ID] && params.serviceWanted(row.Short, row.Name) {
						return evs, true
					}
				}
			}
		}
		return evs, false
	}
	return fol, nil
}

// watchRun passes the records of a read on and, when the read ends with an error other than its
// own cancelling (the connection to the manager broke), signals broken so the stream ends and the
// browser opens it again. The error of a read is valid once its channel is closed.
func watchRun(ctx context.Context, run *svcRun, broken chan<- struct{}) <-chan svcRec {
	out := make(chan svcRec, 256)
	go func() {
		defer close(out)
		for r := range run.records {
			select {
			case out <- r:
			case <-ctx.Done():
				return
			}
		}
		if run.err != nil && !errors.Is(run.err, context.Canceled) {
			select {
			case broken <- struct{}{}:
			default:
			}
		}
	}()
	return out
}

// stackTaskOf is how a task is told to the browser.
func stackTaskOf(ti taskInfo) StackLogTask {
	return StackLogTask{ID: ti.ID, Service: ti.Service, Slot: ti.Slot, Node: ti.Node, State: ti.State}
}

// taskIDs is the set of task IDs a map of announced tasks holds, in the form followedNodes takes.
func taskIDs(announced map[string]StackLogTask) map[string]ServiceLogTask {
	out := make(map[string]ServiceLogTask, len(announced))
	for id, t := range announced {
		out[id] = ServiceLogTask{ID: t.ID, Slot: t.Slot, Node: t.Node, State: t.State}
	}
	return out
}

// ---- the download ----

// downloadStackLogs streams a range of a stack's log, every service's lines merged in time order,
// as a text file named {stack}-{YYYYMMDD-HHMM}.log in UTC. Each service is read from Docker once and
// to its end, without following it; the lines are held by task or container up to a size (past it
// the oldest are left out and the first line of the file says so), merged k ways and written
// through one buffer. Each line is the name of its task or container as the viewer shows it, the
// UTC time and the text with the stack's known secrets masked.
//
// One logs.downloaded row is written once the range has been read and before the file starts; the
// text of the log is never audited or logged. A user runs at most two downloads at once and the
// server four (T-06-91, T-06-92).
func downloadStackLogs(w http.ResponseWriter, req *http.Request, d StacksDeps, userLim, serverLim *streamLimiter, subject stackLogSubject) {
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

	st, masker, err := subject(ctx, d, env.ID, req)
	if err != nil {
		writeProblemError(w, err)
		return
	}

	// Reading a range can take longer than the server's write timeout, which started with the
	// request, so it must not cut the response that follows.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	readCtx, cancel := context.WithTimeout(ctx, downloadReadTimeout)
	defer cancel()
	holder := newLineHolder(cmp.Or(d.Logs.MaxDownloadBytes, maxDownloadBytes))
	sinceStamp, untilStamp := "", ""
	if dp.hasSince {
		sinceStamp = dp.since.Format(logTimeLayout)
	}
	if dp.hasUntil {
		untilStamp = dp.until.Format(logTimeLayout)
	}
	var prefixOf func(r svcRec) string
	switch st.Target {
	case stacks.TargetSwarm:
		prefixOf, err = d.readSwarmStackLog(readCtx, env.ID, st, sinceStamp, untilStamp, holder)
	case stacks.TargetStandalone:
		prefixOf, err = d.readStandaloneStackLog(readCtx, env.ID, st, sinceStamp, untilStamp, holder)
	default:
		err = errors.New("unknown stack target " + string(st.Target))
	}
	if err != nil {
		writeProblemError(w, err)
		return
	}

	now := d.Logs.Now()
	until := now.UTC()
	if dp.hasUntil {
		until = dp.until
	}
	auditStackLogsDownloaded(ctx, d, p, env.ID, st, dp, until, req.UserAgent())

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+logFileName(st.Name, now)+`"`)
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
		put(prefixOf(r))
		put(" ")
		put(r.T)
		put(" ")
		put(masker.Mask(r.M))
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

// readSwarmStackLog reads the log of each service of a swarm stack from a manager, one after the
// other, into the holder, and returns the way a line of the file is named.
func (d StacksDeps) readSwarmStackLog(ctx context.Context, envID string, st stackops.Stack, since, until string, holder *lineHolder) (func(svcRec) string, error) {
	if d.Clusters == nil {
		d.Log.Error("the stack log route has no cluster reader")
		return nil, httpx.Problem(http.StatusInternalServerError, "", "internal error")
	}
	var read dockerx.ClusterRead
	err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		read, e = c.Cluster(ctx)
		return e
	})
	if err != nil {
		return nil, serviceLogProblem(d.Log, envID, err)
	}
	rows := stackops.SwarmRows(st, read.Services)
	ids := make([]string, 0, len(rows))
	names := make(map[string]string, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
		names[row.ID] = row.Short
	}
	meta := newTaskMeta(func(ctx context.Context) ([]taskInfo, error) {
		if len(ids) == 0 {
			return nil, nil
		}
		return readTaskInfos(ctx, d.Clusters, envID, ids, names)
	}, time.Now)
	meta.ttl = d.Logs.TaskRefresh
	infos := map[string]taskInfo{}
	known := map[string]bool{}
	sd := ServiceLogsDeps{Clusters: d.Clusters}
	for _, row := range rows {
		err := sd.readServiceLog(ctx, envID, row.ID, since, func(r svcRec) {
			if (since != "" && r.T < since) || (until != "" && r.T >= until) {
				return
			}
			r.Svc = row.Short
			ti, seen := infos[r.TaskID]
			if !seen {
				var ok bool
				if ti, ok = meta.lookup(ctx, r.TaskID); ok {
					known[r.TaskID] = true
				}
				infos[r.TaskID] = ti
			}
			holder.add(r, ti.Slot)
		})
		if err != nil {
			return nil, serviceLogProblem(d.Log, envID, err)
		}
	}
	return func(r svcRec) string { return downloadPrefix(r.Svc, r, infos[r.TaskID], known[r.TaskID]) }, nil
}

// readStandaloneStackLog reads the log of each container of a standalone stack, the ones that
// ended included, into the holder, and returns the way a line of the file is named.
func (d StacksDeps) readStandaloneStackLog(ctx context.Context, envID string, st stackops.Stack, since, until string, holder *lineHolder) (func(svcRec) string, error) {
	if d.Aggregator == nil {
		d.Log.Error("the stack log route has no aggregator")
		return nil, httpx.Problem(http.StatusInternalServerError, "", "internal error")
	}
	node, err := d.ops().StandaloneNode(ctx, envID)
	if err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	var rows []dockerx.ContainerRow
	if err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
		var e error
		rows, e = c.ContainersWithLabel(ctx, stacks.LabelComposeProject, st.Name)
		return e
	}); err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	containers := containersOf(st, rows)
	slices.SortFunc(containers, func(a, b stackContainer) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Number, b.Number), cmp.Compare(a.ID, b.ID))
	})
	numbers := make(map[string]int, len(containers))
	logsDeps := LogsDeps{Aggregator: d.Aggregator, Log: d.Log}
	for _, c := range containers {
		numbers[c.ID] = c.Number
		rc, tty, err := openLogs(ctx, logsDeps, envID, node, c.ID, logParams{nodeID: node, since: since, until: until})
		if errors.Is(err, dockerx.ErrLogsUnreadable) {
			// Docker has no log to give for this container; the others are still the stack's log.
			continue
		}
		if err != nil {
			return nil, err
		}
		var seq uint64
		err = pumpLogs(ctx, rc, tty, func(l LogLine) {
			seq++
			holder.add(svcRec{T: l.T, S: l.S, M: l.M, TaskID: c.ID, Svc: c.Service, seq: seq}, c.Number)
		})
		_ = rc.Close()
		if err != nil {
			return nil, snapshotProblem(d.Log, envID, err)
		}
	}
	return func(r svcRec) string { return r.Svc + "-" + strconv.Itoa(numbers[r.TaskID]) }, nil
}

// auditStackLogsDownloaded writes the logs.downloaded row of a download: what was asked for,
// never what the log held.
func auditStackLogsDownloaded(ctx context.Context, d StacksDeps, p *rbac.Principal, envID string, st stackops.Stack, dp serviceLogDownloadParams, until time.Time, userAgent string) {
	if d.Audit == nil {
		return
	}
	details := map[string]string{
		"stack": st.Name,
		"until": until.UTC().Format(time.RFC3339Nano),
	}
	if st.Discovered {
		details["managed"] = "false"
	}
	if dp.hasSince {
		details["since"] = dp.since.UTC().Format(time.RFC3339Nano)
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionLogsDownloaded, TargetType: stackTargetType, TargetID: st.ID,
		EnvID: envID, Result: audit.ResultSuccess,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: details,
	}); err != nil {
		d.Log.Error("could not audit a stack log download", "stack", st.ID, "error", err)
	}
}
