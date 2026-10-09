// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var _ = registerFakeHandler((*Fake).serveExec)

var (
	execCreateRoute  = regexp.MustCompile(`^/containers/([^/]+)/exec$`)
	execRoute        = regexp.MustCompile(`^/exec/([^/]+)/(start|resize|json)$`)
	execOCIMissing = "OCI runtime exec failed: exec failed: unable to start container process: exec: %q: stat %s: no such file or directory\r\n"
)

// fakeExec is one exec of the fake daemon.
type fakeExec struct {
	container string
	cmd       []string
	started   bool
	running   bool
	exitCode  *int
}

// fakeExecs is the Fake's exec state. The zero value is ready to use.
type fakeExecs struct {
	mu      sync.Mutex
	next    int
	byID    map[string]*fakeExec
	bodies  []string
	resizes []string
}

// serveExec answers the four exec routes a hijacked shell uses:
//
//	POST /containers/{id}/exec   create, 201 {Id}; 404 unknown, 409 not running
//	POST /exec/{id}/start        hijack: 101, then a TTY that echoes each line
//	POST /exec/{id}/resize       records h and w
//	GET  /exec/{id}/json         Running and ExitCode
//
// After the hijack, each line written is echoed back with a CR LF, as a terminal
// does; "exit N" ends the process with exit code N and closes the stream. An exec
// whose first argument is in FakeConfig.MissingExecutables prints the runtime's
// "no such file or directory" message and ends with 127. A client that closes the
// stream leaves the process running, as a real daemon does (D-30).
func (f *Fake) serveExec(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if m := execCreateRoute.FindStringSubmatch(p); m != nil && r.Method == http.MethodPost {
		f.recordWrite(r)
		f.createExec(w, r, m[1], cfg)
		return true
	}
	m := execRoute.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	id, verb := m[1], m[2]
	switch {
	case verb == "json" && r.Method == http.MethodGet:
		f.inspectExec(w, id)
	case verb == "resize" && r.Method == http.MethodPost:
		f.recordWrite(r)
		f.resizeExec(w, r, id)
	case verb == "start" && r.Method == http.MethodPost:
		f.recordWrite(r)
		f.startExec(w, r, id, cfg)
	default:
		return false
	}
	return true
}

func (f *Fake) createExec(w http.ResponseWriter, r *http.Request, ref string, cfg FakeConfig) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	c, known := cfg.resolveContainer(ref)
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + ref})
		return
	}
	if c.State != "running" {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Container " + c.ID + " is not running"})
		return
	}
	var req struct {
		Cmd []string `json:"Cmd"`
	}
	_ = json.Unmarshal(body, &req)

	e := &f.execs
	e.mu.Lock()
	e.next++
	id := fmt.Sprintf("exec%04d", e.next)
	if e.byID == nil {
		e.byID = map[string]*fakeExec{}
	}
	e.byID[id] = &fakeExec{container: c.ID, cmd: req.Cmd}
	e.bodies = append(e.bodies, string(body))
	e.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"Id": id})
}

func (f *Fake) execByID(id string) (*fakeExec, bool) {
	e := &f.execs
	e.mu.Lock()
	defer e.mu.Unlock()
	x, ok := e.byID[id]
	return x, ok
}

func (f *Fake) inspectExec(w http.ResponseWriter, id string) {
	x, ok := f.execByID(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such exec instance: " + id})
		return
	}
	e := &f.execs
	e.mu.Lock()
	out := map[string]any{"ID": id, "ContainerID": x.container, "Running": x.running, "ExitCode": x.exitCode, "Pid": 0}
	if x.started {
		out["Pid"] = 1
	}
	e.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) resizeExec(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := f.execByID(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such exec instance: " + id})
		return
	}
	q := r.URL.Query()
	h, errH := strconv.Atoi(q.Get("h"))
	wd, errW := strconv.Atoi(q.Get("w"))
	if errH != nil || errW != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad size"})
		return
	}
	f.execs.mu.Lock()
	f.execs.resizes = append(f.execs.resizes, fmt.Sprintf("h=%d&w=%d", h, wd))
	f.execs.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *Fake) startExec(w http.ResponseWriter, r *http.Request, id string, cfg FakeConfig) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	x, ok := f.execByID(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such exec instance: " + id})
		return
	}
	f.execs.mu.Lock()
	already := x.started
	f.execs.mu.Unlock()
	if already {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Exec command " + id + " is already running"})
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "the fake cannot hijack"})
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")

	end := func(code int) {
		f.execs.mu.Lock()
		x.running = false
		x.exitCode = &code
		f.execs.mu.Unlock()
	}
	f.execs.mu.Lock()
	x.started = true
	f.execs.mu.Unlock()

	if len(x.cmd) > 0 && slices.Contains(cfg.MissingExecutables, x.cmd[0]) {
		_, _ = fmt.Fprintf(rw, execOCIMissing, x.cmd[0], x.cmd[0])
		_ = rw.Flush()
		end(127)
		return
	}
	f.execs.mu.Lock()
	x.running = true
	f.execs.mu.Unlock()
	_ = rw.Flush()

	br := bufio.NewReader(rw.Reader)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			text := strings.TrimRight(line, "\r\n")
			if rest, isExit := strings.CutPrefix(text, "exit"); isExit {
				code, convErr := strconv.Atoi(strings.TrimSpace(rest))
				if rest == "" || convErr == nil {
					_, _ = rw.WriteString(line + "\r\n")
					_ = rw.Flush()
					end(code)
					return
				}
			}
			_, _ = rw.WriteString(text + "\r\n")
			_ = rw.Flush()
		}
		if err != nil {
			// The client went away; the process keeps running (D-30).
			return
		}
	}
}

// Resizes returns every terminal resize the daemon served, as "h=30&w=100".
func (f *Fake) Resizes() []string {
	f.execs.mu.Lock()
	defer f.execs.mu.Unlock()
	return slices.Clone(f.execs.resizes)
}

// ExecCreateBodies returns the JSON body of every exec create the daemon served, so
// a test can check exactly what was asked for.
func (f *Fake) ExecCreateBodies() []string {
	f.execs.mu.Lock()
	defer f.execs.mu.Unlock()
	return slices.Clone(f.execs.bodies)
}
