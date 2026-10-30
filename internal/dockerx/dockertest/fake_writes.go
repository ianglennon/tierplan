// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// fakeHandler answers one family of requests for the fake daemon. p is the path
// with any /vX.Y prefix removed. It reports true when it answered; false lets the
// next handler, and then the read routes, try. A handler that changes what the
// daemon reports does it through the Fake, so the next list or inspect shows it.
//
// The registry exists so each plan that adds a write (networks, volumes, exec) puts
// its fake routes in its own file and registers them with registerFakeHandler,
// without editing dockertest.go or another plan's file.
type fakeHandler func(f *Fake, w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool

// fakeHandlers are consulted, in registration order, before the read routes.
var fakeHandlers []fakeHandler

// registerFakeHandler adds h to the registry. It returns true so a file can call
// it at package level: var _ = registerFakeHandler(...).
func registerFakeHandler(h fakeHandler) bool {
	fakeHandlers = append(fakeHandlers, h)
	return true
}

var _ = registerFakeHandler((*Fake).serveWrite)

// containerVerb matches the six container verbs that take no body.
var containerVerb = regexp.MustCompile(`^/containers/([^/]+)/(start|stop|restart|kill|pause|unpause)$`)

// verbState is the state each verb leaves a container in, and the Status text a
// list then shows.
var verbState = map[string]fakeState{
	"start":   {"running", "Up 1 second"},
	"restart": {"running", "Up 1 second"},
	"unpause": {"running", "Up 1 second"},
	"stop":    {"exited", "Exited (0) 1 second ago"},
	"kill":    {"exited", "Exited (137) 1 second ago"},
	"pause":   {"paused", "Up 1 second (Paused)"},
}

// fakeState is the State and Status text a write left a container with.
type fakeState struct{ state, status string }

// serveWrite answers POST /containers/{id}/{start|stop|restart|kill|pause|unpause}.
// An ID the daemon does not know is the 404 a real one gives. Starting a container
// that runs and stopping one that does not are the 304 a real one gives, which the
// client takes as success. Every write is recorded for Writes, whatever the answer.
func (f *Fake) serveWrite(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost {
		return false
	}
	m := containerVerb.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	id, verb := m[1], m[2]
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.WriteError != "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": cfg.WriteError})
		return true
	}
	if cfg.PolicyDeny {
		// The refusal an agent gives for a write its policy does not allow, in the
		// words the server maps to agent_policy_denied.
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}

	c, known := cfg.resolveContainer(id)
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + id})
		return true
	}
	id, current := c.ID, c.State
	if (verb == "start" && current == "running") || (verb == "stop" && current != "running" && current != "paused" && current != "restarting") {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	f.mu.Lock()
	f.states[id] = verbState[verb]
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return true
}

// dropConnection closes the connection under w without writing an answer. It reports
// false when the connection cannot be taken over, and the caller answers as usual.
func dropConnection(w http.ResponseWriter) bool {
	h, ok := w.(http.Hijacker)
	if !ok {
		return false
	}
	conn, _, err := h.Hijack()
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// beginWrite counts one write as in flight, keeps the high-water mark and waits for
// delay; the returned function ends the count.
func (f *Fake) beginWrite(delay time.Duration) func() {
	f.mu.Lock()
	f.inflight++
	f.maxInflight = max(f.maxInflight, f.inflight)
	f.mu.Unlock()
	f.await.notify()
	if delay > 0 {
		time.Sleep(delay)
	}
	return func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
		f.await.notify()
	}
}

// MaxConcurrentWrites is the most container writes the daemon has served at the
// same time since it started. With FakeConfig.WriteDelay set it shows how many calls
// a caller made in parallel.
func (f *Fake) MaxConcurrentWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInflight
}

// containerRemove matches DELETE /containers/{id}.
var containerRemove = regexp.MustCompile(`^/containers/([^/]+)$`)

// removedState is the state a removed container is given; withStates then leaves it
// out of every list and inspect, as a real daemon does.
var removedState = fakeState{state: "removed"}

// serveRemove answers DELETE /containers/{id}. A container that runs is the 409 a real
// daemon gives unless force=1. The write is recorded whatever the answer, with its
// query, so a test sees whether force and v were sent.
func (f *Fake) serveRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := containerRemove.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	c, known := cfg.resolveContainer(m[1])
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + m[1]})
		return true
	}
	if c.State == "running" && r.URL.Query().Get("force") != "1" && r.URL.Query().Get("force") != "true" {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "cannot remove container \"" + strings.TrimPrefix(c.Names[0], "/") + "\": container is running: stop the container before removing it or force remove"})
		return true
	}
	f.mu.Lock()
	f.states[c.ID] = removedState
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return true
}

var _ = registerFakeHandler((*Fake).serveRemove)

// recordWrite notes a write as "METHOD path?query", the path as the daemon saw it
// without the version prefix.
func (f *Fake) recordWrite(r *http.Request) {
	p := r.URL.Path
	if m := versionPrefix.FindStringSubmatch(p); m != nil {
		p = m[3]
	}
	if q := r.URL.RawQuery; q != "" {
		p += "?" + q
	}
	write := r.Method + " " + p
	f.mu.Lock()
	f.writes = append(f.writes, write)
	f.mu.Unlock()
	if f.cfg.OnWrite != nil {
		f.cfg.OnWrite(write)
	}
}

// Writes returns every write the daemon served so far, in order, as
// "METHOD path?query" without any version prefix. Reads are not listed.
func (f *Fake) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// withStates returns c with each container the writes changed in the state they
// left it. The slice is copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withStates(states map[string]fakeState) FakeConfig {
	if len(states) == 0 || len(c.Containers) == 0 {
		return c
	}
	cs := make([]FakeContainer, 0, len(c.Containers))
	for _, fc := range c.Containers {
		if s, ok := states[fc.ID]; ok {
			if s == removedState {
				continue
			}
			fc.State, fc.Status = s.state, s.status
		}
		cs = append(cs, fc)
	}
	c.Containers = cs
	return c
}
