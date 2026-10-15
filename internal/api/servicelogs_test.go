// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// The merged service log stream (plan 06-29, OBS-02, D-35, D-37, RESEARCH E1 to E6). Docker is
// a fake swarm manager whose service logs come grouped per task, behind the real monitor,
// cluster reader and router; the stream is read frame by frame as the container log tests read
// theirs.

var svcT0 = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

type svcLogLine struct {
	T    string `json:"t"`
	S    string `json:"s"`
	M    string `json:"m"`
	Task string `json:"task"`
	Slot int    `json:"slot"`
	Node string `json:"node"`
}

type svcLogEvent struct {
	Lines []svcLogLine `json:"lines"`
}

type svcReadyTask struct {
	ID    string `json:"id"`
	Slot  int    `json:"slot"`
	Node  string `json:"node"`
	State string `json:"state"`
}

type svcReady struct {
	TTY   bool           `json:"tty"`
	Tasks []svcReadyTask `json:"tasks"`
}

// svcLine is a line a task wrote ms milliseconds after svcT0.
func svcLine(task, node string, ms int, text string) dockertest.FakeServiceLogLine {
	return dockertest.FakeServiceLogLine{TaskID: task, NodeID: node, Time: svcT0.Add(time.Duration(ms) * time.Millisecond), Text: text}
}

// svcTasks are the tasks of the web service: three running, in slots 1 to 3 on the manager and
// a ready worker, and one that failed in slot 1 and is shut down.
func svcTasks() []swarm.Task {
	return []swarm.Task{
		tkRunning("tk-1", "svc-web", "node-1", 1, "c1", time.Minute),
		tkRunning("tk-2", "svc-web", "node-3", 2, "c2", time.Minute),
		tkRunning("tk-3", "svc-web", "node-1", 3, "c3", time.Minute),
		tkExited("tk-old", "svc-web", "node-1", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	}
}

func svcBurstLines() []dockertest.FakeServiceLogLine {
	// The engine answers task by task; the times interleave.
	return []dockertest.FakeServiceLogLine{
		svcLine("tk-1", "node-1", 10, "a1"), svcLine("tk-1", "node-1", 40, "a2"), svcLine("tk-1", "node-1", 70, "a3"),
		svcLine("tk-2", "node-3", 20, "b1"), svcLine("tk-2", "node-3", 50, "b2"),
		svcLine("tk-3", "node-1", 30, "c1"),
		svcLine("tk-old", "node-1", 5, "old"),
	}
}

type svcLogsFix struct {
	*envFix
	env envBody
	cfg dockertest.FakeConfig
}

// newSvcLogsFix builds a local swarm manager of four nodes with the web service, its tasks and
// the given lines, and a router whose service log stream is tuned by tune.
func newSvcLogsFix(t *testing.T, tasks []swarm.Task, lines []dockertest.FakeServiceLogLine, tune func(*envFix)) *svcLogsFix {
	t.Helper()
	cfg := swarmManagerEngine(webService())
	cfg.SwarmNodeID, cfg.SwarmClusterID = "node-1", "cluster-1"
	cfg.NodeRows = tkNodes()
	cfg.Tasks = tasks
	cfg.ServiceLogs = map[string][]dockertest.FakeServiceLogLine{"svc-web": lines}
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	if tune != nil {
		tune(f)
	}
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return &svcLogsFix{envFix: f, env: e, cfg: cfg}
}

func (f *svcLogsFix) path(service, query string) string {
	return "/api/env/" + f.env.ID + "/services/" + service + "/logs/stream?" + query
}

func (f *svcLogsFix) open(t *testing.T, cookie, service, query, lastEventID string) *sseConn {
	t.Helper()
	return openSSE(t, f.authFixture, f.path(service, query), cookie, lastEventID)
}

// readSvcLines reads log events until n lines have arrived and returns them with the id of the
// last event. Other events (ready, tasks, state) are skipped.
func readSvcLines(t *testing.T, c *sseConn, n int) (lines []svcLogLine, lastID string) {
	t.Helper()
	for len(lines) < n {
		fr, ok := c.next(8 * time.Second)
		if !ok {
			t.Fatalf("only %d of %d lines arrived: %+v", len(lines), n, lines)
		}
		if fr.event != "log" {
			continue
		}
		var ev svcLogEvent
		if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
			t.Fatalf("log data %q: %v", fr.data, err)
		}
		lines = append(lines, ev.Lines...)
		lastID = fr.id
	}
	return lines, lastID
}

