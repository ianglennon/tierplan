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
	"github.com/ianglennon/bayplan/proto/testsupport"
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
	// burstEnded gets one value each time a stream takes its opening burst as complete.
	burstEnded chan struct{}
}

// awaitBurstEnd waits until a stream has taken the burst the engine opened with as complete, so
// what the engine sends after that is live. It waits on the stream's own signal, not on the
// quiet period having passed.
func (f *svcLogsFix) awaitBurstEnd(t *testing.T) {
	t.Helper()
	select {
	case <-f.burstEnded:
	case <-testsupport.Context(t).Done():
		t.Fatal("the service log stream never took its opening burst as complete")
	}
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
	burstEnded := make(chan struct{}, 16)
	f.serviceLogsBurstEnded = func() {
		select {
		case burstEnded <- struct{}{}:
		default:
		}
	}
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	if tune != nil {
		tune(f)
	}
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return &svcLogsFix{envFix: f, env: e, cfg: cfg, burstEnded: burstEnded}
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
	f.awaitBurstEnd(t)
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
	f.awaitBurstEnd(t)

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
	awaitOnline(t, f.mon, e.ID)
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
	awaitFake(t, f.fake, "the engine follow to be released", func() bool { return f.fake.ServiceLogFollows() == 0 })
}

func TestServiceLogStreamEndsAtItsMaximumLifetimeAndFreesTheEngineFollow(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), func(f *envFix) { f.serviceLogsMaxLifetime = 80 * time.Millisecond })
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	c.waitEnded(t, 5*time.Second, "its lifetime of 80 ms")
	awaitFake(t, f.fake, "the engine follow to be released", func() bool { return f.fake.ServiceLogFollows() == 0 })
}

func TestServiceLogStreamReleasesTheEngineFollowWhenTheBrowserLeaves(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	awaitFake(t, f.fake, "the engine follow to open", func() bool { return f.fake.ServiceLogFollows() == 1 })
	c.close()
	awaitFake(t, f.fake, "the engine follow to be released", func() bool { return f.fake.ServiceLogFollows() == 0 })
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

// ---- markers, node notices and the tasks event (plan 06-30, OBS-02, D-34, D-37) ----

type svcMarker struct {
	Kind     string `json:"kind"`
	Slot     int    `json:"slot"`
	Old      string `json:"old"`
	OldState string `json:"oldState"`
	OldNode  string `json:"oldNode"`
	ExitCode *int   `json:"exitCode"`
	New      string `json:"new"`
	Node     string `json:"node"`
	At       string `json:"at"`
}

type svcNodeNotice struct {
	Kind string `json:"kind"`
	Node string `json:"node"`
	At   string `json:"at"`
}

// fastRefresh makes the stream read its tasks every 20 ms, so a change shows within the time
// it takes to wait for the event; the tests wait for events, not for the clock.
func fastRefresh(f *envFix) { f.serviceLogsRefresh = 20 * time.Millisecond }

// readEvents reads frames until n events called name have arrived and returns their data.
func readEvents(t *testing.T, c *sseConn, name string, n int) []string {
	t.Helper()
	var out []string
	for len(out) < n {
		fr, ok := c.next(10 * time.Second)
		if !ok {
			t.Fatalf("only %d of %d %q events arrived: %v", len(out), n, name, out)
		}
		if fr.event == name {
			out = append(out, fr.data)
		}
	}
	return out
}

func readMarkers(t *testing.T, c *sseConn, n int) []svcMarker {
	t.Helper()
	var out []svcMarker
	for _, data := range readEvents(t, c, "marker", n) {
		var m svcMarker
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("marker data %q: %v", data, err)
		}
		out = append(out, m)
	}
	return out
}

func readNotices(t *testing.T, c *sseConn, n int) []svcNodeNotice {
	t.Helper()
	var out []svcNodeNotice
	for _, data := range readEvents(t, c, "node", n) {
		var m svcNodeNotice
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("node data %q: %v", data, err)
		}
		out = append(out, m)
	}
	return out
}

// retask replaces the fake manager's tasks.
func (f *svcLogsFix) retask(tasks []swarm.Task) {
	cfg := f.cfg
	cfg.Tasks = tasks
	f.fake.Reconfigure(cfg)
}

// at sets the times a task was made and reached its state.
func at(tk swarm.Task, created, since time.Time) swarm.Task {
	tk.CreatedAt, tk.UpdatedAt, tk.Status.Timestamp = created, since, since
	return tk
}

