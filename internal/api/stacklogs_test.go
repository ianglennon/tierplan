// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The merged log of a stack (plan 07-29, STK-10, D-22). Docker is a fake swarm manager (or a
// standalone host) whose services and containers belong to a stack created through the API,
// behind the real monitor, cluster reader and router; the stream is read frame by frame as the
// container and service log tests read theirs.

type stackLogLine struct {
	T       string `json:"t"`
	S       string `json:"s"`
	M       string `json:"m"`
	Task    string `json:"task"`
	Slot    int    `json:"slot"`
	Node    string `json:"node"`
	Service string `json:"service"`
}

type stackLogEvent struct {
	Lines []stackLogLine `json:"lines"`
}

type stackReadyTask struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	Slot    int    `json:"slot"`
	Node    string `json:"node"`
	State   string `json:"state"`
}

type stackReady struct {
	TTY    bool             `json:"tty"`
	Target string           `json:"target"`
	Tasks  []stackReadyTask `json:"tasks"`
}

type stackLogMarker struct {
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Service string `json:"service"`
	Unit    string `json:"unit"`
	Slot    int    `json:"slot"`
	Old     string `json:"old"`
	New     string `json:"new"`
	OldNode string `json:"oldNode"`
}

type stackLogsFix struct {
	*envFix
	env   envBody
	stack stackAnswer
	cfg   dockertest.FakeConfig
	user  *roleCaller
	// burstEnded gets one value each time a stream has sent the burst the engine opened with.
	burstEnded chan struct{}
}

// awaitBurstEnd waits until a stream has sent its opening burst, so what the engine writes after
// that is live.
func (f *stackLogsFix) awaitBurstEnd(t *testing.T) {
	t.Helper()
	select {
	case <-f.burstEnded:
	case <-testsupport.Context(t).Done():
		t.Fatal("the stack log stream never finished its opening burst")
	}
}

