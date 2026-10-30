// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// sharedWorkerShellAndLogs opens a shell and a log stream in a swarm task container
// that runs on a worker node, through the real agents on a swarm of three managers and
// a worker (plan 04-25, OBS-03 "including swarm task containers on worker nodes",
// OBS-01): the container is found in the aggregated containers list, a shell opened with
// the worker's node ID exchanges bytes over the WebSocket the way the browser's does,
// its end is audited, and the same container's log reaches an event stream.
func sharedWorkerShellAndLogs(t *testing.T, f *swarmFix) {
	started := time.Now()
	dockertest.LoadImage(t, "alpine", f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	base := "/api/env/" + f.env.ID

	// A task that can only run on the worker, and says tick once a second.
	sh(t, mgr, "docker service create --name w --constraint node.role==worker alpine sh -c 'while true; do echo tick; sleep 1; done'")

	// Its container, as the aggregated list shows it: a row on the worker's node.
	type row struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Node   string `json:"node"`
		NodeID string `json:"nodeId"`
		State  string `json:"state"`
	}
	var task row
	f.srv.WaitFor("the service's task container on the worker in the containers list", 120*time.Second, func() (bool, string) {
		code, raw := f.srv.Do(http.MethodGet, base+"/containers", nil)
		if code != http.StatusOK {
			return false, string(raw)
		}
		var list struct{ Items []row }
		if err := json.Unmarshal(raw, &list); err != nil {
			return false, err.Error()
		}
		for _, r := range list.Items {
			if strings.HasPrefix(r.Name, "w.") && r.State == "running" {
				task = r
				return r.Node == worker.Hostname && r.NodeID != "", r.Node + " " + r.Name
			}
		}
		return false, "no row named w.* is running yet"
	})
	if task.Node != worker.Hostname {
		t.Fatalf("the task container is on %q, want the worker %q", task.Node, worker.Hostname)
	}

	// Open the shell the way the browser does: a POST naming the worker's node, then a
	// WebSocket with the signed-in cookie from the server's own origin.
	code, raw := f.srv.Do(http.MethodPost, base+"/containers/"+task.ID+"/shell", map[string]any{
		"nodeId": task.NodeID, "shell": "auto", "cols": 80, "rows": 24,
	})
	if code != http.StatusCreated {
		t.Fatalf("open a shell in %s on the worker: %d %s", task.Name, code, raw)
	}
	var session struct {
		SessionID string `json:"sessionId"`
		WSPath    string `json:"wsPath"`
	}
	if err := json.Unmarshal(raw, &session); err != nil || session.WSPath == "" {
		t.Fatalf("shell answer %s (%v)", raw, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := *f.srv.Client() // the same trust; a WebSocket has no whole-request timeout
	client.Timeout = 0
	conn, resp, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(f.srv.URL, "https")+session.WSPath, &websocket.DialOptions{
		HTTPClient: &client,
		HTTPHeader: http.Header{"Cookie": {f.srv.Cookie().String()}, "Origin": {f.srv.URL}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("attach to the shell: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// 6*7 is computed by the shell, so the answer cannot be the terminal echoing what was typed.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("echo worker-$((6*7))-ok; hostname\n")); err != nil {
		t.Fatalf("type into the shell: %v", err)
	}
	var printed strings.Builder
	readCtx, stopReading := context.WithTimeout(ctx, 30*time.Second)
	defer stopReading()
	for !strings.Contains(printed.String(), "worker-42-ok") {
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("read from the shell: %v; printed %q", err, printed.String())
		}
		if typ == websocket.MessageBinary {
			printed.Write(data)
		}
	}
	// The shell really is in the container on the worker: its hostname is the container's
	// own ID prefix, which no engine on a manager has.
	for !strings.Contains(printed.String(), task.ID[:12]) {
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("read the hostname: %v; printed %q", err, printed.String())
		}
		if typ == websocket.MessageBinary {
			printed.Write(data)
		}
	}
	stopReading()

	// Closing the socket ends the session, and the end is on record.
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Logf("close the socket: %v", err)
	}
	f.srv.WaitFor("the shell.closed row", 30*time.Second, func() (bool, string) {
		rows := f.srv.AuditRows("shell.closed")
		return len(rows) == 1, "no shell.closed row yet"
	})
	opened, closed := f.srv.AuditRows("shell.opened"), f.srv.AuditRows("shell.closed")
	if len(opened) != 1 || opened[0].TargetID != task.ID || opened[0].Details["node"] != task.NodeID || opened[0].Details["sid"] != session.SessionID {
		t.Errorf("shell.opened = %+v, want the one shell in %s on node %s", opened, task.ID, task.NodeID)
	}
	if closed[0].TargetID != task.ID || closed[0].Details["sid"] != session.SessionID || closed[0].Details["reason"] == "" {
		t.Errorf("shell.closed = %+v, want the same shell with a reason", closed)
	}

	// The same container's log streams over SSE: at least one tick.
	logURL := f.srv.URL + base + "/containers/" + task.ID + "/logs/stream?sid=swarm-worker-logs-0001&nodeId=" + task.NodeID + "&tail=20"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, logURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(f.srv.Cookie())
	req.Header.Set("Accept", "text/event-stream")
	logResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open the log stream: %v", err)
	}
	defer func() { _ = logResp.Body.Close() }()
	if logResp.StatusCode != http.StatusOK || !strings.HasPrefix(logResp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("log stream: %d %s", logResp.StatusCode, logResp.Header.Get("Content-Type"))
	}
	ticks := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(logResp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") && strings.Contains(line, "tick") {
				ticks <- line
				return
			}
		}
		ticks <- ""
	}()
	select {
	case line := <-ticks:
		if line == "" {
			t.Fatal("the log stream ended without a tick")
		}
		t.Logf("log line over SSE: %s", line)
	case <-time.After(60 * time.Second):
		t.Fatal("no tick reached the log stream within a minute")
	}
	t.Logf("TestSwarmWorkerShellAndLogs took %s", time.Since(started).Round(time.Second))
}