func TestServiceLogStreamMarkersTellOfATaskThatWasReplacedAndAnnounceTheNewOne(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), nil, fastRefresh)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)

	// tk-1 (slot 1) fails with exit 3 and the swarm starts tk-new in its slot on the worker.
	f.retask([]swarm.Task{
		tkExited("tk-1", "svc-web", "node-1", 1, "failed", "shutdown", 3, "boom", time.Minute),
		tkTask("tk-new", "svc-web", "node-3", 1, "starting", "running", time.Second),
		tkRunning("tk-2", "svc-web", "node-3", 2, "c2", time.Minute),
		tkRunning("tk-3", "svc-web", "node-1", 3, "c3", time.Minute),
		tkExited("tk-old", "svc-web", "node-1", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	})

	var announced svcReady
	var marker svcMarker
	gotMarker := false
	for !gotMarker {
		fr, ok := c.next(10 * time.Second)
		if !ok {
			t.Fatal("no marker arrived")
		}
		switch fr.event {
		case "tasks":
			if err := json.Unmarshal([]byte(fr.data), &announced); err != nil {
				t.Fatal(err)
			}
		case "marker":
			if err := json.Unmarshal([]byte(fr.data), &marker); err != nil {
				t.Fatal(err)
			}
			gotMarker = true
		}
	}
	if marker.Kind != "replaced" || marker.Slot != 1 || marker.Old != "tk-1" || marker.OldState != "failed" || marker.New != "tk-new" || marker.Node != "wrk-ready" || marker.OldNode != "mgr-1" {
		t.Errorf("marker = %+v, want tk-1 (failed) replaced by tk-new on wrk-ready in slot 1", marker)
	}
	if marker.ExitCode == nil || *marker.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", marker.ExitCode)
	}
	if marker.At == "" {
		t.Error("the marker has no time, so the viewer cannot put it where the task's last line was")
	}
	// The new task joins before the marker, so its lines have a name.
	if !slices.ContainsFunc(announced.Tasks, func(x svcReadyTask) bool { return x.ID == "tk-new" && x.Slot == 1 && x.Node == "wrk-ready" }) {
		t.Errorf("tasks event = %+v, want it to name tk-new in slot 1 on wrk-ready", announced.Tasks)
	}
}

func TestServiceLogStreamSaysASlotStoppedWhenNoNewTaskHasStartedAndThenThatItWasReplaced(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), nil, fastRefresh)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)

	base := svcTasks()
	failed := slices.Clone(base)
	failed[1] = tkExited("tk-2", "svc-web", "node-3", 2, "rejected", "shutdown", 0, "no suitable node", time.Minute)
	f.retask(failed)
	m := readMarkers(t, c, 1)[0]
	if m.Kind != "stopped" || m.Slot != 2 || m.Old != "tk-2" || m.OldState != "rejected" || m.New != "" {
		t.Fatalf("marker = %+v, want slot 2 stopped: tk-2 rejected, with no new task", m)
	}

	f.retask(append(slices.Clone(failed), tkTask("tk-2b", "svc-web", "node-3", 2, "starting", "running", time.Second)))
	m = readMarkers(t, c, 1)[0]
	if m.Kind != "replaced" || m.Old != "tk-2" || m.New != "tk-2b" {
		t.Fatalf("marker = %+v, want tk-2 replaced by tk-2b", m)
	}
}

func TestServiceLogStreamOnlySendsMarkersForTheTasksAndNodesItWasNarrowedTo(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), nil, fastRefresh)
	c := f.open(t, f.cookie, "web", "sid="+sidA+"&node=mgr-1", "")
	nextEvent(t, c, "ready", 5*time.Second)

	// tk-2 (on wrk-ready) and tk-3 (on mgr-1) both fail and are replaced on their nodes.
	step1 := []swarm.Task{
		tkRunning("tk-1", "svc-web", "node-1", 1, "c1", time.Minute),
		tkExited("tk-2", "svc-web", "node-3", 2, "failed", "shutdown", 1, "x", time.Minute),
		tkTask("tk-2n", "svc-web", "node-3", 2, "starting", "running", time.Second),
		tkExited("tk-3", "svc-web", "node-1", 3, "failed", "shutdown", 1, "x", time.Minute),
		tkTask("tk-3n", "svc-web", "node-1", 3, "starting", "running", time.Second),
		tkExited("tk-old", "svc-web", "node-1", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	}
	f.retask(step1)
	if m := readMarkers(t, c, 1)[0]; m.Old != "tk-3" || m.New != "tk-3n" {
		t.Fatalf("first marker = %+v, want the one on mgr-1 (tk-3)", m)
	}
	// Later, tk-1 (mgr-1) fails too: the next marker is its, so tk-2's was not sent in between.
	step2 := slices.Clone(step1)
	step2[0] = tkExited("tk-1", "svc-web", "node-1", 1, "shutdown", "shutdown", 0, "", time.Minute)
	step2 = append(step2, tkTask("tk-1n", "svc-web", "node-1", 1, "starting", "running", time.Second))
	f.retask(step2)
	if m := readMarkers(t, c, 1)[0]; m.Old != "tk-1" || m.New != "tk-1n" || m.OldState != "shutdown" {
		t.Fatalf("second marker = %+v, want tk-1 shut down and replaced by tk-1n", m)
	}
}

