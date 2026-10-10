// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// The container log stream (plan 04-19, OBS-01, D-15, D-23, D-24, P3 D-10). Docker is
// a FakeDaemon with a scripted log behind the real monitor, aggregator and router;
// the stream is read frame by frame as the events tests read theirs.

const (
	sidA = "abcdefghijklmnopqrstuv"
	sidB = "ZYXWVUTSRQPONMLKJIHGFE"
)

// logLine is one line of a log event.
type logLine struct {
	T string `json:"t"`
	S string `json:"s"`
	M string `json:"m"`
}

type logEvent struct {
	Lines []logLine `json:"lines"`
}

type logsFix struct {
	*envFix
	env envBody
}

func logsEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: webID, Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
			{ID: dbID, Names: []string{"/db"}, Image: "postgres:17", State: "running", Status: "Up 3 hours",
				Inspect: dockertest.FakeInspect{Tty: true}},
		},
	}
}

// newLogsFix builds an environment fixture whose router has the log stream tuned by
// tune, which sets the logs fields of the fixture before the router is built.
func newLogsFix(t *testing.T, cfg dockertest.FakeConfig, tune func(*envFix)) *logsFix {
	t.Helper()
	f := newEnvFix(t, cfg)
	// The stream follows a container across its restarts through the events hub.
	f.hub = envs.NewHub(f.mon, f.snaps, slog.New(slog.DiscardHandler), time.Now)
	t.Cleanup(f.hub.Close)
	f.agg = envs.NewAggregator(f.svc, f.snaps, f.hub, f.clk.now)
	if tune != nil {
		tune(f)
	}
	f.h = f.build(t)
	e := f.add(t)
	// A read goes only to a node the monitor has seen online.
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return &logsFix{envFix: f, env: e}
}

func (f *logsFix) path(container, query string) string {
	return "/api/env/" + f.env.ID + "/containers/" + container + "/logs/stream?" + query
}

func (f *logsFix) open(t *testing.T, cookie, container, query, lastEventID string) *sseConn {
	t.Helper()
	return openSSE(t, f.authFixture, f.path(container, query), cookie, lastEventID)
}

// readLines reads log events until n lines have arrived, and returns them with the id of
// the last event.
func readLines(t *testing.T, c *sseConn, n int) (lines []logLine, lastID string) {
	t.Helper()
	for len(lines) < n {
		fr, ok := c.next(5 * time.Second)
		if !ok {
			t.Fatalf("only %d of %d lines arrived: %+v", len(lines), n, lines)
		}
		switch fr.event {
		case "ready", "state":
			continue
		case "log":
		default:
			t.Fatalf("unexpected event %q: %+v", fr.event, fr)
		}
		var ev logEvent
		if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
			t.Fatalf("log data %q: %v", fr.data, err)
		}
		lines = append(lines, ev.Lines...)
		lastID = fr.id
	}
	return lines, lastID
}

// nextEvent returns the next frame of the named event, skipping others.
func nextEvent(t *testing.T, c *sseConn, name string, wait time.Duration) frame {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		fr, ok := c.next(time.Until(deadline))
		if !ok {
			t.Fatalf("no %q event arrived", name)
		}
		if fr.event == name {
			return fr
		}
	}
}

type logAuditRow struct {
	Actor, Target, Env, Result string
	Details                    map[string]string
}

func (f *logsFix) logAudits(t *testing.T) []logAuditRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_id, target_id, COALESCE(env_id, ''), result, COALESCE(details, '{}')
		FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionLogsViewed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []logAuditRow
	for rows.Next() {
		var r logAuditRow
		var details string
		if err := rows.Scan(&r.Actor, &r.Target, &r.Env, &r.Result, &details); err != nil {
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

func TestLogStreamSendsReadyThenBatchedLinesWithTheStreamAndTheLastTimestampAsEventID(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 58, 0, 123456789, time.UTC)
	f.fake.AppendLogs("web",
		dockertest.FakeLogLine{Text: "a", Time: at},
		dockertest.FakeLogLine{Stream: "stderr", Text: "b", Time: at.Add(time.Second)})

	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	if got := c.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := c.header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := c.header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no so nginx does not buffer the stream", got)
	}
	ready, ok := c.next(5 * time.Second)
	if !ok || ready.event != "ready" || ready.data != `{"tty":false}` {
		t.Fatalf("first frame = %+v, want ready {tty:false}", ready)
	}
	lines, id := readLines(t, c, 2)
	want := []logLine{{T: "2026-10-09T12:58:00.123456789Z", S: "o", M: "a"}, {T: "2026-10-09T12:58:01.123456789Z", S: "e", M: "b"}}
	if lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("lines = %+v, want %+v", lines, want)
	}
	if id != want[1].T {
		t.Errorf("event id = %q, want the last line's timestamp %q", id, want[1].T)
	}

	// A line written afterwards follows.
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: "live", Time: at.Add(2 * time.Second)})
	more, _ := readLines(t, c, 1)
	if more[0].M != "live" || more[0].S != "o" {
		t.Errorf("followed line = %+v", more[0])
	}
}

func TestLogStreamOfATTYContainerIsRawAndReadySaysSo(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	f.fake.AppendLogs("db", dockertest.FakeLogLine{Text: "tty line"})
	c := f.open(t, f.cookie, dbID, "sid="+sidA, "")
	ready := nextEvent(t, c, "ready", 5*time.Second)
	if ready.data != `{"tty":true}` {
		t.Errorf("ready = %s", ready.data)
	}
	lines, _ := readLines(t, c, 1)
	if lines[0].M != "tty line" || lines[0].S != "o" {
		t.Errorf("line = %+v, want the text without its CR LF on stdout", lines[0])
	}
}

func TestLogStreamTailDefaultsTo500AndIsCappedAt50000(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	for query, want := range map[string]string{
		"sid=" + sidA:                  "500",
		"sid=" + sidA + "&tail=7":      "7",
		"sid=" + sidA + "&tail=all":    "50000",
		"sid=" + sidA + "&tail=999999": "50000",
	} {
		c := f.open(t, f.cookie, webID, query, "")
		nextEvent(t, c, "ready", 5*time.Second)
		qs := f.fake.LogQueries()
		if got := qs[len(qs)-1].Get("tail"); got != want {
			t.Errorf("%s: the engine was asked tail=%q, want %q", query, got, want)
		}
		if qs[len(qs)-1].Get("follow") != "1" || qs[len(qs)-1].Get("timestamps") != "1" {
			t.Errorf("%s: the engine was asked %v, want a follow with timestamps", query, qs[len(qs)-1])
		}
		c.close()
	}
}