// stackLogsTasks are the tasks of the shop stack: web has two, api one, and web's slot 1 once
// failed.
func stackLogsTasks() []swarm.Task {
	return []swarm.Task{
		tkRunning("tk-w1", "svc-web", "node-1", 1, "cw1", time.Minute),
		tkRunning("tk-w2", "svc-web", "node-3", 2, "cw2", time.Minute),
		tkRunning("tk-a1", "svc-api", "node-1", 1, "ca1", time.Minute),
		tkExited("tk-w0", "svc-web", "node-1", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	}
}

func stackLogsBurst() map[string][]dockertest.FakeServiceLogLine {
	return map[string][]dockertest.FakeServiceLogLine{
		"svc-web": {
			svcLine("tk-w1", "node-1", 10, "web1 a"), svcLine("tk-w1", "node-1", 60, "web1 b"),
			svcLine("tk-w2", "node-3", 30, "web2 a"),
			svcLine("tk-w0", "node-1", 5, "web0 old"),
		},
		"svc-api": {
			svcLine("tk-a1", "node-1", 20, "api a"), svcLine("tk-a1", "node-1", 40, "api b"),
		},
	}
}

// newStackLogsFix builds a swarm manager of four nodes with the stack shop's two services, their
// tasks and the given lines, and a router whose stack log stream is tuned by tune.
func newStackLogsFix(t *testing.T, tasks []swarm.Task, lines map[string][]dockertest.FakeServiceLogLine, tune func(*envFix)) *stackLogsFix {
	t.Helper()
	burstEnded := make(chan struct{}, 16)
	f, e := stackEnvTuned(t, func(f *envFix) {
		f.stackLogs = api.StackLogsTuning{
			BurstEnded: func() {
				select {
				case burstEnded <- struct{}{}:
				default:
				}
			},
		}
		if tune != nil {
			tune(f)
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "shop")
	fix := &stackLogsFix{envFix: f, env: e, stack: st, burstEnded: burstEnded}
	fix.cfg = swarmManagerEngine(
		ownedService(st, "web", 2, 2), ownedService(st, "api", 1, 1),
	)
	fix.cfg.SwarmNodeID, fix.cfg.SwarmClusterID = "node-1", "cluster-1"
	fix.cfg.NodeRows = tkNodes()
	fix.cfg.Tasks = tasks
	fix.cfg.ServiceLogs = lines
	fix.reconfigure(t, fix.cfg)
	fix.user = f.caller(t, e, "viewer-1", "viewer")
	return fix
}

// reconfigure sets what the fake swarm holds and reads the cluster again.
func (f *stackLogsFix) reconfigure(t *testing.T, cfg dockertest.FakeConfig) {
	t.Helper()
	f.cfg = cfg
	f.fake.Reconfigure(cfg)
	if err := f.clusters.Refresh(testsupport.Context(t), f.env.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func (f *stackLogsFix) path(query string) string {
	return "/api/env/" + f.env.ID + "/stacks/" + f.stack.ID + "/logs/stream?" + query
}

func (f *stackLogsFix) open(t *testing.T, query, lastEventID string) *sseConn {
	t.Helper()
	return openSSE(t, f.authFixture, f.path(query), f.user.cookie, lastEventID)
}

// readStackLines reads log events until n lines have arrived and returns them with the id of the
// last event. Other events are skipped.
func readStackLines(t *testing.T, c *sseConn, n int) (lines []stackLogLine, lastID string) {
	t.Helper()
	for len(lines) < n {
		fr := c.nextData(t, fmt.Sprintf("%d more log lines (have %d: %+v)", n-len(lines), len(lines), lines))
		if fr.event != "log" {
			continue
		}
		var ev stackLogEvent
		if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
			t.Fatalf("log data %q: %v", fr.data, err)
		}
		lines = append(lines, ev.Lines...)
		lastID = fr.id
	}
	return lines, lastID
}

func stackTexts(lines []stackLogLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.M)
	}
	return out
}

func stackReadyOf(t *testing.T, c *sseConn) stackReady {
	t.Helper()
	var rd stackReady
	if err := json.Unmarshal([]byte(nextEvent(t, c, "ready").data), &rd); err != nil {
		t.Fatal(err)
	}
	return rd
}

// auditRowsOf are the audit rows of one action, oldest first.
func (f *envFix) auditRowsOf(t *testing.T, action string) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
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

func (f *stackLogsFix) viewedRows(t *testing.T) []auditRowFull {
	t.Helper()
	return f.auditRowsOf(t, audit.ActionLogsViewed)
}

func TestStackLogsStreamMergesTheLinesOfEveryServiceInTimeOrderWithTheirServiceSlotAndNode(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	c := f.open(t, "sid="+sidA, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	if got := c.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	rd := stackReadyOf(t, c)
	wantTasks := []stackReadyTask{
		{"tk-a1", "api", 1, "mgr-1", "running"},
		{"tk-w1", "web", 1, "mgr-1", "running"},
		{"tk-w2", "web", 2, "wrk-ready", "running"},
	}
	if rd.TTY || rd.Target != "swarm" || !slices.Equal(rd.Tasks, wantTasks) {
		t.Fatalf("ready = %+v, want a swarm stack whose live tasks are %+v (the failed task is hidden: stopped is off)", rd, wantTasks)
	}

	// Five lines are shown: the failed task's line is not one of them.
	lines, id := readStackLines(t, c, 5)
	if got, want := stackTexts(lines), []string{"web1 a", "api a", "web2 a", "api b", "web1 b"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want %v: both services merged by time", got, want)
	}
	web2 := lines[2]
	if web2.Service != "web" || web2.Task != "tk-w2" || web2.Slot != 2 || web2.Node != "wrk-ready" || web2.S != "o" || web2.T != svcStamp(30) {
		t.Errorf("web2 a = %+v, want service web, task tk-w2, slot 2 on wrk-ready, stdout at +30ms (the stack prefix is not in the service name)", web2)
	}
	if api := lines[1]; api.Service != "api" || api.Slot != 1 {
		t.Errorf("api a = %+v", api)
	}
	if id != svcStamp(60) {
		t.Errorf("event id = %q, want the last line's timestamp %q", id, svcStamp(60))
	}
}

func TestStackLogsOrdersLinesAtTheSameTimeByServiceNameThenSlot(t *testing.T) {
	t.Parallel()
	same := map[string][]dockertest.FakeServiceLogLine{
		"svc-web": {svcLine("tk-w2", "node-3", 10, "web.2"), svcLine("tk-w1", "node-1", 10, "web.1")},
		"svc-api": {svcLine("tk-a1", "node-1", 10, "api.1")},
	}
	f := newStackLogsFix(t, stackLogsTasks(), same, nil)
	c := f.open(t, "sid="+sidA, "")
	got, _ := readStackLines(t, c, 3)
	if want := []string{"api.1", "web.1", "web.2"}; !slices.Equal(stackTexts(got), want) {
		t.Fatalf("lines = %v, want %v: api before web, web's lower slot first", stackTexts(got), want)
	}
}

func TestStackLogsWritesOneViewedRowPerViewerWithTargetTypeStack(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	c := f.open(t, "sid="+sidA, "")
	readStackLines(t, c, 5)
	// The same viewer reconnects (the browser sends the last event ID): no second row.
	again := f.open(t, "sid="+sidA, svcStamp(60))
	nextEvent(t, again, "ready")
	// Another viewer, another window: its own row. The fence is that row.
	other := f.open(t, "sid="+sidB, "")
	nextEvent(t, other, "ready")

	rows := f.viewedRows(t)
	if len(rows) != 2 {
		t.Fatalf("logs.viewed rows = %+v, want one per viewer (2)", rows)
	}
	first := rows[0]
	if first.TargetType != "stack" || first.TargetID != f.stack.ID || first.Details["stack"] != "shop" || first.Details["sid"] != sidA {
		t.Errorf("first row = %+v, want target type stack, the stack's ID and name, and the viewer's sid", first)
	}
	if rows[1].Details["sid"] != sidB {
		t.Errorf("second row = %+v, want the other viewer", rows[1])
	}
}

func TestStackLogsOfAStackOfAnotherEnvironmentIsNotFound(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), nil, nil)
	c := openSSE(t, f.authFixture, "/api/env/"+f.env.ID+"/stacks/00000000-0000-0000-0000-000000000000/logs/stream?sid="+sidA, f.user.cookie, "")
	if c.status0() != http.StatusNotFound {
		t.Fatalf("status = %d: %s, want 404 stack_not_found", c.status0(), c.bodyString())
	}
}

func TestStackLogsRefusesAMalformedQuery(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), nil, nil)
	for name, query := range map[string]string{
		"no viewer id":  "tail=5",
		"a bad stopped": "sid=" + sidA + "&stopped=maybe",
	} {
		c := f.open(t, query, "")
		if c.status0() != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d: %s, want 422", name, c.status0(), c.bodyString())
		}
	}
}

// fastStackRefresh makes the stream read its tasks every 20 ms, so a change shows within the time
// it takes to wait for the event; the tests wait for events, not for the clock.
func fastStackRefresh(f *envFix) { f.stackLogs.TaskRefresh = 20 * time.Millisecond }

func TestStackLogsHoldsLiveLinesOfSeveralServicesInAWindowAndSendsThemInTimeOrder(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), nil, nil)
	c := f.open(t, "sid="+sidA, "")
	nextEvent(t, c, "ready")
	// Let the (empty) burst end, so what follows is live.
	f.awaitBurstEnd(t)
	// The later line of one service is written first; both are within the window.
	f.fake.PushServiceLog("svc-web", svcLine("tk-w2", "node-3", 200, "web later"))
	f.fake.PushServiceLog("svc-api", svcLine("tk-a1", "node-1", 100, "api earlier"))
	lines, id := readStackLines(t, c, 2)
	if got, want := stackTexts(lines), []string{"api earlier", "web later"}; !slices.Equal(got, want) {
		t.Fatalf("live lines = %v, want %v", got, want)
	}
	if id != svcStamp(200) {
		t.Errorf("event id = %q, want %q", id, svcStamp(200))
	}
}