func svcTexts(lines []svcLogLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.M)
	}
	return out
}

func svcStamp(ms int) string {
	return svcT0.Add(time.Duration(ms) * time.Millisecond).Format("2006-01-02T15:04:05.000000000Z")
}

func (f *svcLogsFix) viewedRows(t *testing.T) []struct {
	TargetType, TargetID string
	Details              map[string]string
} {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT target_type, target_id, COALESCE(details, '{}') FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionLogsViewed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []struct {
		TargetType, TargetID string
		Details              map[string]string
	}
	for rows.Next() {
		var r struct {
			TargetType, TargetID string
			Details              map[string]string
		}
		var details string
		if err := rows.Scan(&r.TargetType, &r.TargetID, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServiceLogStreamSendsReadyWithTheTasksThenTheBurstMergedInTimeOrderWithATaskOnEveryLine(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	if got := c.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := c.header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	ready, ok := c.next(5 * time.Second)
	if !ok || ready.event != "ready" {
		t.Fatalf("first frame = %+v, want ready", ready)
	}
	var rd svcReady
	if err := json.Unmarshal([]byte(ready.data), &rd); err != nil {
		t.Fatal(err)
	}
	wantTasks := []svcReadyTask{{"tk-1", 1, "mgr-1", "running"}, {"tk-2", 2, "wrk-ready", "running"}, {"tk-3", 3, "mgr-1", "running"}}
	if rd.TTY || !slices.Equal(rd.Tasks, wantTasks) {
		t.Fatalf("ready = %+v, want not tty and the three live tasks by slot %+v (the failed task is hidden: stopped=0)", rd, wantTasks)
	}

	lines, id := readSvcLines(t, c, 6)
	if got, want := svcTexts(lines), []string{"a1", "b1", "c1", "a2", "b2", "a3"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want %v: the engine's per-task groups merged by time", got, want)
	}
	b1 := lines[1]
	if b1.Task != "tk-2" || b1.Slot != 2 || b1.Node != "wrk-ready" || b1.S != "o" || b1.T != svcStamp(20) {
		t.Errorf("b1 = %+v, want task tk-2 slot 2 on wrk-ready, stdout at +20ms", b1)
	}
	if id != svcStamp(70) {
		t.Errorf("event id = %q, want the last line's timestamp %q", id, svcStamp(70))
	}
}

func TestServiceLogStreamKeepsTheNewestTailLinesOfTheMergedBurst(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA+"&tail=3", "")
	lines, _ := readSvcLines(t, c, 3)
	if got, want := svcTexts(lines), []string{"a2", "b2", "a3"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want the newest 3 of the merge %v", got, want)
	}
	// Nothing more comes of the burst.
	if fr, ok := c.next(800 * time.Millisecond); ok && fr.event == "log" {
		t.Errorf("an extra log event after the burst: %+v", fr)
	}
	// The engine was asked for tail=3 (per task) and for the details it needs.
	qs := f.fake.ServiceLogQueries()
	if len(qs) != 1 || qs[0].Get("tail") != "3" || qs[0].Get("follow") != "1" || qs[0].Get("details") != "1" || qs[0].Get("timestamps") != "1" {
		t.Errorf("engine queries = %v", qs)
	}
}

func TestServiceLogStreamOrdersLinesAtTheSameTimeByLowerSlot(t *testing.T) {
	lines := []dockertest.FakeServiceLogLine{
		svcLine("tk-3", "node-1", 10, "slot3"), svcLine("tk-2", "node-3", 10, "slot2"), svcLine("tk-1", "node-1", 10, "slot1"),
	}
	f := newSvcLogsFix(t, svcTasks(), lines, nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	got, _ := readSvcLines(t, c, 3)
	if want := []string{"slot1", "slot2", "slot3"}; !slices.Equal(svcTexts(got), want) {
		t.Fatalf("lines = %v, want %v", svcTexts(got), want)
	}
}

func TestServiceLogStreamShowsTheFailedTasksToo(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA+"&stopped=1", "")
	ready := nextEvent(t, c, "ready", 5*time.Second)
	var rd svcReady
	if err := json.Unmarshal([]byte(ready.data), &rd); err != nil {
		t.Fatal(err)
	}
	if len(rd.Tasks) != 4 {
		t.Errorf("ready tasks = %+v, want all four with stopped=1", rd.Tasks)
	}
	lines, _ := readSvcLines(t, c, 7)
	if lines[0].M != "old" || lines[0].Task != "tk-old" || lines[0].Slot != 1 {
		t.Errorf("first line = %+v, want the failed task's line first", lines[0])
	}
}

func TestServiceLogStreamHoldsLiveLinesInAWindowAndSendsThemInTimeOrder(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), nil, nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	// Let the (empty) burst end, so what follows is live.
	time.Sleep(600 * time.Millisecond)
	// The later line is written first; both are within the window.
	f.fake.PushServiceLog("web", svcLine("tk-2", "node-3", 200, "later"))
	f.fake.PushServiceLog("web", svcLine("tk-1", "node-1", 100, "earlier"))
	lines, id := readSvcLines(t, c, 2)
	if got, want := svcTexts(lines), []string{"earlier", "later"}; !slices.Equal(got, want) {
		t.Fatalf("live lines = %v, want %v", got, want)
	}
	if id != svcStamp(200) {
		t.Errorf("event id = %q, want %q", id, svcStamp(200))
	}
}

func TestServiceLogStreamKeepsATaskFirstSeenAfterTheStreamOpenedAndAnnouncesIt(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), nil, nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	time.Sleep(600 * time.Millisecond)

	cfg := f.cfg
	cfg.Tasks = append(slices.Clone(svcTasks()), tkRunning("tk-new", "svc-web", "node-3", 4, "c4", time.Second))
	cfg.ServiceLogs = f.cfg.ServiceLogs
	f.fake.Reconfigure(cfg)
	f.fake.PushServiceLog("web", svcLine("tk-new", "node-3", 300, "from a replacement"))

	// The tasks event comes before the batch that holds the new task's first line.
	var announced svcReady
	var lines []svcLogLine
	for len(lines) == 0 {
		fr, ok := c.next(8 * time.Second)
		if !ok {
			t.Fatal("neither a tasks event nor the new task's line arrived")
		}
		switch fr.event {
		case "tasks":
			if err := json.Unmarshal([]byte(fr.data), &announced); err != nil {
				t.Fatal(err)
			}
		case "log":
			var ev svcLogEvent
			if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
				t.Fatal(err)
			}
			lines = ev.Lines
		}
	}
	if lines[0].M != "from a replacement" || lines[0].Task != "tk-new" || lines[0].Slot != 4 || lines[0].Node != "wrk-ready" {
		t.Fatalf("line = %+v, want the new task's, resolved to slot 4 on wrk-ready", lines[0])
	}
	if !slices.ContainsFunc(announced.Tasks, func(x svcReadyTask) bool { return x.ID == "tk-new" && x.Slot == 4 }) {
		t.Errorf("tasks event = %+v, want it to name tk-new in slot 4", announced.Tasks)
	}
}