func TestLogStreamNeedsAWellFormedViewerID(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	for _, query := range []string{"", "sid=short", "sid=" + strings.Repeat("a", 65), "sid=has%20spaces%20in%20it%20here", "sid=" + sidA + "&tail=-1", "sid=" + sidA + "&tail=lots", "sid=" + sidA + "&since=yesterday", "sid=" + sidA + "&after=nope"} {
		rec := f.req(t, http.MethodGet, f.path(webID, query), "")
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "validation_failed" {
			t.Errorf("%q: status = %d, type %q: %s", query, rec.code, rec.ptyp, rec.body)
		}
	}
	if q := f.fake.LogQueries(); len(q) != 0 {
		t.Errorf("a refused request still reached the engine: %v", q)
	}
}

func TestLogStreamOfAContainerTheEngineDoesNotHaveIs404BeforeAnyStream(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	rec := f.req(t, http.MethodGet, f.path("nope", "sid="+sidA), "")
	if rec.code != http.StatusNotFound || rec.ptyp != "container_not_found" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if rows := f.logAudits(t); len(rows) != 0 {
		t.Errorf("a stream that never opened was audited: %+v", rows)
	}
}

func TestLogStreamSendsAHeartbeatAndStateStoppedWhenTheContainerEnds(t *testing.T) {
	f := newLogsFix(t, logsEngine(), func(f *envFix) { f.logsHeartbeat = 30 * time.Millisecond })
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: "last words"})
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	readLines(t, c, 1)
	f.fake.EndLogs("web")
	st := nextEvent(t, c, "state", 5*time.Second)
	var body struct{ State, At string }
	if err := json.Unmarshal([]byte(st.data), &body); err != nil || body.State != "stopped" {
		t.Fatalf("state = %s (%v)", st.data, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, body.At); err != nil {
		t.Errorf("at = %q is not a time: %v", body.At, err)
	}
	// The stream stays open, beating.
	beats := 0
	deadline := time.After(5 * time.Second)
	for beats < 2 {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				t.Fatal("the stream ended after the container did; it should wait")
			}
			if fr.comment == "heartbeat" {
				beats++
			}
		case <-deadline:
			t.Fatalf("only %d heartbeats", beats)
		}
	}
}

func TestLogStreamResumesAfterTheLastEventIDWithoutRepeatingALine(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 0, 0, 5, time.UTC)
	for i := range 4 {
		f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: fmt.Sprintf("line %d", i), Time: at.Add(time.Duration(i) * time.Second)})
	}
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	lines, id := readLines(t, c, 4)
	if len(lines) != 4 || id != lines[3].T {
		t.Fatalf("lines = %+v, id %q", lines, id)
	}
	c.close()

	// The browser reconnects with the id of the last event it saw, here the second line.
	c2 := f.open(t, f.cookie, webID, "sid="+sidA, lines[1].T)
	got, _ := readLines(t, c2, 2)
	if got[0].M != "line 2" || got[1].M != "line 3" {
		t.Fatalf("resumed lines = %+v, want line 2 and line 3 only", got)
	}
	if fr, ok := c2.next(150 * time.Millisecond); ok && fr.event == "log" {
		t.Errorf("an extra log event arrived: %+v", fr)
	}
	qs := f.fake.LogQueries()
	last := qs[len(qs)-1]
	if last.Get("since") == "" || last.Get("tail") != "" {
		t.Errorf("the resume asked the engine %v, want a since and no tail", last)
	}
}

func TestLogStreamBatchesAChattyContainerIntoEventsOfAtMostAHundredLines(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	for i := range 250 {
		f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: fmt.Sprintf("n%d", i)})
	}
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	seen, events := 0, 0
	for seen < 250 {
		fr := nextEvent(t, c, "log", 5*time.Second)
		var ev logEvent
		if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
			t.Fatal(err)
		}
		if len(ev.Lines) == 0 || len(ev.Lines) > 100 {
			t.Fatalf("an event carried %d lines", len(ev.Lines))
		}
		seen += len(ev.Lines)
		events++
	}
	if events < 3 || events > 25 {
		t.Errorf("250 lines came as %d events, want a few batches", events)
	}
}