func TestStackLogsMarkersNameTheServiceWhoseTaskWasReplaced(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), nil, fastStackRefresh)
	c := f.open(t, "sid="+sidA, "")
	nextEvent(t, c, "ready")
	f.awaitBurstEnd(t)

	// api's only task fails with exit 3 and the swarm starts tk-a2 in its slot on the worker; web's
	// slot 1 is also slot 1 and must not be taken for it.
	cfg := f.cfg
	cfg.Tasks = []swarm.Task{
		tkRunning("tk-w1", "svc-web", "node-1", 1, "cw1", time.Minute),
		tkRunning("tk-w2", "svc-web", "node-3", 2, "cw2", time.Minute),
		tkExited("tk-a1", "svc-api", "node-1", 1, "failed", "shutdown", 3, "boom", time.Minute),
		tkTask("tk-a2", "svc-api", "node-3", 1, "starting", "running", time.Second),
		tkExited("tk-w0", "svc-web", "node-1", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	}
	f.fake.Reconfigure(cfg)

	var m stackLogMarker
	var tasks stackReady
	for m.Kind == "" {
		fr := c.nextData(t, "a marker")
		switch fr.event {
		case "tasks":
			if err := json.Unmarshal([]byte(fr.data), &tasks); err != nil {
				t.Fatal(err)
			}
		case "marker":
			if err := json.Unmarshal([]byte(fr.data), &m); err != nil {
				t.Fatal(err)
			}
		}
	}
	if m.Kind != "replaced" || m.Service != "api" || m.Slot != 1 || m.Old != "tk-a1" || m.New != "tk-a2" || m.Unit != "" {
		t.Errorf("marker = %+v, want api's tk-a1 replaced by tk-a2 in slot 1 (a task, not a container)", m)
	}
	if !slices.ContainsFunc(tasks.Tasks, func(x stackReadyTask) bool { return x.ID == "tk-a2" && x.Service == "api" && x.Node == "wrk-ready" }) {
		t.Errorf("tasks event = %+v, want it to name api's tk-a2 on wrk-ready", tasks.Tasks)
	}
}

func TestStackLogsHideTheKnownSecretValuesOfTheStackInEveryLine(t *testing.T) {
	t.Parallel()
	lines := map[string][]dockertest.FakeServiceLogLine{
		"svc-web": {
			svcLine("tk-w1", "node-1", 10, "connecting with password s3cr3t-value now"),
			svcLine("tk-w2", "node-3", 20, "nothing to hide here"),
		},
		"svc-api": {svcLine("tk-a1", "node-1", 30, "token=s3cr3t-value and again s3cr3t-value")},
	}
	f := newStackLogsFix(t, stackLogsTasks(), lines, nil)
	operator := f.caller(t, f.env, "operator-1", "operator")
	compose := "services:\n  web:\n    image: busybox:1\n    environment:\n      PW: ${DB_PASSWORD}\n"
	if rec := operator.saveDraft(f.stack.ID, saveBody(1, compose, `[{"name":"DB_PASSWORD","value":"s3cr3t-value","secret":true}]`)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	c := f.open(t, "sid="+sidA, "")
	got, _ := readStackLines(t, c, 3)

	if want := []string{"connecting with password <hidden> now", "nothing to hide here", "token=<hidden> and again <hidden>"}; !slices.Equal(stackTexts(got), want) {
		t.Fatalf("lines = %v, want the known value hidden wherever it appears and the rest left as written", stackTexts(got))
	}
	if strings.Contains(c.bodyString(), "s3cr3t-value") {
		t.Errorf("the stream holds the secret value: %q", c.bodyString())
	}
}

// ---- narrowing, the follow cap and the swarm cases the tracer left ----

type stackLogCapEvent struct {
	Count int    `json:"count"`
	Cap   int    `json:"cap"`
	Unit  string `json:"unit"`
}

func capOf(t *testing.T, c *sseConn) stackLogCapEvent {
	t.Helper()
	var ev stackLogCapEvent
	if err := json.Unmarshal([]byte(nextEvent(t, c, "cap").data), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestStackLogsNarrowToTheServicesNamedByTheirNameInTheComposeFile(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	c := f.open(t, "sid="+sidA+"&services=api", "")
	rd := stackReadyOf(t, c)
	if len(rd.Tasks) != 1 || rd.Tasks[0].Service != "api" {
		t.Fatalf("ready tasks = %+v, want api's only", rd.Tasks)
	}
	got, _ := readStackLines(t, c, 2)
	if want := []string{"api a", "api b"}; !slices.Equal(stackTexts(got), want) {
		t.Fatalf("lines = %v, want %v", stackTexts(got), want)
	}
	// The fence: once the burst is over a live line of the service left out is not shown, and
	// the one after it for the service kept is the next line.
	f.awaitBurstEnd(t)
	f.fake.PushServiceLog("svc-web", svcLine("tk-w1", "node-1", 300, "web live"))
	f.fake.PushServiceLog("svc-api", svcLine("tk-a1", "node-1", 400, "api live"))
	if more, _ := readStackLines(t, c, 1); more[0].M != "api live" {
		t.Errorf("the next line = %+v, want api's: the service left out shows nothing", more)
	}
}

func TestStackLogsNarrowToTheTasksAndNodesNamed(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)

	byTask := f.open(t, "sid="+sidA+"&tasks=tk-w2", "")
	got, _ := readStackLines(t, byTask, 1)
	if got[0].M != "web2 a" || got[0].Task != "tk-w2" {
		t.Fatalf("lines = %+v, want web's tk-w2 alone", got)
	}

	byNode := f.open(t, "sid="+sidB+"&nodes=wrk-ready", "")
	got, _ = readStackLines(t, byNode, 1)
	if got[0].M != "web2 a" || got[0].Node != "wrk-ready" {
		t.Fatalf("lines = %+v, want the one task on wrk-ready", got)
	}
}

func TestStackLogsShowTheTasksThatEndedOnlyWhenStoppedIsSet(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	c := f.open(t, "sid="+sidA+"&stopped=1", "")
	rd := stackReadyOf(t, c)
	if len(rd.Tasks) != 4 {
		t.Errorf("ready tasks = %+v, want all four with stopped=1", rd.Tasks)
	}
	lines, _ := readStackLines(t, c, 6)
	if lines[0].M != "web0 old" || lines[0].Task != "tk-w0" || lines[0].Service != "web" {
		t.Errorf("first line = %+v, want the failed task's line first", lines[0])
	}
}

func TestStackLogsOfMoreServicesThanTheCapFollowTheCapAndSayHowManyTasksThereAre(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), func(f *envFix) { f.stackLogs.MaxFollow = 1 })
	c := f.open(t, "sid="+sidA, "")
	rd := stackReadyOf(t, c)
	cp := capOf(t, c)
	if cp.Count != 3 || cp.Cap != 1 || cp.Unit != "tasks" {
		t.Errorf("cap = %+v, want 1 of the stack's 3 tasks", cp)
	}
	services := map[string]bool{}
	for _, tk := range rd.Tasks {
		services[tk.Service] = true
	}
	if len(services) != 1 {
		t.Errorf("ready tasks = %+v, want the tasks of one service", rd.Tasks)
	}
	// One upstream stream was opened, not two.
	got, _ := readStackLines(t, c, 2)
	if got[0].Service != got[1].Service {
		t.Errorf("lines = %+v, want one service's", got)
	}
}