func TestServiceLogStreamNarrowsToTasksAndNodesAndUsesTheTaskRouteForOne(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)

	one := f.open(t, f.cookie, "web", "sid="+sidA+"&task=tk-2", "")
	lines, _ := readSvcLines(t, one, 2)
	if got, want := svcTexts(lines), []string{"b1", "b2"}; !slices.Equal(got, want) {
		t.Fatalf("task=tk-2 gave %v, want %v", got, want)
	}
	if !slices.ContainsFunc(f.fake.Requests(), func(r string) bool { return strings.HasSuffix(r, "/tasks/tk-2/logs") }) {
		t.Errorf("requests = %v, want one task's logs asked of the engine for a single task filter", f.fake.Requests())
	}
	one.close()

	two := f.open(t, f.cookie, "web", "sid="+sidB+"&task=tk-1,tk-3", "")
	lines, _ = readSvcLines(t, two, 4)
	if got, want := svcTexts(lines), []string{"a1", "c1", "a2", "a3"}; !slices.Equal(got, want) {
		t.Fatalf("task=tk-1,tk-3 gave %v, want %v", got, want)
	}
	two.close()

	byNode := f.open(t, f.cookie, "web", "sid="+sidA+"1&node=wrk-ready", "")
	lines, _ = readSvcLines(t, byNode, 2)
	if got, want := svcTexts(lines), []string{"b1", "b2"}; !slices.Equal(got, want) {
		t.Fatalf("node=wrk-ready gave %v, want %v", got, want)
	}
	if fr, ok := byNode.next(800 * time.Millisecond); ok && fr.event == "log" {
		t.Errorf("a line of another node came through the node filter: %+v", fr)
	}
}

