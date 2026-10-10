// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/shells"
)

// The browser shell (plan 04-22, OBS-03, RESEARCH Pattern 5). Docker is a FakeDaemon
// whose exec echoes every line, behind the real monitor, aggregator and router; the
// WebSocket is dialled with the same library the server uses, against a real HTTP
// server, because a hijacked connection cannot be recorded.

type shellFix struct {
	*envFix
	env envBody
	srv *httptest.Server
}

func shellEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: webID, Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
			{ID: dbID, Names: []string{"/db"}, Image: "postgres:17", State: "exited", Status: "Exited (0) 3 hours ago"},
		},
	}
}

// newShellFix builds the environment fixture with the shell routes; tune sets the
// shell knobs of the fixture before the router is built.
func newShellFix(t *testing.T, tune func(*envFix)) *shellFix {
	t.Helper()
	f := newEnvFix(t, shellEngine())
	if tune != nil {
		tune(f)
	}
	f.h = f.build(t)
	e := f.add(t)
	// A write goes only to a node the monitor has seen online.
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	srv := httptest.NewServer(f.h)
	t.Cleanup(srv.Close)
	return &shellFix{envFix: f, env: e, srv: srv}
}

type shellSession struct {
	SessionID  string   `json:"sessionId"`
	WSPath     string   `json:"wsPath"`
	Command    []string `json:"command"`
	User       string   `json:"user"`
	WorkingDir string   `json:"workingDir"`
}

func (f *shellFix) openPath(container string) string {
	return "/api/env/" + f.env.ID + "/containers/" + container + "/shell"
}

// open starts a shell as the admin and returns the answer.
func (f *shellFix) open(t *testing.T, container, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, f.openPath(container), body)
}