func TestStackLogsOfAStackWithinTheCapSendNoCapEvent(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	c := f.open(t, "sid="+sidA, "")
	stackReadyOf(t, c)
	readStackLines(t, c, 5)
	if strings.Contains(c.bodyString(), "event: cap") {
		t.Errorf("the stream told of a cap: %q", c.bodyString())
	}
}

// ---- a standalone stack ----

const soloCompose = "services:\n  web:\n    image: nginx:1.27-alpine\n  db:\n    image: postgres:17\n"

var soloT0 = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

func soloLine(ms int, text string) dockertest.FakeLogLine {
	return dockertest.FakeLogLine{Time: soloT0.Add(time.Duration(ms) * time.Millisecond), Text: text}
}

type soloFix struct {
	*envFix
	env        envBody
	stack      stackAnswer
	user       *roleCaller
	burstEnded chan struct{}
}

func (f *soloFix) awaitBurstEnd(t *testing.T) {
	t.Helper()
	select {
	case <-f.burstEnded:
	case <-testsupport.Context(t).Done():
		t.Fatal("the stack log stream never finished its opening burst")
	}
}

// soloContainer is a container of the stack solo as the engine lists it.
func soloContainer(stackID, id, service string, number int, state string, created int64) dockertest.FakeContainer {
	return dockertest.FakeContainer{
		ID: id, Names: []string{"/solo-" + service + "-" + strconv.Itoa(number)}, Image: "img", State: state, Created: created,
		Labels: map[string]string{
			"com.docker.compose.project": "solo", "com.docker.compose.service": service,
			"com.docker.compose.container-number": strconv.Itoa(number), "tierplan.stack.id": stackID,
		},
	}
}

// newSoloFix builds the server's own standalone host with the stack solo, whose containers are
// made by build from the stack's ID, and a router whose stack log stream is tuned by tune.
func newSoloFix(t *testing.T, build func(stackID string) []dockertest.FakeContainer, tune func(*envFix)) *soloFix {
	t.Helper()
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	f := newEnvFix(t, cfg)
	// The stream follows a container across its recreation through the events hub.
	f.hub = envs.NewHub(f.mon, f.snaps, slog.New(slog.DiscardHandler), time.Now)
	t.Cleanup(f.hub.Close)
	f.agg = envs.NewAggregator(f.svc, f.snaps, f.hub, f.clk.now)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	burstEnded := make(chan struct{}, 16)
	f.stackLogs = api.StackLogsTuning{
		BurstEnded: func() {
			select {
			case burstEnded <- struct{}{}:
			default:
			}
		},
	}
	if tune != nil {
		tune(f)
	}
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "solo")
	if st.Target != "standalone" {
		t.Fatalf("target = %q, want standalone", st.Target)
	}
	cfg.Containers = build(st.ID)
	f.fake.Reconfigure(cfg)
	return &soloFix{envFix: f, env: e, stack: st, user: f.caller(t, e, "viewer-1", "viewer"), burstEnded: burstEnded}
}

func (f *soloFix) reconfigure(containers []dockertest.FakeContainer) {
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	cfg.Containers = containers
	f.fake.Reconfigure(cfg)
}

func (f *soloFix) open(t *testing.T, query, lastEventID string) *sseConn {
	t.Helper()
	return openSSE(t, f.authFixture, "/api/env/"+f.env.ID+"/stacks/"+f.stack.ID+"/logs/stream?"+query, f.user.cookie, lastEventID)
}

// soloTwoServices are web's two containers and db's one, all running.
func soloTwoServices(stackID string) []dockertest.FakeContainer {
	return []dockertest.FakeContainer{
		soloContainer(stackID, "cweb1", "web", 1, "running", 100),
		soloContainer(stackID, "cweb2", "web", 2, "running", 110),
		soloContainer(stackID, "cdb1", "db", 1, "running", 90),
	}
}

