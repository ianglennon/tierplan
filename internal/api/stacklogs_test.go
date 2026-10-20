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

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
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
func (f *stackLogsFix) auditRowsOf(t *testing.T, action string) []auditRowFull {
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