// session opens a shell and decodes the 201.
func (f *shellFix) session(t *testing.T) shellSession {
	t.Helper()
	rec := f.open(t, "web", `{"shell":"auto","cols":80,"rows":24}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("open: %d %s\n%s", rec.code, rec.body, f.logs)
	}
	return decode[shellSession](t, rec)
}

// dial attaches to a session over a WebSocket with the given cookie and origin. When
// the server refuses the upgrade it returns the status and the problem type of the
// refusal; on success the status is 101.
func (f *shellFix) dial(t *testing.T, cookie, wsPath, origin string) (conn *websocket.Conn, status int, ptype string, err error) {
	t.Helper()
	h := http.Header{}
	if cookie != "" {
		h.Set("Cookie", auth.CookieName(f.inst.CookieScope())+"="+cookie)
	}
	if origin != "" {
		h.Set("Origin", origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+wsPath, &websocket.DialOptions{HTTPHeader: h})
	if conn != nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
	}
	if resp != nil {
		status = resp.StatusCode
		// A refused upgrade has a body to read and close; an accepted one has none.
		if err != nil {
			defer func() { _ = resp.Body.Close() }()
			var p struct {
				Type string `json:"type"`
			}
			if json.NewDecoder(resp.Body).Decode(&p) == nil {
				ptype = p.Type
			}
		}
	}
	return conn, status, ptype, err
}

// attach dials as the admin from the server's own origin and fails the test if it
// does not connect.
func (f *shellFix) attach(t *testing.T, s shellSession) *websocket.Conn {
	t.Helper()
	conn, status, _, err := f.dial(t, f.cookie, s.WSPath, f.srv.URL)
	if err != nil {
		t.Fatalf("attach: %v (status %d)\n%s", err, status, f.logs)
	}
	return conn
}

// readUntilBinary reads frames until the binary ones, put together, hold want; it
// returns what was read.
func readUntilBinary(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (read so far %q)", want, err, got.String())
		}
		if typ == websocket.MessageBinary {
			got.Write(data)
		}
	}
	return got.String()
}

// readControl reads frames until a text frame of the given type arrives, and returns
// it decoded.
func readControl(t *testing.T, c *websocket.Conn, typ string) shells.Control {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		mt, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for a %q frame: %v", typ, err)
		}
		if mt != websocket.MessageText {
			continue
		}
		var ctl shells.Control
		if err := json.Unmarshal(data, &ctl); err != nil {
			t.Fatalf("control frame %q: %v", data, err)
		}
		if ctl.Type == typ {
			return ctl
		}
	}
}

// drain reads until the socket ends, so the client answers the server's close frame
// as a browser does; a client that does not holds the server's close for five seconds.
func drain(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}

// dialRefused fails the test unless the dial was refused with the status, and returns the
// problem type of the refusal.
func dialRefused(t *testing.T, what string, status int, ptype string, err error, want int) string {
	t.Helper()
	if err == nil || status != want {
		t.Fatalf("%s: err %v, status %d, want a refusal with %d", what, err, status, want)
	}
	return ptype
}

func sendBinary(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte(s)); err != nil {
		t.Fatalf("send %q: %v", s, err)
	}
}

type shellAuditRow struct {
	Action, Actor, Target, Env, Result, IP string
	Details                                map[string]string
}

// shellAudits reads the shell.* rows in order.
func (f *shellFix) shellAudits(t *testing.T) []shellAuditRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, actor_id, target_id, COALESCE(env_id, ''), result, COALESCE(ip, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'shell.%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []shellAuditRow
	for rows.Next() {
		var r shellAuditRow
		var details string
		if err := rows.Scan(&r.Action, &r.Actor, &r.Target, &r.Env, &r.Result, &r.IP, &details); err != nil {
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

// waitShellRows waits until n shell.* rows are written and returns them.
func (f *shellFix) waitShellRows(t *testing.T, n int) []shellAuditRow {
	t.Helper()
	var rows []shellAuditRow
	waitFor(t, "the shell audit rows", func() bool {
		rows = f.shellAudits(t)
		return len(rows) >= n
	})
	return rows
}

func TestShellOpenCreatesTheExecWithATTYAndAPendingSession(t *testing.T) {
	f := newShellFix(t, nil)
	s := f.session(t)
	if s.SessionID == "" || s.WSPath != "/api/env/"+f.env.ID+"/shell-sessions/"+s.SessionID+"/ws" {
		t.Errorf("session = %+v", s)
	}
	if len(s.Command) != 3 || s.Command[0] != "/bin/sh" || s.Command[1] != "-c" || !strings.Contains(s.Command[2], "/bin/bash") {
		t.Errorf("command = %q, want the sh -c wrapper that prefers bash", s.Command)
	}
	bodies := f.fake.ExecCreateBodies()
	if len(bodies) != 1 {
		t.Fatalf("exec creates = %d, want 1: %v", len(bodies), bodies)
	}
	var created struct {
		Tty          bool
		AttachStdin  bool
		AttachStdout bool
		Cmd          []string
		ConsoleSize  []int
	}
	if err := json.Unmarshal([]byte(bodies[0]), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Tty || !created.AttachStdin || !created.AttachStdout || len(created.Cmd) != 3 {
		t.Errorf("exec create = %s, want a TTY with stdin and stdout and the wrapper command", bodies[0])
	}
	if got := f.shellsReg.List(shells.Filter{}); len(got) != 1 || got[0].State != shells.StatePending || got[0].ContainerName != "web" {
		t.Errorf("registry = %+v, want one pending session of web", got)
	}
}

func TestShellOpenIsRefusedForAViewerAndForAnOperatorWithoutTheGrant(t *testing.T) {
	f := newShellFix(t, nil)
	f.addEnvUser(t, "vic", f.env.ID, "viewer")
	f.addEnvUser(t, "olive", f.env.ID, "operator")
	for _, user := range []string{"vic", "olive"} {
		rec := f.asUser(t, user, http.MethodPost, f.openPath("web"), `{"shell":"auto","cols":80,"rows":24}`)
		if rec.code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403 (containers.exec, D-51)", user, rec.code, rec.body)
		}
	}
	if n := len(f.fake.ExecCreateBodies()); n != 0 {
		t.Errorf("a refused caller reached the engine %d times", n)
	}
	if rows := f.shellAudits(t); len(rows) != 0 {
		t.Errorf("shell rows = %+v, want none", rows)
	}
}

func TestShellWebSocketCarriesTheTerminalBytesBothWaysAndReportsTheExitCode(t *testing.T) {
	f := newShellFix(t, nil)
	s := f.session(t)
	c := f.attach(t, s)

	sendBinary(t, c, "echo hi\n")
	readUntilBinary(t, c, "echo hi")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the resize to reach the engine", func() bool {
		for _, r := range f.fake.Resizes() {
			if r == "h=30&w=100" {
				return true
			}
		}
		return false
	})

	sendBinary(t, c, "exit 2\n")
	ctl := readControl(t, c, "exit")
	if ctl.Code == nil || *ctl.Code != 2 {
		t.Fatalf("exit frame = %+v, want code 2", ctl)
	}
	// The server then closes the socket normally.
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	for {
		if _, _, err := c.Read(rctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("the socket ended with %v, want a normal close", err)
			}
			break
		}
	}
}

func TestShellWebSocketRefusesAnotherOriginAnotherUserAnotherSignInAndASecondAttach(t *testing.T) {
	f := newShellFix(t, nil)
	f.addEnvUser(t, "bob", f.env.ID, "admin")
	bobCookie, _ := f.signIn(t, "bob", false)
	otherCookie, _ := f.signIn(t, "admin", false)
	s := f.session(t)

	// Another origin is refused by the upgrade itself, and does not use the session up.
	_, status, ptype, err := f.dial(t, f.cookie, s.WSPath, "https://evil.example")
	dialRefused(t, "a foreign origin", status, ptype, err, http.StatusForbidden)
	// Another user and another sign-in of the same user get the one answer.
	for name, cookie := range map[string]string{"another user": bobCookie, "another sign-in": otherCookie} {
		_, status, ptype, err := f.dial(t, cookie, s.WSPath, f.srv.URL)
		dialRefused(t, name, status, ptype, err, http.StatusNotFound)
	}
	// A caller with no cookie is not signed in.
	_, status, ptype, err = f.dial(t, "", s.WSPath, f.srv.URL)
	dialRefused(t, "no cookie", status, ptype, err, http.StatusUnauthorized)

	// The owner still can, once.
	c := f.attach(t, s)
	sendBinary(t, c, "echo still-here\n")
	readUntilBinary(t, c, "still-here")
	_, status, ptype, err = f.dial(t, f.cookie, s.WSPath, f.srv.URL)
	if got := dialRefused(t, "a second attach", status, ptype, err, http.StatusNotFound); got != "shell_session_not_found" {
		t.Errorf("problem type = %q, want shell_session_not_found", got)
	}
}

func TestShellWebSocketAcceptsTheInstancesPublicOriginAndNoOther(t *testing.T) {
	f := newShellFix(t, nil)
	// Behind a proxy the Host is the proxy's own, so the public origin is what counts.
	f.inst.Apply("", "http://bayplan.example")
	s := f.session(t)

	_, status, ptype, err := f.dial(t, f.cookie, s.WSPath, "http://other.example")
	dialRefused(t, "another origin", status, ptype, err, http.StatusForbidden)
	c, status, _, err := f.dial(t, f.cookie, s.WSPath, "http://bayplan.example")
	if err != nil {
		t.Fatalf("the public origin: %v (status %d)", err, status)
	}
	sendBinary(t, c, "echo from-public\n")
	readUntilBinary(t, c, "from-public")
}

func TestShellClosingTheSocketEndsTheSessionAndCannotBeReattached(t *testing.T) {
	f := newShellFix(t, nil)
	s := f.session(t)
	c := f.attach(t, s)
	sendBinary(t, c, "echo up\n")
	readUntilBinary(t, c, "up")

	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Logf("close: %v", err)
	}
	waitFor(t, "the session to leave the registry", func() bool { return len(f.shellsReg.List(shells.Filter{})) == 0 })
	rows := f.waitShellRows(t, 2)
	closed := rows[1]
	if closed.Action != audit.ActionShellClosed || closed.Details["reason"] != "disconnected" || closed.Details["still_running"] != "true" {
		t.Errorf("closed row = %+v, want reason disconnected and still_running (D-30)", closed)
	}
	_, status, ptype, err := f.dial(t, f.cookie, s.WSPath, f.srv.URL)
	dialRefused(t, "re-attach", status, ptype, err, http.StatusNotFound)
}

func TestShellAuditsTheStartAndTheEndOfASessionAndNeverWhatWasTyped(t *testing.T) {
	f := newShellFix(t, nil)
	s := f.session(t)
	c := f.attach(t, s)
	const typed = "hunter2-typed-text"
	sendBinary(t, c, "echo "+typed+"\n")
	readUntilBinary(t, c, typed)
	sendBinary(t, c, "exit 3\n")
	readControl(t, c, "exit")
	drain(t, c)

	rows := f.waitShellRows(t, 2)
	if len(rows) != 2 {
		t.Fatalf("shell rows = %+v, want one opened and one closed", rows)
	}
	opened, closed := rows[0], rows[1]
	if opened.Action != audit.ActionShellOpened || opened.Actor != "id-admin" || opened.Target != webID || opened.Env != f.env.ID || opened.Result != "success" || opened.IP != authIP {
		t.Errorf("opened row = %+v", opened)
	}
	for k, want := range map[string]string{"container": "web", "node": f.env.ID, "sid": s.SessionID, "command": "auto", "user": "", "workdir": ""} {
		if got, ok := opened.Details[k]; !ok || got != want {
			t.Errorf("opened details[%s] = %q (present %v), want %q", k, got, ok, want)
		}
	}
	if closed.Action != audit.ActionShellClosed || closed.Target != webID || closed.IP != authIP {
		t.Errorf("closed row = %+v", closed)
	}
	if closed.Details["sid"] != s.SessionID || closed.Details["reason"] != "exited" || closed.Details["exit_code"] != "3" || closed.Details["duration_ms"] == "" {
		t.Errorf("closed details = %+v, want the sid, reason exited, exit_code 3 and a duration", closed.Details)
	}
	if _, running := closed.Details["still_running"]; running {
		t.Errorf("a process that exited is not still running: %+v", closed.Details)
	}

	// Nothing typed or printed is anywhere: not in the audit log, not in the server's log.
	var all string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(group_concat(COALESCE(details, '') || COALESCE(reason, '') || COALESCE(target_id, ''), ' '), '') FROM audit_log`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all, typed) {
		t.Errorf("what was typed reached the audit log: %s", all)
	}
	if strings.Contains(f.logs.String(), typed) {
		t.Errorf("what was typed reached the server's log")
	}
}

func TestShellServerShutdownTellsAnAttachedBrowserWhy(t *testing.T) {
	f := newShellFix(t, nil)
	s := f.session(t)
	c := f.attach(t, s)
	sendBinary(t, c, "echo up\n")
	readUntilBinary(t, c, "up")

	if n := f.shellsReg.CloseAll("server_shutdown"); n != 1 {
		t.Fatalf("CloseAll ended %d sessions, want 1", n)
	}
	ctl := readControl(t, c, "closed")
	if ctl.Reason != "server_shutdown" {
		t.Errorf("closed frame = %+v, want reason server_shutdown", ctl)
	}
	rows := f.waitShellRows(t, 2)
	if rows[1].Details["reason"] != "server_shutdown" {
		t.Errorf("closed row = %+v, want reason server_shutdown", rows[1])
	}
}