func (f *soloFix) seed() {
	f.fake.AppendLogs("cweb1", soloLine(10, "web1 a"), soloLine(60, "web1 b"))
	f.fake.AppendLogs("cweb2", soloLine(30, "web2 a"))
	f.fake.AppendLogs("cdb1", soloLine(20, "db a"), soloLine(40, "db b"))
}

func TestStackLogsOfAStandaloneStackFollowEachContainerAndMergeThemWithTheirServiceAndNumber(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, soloTwoServices, nil)
	f.seed()
	c := f.open(t, "sid="+sidA, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	rd := stackReadyOf(t, c)
	want := []stackReadyTask{
		{"cdb1", "db", 1, "", "running"},
		{"cweb1", "web", 1, "", "running"},
		{"cweb2", "web", 2, "", "running"},
	}
	if rd.Target != "standalone" || !slices.Equal(rd.Tasks, want) {
		t.Fatalf("ready = %+v, want a standalone stack whose containers are %+v", rd, want)
	}
	lines, _ := readStackLines(t, c, 5)
	if got, want := stackTexts(lines), []string{"web1 a", "db a", "web2 a", "db b", "web1 b"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want %v: the containers merged by time", got, want)
	}
	if l := lines[2]; l.Service != "web" || l.Slot != 2 || l.Task != "cweb2" || l.Node != "" {
		t.Errorf("web2 a = %+v, want service web, number 2, the container's ID as task and no node", l)
	}
	if rows := f.auditRowsOf(t, audit.ActionLogsViewed); len(rows) != 1 || rows[0].TargetType != "stack" || rows[0].TargetID != f.stack.ID {
		t.Errorf("logs.viewed rows = %+v, want one for the stack", rows)
	}
}

// soloCLIContainer is the container docker compose up --scale adds to the project from the host CLI: none of
// TierPlan's labels, the CLI's own config hash and version (G-08-6).
func soloCLIContainer(id, service string, number int, state string, created int64) dockertest.FakeContainer {
	c := soloContainer("", id, service, number, state, created)
	delete(c.Labels, "tierplan.stack.id")
	c.Labels["com.docker.compose.config-hash"] = "cli-hash"
	c.Labels["com.docker.compose.version"] = "5.6.1"
	return c
}

// soloWithCLIAndStranger is web-1 and db-1 the stack owns, web-2 the host CLI added, and web-3 that carries another
// stack's ID.
func soloWithCLIAndStranger(stackID string) []dockertest.FakeContainer {
	return []dockertest.FakeContainer{
		soloContainer(stackID, "cweb1", "web", 1, "running", 100),
		soloCLIContainer("cweb2", "web", 2, "running", 110),
		soloContainer("stack-other", "cweb3", "web", 3, "running", 120),
		soloContainer(stackID, "cdb1", "db", 1, "running", 90),
	}
}

// TestStackLogsOfAStandaloneStackFollowAContainerTheHostCLIAdded: docker compose up --scale web=2 adds web-2 to the
// project with none of TierPlan's labels. Beside the containers the stack owns it is the person's own change to the
// stack, so the stack's merged log follows it under its service and number, and masks the stack's known secret
// values in it. A container that carries another stack's ID is not followed (G-08-6, P7 D-12, T-08-47).
func TestStackLogsOfAStandaloneStackFollowAContainerTheHostCLIAdded(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, soloWithCLIAndStranger, nil)
	f.fake.AppendLogs("cweb1", soloLine(10, "web1 a"))
	f.fake.AppendLogs("cweb2", soloLine(30, "login with s3cr3t-value ok"))
	f.fake.AppendLogs("cweb3", soloLine(40, "stranger"))
	f.fake.AppendLogs("cdb1", soloLine(20, "db a"))
	operator := f.caller(t, f.env, "operator-1", "operator")
	if rec := operator.saveDraft(f.stack.ID, saveBody(1, soloCompose, `[{"name":"DB_PASSWORD","value":"s3cr3t-value","secret":true}]`)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	c := f.open(t, "sid="+sidA, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	rd := stackReadyOf(t, c)
	want := []stackReadyTask{
		{"cdb1", "db", 1, "", "running"},
		{"cweb1", "web", 1, "", "running"},
		{"cweb2", "web", 2, "", "running"},
	}
	if !slices.Equal(rd.Tasks, want) {
		t.Fatalf("ready tasks = %+v, want the stack's containers and the one the host CLI added, not another stack's: %+v", rd.Tasks, want)
	}
	lines, _ := readStackLines(t, c, 3)
	if got, want := stackTexts(lines), []string{"web1 a", "db a", "login with <hidden> ok"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want %v: the added container's line merged by time, its secret hidden", got, want)
	}
	if l := lines[2]; l.Service != "web" || l.Slot != 2 || l.Task != "cweb2" {
		t.Errorf("web-2 line = %+v, want service web, number 2 and the container's ID as task", l)
	}
	if strings.Contains(c.bodyString(), "s3cr3t-value") || strings.Contains(c.bodyString(), "stranger") {
		t.Errorf("the stream holds a secret value or another stack's line: %q", c.bodyString())
	}
}

func TestStackLogsOfAStandaloneStackOrderLinesAtTheSameTimeByServiceThenNumber(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, soloTwoServices, nil)
	f.fake.AppendLogs("cweb2", soloLine(10, "web.2"))
	f.fake.AppendLogs("cweb1", soloLine(10, "web.1"))
	f.fake.AppendLogs("cdb1", soloLine(10, "db.1"))
	c := f.open(t, "sid="+sidA, "")
	got, _ := readStackLines(t, c, 3)
	if want := []string{"db.1", "web.1", "web.2"}; !slices.Equal(stackTexts(got), want) {
		t.Fatalf("lines = %v, want %v", stackTexts(got), want)
	}
}