func TestServiceLogStreamTellsOfANodeThatStopsReportingAndOfItsReturn(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), nil, fastRefresh)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)

	setNode := func(state string) {
		cfg := f.cfg
		cfg.NodeRows = tkNodes()
		for i := range cfg.NodeRows {
			if cfg.NodeRows[i].ID == "node-3" {
				cfg.NodeRows[i].State = state
			}
		}
		f.fake.Reconfigure(cfg)
		if err := f.clusters.Refresh(t.Context(), f.env.ID); err != nil {
			t.Fatalf("refresh the cluster: %v", err)
		}
	}
	setNode("down")
	n := readNotices(t, c, 1)[0]
	if n.Kind != "down" || n.Node != "wrk-ready" || n.At == "" {
		t.Fatalf("notice = %+v, want wrk-ready down", n)
	}
	setNode("ready")
	n = readNotices(t, c, 1)[0]
	if n.Kind != "back" || n.Node != "wrk-ready" {
		t.Fatalf("notice = %+v, want wrk-ready back", n)
	}
}

func TestServiceLogStreamTellsOnceAfterTheBurstOfANodeThatIsAlreadyDown(t *testing.T) {
	tasks := append(svcTasks(), tkRunning("tk-dn", "svc-web", "node-2", 4, "c4", time.Minute))
	f := newSvcLogsFix(t, tasks, svcBurstLines(), fastRefresh)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	// The notice comes after the burst, so it is not above the lines it follows.
	logs := 0
	var notice svcNodeNotice
	for notice.Kind == "" {
		fr, ok := c.next(10 * time.Second)
		if !ok {
			t.Fatalf("no node notice arrived after %d log events", logs)
		}
		switch fr.event {
		case "log":
			logs++
		case "node":
			if err := json.Unmarshal([]byte(fr.data), &notice); err != nil {
				t.Fatal(err)
			}
		}
	}
	if logs == 0 {
		t.Error("the node notice came before the burst's lines")
	}
	if notice.Kind != "down" || notice.Node != "wrk-down" {
		t.Errorf("notice = %+v, want wrk-down down", notice)
	}
}

func TestServiceLogStreamWithStoppedTasksShowsTheMarkersOfTheHistoryThatFallInTheBurst(t *testing.T) {
	// tk-a (slot 1) failed at +20 ms and tk-b took over at +10 ms; tk-c (slot 2) failed an hour
	// before the first line and tk-d replaced it: that marker is older than anything shown.
	tasks := []swarm.Task{
		at(tkExited("tk-a", "svc-web", "node-1", 1, "failed", "shutdown", 1, "x", 0), svcT0.Add(-time.Minute), svcT0.Add(20*time.Millisecond)),
		at(tkRunning("tk-b", "svc-web", "node-1", 1, "cb", 0), svcT0.Add(10*time.Millisecond), svcT0.Add(10*time.Millisecond)),
		at(tkExited("tk-c", "svc-web", "node-3", 2, "shutdown", "shutdown", 0, "", 0), svcT0.Add(-2*time.Hour), svcT0.Add(-time.Hour)),
		at(tkRunning("tk-d", "svc-web", "node-3", 2, "cd", 0), svcT0.Add(-time.Hour), svcT0.Add(-time.Hour)),
		tkRunning("tk-3", "svc-web", "node-1", 3, "c3", time.Minute),
	}
	lines := []dockertest.FakeServiceLogLine{
		svcLine("tk-a", "node-1", 5, "a1"), svcLine("tk-a", "node-1", 15, "a2"), svcLine("tk-b", "node-1", 40, "b1"),
	}
	f := newSvcLogsFix(t, tasks, lines, fastRefresh)
	c := f.open(t, f.cookie, "web", "sid="+sidA+"&stopped=1", "")
	m := readMarkers(t, c, 1)[0]
	if m.Kind != "replaced" || m.Old != "tk-a" || m.New != "tk-b" || m.At != svcStamp(20) {
		t.Fatalf("marker = %+v, want tk-a replaced by tk-b at +20 ms", m)
	}
	// A live marker follows; tk-c's, older than the first line, was not sent before it.
	f.retask(append(slices.Clone(tasks[:4]),
		tkExited("tk-3", "svc-web", "node-1", 3, "failed", "shutdown", 2, "x", time.Minute),
		tkTask("tk-3n", "svc-web", "node-1", 3, "starting", "running", time.Second)))
	if next := readMarkers(t, c, 1)[0]; next.Old != "tk-3" {
		t.Fatalf("next marker = %+v, want tk-3's: the history before the first line is not shown", next)
	}
}

func TestServiceLogStreamWithoutStoppedTasksSendsNoHistoryMarkers(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), fastRefresh)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	readSvcLines(t, c, 6)
	// The next marker is a live one: tk-old's replacement by tk-1 happened before the stream.
	f.retask([]swarm.Task{
		tkExited("tk-1", "svc-web", "node-1", 1, "failed", "shutdown", 1, "x", time.Minute),
		tkTask("tk-1n", "svc-web", "node-1", 1, "starting", "running", time.Second),
		tkRunning("tk-2", "svc-web", "node-3", 2, "c2", time.Minute),
		tkRunning("tk-3", "svc-web", "node-1", 3, "c3", time.Minute),
		tkExited("tk-old", "svc-web", "node-1", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	})
	if m := readMarkers(t, c, 1)[0]; m.Old != "tk-1" {
		t.Fatalf("first marker = %+v, want the live one (tk-1)", m)
	}
}