func TestLogsAuditWritesOneRowPerViewerAndNeverALineOfTheLog(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	const secret = "LOG-LINE-THAT-MUST-NOT-BE-KEPT-1f9e" //nolint:gosec // a marker, not a credential
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: secret})

	c := f.open(t, f.cookie, webID, "sid="+sidA+"&tail=20&since=2025-01-01T00:00:00Z", "")
	readLines(t, c, 1)
	rows := f.logAudits(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows after the first open, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Actor != "id-admin" || r.Target != webID || r.Env != f.env.ID || r.Result != "success" {
		t.Errorf("row = %+v", r)
	}
	d := r.Details
	if d["container"] != "web" || d["node"] != f.env.ID || d["sid"] != sidA || d["tail"] != "20" || d["since"] != "2025-01-01T00:00:00Z" {
		t.Errorf("details = %v, want container web, node, sid, tail 20 and the since", d)
	}
	c.close()

	// The same viewer coming back (a reconnect) writes nothing more.
	c2 := f.open(t, f.cookie, webID, "sid="+sidA, "")
	readLines(t, c2, 1)
	if n := len(f.logAudits(t)); n != 1 {
		t.Errorf("a reconnect of the same viewer made it %d rows", n)
	}
	// A new viewer, the same person in another window, writes its own.
	c3 := f.open(t, f.cookie, webID, "sid="+sidB, "")
	readLines(t, c3, 1)
	rows = f.logAudits(t)
	if len(rows) != 2 || rows[1].Details["sid"] != sidB {
		t.Errorf("rows = %+v, want a second one for the other viewer", rows)
	}

	// Nothing of the log reaches the audit log or the server's own log (D-23).
	var stored int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE details LIKE ? OR reason LIKE ?`, "%"+secret+"%", "%"+secret+"%").Scan(&stored); err != nil || stored != 0 {
		t.Errorf("the audit log holds %d rows with a log line (%v)", stored, err)
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Error("a line of the log was written to the server's own log")
	}
}

func TestLogStreamTwoUsersEachGetTheirOwnStreamAndTheirOwnAuditRow(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", f.env.ID, "viewer")
	viewerCookie, _ := f.signIn(t, "viewer", false)
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: "hello"})

	a := f.open(t, f.cookie, webID, "sid="+sidA, "")
	b := f.open(t, viewerCookie, webID, "sid="+sidA, "")
	if lines, _ := readLines(t, a, 1); lines[0].M != "hello" {
		t.Errorf("admin's line = %+v", lines)
	}
	if lines, _ := readLines(t, b, 1); lines[0].M != "hello" {
		t.Errorf("viewer's line = %+v", lines)
	}
	rows := f.logAudits(t)
	if len(rows) != 2 || rows[0].Actor == rows[1].Actor {
		t.Fatalf("rows = %+v, want one per user even with the same viewer ID", rows)
	}
	if n := f.fake.LogFollows(); n != 2 {
		t.Errorf("%d follows open, want one per stream", n)
	}

	// The same user with two viewer IDs writes two rows.
	a2 := f.open(t, f.cookie, webID, "sid="+sidB, "")
	readLines(t, a2, 1)
	if n := len(f.logAudits(t)); n != 3 {
		t.Errorf("%d rows, want 3", n)
	}
}

func TestLogStreamAllowsEightPerUserAndRefusesTheNinthWith429(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	conns := make([]*sseConn, 0, 8)
	for i := range 8 {
		c := f.open(t, f.cookie, webID, fmt.Sprintf("sid=%s%02d", sidA, i), "")
		if c.status0() != http.StatusOK {
			t.Fatalf("stream %d status = %d", i+1, c.status0())
		}
		conns = append(conns, c)
	}
	ninth := f.open(t, f.cookie, webID, "sid="+sidB, "")
	if ninth.status0() != http.StatusTooManyRequests {
		t.Fatalf("the 9th stream = %d, want 429", ninth.status0())
	}
	if body := ninth.bodyString(); !strings.Contains(body, `"type":"rate_limited"`) {
		t.Errorf("429 body = %s", body)
	}
	if n := len(f.logAudits(t)); n != 8 {
		t.Errorf("%d audit rows, want 8: a refused stream is not a viewing", n)
	}
	conns[0].close()
	if c := f.open(t, f.cookie, webID, "sid="+sidB, ""); c.status0() != http.StatusOK {
		t.Errorf("a stream after one closed = %d, want 200", c.status0())
	}
}

func TestLogStreamEndsWithClosedAccessLostWithinAHeartbeatOfLosingTheGrant(t *testing.T) {
	const heartbeat = 40 * time.Millisecond
	f := newLogsFix(t, logsEngine(), func(f *envFix) { f.logsHeartbeat = heartbeat })
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", f.env.ID, "viewer")
	cookie, _ := f.signIn(t, "viewer", false)

	c := f.open(t, cookie, webID, "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	if _, err := f.st.Write.Exec(`DELETE FROM role_assignments WHERE principal_id = 'id-viewer'`); err != nil {
		t.Fatal(err)
	}
	closed := nextEvent(t, c, "closed", 2*heartbeat+2*time.Second)
	if closed.data != `{"reason":"access_lost"}` {
		t.Errorf("closed = %s", closed.data)
	}
	c.waitEnded(t, time.Second, "it told the viewer the access was lost")
	// The engine's request is cancelled with the stream.
	deadline := time.Now().Add(5 * time.Second)
	for f.fake.LogFollows() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.fake.LogFollows(); n != 0 {
		t.Errorf("%d engine follows stayed open", n)
	}
}

func TestLogStreamEndsCleanlyAtItsMaximumLifetimeAndFreesTheEngineFollow(t *testing.T) {
	f := newLogsFix(t, logsEngine(), func(f *envFix) { f.logsMaxLifetime = 50 * time.Millisecond })
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	c.waitEnded(t, 3*time.Second, "its lifetime of 50 ms")
	deadline := time.Now().Add(5 * time.Second)
	for f.fake.LogFollows() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the engine follow stayed open after the stream ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLogStreamReleasesTheEngineFollowWhenTheBrowserLeaves(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	if n := f.fake.LogFollows(); n != 1 {
		t.Fatalf("%d follows open, want 1", n)
	}
	c.close()
	deadline := time.Now().Add(5 * time.Second)
	for f.fake.LogFollows() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the engine follow stayed open after the browser left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// busyAgg refuses every node call as an agent with no interactive slot left does.
type busyAgg struct{ api.LogsSource }

func (busyAgg) OnNode(_ context.Context, _, _ string, _ func(*dockerx.Client) error) error {
	return fmt.Errorf("agent a1: %w (32 log follows and shells are open)", tunnel.ErrBusy)
}

func TestLogStreamOfABusyAgentIs503StreamBusyAndIsNotAudited(t *testing.T) {
	f := newLogsFix(t, logsEngine(), func(f *envFix) {
		f.logsWrap = func(src api.LogsSource) api.LogsSource { return busyAgg{src} }
	})
	rec := f.req(t, http.MethodGet, f.path(webID, "sid="+sidA), "")
	if rec.code != http.StatusServiceUnavailable || rec.ptyp != "stream_busy" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if n := len(f.logAudits(t)); n != 0 {
		t.Errorf("%d audit rows for a stream that never opened", n)
	}
	// The slot of the user was given back: a later stream is not counted as open.
	rec = f.req(t, http.MethodGet, f.path(webID, "sid="+sidB), "")
	if rec.code != http.StatusServiceUnavailable {
		t.Errorf("second try = %d, want 503 again, not 429", rec.code)
	}
}

// Following a container across its restarts (plan 04-20, D-20).

func logStateOf(t *testing.T, fr frame) (state, at string) {
	t.Helper()
	var body struct{ State, At string }
	if err := json.Unmarshal([]byte(fr.data), &body); err != nil {
		t.Fatalf("state data %q: %v", fr.data, err)
	}
	return body.State, body.At
}

func TestLogStopSendsStateStoppedAndTheStreamWaitsForTheContainerToStart(t *testing.T) {
	f := newLogsFix(t, logsEngine(), func(f *envFix) { f.logsHeartbeat = 30 * time.Millisecond })
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: "last words"})
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	readLines(t, c, 1)
	f.fake.EndLogs("web")
	if state, _ := logStateOf(t, nextEvent(t, c, "state", 5*time.Second)); state != "stopped" {
		t.Fatalf("state = %q, want stopped", state)
	}
	// Nothing ends the stream: it beats and waits.
	beats := 0
	deadline := time.After(5 * time.Second)
	for beats < 2 {
		select {
		case fr, ok := <-c.frames:
			if !ok || fr.event == "closed" {
				t.Fatal("the stream ended after the container did; it should wait")
			}
			if fr.comment == "heartbeat" {
				beats++
			}
		case <-deadline:
			t.Fatalf("only %d heartbeats", beats)
		}
	}
}

func TestLogRestartReopensTheLogFromTheLastLineAndNeverRepeatsOrLosesOne(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 0, 0, 7, time.UTC)
	f.fake.AppendLogs("web",
		dockertest.FakeLogLine{Text: "first run 1", Time: at},
		dockertest.FakeLogLine{Text: "first run 2", Time: at.Add(time.Second)})
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	readLines(t, c, 2)
	f.fake.EndLogs("web")
	if state, _ := logStateOf(t, nextEvent(t, c, "state", 5*time.Second)); state != "stopped" {
		t.Fatalf("state = %q, want stopped", state)
	}

	// The container starts again. Its log keeps the old lines; the new run writes one
	// at the very stamp of the last line sent, and another after it.
	f.fake.StartLogs("web")
	f.fake.AppendLogs("web",
		dockertest.FakeLogLine{Text: "same stamp, new text", Time: at.Add(time.Second)},
		dockertest.FakeLogLine{Text: "second run", Time: at.Add(2 * time.Second)})
	f.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: webID, Attributes: map[string]string{"name": "web"}})
	state, when := logStateOf(t, nextEvent(t, c, "state", 5*time.Second))
	if state != "restarted" {
		t.Fatalf("state = %q, want restarted", state)
	}
	if _, err := time.Parse(time.RFC3339Nano, when); err != nil {
		t.Errorf("at = %q is not a time: %v", when, err)
	}
	got, _ := readLines(t, c, 2)
	if got[0].M != "same stamp, new text" || got[1].M != "second run" {
		t.Fatalf("lines after the restart = %+v, want the two new ones and none of the first run", got)
	}
	// The reopened read starts at the last line sent and takes no tail.
	qs := f.fake.LogQueries()
	last := qs[len(qs)-1]
	if last.Get("tail") != "" || last.Get("since") == "" || last.Get("follow") != "1" {
		t.Errorf("the reopen asked the engine %v, want a follow since the last line and no tail", last)
	}
	// And it follows: a line of the new run arrives.
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: "still going", Time: at.Add(3 * time.Second)})
	if more, _ := readLines(t, c, 1); more[0].M != "still going" {
		t.Errorf("followed line = %+v", more[0])
	}
}

func TestLogRestartKeepsTwoIdenticalLinesWrittenAtTheSameNanosecond(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 0, 0, 7, time.UTC)
	f.fake.AppendLogs("web",
		dockertest.FakeLogLine{Text: "tick", Time: at},
		dockertest.FakeLogLine{Text: "tick", Time: at})
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	readLines(t, c, 2)
	f.fake.EndLogs("web")
	nextEvent(t, c, "state", 5*time.Second)
	f.fake.StartLogs("web")
	// A third identical line at the same stamp was written by the new run.
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: "tick", Time: at})
	f.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: webID})
	nextEvent(t, c, "state", 5*time.Second)
	got, _ := readLines(t, c, 1)
	if got[0].M != "tick" {
		t.Fatalf("got %+v, want the one new tick", got)
	}
	if fr, ok := c.next(200 * time.Millisecond); ok && fr.event == "log" {
		t.Errorf("a repeat of the first run arrived: %+v", fr)
	}
}

func TestLogRestartIgnoresTheEventsOfOtherContainers(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: "a"})
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	readLines(t, c, 1)
	f.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: dbID})
	f.fake.PublishEvent(dockertest.FakeEvent{Type: "network", Action: "start", ActorID: webID})
	f.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "die", ActorID: webID})
	for {
		fr, ok := c.next(300 * time.Millisecond)
		if !ok {
			break
		}
		if fr.event == "state" {
			t.Fatalf("an event of another container or action moved the stream: %+v", fr)
		}
	}
	if n := len(f.fake.LogQueries()); n != 1 {
		t.Errorf("the engine's log was opened %d times, want 1", n)
	}
}

// spyEvents counts the event subscriptions that are open.
type spyEvents struct {
	api.LogsSource
	open atomic.Int32
}

func (s *spyEvents) Events(ctx context.Context, envID string) ([]envs.NodeEvent, <-chan envs.NodeEvent, func()) {
	backlog, ch, cancel := s.LogsSource.Events(ctx, envID)
	s.open.Add(1)
	return backlog, ch, func() {
		s.open.Add(-1)
		cancel()
	}
}

func TestLogRestartCancelsItsEventSubscriptionWhenTheStreamEnds(t *testing.T) {
	spy := &spyEvents{}
	f := newLogsFix(t, logsEngine(), func(f *envFix) {
		f.logsWrap = func(src api.LogsSource) api.LogsSource { spy.LogsSource = src; return spy }
	})
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	if n := spy.open.Load(); n != 1 {
		t.Fatalf("%d event subscriptions while the stream is open, want 1", n)
	}
	c.close()
	waitFor(t, "the subscription to be cancelled", func() bool { return spy.open.Load() == 0 })
}

// The page of earlier lines behind "Load earlier lines" (plan 04-20, D-15, Pitfall 12).

type logPage struct {
	Lines       []logLine `json:"lines"`
	StartOfLogs bool      `json:"startOfLogs"`
}

func (f *logsFix) pagePath(container, query string) string {
	return "/api/env/" + f.env.ID + "/containers/" + container + "/logs?" + query
}

func (f *logsFix) page(t *testing.T, container, query string) logPage {
	t.Helper()
	rec := f.req(t, http.MethodGet, f.pagePath(container, query), "")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d (%s): %s", rec.code, rec.ptyp, rec.body)
	}
	var out logPage
	if err := json.Unmarshal(rec.body, &out); err != nil {
		t.Fatalf("page %s: %v", rec.body, err)
	}
	return out
}

func numberedLines(f *logsFix, n int, at time.Time) {
	for i := range n {
		f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: fmt.Sprintf("n%02d", i), Time: at.Add(time.Duration(i) * time.Second)})
	}
}

func TestLogPageAnswersTheLinesBeforeUntilOldestFirstAndSaysWhenThereAreMore(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	numberedLines(f, 10, at)

	until := at.Add(6 * time.Second).Format(time.RFC3339Nano) // n00..n05 are before it
	got := f.page(t, webID, "sid="+sidA+"&tail=4&until="+until)
	if len(got.Lines) != 4 || got.Lines[0].M != "n02" || got.Lines[3].M != "n05" {
		t.Fatalf("lines = %+v, want n02..n05", got.Lines)
	}
	if got.StartOfLogs {
		t.Error("startOfLogs is true though four of six earlier lines came back")
	}
	if got.Lines[0].T != "2026-10-09T12:00:02.000000000Z" || got.Lines[0].S != "o" {
		t.Errorf("first line = %+v", got.Lines[0])
	}

	all := f.page(t, webID, "sid="+sidA+"&tail=50&until="+until)
	if len(all.Lines) != 6 || !all.StartOfLogs {
		t.Fatalf("page = %d lines, startOfLogs %v, want the 6 earlier lines and the start", len(all.Lines), all.StartOfLogs)
	}
	// The engine applies a tail before an until, so a tail with an until would answer
	// the wrong lines: the page asks for a window of time instead.
	qs := f.fake.LogQueries()
	last := qs[len(qs)-1]
	if last.Get("follow") == "1" {
		t.Errorf("a page followed the log: %v", last)
	}
	if last.Get("tail") != "" || last.Get("until") == "" {
		t.Errorf("the page asked the engine %v, want an until and no tail", last)
	}
}

func TestLogPageFindsEarlierLinesFarBackWithoutReadingTheWholeLogFirst(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.fake.AppendLogs("web",
		dockertest.FakeLogLine{Text: "ancient 1", Time: old},
		dockertest.FakeLogLine{Text: "ancient 2", Time: old.Add(time.Second)},
		dockertest.FakeLogLine{Text: "recent", Time: old.Add(90 * 24 * time.Hour)})
	got := f.page(t, webID, "sid="+sidA+"&tail=2&until="+old.Add(91*24*time.Hour).Format(time.RFC3339Nano))
	if len(got.Lines) != 2 || got.Lines[0].M != "ancient 2" || got.Lines[1].M != "recent" || got.StartOfLogs {
		t.Fatalf("page = %+v, want the last two lines and not the start", got)
	}
	again := f.page(t, webID, "sid="+sidA+"&tail=2&until="+got.Lines[0].T)
	if len(again.Lines) != 1 || again.Lines[0].M != "ancient 1" || !again.StartOfLogs {
		t.Fatalf("second page = %+v, want ancient 1 and the start", again)
	}
	// The first page needed several windows; each is a read bounded by a since, and
	// only the last one is not.
	qs := f.fake.LogQueries()
	if len(qs) < 3 {
		t.Fatalf("%d engine reads, want the windows to widen", len(qs))
	}
	if qs[0].Get("since") == "" {
		t.Errorf("the first read had no since: %v", qs[0])
	}
}

func TestLogPageIsEmptyAndTheStartWhenNothingIsEarlier(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	numberedLines(f, 3, at)
	got := f.page(t, webID, "sid="+sidA+"&tail=10&until="+at.Format(time.RFC3339Nano))
	if len(got.Lines) != 0 || !got.StartOfLogs {
		t.Errorf("page = %+v, want no lines and the start of logs", got)
	}
}

func TestLogPageKeepsTwoLinesThatShareAStampOnEitherSideOfThePageBoundary(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 0, 0, 5, time.UTC)
	// Three lines written at one nanosecond, then one a second later.
	f.fake.AppendLogs("web",
		dockertest.FakeLogLine{Text: "x1", Time: at},
		dockertest.FakeLogLine{Text: "x2", Time: at},
		dockertest.FakeLogLine{Text: "x3", Time: at},
		dockertest.FakeLogLine{Text: "y", Time: at.Add(time.Second)})
	// The viewer holds x3 and y. To page past x3 without losing x1 and x2 it asks for
	// everything up to one nanosecond after x3's stamp and drops the x3 it already has.
	until := at.Add(time.Nanosecond).Format(logTimeLayoutTest)
	got := f.page(t, webID, "sid="+sidA+"&tail=2&until="+until)
	texts := []string{}
	for _, l := range got.Lines {
		texts = append(texts, l.M)
	}
	if len(texts) != 2 || texts[0] != "x2" || texts[1] != "x3" {
		t.Fatalf("lines = %v, want the last two of the three at the stamp", texts)
	}
	if got.StartOfLogs {
		t.Error("x1 is still earlier, so this is not the start")
	}
}

const logTimeLayoutTest = "2006-01-02T15:04:05.000000000Z"

func TestLogPageSharesTheViewersAuditMemoryAndWritesNoRowForTheSameViewer(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	numberedLines(f, 3, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	c := f.open(t, f.cookie, webID, "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	if n := len(f.logAudits(t)); n != 1 {
		t.Fatalf("%d audit rows after the stream opened, want 1", n)
	}
	f.page(t, webID, "sid="+sidA+"&tail=5&until=2026-10-09T12:00:02Z")
	f.page(t, webID, "sid="+sidA+"&tail=5&until=2026-10-09T12:00:01Z")
	if n := len(f.logAudits(t)); n != 1 {
		t.Errorf("%d audit rows after two pages of the same viewer, want still 1", n)
	}
	// Another viewer reading a page is another viewer.
	f.page(t, webID, "sid="+sidB+"&tail=5&until=2026-10-09T12:00:02Z")
	if n := len(f.logAudits(t)); n != 2 {
		t.Errorf("%d audit rows after a second viewer's page, want 2", n)
	}
}

func TestLogPageRefusesWhatIsNotAPage(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	for _, query := range []string{
		"", "sid=short&tail=5", "sid=" + sidA + "&tail=0", "sid=" + sidA + "&tail=2001",
		"sid=" + sidA + "&until=yesterday", "sid=" + sidA + "&since=nope",
	} {
		rec := f.req(t, http.MethodGet, f.pagePath(webID, query), "")
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%q: status = %d, want 422: %s", query, rec.code, rec.body)
		}
	}
	rec := f.req(t, http.MethodGet, f.pagePath("nope", "sid="+sidA+"&tail=5"), "")
	if rec.code != http.StatusNotFound || rec.ptyp != "container_not_found" {
		t.Errorf("unknown container: status = %d, type %q", rec.code, rec.ptyp)
	}
	if n := len(f.logAudits(t)); n != 0 {
		t.Errorf("%d audit rows for pages that read nothing", n)
	}
}

func TestLogPageNeedsContainersRead(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: f.pagePath(webID, "sid="+sidA+"&tail=5"), cookie: cookie})
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want a refusal", rec.Code)
	}
}

func TestLogPageKeepsTheNewestLinesWhenThePageIsOverItsByteBudget(t *testing.T) {
	f := newLogsFix(t, logsEngine(), func(f *envFix) { f.logsMaxPageBytes = 100 })
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for i := range 10 {
		f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: fmt.Sprintf("%02d-%s", i, strings.Repeat("z", 37)), Time: at.Add(time.Duration(i) * time.Second)})
	}
	got := f.page(t, webID, "sid="+sidA+"&tail=10&until=2026-10-09T13:00:00Z")
	if len(got.Lines) == 0 || len(got.Lines) >= 10 {
		t.Fatalf("%d lines, want some but fewer than all ten within 100 bytes", len(got.Lines))
	}
	if last := got.Lines[len(got.Lines)-1]; !strings.HasPrefix(last.M, "09-") {
		t.Errorf("last line = %q, want the newest kept", last.M)
	}
	if got.StartOfLogs {
		t.Error("lines were left out for size, so this is not the start of the logs")
	}
}

// Logs Docker cannot read back (plan 04-20, D-25 as amended).

func unreadableEngine() dockertest.FakeConfig {
	cfg := logsEngine()
	cfg.Containers = append(cfg.Containers,
		dockertest.FakeContainer{ID: "cccccccccccc33333333333333333333333333333333333333333333333333", Names: []string{"/quiet"}, Image: "alpine", State: "running", Status: "Up 1 hour",
			Inspect: dockertest.FakeInspect{LogType: "none", LogOptions: map[string]string{"tag": "web", "syslog-address": syslogAddr}}},
		dockertest.FakeContainer{ID: "dddddddddddd44444444444444444444444444444444444444444444444444", Names: []string{"/sys"}, Image: "alpine", State: "running", Status: "Up 1 hour",
			Inspect: dockertest.FakeInspect{LogType: "syslog", LogOptions: map[string]string{"syslog-address": "tcp://10.0.0.9:514"}}},
	)
	return cfg
}

// A made-up address with a made-up password, to prove a value is never sent unmasked.
//
//nolint:gosec // G101: not a credential
const syslogAddr = "tcp://user:pw@10.0.0.9:514"

const (
	quietID = "cccccccccccc33333333333333333333333333333333333333333333333333"
	sysID   = "dddddddddddd44444444444444444444444444444444444444444444444444"
)

type unreadableBody struct {
	Driver  string `json:"driver"`
	Options []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Masked bool   `json:"masked"`
	} `json:"options"`
}

func TestLogStreamOfAContainerTheEngineCannotReadSendsUnreadableWithTheDriverAndEndsWithoutAnAuditRow(t *testing.T) {
	f := newLogsFix(t, unreadableEngine(), nil)
	c := f.open(t, f.cookie, quietID, "sid="+sidA, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	fr := nextEvent(t, c, "unreadable", 5*time.Second)
	var body unreadableBody
	if err := json.Unmarshal([]byte(fr.data), &body); err != nil {
		t.Fatalf("unreadable data %q: %v", fr.data, err)
	}
	if body.Driver != "none" {
		t.Errorf("driver = %q, want none", body.Driver)
	}
	if len(body.Options) != 2 || body.Options[0].Name != "syslog-address" || body.Options[1].Name != "tag" {
		t.Errorf("options = %+v, want the two by name", body.Options)
	}
	c.waitEnded(t, 3*time.Second, "an unreadable log has nothing to stream")
	if n := len(f.logAudits(t)); n != 0 {
		t.Errorf("%d audit rows for a log that was never read", n)
	}
	if n := f.fake.LogFollows(); n != 0 {
		t.Errorf("%d engine follows open", n)
	}
}

func TestLogStreamUnreadableMasksTheOptionValuesForAViewerAndShowsThemToAHolderOfSecretsReveal(t *testing.T) {
	f := newLogsFix(t, unreadableEngine(), nil)
	admin := f.open(t, f.cookie, quietID, "sid="+sidA, "")
	var shown unreadableBody
	if err := json.Unmarshal([]byte(nextEvent(t, admin, "unreadable", 5*time.Second).data), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Options[0].Masked || shown.Options[0].Value != syslogAddr {
		t.Errorf("a revealer saw %+v, want the value", shown.Options[0])
	}

	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", f.env.ID, "viewer")
	cookie, _ := f.signIn(t, "viewer", false)
	viewer := f.open(t, cookie, quietID, "sid="+sidB, "")
	frame := nextEvent(t, viewer, "unreadable", 5*time.Second)
	if strings.Contains(frame.data, "pw@") || strings.Contains(frame.data, "10.0.0.9") || strings.Contains(frame.data, `"web"`) {
		t.Fatalf("a viewer was sent a value: %s", frame.data)
	}
	var masked unreadableBody
	if err := json.Unmarshal([]byte(frame.data), &masked); err != nil {
		t.Fatal(err)
	}
	for _, o := range masked.Options {
		if !o.Masked || o.Value != "" {
			t.Errorf("option %+v should be masked with no value", o)
		}
	}
}

func TestLogStreamOfASyslogContainerWhoseLogsReadStreamsNormally(t *testing.T) {
	f := newLogsFix(t, unreadableEngine(), nil)
	f.fake.AppendLogs("sys", dockertest.FakeLogLine{Text: "dual logging keeps a copy"})
	c := f.open(t, f.cookie, sysID, "sid="+sidA, "")
	nextEvent(t, c, "ready", 5*time.Second)
	lines, _ := readLines(t, c, 1)
	if lines[0].M != "dual logging keeps a copy" {
		t.Errorf("line = %+v", lines[0])
	}
	if fr, ok := c.next(150 * time.Millisecond); ok && fr.event == "unreadable" {
		t.Errorf("a readable syslog container was called unreadable: %+v", fr)
	}
}

func TestLogPageOfAContainerTheEngineCannotReadIs409LogsUnreadable(t *testing.T) {
	f := newLogsFix(t, unreadableEngine(), nil)
	rec := f.req(t, http.MethodGet, f.pagePath(quietID, "sid="+sidA+"&tail=5"), "")
	if rec.code != http.StatusConflict || rec.ptyp != "logs_unreadable" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

// The log download (plan 04-21, OBS-01, D-18, D-24, T-04-71..73). The response is a
// file streamed from the engine, so it is read through a writer that counts what is
// written to it and can hold a write back.

// dlWriter is an http.ResponseWriter that counts the writes made to it.
type dlWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	// maxWrite is the largest single Write; total all bytes; newlines the lines.
	maxWrite, total, newlines int
	// keep, when set, holds the whole body.
	keep    bool
	body    bytes.Buffer
	deadlns []time.Time
	// gate, when not nil, holds every Write until it is closed.
	gate    chan struct{}
	wrote   chan struct{}
	wroteOn sync.Once
}

func newDLWriter(keep bool) *dlWriter {
	return &dlWriter{header: http.Header{}, keep: keep, wrote: make(chan struct{})}
}

func (w *dlWriter) Header() http.Header { return w.header }

func (w *dlWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = code
	}
}

func (w *dlWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.wroteOn.Do(func() { close(w.wrote) })
	if w.gate != nil {
		<-w.gate
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.maxWrite = max(w.maxWrite, len(b))
	w.total += len(b)
	w.newlines += bytes.Count(b, []byte{'\n'})
	if w.keep {
		w.body.Write(b)
	}
	return len(b), nil
}

func (w *dlWriter) Flush() {}

func (w *dlWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlns = append(w.deadlns, t)
	return nil
}

func (w *dlWriter) code() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (f *logsFix) downloadPath(container, query string) string {
	return "/api/env/" + f.env.ID + "/containers/" + container + "/logs/download?" + query
}

// startDownload runs a download as the signed-in admin and returns a channel closed
// when the handler has returned.
func (f *logsFix) startDownload(cookie, container, query string, w http.ResponseWriter) <-chan struct{} {
	req := httptest.NewRequest(http.MethodGet, f.downloadPath(container, query), nil)
	req.Header.Set("User-Agent", authUA)
	req.RemoteAddr = authIP + ":40000"
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(w, req)
	}()
	return done
}

func (f *logsFix) download(t *testing.T, container, query string) *dlWriter {
	t.Helper()
	w := newDLWriter(true)
	select {
	case <-f.startDownload(f.cookie, container, query, w):
	case <-time.After(20 * time.Second):
		t.Fatal("the download never finished")
	}
	return w
}

func (f *logsFix) downloadAudits(t *testing.T) []logAuditRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_id, target_id, COALESCE(env_id, ''), result, COALESCE(details, '{}')
		FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionLogsDownloaded)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []logAuditRow
	for rows.Next() {
		var r logAuditRow
		var details string
		if err := rows.Scan(&r.Actor, &r.Target, &r.Env, &r.Result, &details); err != nil {
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

func TestLogDownloadAnswersATextFileNamedByContainerAndUTCMinuteWithBothStreamsInOrder(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 58, 0, 123456789, time.UTC)
	f.fake.AppendLogs("web",
		dockertest.FakeLogLine{Text: "a", Time: at},
		dockertest.FakeLogLine{Stream: "stderr", Text: "b", Time: at.Add(time.Second)},
		dockertest.FakeLogLine{Text: "c", Time: at.Add(2 * time.Second)})

	w := f.download(t, webID, "")
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	if got := w.header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q", got)
	}
	if got, want := w.header.Get("Content-Disposition"), `attachment; filename="web-20260101-0000.log"`; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if got := w.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := w.body.String(); got != "a\nb\nc\n" {
		t.Errorf("body = %q, want the lines of both streams in order and no timestamps", got)
	}
	if len(w.deadlns) == 0 || !w.deadlns[0].IsZero() {
		t.Errorf("write deadlines = %v, want the server's write timeout lifted for a long copy", w.deadlns)
	}
	if n := f.fake.LogFollows(); n != 0 {
		t.Errorf("%d engine follows open: a download never follows", n)
	}
	last := f.fake.LogQueries()
	if q := last[len(last)-1]; q.Get("follow") == "1" || q.Get("tail") != "" {
		t.Errorf("the engine was asked %v, want no follow and no tail", q)
	}
}

func TestLogDownloadPrefixesDockersRawUTCTimestampWhenAskedAndReadsTheRangeFromTheEngine(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	at := time.Date(2026, 10, 9, 12, 58, 0, 123456789, time.UTC)
	for i := range 5 {
		f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: fmt.Sprintf("n%d", i), Time: at.Add(time.Duration(i) * time.Minute)})
	}
	since, until := at.Add(time.Minute), at.Add(3*time.Minute)
	q := "timestamps=1&since=" + since.Format(time.RFC3339Nano) + "&until=" + until.Format(time.RFC3339Nano)
	w := f.download(t, webID, q)
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	want := "2026-10-09T12:59:00.123456789Z n1\n2026-10-09T13:00:00.123456789Z n2\n2026-10-09T13:01:00.123456789Z n3\n"
	if got := w.body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	qs := f.fake.LogQueries()
	got := qs[len(qs)-1]
	if got.Get("since") == "" || got.Get("until") == "" || got.Get("tail") != "" {
		t.Errorf("the engine was asked %v, want the whole range by since and until and no tail", got)
	}
}

func TestLogDownloadOfSeventyMiBStreamsInBoundedWritesWithoutHoldingItInMemory(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	const lineLen, count = 200 << 10, 360 // 70.3 MiB
	text := strings.Repeat("x", lineLen)
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	lines := make([]dockertest.FakeLogLine, count)
	for i := range lines {
		lines[i] = dockertest.FakeLogLine{Text: text, Time: at.Add(time.Duration(i) * time.Millisecond)}
	}
	f.fake.AppendLogs("web", lines...)

	w := newDLWriter(false)
	select {
	case <-f.startDownload(f.cookie, webID, "", w):
	case <-time.After(60 * time.Second):
		t.Fatal("the download never finished")
	}
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d", w.code())
	}
	if want := count * (lineLen + 1); w.total != want {
		t.Errorf("wrote %d bytes, want %d", w.total, want)
	}
	if w.newlines != count {
		t.Errorf("wrote %d lines, want %d", w.newlines, count)
	}
	// A write is at most one line: far below the size of the log.
	if w.maxWrite > 2*lineLen {
		t.Errorf("the largest write was %d bytes, want it bounded by a line (%d)", w.maxWrite, lineLen)
	}
}

func TestLogDownloadAllowsTwoAtOnceAndRefusesTheThirdWith429(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	text := strings.Repeat("y", 60<<10)
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: text}, dockertest.FakeLogLine{Text: text})

	gate := make(chan struct{})
	var dones []<-chan struct{}
	for range 2 {
		w := newDLWriter(false)
		w.gate = gate
		dones = append(dones, f.startDownload(f.cookie, webID, "", w))
		select {
		case <-w.wrote:
		case <-time.After(5 * time.Second):
			t.Fatal("a download never started writing")
		}
	}
	third := newDLWriter(true)
	select {
	case <-f.startDownload(f.cookie, webID, "", third):
	case <-time.After(5 * time.Second):
		t.Fatal("the third download was not answered")
	}
	if third.code() != http.StatusTooManyRequests || !strings.Contains(third.body.String(), `"type":"rate_limited"`) {
		t.Fatalf("the third download = %d %s, want 429 rate_limited", third.code(), third.body.String())
	}
	if n := len(f.downloadAudits(t)); n != 2 {
		t.Errorf("%d audit rows, want 2: a refused download is not a download", n)
	}
	close(gate)
	for _, d := range dones {
		<-d
	}
	if w := f.download(t, webID, ""); w.code() != http.StatusOK {
		t.Errorf("a download after the others finished = %d, want 200", w.code())
	}
}

func TestLogDownloadIsAuditedOncePerDownloadWithItsRangeAndNeverALineOfTheLog(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	const secret = "DOWNLOADED-LINE-THAT-MUST-NOT-BE-KEPT-77c1" //nolint:gosec // a marker, not a credential
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: secret})

	f.download(t, webID, "timestamps=1&since=2025-01-01T00:00:00Z&until=2027-01-01T00:00:00Z")
	f.download(t, webID, "")
	rows := f.downloadAudits(t)
	if len(rows) != 2 {
		t.Fatalf("%d rows for two downloads: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Actor != "id-admin" || r.Target != webID || r.Env != f.env.ID || r.Result != "success" {
		t.Errorf("row = %+v", r)
	}
	d := r.Details
	if d["container"] != "web" || d["node"] != f.env.ID || d["since"] != "2025-01-01T00:00:00Z" || d["until"] != "2027-01-01T00:00:00Z" || d["timestamps"] != "true" {
		t.Errorf("details = %v, want container, node, since, until and timestamps", d)
	}
	d = rows[1].Details
	if d["timestamps"] != "false" || d["since"] != "" || d["until"] != "2026-01-01T00:00:00Z" {
		t.Errorf("details of a download without a range = %v, want no since, until now and timestamps false", d)
	}
	var stored int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE details LIKE ? OR reason LIKE ?`, "%"+secret+"%", "%"+secret+"%").Scan(&stored); err != nil || stored != 0 {
		t.Errorf("the audit log holds %d rows with a log line (%v)", stored, err)
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Error("a line of the log was written to the server's own log")
	}
}