func TestStackLogsOfAStandaloneStackNarrowToServicesContainersAndStopped(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, func(id string) []dockertest.FakeContainer {
		return append(soloTwoServices(id), soloContainer(id, "cweb3", "web", 3, "exited", 80))
	}, nil)
	f.seed()
	f.fake.AppendLogs("cweb3", soloLine(5, "web3 old"))

	// By service.
	byService := f.open(t, "sid="+sidA+"&services=db", "")
	rd := stackReadyOf(t, byService)
	if len(rd.Tasks) != 1 || rd.Tasks[0].ID != "cdb1" {
		t.Fatalf("ready tasks = %+v, want db's container", rd.Tasks)
	}
	if got, _ := readStackLines(t, byService, 2); !slices.Equal(stackTexts(got), []string{"db a", "db b"}) {
		t.Fatalf("lines = %v", stackTexts(got))
	}

	// By container: a name and the start of an ID both name one.
	byName := f.open(t, "sid="+sidB+"&containers=solo-web-2", "")
	if got, _ := readStackLines(t, byName, 1); got[0].M != "web2 a" {
		t.Fatalf("lines = %+v, want web's second container named by its name", got)
	}

	// A container that ended is left out unless stopped is set.
	running := f.open(t, "sid=ZZZZZZZZZZZZZZZZZZZZZZ&services=web", "")
	if rd := stackReadyOf(t, running); len(rd.Tasks) != 2 {
		t.Errorf("ready tasks = %+v, want web's two running containers", rd.Tasks)
	}
	all := f.open(t, "sid=YYYYYYYYYYYYYYYYYYYYYY&services=web&stopped=1", "")
	rd = stackReadyOf(t, all)
	if len(rd.Tasks) != 3 {
		t.Fatalf("ready tasks = %+v, want web's three containers with stopped=1", rd.Tasks)
	}
	if got, _ := readStackLines(t, all, 4); got[0].M != "web3 old" || got[0].Slot != 3 {
		t.Errorf("first line = %+v, want the exited container's, number 3", got[0])
	}
}

func TestStackLogsOfAStandaloneStackOverTheCapFollowTheNewestContainersAndSayHowManyThereAre(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, soloTwoServices, func(f *envFix) { f.stackLogs.MaxFollow = 2 })
	f.seed()
	c := f.open(t, "sid="+sidA, "")
	rd := stackReadyOf(t, c)
	cp := capOf(t, c)
	if cp.Count != 3 || cp.Cap != 2 || cp.Unit != "containers" {
		t.Errorf("cap = %+v, want 2 of the stack's 3 containers", cp)
	}
	var ids []string
	for _, tk := range rd.Tasks {
		ids = append(ids, tk.ID)
	}
	slices.Sort(ids)
	if want := []string{"cweb1", "cweb2"}; !slices.Equal(ids, want) {
		t.Errorf("followed = %v, want the newest two (web's, made later than db's) %v", ids, want)
	}
}

func TestStackLogsOfAStandaloneStackHideTheKnownSecretValuesOfTheStack(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, soloTwoServices, nil)
	f.fake.AppendLogs("cweb1", soloLine(10, "login with s3cr3t-value ok"))
	f.fake.AppendLogs("cdb1", soloLine(20, "nothing to hide"))
	operator := f.caller(t, f.env, "operator-1", "operator")
	if rec := operator.saveDraft(f.stack.ID, saveBody(1, soloCompose, `[{"name":"DB_PASSWORD","value":"s3cr3t-value","secret":true}]`)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	c := f.open(t, "sid="+sidA, "")
	got, _ := readStackLines(t, c, 2)

	if want := []string{"login with <hidden> ok", "nothing to hide"}; !slices.Equal(stackTexts(got), want) {
		t.Fatalf("lines = %v, want the value hidden", stackTexts(got))
	}
	if strings.Contains(c.bodyString(), "s3cr3t-value") {
		t.Errorf("the stream holds the secret value: %q", c.bodyString())
	}
}

func TestStackLogsOfAStandaloneStackReplaceARecreatedContainerWithAMarkerAndFollowTheNewOne(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, soloTwoServices, nil)
	f.seed()
	c := f.open(t, "sid="+sidA, "")
	stackReadyOf(t, c)
	readStackLines(t, c, 5)
	f.awaitBurstEnd(t)

	// A deploy recreates web's first container: the engine now lists the new one in its place, and
	// the new one starts.
	f.reconfigure([]dockertest.FakeContainer{
		soloContainer(f.stack.ID, "cweb1n", "web", 1, "running", 200),
		soloContainer(f.stack.ID, "cweb2", "web", 2, "running", 110),
		soloContainer(f.stack.ID, "cdb1", "db", 1, "running", 90),
	})
	f.fake.AppendLogs("cweb1n", soloLine(500, "web1 again"))
	started := soloT0.Add(400 * time.Millisecond)
	f.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "cweb1n", TimeNano: started.UnixNano(),
		Attributes: map[string]string{"name": "solo-web-1", "com.docker.compose.project": "solo"}})

	// The marker and the new container's first line may arrive in either order: the viewer places
	// the marker by its time. Both are read, whichever comes first.
	var m stackLogMarker
	var tasks stackReady
	var lines []stackLogLine
	for m.Kind == "" || len(lines) == 0 {
		fr := c.nextData(t, "the replacement marker and the new container's line")
		switch fr.event {
		case "tasks":
			if err := json.Unmarshal([]byte(fr.data), &tasks); err != nil {
				t.Fatal(err)
			}
		case "marker":
			if err := json.Unmarshal([]byte(fr.data), &m); err != nil {
				t.Fatal(err)
			}
		case "log":
			var ev stackLogEvent
			if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, ev.Lines...)
		}
	}
	if m.Kind != "replaced" || m.Service != "web" || m.Unit != "container" || m.Slot != 1 || m.Old != "cweb1" || m.New != "cweb1n" {
		t.Errorf("marker = %+v, want web's container cweb1 replaced by cweb1n", m)
	}
	if m.At != started.Format("2006-01-02T15:04:05.000000000Z") {
		t.Errorf("marker time = %q, want the engine's time of the start %q", m.At, started.Format("2006-01-02T15:04:05.000000000Z"))
	}
	if !slices.ContainsFunc(tasks.Tasks, func(x stackReadyTask) bool { return x.ID == "cweb1n" && x.Service == "web" && x.Slot == 1 }) ||
		slices.ContainsFunc(tasks.Tasks, func(x stackReadyTask) bool { return x.ID == "cweb1" }) {
		t.Errorf("tasks event = %+v, want the new container in the old one's place", tasks.Tasks)
	}
	if lines[0].M != "web1 again" || lines[0].Task != "cweb1n" || lines[0].Slot != 1 {
		t.Errorf("line = %+v, want the new container's first line", lines[0])
	}
}