func TestServiceLogStreamWithNoOutputOrNoTasksIsReadyAndQuiet(t *testing.T) {
	for name, tasks := range map[string][]swarm.Task{"no output": svcTasks(), "no tasks": nil} {
		t.Run(name, func(t *testing.T) {
			f := newSvcLogsFix(t, tasks, nil, nil)
			c := f.open(t, f.cookie, "web", "sid="+sidA, "")
			if c.status0() != http.StatusOK {
				t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
			}
			ready := nextEvent(t, c, "ready", 5*time.Second)
			var rd svcReady
			if err := json.Unmarshal([]byte(ready.data), &rd); err != nil {
				t.Fatal(err)
			}
			if len(rd.Tasks) != len(tasks)-boolInt(len(tasks) > 0) { // the failed task is hidden
				t.Errorf("ready tasks = %+v for %d tasks", rd.Tasks, len(tasks))
			}
			if fr, ok := c.next(900 * time.Millisecond); ok && fr.event == "log" {
				t.Errorf("a log event from a service that wrote nothing: %+v", fr)
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestServiceLogStreamResumesAfterTheLastEventIDWithoutRepeatingALine(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, svcStamp(40))
	lines, _ := readSvcLines(t, c, 2)
	if got, want := svcTexts(lines), []string{"b2", "a3"}; !slices.Equal(got, want) {
		t.Fatalf("resume after +40ms gave %v, want only the later lines %v", got, want)
	}
	qs := f.fake.ServiceLogQueries()
	if len(qs) != 1 || qs[0].Get("since") == "" || qs[0].Get("tail") != "" {
		t.Errorf("engine queries = %v, want since set and no tail on a resume", qs)
	}
	// The query's after works the same, and the later of the two wins.
	c2 := f.open(t, f.cookie, "web", "sid="+sidB+"&after="+svcStamp(50), svcStamp(10))
	lines, _ = readSvcLines(t, c2, 1)
	if lines[0].M != "a3" {
		t.Errorf("resume after +50ms gave %q first, want a3", lines[0].M)
	}
}

func TestServiceLogStreamRefusesWhatIsNotAStreamBeforeAnythingIsSent(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	for _, query := range []string{"", "sid=short", "sid=" + sidA + "&tail=-1", "sid=" + sidA + "&tail=lots", "sid=" + sidA + "&since=yesterday",
		"sid=" + sidA + "&after=nope", "sid=" + sidA + "&stopped=maybe", "sid=" + sidA + "&task=" + strings.Repeat("x", 129)} {
		rec := f.req(t, http.MethodGet, f.path("web", query), "")
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "validation_failed" {
			t.Errorf("%q: status = %d, type %q: %s", query, rec.code, rec.ptyp, rec.body)
		}
	}
	rec := f.req(t, http.MethodGet, f.path("nope", "sid="+sidA), "")
	if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Errorf("an unknown service: status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if q := f.fake.ServiceLogQueries(); len(q) != 0 {
		t.Errorf("a refused request still reached the engine: %v", q)
	}
	if rows := f.viewedRows(t); len(rows) != 0 {
		t.Errorf("a stream that never opened was audited: %+v", rows)
	}
}

func TestServiceLogStreamOfAStandaloneHostIs409NotASwarm(t *testing.T) {
	f := newEnvFix(t, logsEngine())
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/services/web/logs/stream?sid="+sidA, "")
	if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestServiceLogsAuditWritesOneRowPerViewerWithTargetTypeServiceAndNeverALine(t *testing.T) {
	const secret = "SERVICE-LOG-LINE-THAT-MUST-NOT-BE-KEPT-77c1" //nolint:gosec // a marker, not a credential
	lines := append(svcBurstLines(), svcLine("tk-2", "node-3", 80, secret))
	f := newSvcLogsFix(t, svcTasks(), lines, nil)

	c := f.open(t, f.cookie, "web", "sid="+sidA+"&tail=20&task=tk-2&node=wrk-ready", "")
	readSvcLines(t, c, 3)
	rows := f.viewedRows(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows after the first open, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.TargetType != "service" || r.TargetID != "svc-web" {
		t.Errorf("row target = %s %s, want service svc-web", r.TargetType, r.TargetID)
	}
	d := r.Details
	if d["service"] != "web" || d["sid"] != sidA || d["tail"] != "20" || d["task"] != "tk-2" || d["node"] != "wrk-ready" {
		t.Errorf("details = %v, want the service, sid, tail and the task and node filters", d)
	}
	c.close()

	// The same viewer coming back writes nothing more; another viewer writes its own.
	c2 := f.open(t, f.cookie, "web", "sid="+sidA, "")
	readSvcLines(t, c2, 1)
	if n := len(f.viewedRows(t)); n != 1 {
		t.Errorf("a reconnect of the same viewer made it %d rows", n)
	}
	c3 := f.open(t, f.cookie, "web", "sid="+sidB, "")
	readSvcLines(t, c3, 1)
	if rows = f.viewedRows(t); len(rows) != 2 || rows[1].Details["sid"] != sidB {
		t.Errorf("rows = %+v, want a second one for the other viewer", rows)
	}

	// Nothing of the log reaches the audit log or the server's own log (D-35, prohibition).
	var stored int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE details LIKE ? OR reason LIKE ?`, "%"+secret+"%", "%"+secret+"%").Scan(&stored); err != nil || stored != 0 {
		t.Errorf("the audit log holds %d rows with a log line (%v)", stored, err)
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Error("a line of the log was written to the server's own log")
	}
}

func TestServiceLogStreamAllowsTheConfiguredNumberPerUserAndRefusesTheNextWith429(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), func(f *envFix) { f.serviceLogsMaxStreams = 2 })
	var conns []*sseConn
	for i := range 2 {
		c := f.open(t, f.cookie, "web", fmt.Sprintf("sid=%s%02d", sidA, i), "")
		if c.status0() != http.StatusOK {
			t.Fatalf("stream %d status = %d", i+1, c.status0())
		}
		conns = append(conns, c)
	}
	third := f.open(t, f.cookie, "web", "sid="+sidB, "")
	if third.status0() != http.StatusTooManyRequests {
		t.Fatalf("the 3rd stream = %d, want 429", third.status0())
	}
	third.waitEnded(t, 5*time.Second, "the 429 was answered")
	if body := third.bodyString(); !strings.Contains(body, `"type":"rate_limited"`) {
		t.Errorf("429 body = %s", body)
	}
	conns[0].close()
	if c := f.open(t, f.cookie, "web", "sid="+sidB, ""); c.status0() != http.StatusOK {
		t.Errorf("a stream after one closed = %d, want 200", c.status0())
	}
}

func TestServiceLogStreamEndsWithClosedAccessLostWithinAHeartbeatOfLosingTheGrant(t *testing.T) {
	const heartbeat = 40 * time.Millisecond
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), func(f *envFix) { f.serviceLogsHeartbeat = heartbeat })
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", f.env.ID, "viewer")
	cookie, _ := f.signIn(t, "viewer", false)

	c := f.open(t, cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	if _, err := f.st.Write.Exec(`DELETE FROM role_assignments WHERE principal_id = 'id-viewer'`); err != nil {
		t.Fatal(err)
	}
	closed := nextEvent(t, c, "closed", 2*heartbeat+2*time.Second)
	if closed.data != `{"reason":"access_lost"}` {
		t.Errorf("closed = %s", closed.data)
	}
	c.waitEnded(t, time.Second, "it told the viewer the access was lost")
	waitFor(t, "the engine follow to be released", func() bool { return f.fake.ServiceLogFollows() == 0 })
}

func TestServiceLogStreamEndsAtItsMaximumLifetimeAndFreesTheEngineFollow(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), func(f *envFix) { f.serviceLogsMaxLifetime = 80 * time.Millisecond })
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	c.waitEnded(t, 5*time.Second, "its lifetime of 80 ms")
	waitFor(t, "the engine follow to be released", func() bool { return f.fake.ServiceLogFollows() == 0 })
}

func TestServiceLogStreamReleasesTheEngineFollowWhenTheBrowserLeaves(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	waitFor(t, "the engine follow to open", func() bool { return f.fake.ServiceLogFollows() == 1 })
	c.close()
	waitFor(t, "the engine follow to be released", func() bool { return f.fake.ServiceLogFollows() == 0 })
}

func TestServiceLogStreamNeedsServicesRead(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: f.path("web", "sid="+sidA), cookie: cookie})
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want a refusal", rec.Code)
	}
	if q := f.fake.ServiceLogQueries(); len(q) != 0 {
		t.Errorf("a refused viewer reached the engine: %v", q)
	}
}