func TestLogDownloadNameKeepsOnlySafeCharacters(t *testing.T) {
	cfg := logsEngine()
	cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
		ID: "eeeeeeeeeeee55555555555555555555555555555555555555555555555555", Names: []string{`/we"b;../x y`}, Image: "alpine", State: "running", Status: "Up 1 hour"})
	f := newLogsFix(t, cfg, nil)
	w := f.download(t, "eeeeeeeeeeee55555555555555555555555555555555555555555555555555", "")
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	cd := w.header.Get("Content-Disposition")
	if cd != `attachment; filename="we_b_.._x_y-20260101-0000.log"` {
		t.Errorf("Content-Disposition = %q, want the name reduced to letters, digits, underscore, dot and dash", cd)
	}
}

func TestLogDownloadRefusalsAreAnsweredBeforeAnythingIsSentAndAreNotAudited(t *testing.T) {
	f := newLogsFix(t, unreadableEngine(), nil)
	for name, tc := range map[string]struct {
		container, query string
		status           int
		typ              string
	}{
		"a bad since":         {webID, "since=yesterday", http.StatusUnprocessableEntity, "validation_failed"},
		"a bad until":         {webID, "until=soon", http.StatusUnprocessableEntity, "validation_failed"},
		"an empty range":      {webID, "since=2026-02-01T00:00:00Z&until=2026-01-01T00:00:00Z", http.StatusUnprocessableEntity, "validation_failed"},
		"a bad timestamps":    {webID, "timestamps=maybe", http.StatusUnprocessableEntity, "validation_failed"},
		"a missing container": {"nope", "", http.StatusNotFound, "container_not_found"},
		"an unreadable log":   {quietID, "", http.StatusConflict, "logs_unreadable"},
	} {
		w := f.download(t, tc.container, tc.query)
		if w.code() != tc.status || !strings.Contains(w.body.String(), `"type":"`+tc.typ+`"`) {
			t.Errorf("%s = %d %s, want %d %s", name, w.code(), w.body.String(), tc.status, tc.typ)
		}
		if w.header.Get("Content-Disposition") != "" {
			t.Errorf("%s: a refusal must not be a download", name)
		}
	}
	if n := len(f.downloadAudits(t)); n != 0 {
		t.Errorf("%d audit rows for refused downloads", n)
	}
}

func TestLogDownloadNeedsContainersRead(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)
	w := newDLWriter(true)
	<-f.startDownload(cookie, webID, "", w)
	if w.code() != http.StatusForbidden && w.code() != http.StatusNotFound {
		t.Errorf("status = %d, want a refusal", w.code())
	}
}

func TestLogDownloadStopsReadingTheEngineWhenTheBrowserLeaves(t *testing.T) {
	f := newLogsFix(t, logsEngine(), nil)
	text := strings.Repeat("z", 60<<10)
	f.fake.AppendLogs("web", dockertest.FakeLogLine{Text: text}, dockertest.FakeLogLine{Text: text})
	gate := make(chan struct{})
	w := newDLWriter(false)
	w.gate = gate
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, f.downloadPath(webID, ""), nil).WithContext(ctx)
	req.Header.Set("User-Agent", authUA)
	req.RemoteAddr = authIP + ":40000"
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: f.cookie}) //nolint:gosec // a request cookie as a browser sends it
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(w, req)
	}()
	select {
	case <-w.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the download never started writing")
	}
	cancel()
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the download kept running after the browser left")
	}
}