func TestStackLogsOfAStandaloneStackFollowAContainerThatRestartedFromItsLastLine(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, soloTwoServices, nil)
	f.fake.AppendLogs("cweb1", soloLine(10, "first run"))
	c := f.open(t, "sid="+sidA+"&services=web&containers=cweb1", "")
	stackReadyOf(t, c)
	readStackLines(t, c, 1)
	f.awaitBurstEnd(t)

	f.fake.EndLogs("cweb1")
	f.fake.StartLogs("cweb1")
	// The log keeps the first run's line; the new run writes after it.
	f.fake.AppendLogs("cweb1", soloLine(900, "second run"))
	f.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "cweb1",
		Attributes: map[string]string{"name": "solo-web-1", "com.docker.compose.project": "solo"}})

	got, _ := readStackLines(t, c, 1)
	if got[0].M != "second run" {
		t.Errorf("line = %+v, want only the new run's line: the first is not repeated", got[0])
	}
}

// ---- the download ----

// stackDownload runs a download of a stack's log as the given signed-in user, to its end.
func stackDownload(t *testing.T, f *envFix, envID, stackID, query, cookie string) *dlWriter {
	t.Helper()
	return downloadAt(t, f, "/api/env/"+envID+"/stacks/"+stackID+"/logs/download?"+query, cookie)
}

// downloadAt runs a download of a log at a path, as the given signed-in user, to its end.
func downloadAt(t *testing.T, f *envFix, path, cookie string) *dlWriter {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("User-Agent", authUA)
	req.RemoteAddr = authIP + ":40000"
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
	w := newDLWriter(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(w, req)
	}()
	testsupport.Await(t, "the download to finish", done)
	return w
}

func (f *stackLogsFix) download(t *testing.T, query string) *dlWriter {
	t.Helper()
	return stackDownload(t, f.envFix, f.env.ID, f.stack.ID, query, f.user.cookie)
}

func TestStackLogDownloadMergesEveryServiceInTimeOrderEachStartingWithItsNameAndUTCTime(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	w := f.download(t, "")
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	want := strings.Join([]string{
		dlLine("web.1@mgr-1", 5, "web0 old"),
		dlLine("web.1@mgr-1", 10, "web1 a"),
		dlLine("api.1@mgr-1", 20, "api a"),
		dlLine("web.2@wrk-ready", 30, "web2 a"),
		dlLine("api.1@mgr-1", 40, "api b"),
		dlLine("web.1@mgr-1", 60, "web1 b"),
	}, "\n") + "\n"
	if got := w.body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	h := w.Header()
	wantName := "shop-" + f.clk.now().UTC().Format("20060102-1504") + ".log"
	if cd := h.Get("Content-Disposition"); cd != `attachment; filename="`+wantName+`"` {
		t.Errorf("Content-Disposition = %q, want the stack's name and the UTC minute %s", cd, wantName)
	}
	if h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", h)
	}
	if len(w.deadlns) == 0 || !w.deadlns[0].IsZero() {
		t.Errorf("write deadlines = %v, want the first cleared", w.deadlns)
	}
}

func TestStackLogDownloadHidesTheKnownSecretValuesAndIsAuditedOnceAsLogsDownloadedForTheStack(t *testing.T) {
	t.Parallel()
	lines := map[string][]dockertest.FakeServiceLogLine{
		"svc-web": {svcLine("tk-w1", "node-1", 10, "key is s3cr3t-value")},
		"svc-api": {svcLine("tk-a1", "node-1", 20, "fine")},
	}
	f := newStackLogsFix(t, stackLogsTasks(), lines, nil)
	operator := f.caller(t, f.env, "operator-1", "operator")
	compose := "services:\n  web:\n    image: busybox:1\n    environment:\n      PW: ${DB_PASSWORD}\n"
	if rec := operator.saveDraft(f.stack.ID, saveBody(1, compose, `[{"name":"DB_PASSWORD","value":"s3cr3t-value","secret":true}]`)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	w := f.download(t, "")

	if w.code() != http.StatusOK || strings.Contains(w.body.String(), "s3cr3t-value") || !strings.Contains(w.body.String(), "key is <hidden>") {
		t.Fatalf("download = %d %q, want the value hidden", w.code(), w.body.String())
	}
	rows := f.auditRowsOf(t, audit.ActionLogsDownloaded)
	if len(rows) != 1 || rows[0].TargetType != "stack" || rows[0].TargetID != f.stack.ID || rows[0].Details["stack"] != "shop" || rows[0].Result != "success" {
		t.Errorf("logs.downloaded rows = %+v, want one for the stack", rows)
	}
	for _, r := range rows {
		for k, v := range r.Details {
			if strings.Contains(k, "s3cr3t") || strings.Contains(v, "s3cr3t") || strings.Contains(v, "fine") {
				t.Errorf("audit details %v hold something of the log", r.Details)
			}
		}
	}
}

func TestStackLogDownloadReadsTheRangeAndCutsAtUntil(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	since, until := svcT0.Add(20*time.Millisecond), svcT0.Add(60*time.Millisecond)
	w := f.download(t, "since="+since.Format(time.RFC3339Nano)+"&until="+until.Format(time.RFC3339Nano))
	want := strings.Join([]string{
		dlLine("api.1@mgr-1", 20, "api a"),
		dlLine("web.2@wrk-ready", 30, "web2 a"),
		dlLine("api.1@mgr-1", 40, "api b"),
	}, "\n") + "\n"
	if got := w.body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	if rows := f.auditRowsOf(t, audit.ActionLogsDownloaded); len(rows) != 1 || rows[0].Details["since"] == "" {
		t.Errorf("audit rows = %+v, want the range asked for", rows)
	}
}

func TestStackLogDownloadPastItsSizeLeavesOutTheOldestLinesAndSaysSoFirst(t *testing.T) {
	t.Parallel()
	// A line is held as its 30-character stamp and its text; three of them fit.
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), func(f *envFix) { f.stackLogs.MaxDownloadBytes = 3 * 36 })
	w := f.download(t, "")
	lines := strings.Split(strings.TrimSuffix(w.body.String(), "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "TierPlan: the oldest lines of this range were left out") {
		t.Fatalf("body =\n%s\nwant a notice and the newest three lines", w.body.String())
	}
	if !strings.HasSuffix(lines[3], "web1 b") {
		t.Errorf("last line = %q, want the newest", lines[3])
	}
}

func TestStackLogDownloadOfAStandaloneStackIncludesTheContainersThatEnded(t *testing.T) {
	t.Parallel()
	f := newSoloFix(t, func(id string) []dockertest.FakeContainer {
		return append(soloTwoServices(id), soloContainer(id, "cweb3", "web", 3, "exited", 80))
	}, nil)
	f.seed()
	f.fake.AppendLogs("cweb3", soloLine(5, "web3 old"))

	w := stackDownload(t, f.envFix, f.env.ID, f.stack.ID, "", f.user.cookie)

	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	stamp := func(ms int) string {
		return soloT0.Add(time.Duration(ms) * time.Millisecond).Format("2006-01-02T15:04:05.000000000Z")
	}
	want := strings.Join([]string{
		"web-3 " + stamp(5) + " web3 old",
		"web-1 " + stamp(10) + " web1 a",
		"db-1 " + stamp(20) + " db a",
		"web-2 " + stamp(30) + " web2 a",
		"db-1 " + stamp(40) + " db b",
		"web-1 " + stamp(60) + " web1 b",
	}, "\n") + "\n"
	if got := w.body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="solo-`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestStackLogDownloadNeedsStacksReadAndAStackOfThisEnvironment(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)
	if w := stackDownload(t, f.envFix, f.env.ID, f.stack.ID, "", cookie); w.code() != http.StatusForbidden && w.code() != http.StatusNotFound {
		t.Errorf("a user with no role: status = %d, want a refusal", w.code())
	}
	if w := stackDownload(t, f.envFix, f.env.ID, "00000000-0000-0000-0000-000000000000", "", f.user.cookie); w.code() != http.StatusNotFound {
		t.Errorf("a stack of another environment: status = %d, want 404", w.code())
	}
	if w := stackDownload(t, f.envFix, f.env.ID, f.stack.ID, "since=nonsense", f.user.cookie); w.code() != http.StatusUnprocessableEntity {
		t.Errorf("a bad range: status = %d, want 422", w.code())
	}
}

func TestStackLogsRefuseAUsersOverTheirStreamLimit(t *testing.T) {
	t.Parallel()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), func(f *envFix) { f.stackLogs.MaxStreamsPerUser = 1 })
	first := f.open(t, "sid="+sidA, "")
	stackReadyOf(t, first)
	second := f.open(t, "sid="+sidB, "")
	if second.status0() != http.StatusTooManyRequests {
		t.Fatalf("second stream: status = %d: %s, want 429", second.status0(), second.bodyString())
	}
}

func TestStackLogDownloadAllowsOnlyTheServersShareAtOnceWhoeverAsks(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("y", 60<<10)
	lines := map[string][]dockertest.FakeServiceLogLine{"svc-web": {svcLine("tk-w1", "node-1", 1, text), svcLine("tk-w1", "node-1", 2, text)}}
	f := newStackLogsFix(t, stackLogsTasks(), lines, func(f *envFix) { f.stackLogs.MaxServerDownloads = 1 })
	start := func(w http.ResponseWriter) <-chan struct{} {
		req := httptest.NewRequest(http.MethodGet, "/api/env/"+f.env.ID+"/stacks/"+f.stack.ID+"/logs/download", nil)
		req.Header.Set("User-Agent", authUA)
		req.RemoteAddr = authIP + ":40000"
		req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: f.user.cookie}) //nolint:gosec // a request cookie as a browser sends it
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.h.ServeHTTP(w, req)
		}()
		return done
	}

	gate := make(chan struct{})
	first := newDLWriter(false)
	first.gate = gate
	done := start(first)
	testsupport.Await(t, "the first download to start writing", first.wrote)
	second := newDLWriter(true)
	testsupport.Await(t, "the second download to be answered", start(second))
	if second.code() != http.StatusTooManyRequests || !strings.Contains(second.body.String(), `"type":"stream_busy"`) {
		t.Fatalf("the second download = %d %s, want 429 stream_busy from the server's limit", second.code(), second.body.String())
	}
	close(gate)
	<-done
	// The slot is given back, and the user's own slot with it.
	if w := f.download(t, ""); w.code() != http.StatusOK {
		t.Errorf("a download after the first finished = %d, want 200", w.code())
	}
	if rows := f.auditRowsOf(t, audit.ActionLogsDownloaded); len(rows) != 2 {
		t.Errorf("%d logs.downloaded rows, want 2: a refused download is not a download", len(rows))
	}
}
