// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
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
	next := verbState[verb]
	if verb == "start" && cfg.ExitOnStart[c.Image] {
		next = fakeState{"exited", "Exited (1) 1 second ago"}
	}
	f.mu.Lock()
	f.states[id] = next
	if h := cfg.HealthOnStart[c.Image]; verb == "start" && h != "" {
		if f.ctr.health == nil {
			f.ctr.health = map[string]string{}
		}
		f.ctr.health[id] = h
	}
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
	if refusal, refused := cfg.RefuseRemove[m[1]]; refused {
		writeJSON(w, refusal.Status, map[string]string{"message": refusal.Message})
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

// FakeContainerCreate is one container create the fake accepted, as the daemon decoded it.
type FakeContainerCreate struct {
	// ID is the ID the fake gave the new container and Name the name it has, the one asked for or
	// the one the fake made up.
	ID, Name         string
	Config           *container.Config
	HostConfig       *container.HostConfig
	NetworkingConfig *network.NetworkingConfig
}

// FakeRename is one container rename the fake accepted: the container, the name it had and the
// name it was given, without a leading slash.
type FakeRename struct{ ID, From, To string }

// fakeContainerWrites is what creates and renames left: the containers made through the fake,
// the creates and renames themselves, and the current name of every container that was renamed,
// by ID. All of it is guarded by Fake.mu.
type fakeContainerWrites struct {
	created []FakeContainer
	creates []FakeContainerCreate
	renames []FakeRename
	names   map[string]string
	// health is the health status a started container reports (FakeConfig.HealthOnStart), by ID.
	health map[string]string
}

var _ = registerFakeHandler((*Fake).serveContainerCreate)
var _ = registerFakeHandler((*Fake).serveContainerRename)

// containerRename matches POST /containers/{id}/rename.
var containerRename = regexp.MustCompile(`^/containers/([^/]+)/rename$`)

// nameTaken reports whether a container of cfg already has name, with or without the slash.
func (c FakeConfig) nameTaken(name string) (FakeContainer, bool) {
	name = strings.TrimPrefix(name, "/")
	for _, fc := range c.Containers {
		for _, n := range fc.Names {
			if strings.TrimPrefix(n, "/") == name {
				return fc, true
			}
		}
	}
	return FakeContainer{}, false
}

// serveContainerCreate answers POST /containers/create. The container is made in the created
// state under the name asked for (or one the fake makes up), with the labels, command,
// environment and networks of the body, and joins every list and inspect from then on. A name
// in use is the 409 a real daemon gives, an endpoint on a network the daemon does not have the 404.
// The body is kept for ContainerCreates and the write is recorded whatever the answer.
func (f *Fake) serveContainerCreate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/containers/create" {
		return false
	}
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
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	name := r.URL.Query().Get("name")
	if refusal, refused := cfg.RefuseCreate[name]; refused {
		writeJSON(w, refusal.Status, map[string]string{"message": refusal.Message})
		return true
	}
	var req container.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	if req.Config == nil || req.Config.Image == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Config.Image is required"})
		return true
	}
	if other, taken := cfg.nameTaken(name); name != "" && taken {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Conflict. The container name \"/" + strings.TrimPrefix(name, "/") + "\" is already in use by container \"" + other.ID + "\". You have to remove (or rename) that container to be able to reuse that name."})
		return true
	}
	nets := map[string]FakeEndpoint{}
	if req.NetworkingConfig != nil {
		for netName, es := range req.NetworkingConfig.EndpointsConfig {
			fn, ok := cfg.resolveNetwork(netName)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "network " + netName + " not found"})
				return true
			}
			ep := FakeEndpoint{NetworkID: fn.ID, IPv4: "10.99.0.2"}
			if es != nil {
				ep.Aliases = slices.Clone(es.Aliases)
				if es.IPAMConfig != nil && es.IPAMConfig.IPv4Address.IsValid() {
					ep.IPv4 = es.IPAMConfig.IPv4Address.String()
				}
				if es.IPAMConfig != nil && es.IPAMConfig.IPv6Address.IsValid() {
					ep.IPv6 = es.IPAMConfig.IPv6Address.String()
				}
				if len(es.MacAddress) > 0 {
					ep.MAC = es.MacAddress.String()
				}
			}
			nets[fn.Name] = ep
		}
	}
	f.mu.Lock()
	n := len(f.ctr.created) + 1
	id := fmt.Sprintf("%064x", 0xfa4ec0de00000000+uint64(n))
	if name == "" {
		name = fmt.Sprintf("fake_container_%d", n)
	}
	fc := FakeContainer{
		ID: id, Names: []string{"/" + strings.TrimPrefix(name, "/")}, Image: req.Config.Image,
		State: "created", Status: "Created", Created: time.Now().Unix(),
		Labels: req.Config.Labels, Networks: nets,
		StopTimeout: req.Config.StopTimeout, StopSignal: req.Config.StopSignal,
		Inspect: FakeInspect{
			Env: req.Config.Env, Cmd: req.Config.Cmd, Entrypoint: req.Config.Entrypoint,
			WorkingDir: req.Config.WorkingDir, User: req.Config.User, Tty: req.Config.Tty, Healthcheck: req.Config.Healthcheck,
		},
	}
	if req.Config.Env == nil {
		fc.Inspect.Env = []string{}
	}
	if req.HostConfig != nil {
		fc.Inspect.MemoryBytes, fc.Inspect.NanoCPUs = req.HostConfig.Memory, req.HostConfig.NanoCPUs
	}
	f.ctr.created = append(f.ctr.created, fc)
	f.ctr.creates = append(f.ctr.creates, FakeContainerCreate{
		ID: id, Name: strings.TrimPrefix(name, "/"), Config: req.Config, HostConfig: req.HostConfig, NetworkingConfig: req.NetworkingConfig,
	})
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, container.CreateResponse{ID: id})
	return true
}

// serveContainerRename answers POST /containers/{id}/rename?name=. An unknown container is the
// 404 and a name another container has the 409 a real daemon gives. The rename shows in every
// list and inspect from then on, and the write is recorded whatever the answer.
func (f *Fake) serveContainerRename(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost {
		return false
	}
	m := containerRename.FindStringSubmatch(p)
	if m == nil {
		return false
	}
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
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	fc, known := cfg.resolveContainer(m[1])
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + m[1]})
		return true
	}
	to := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
	if to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Neither old nor new names may be empty"})
		return true
	}
	if other, taken := cfg.nameTaken(to); taken && other.ID != fc.ID {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Conflict. The container name \"/" + to + "\" is already in use by container \"" + other.ID + "\". You have to remove (or rename) that container to be able to reuse that name."})
		return true
	}
	from := strings.TrimPrefix(firstOf(fc.Names, fc.ID), "/")
	f.mu.Lock()
	if f.ctr.names == nil {
		f.ctr.names = map[string]string{}
	}
	f.ctr.names[fc.ID] = to
	f.ctr.renames = append(f.ctr.renames, FakeRename{ID: fc.ID, From: from, To: to})
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return true
}

// ContainerCreates returns every container create the daemon accepted, in order, as the daemon
// decoded it.
func (f *Fake) ContainerCreates() []FakeContainerCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ctr.creates)
}

// ContainerRenames returns every container rename the daemon accepted, in order.
func (f *Fake) ContainerRenames() []FakeRename {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ctr.renames)
}

// withContainerWrites returns c with the containers made through the daemon listed after its
// own and every renamed container under its new name. The slice is copied, so the Fake's own
// configuration is not touched.
func (c FakeConfig) withContainerWrites(w *fakeContainerWrites) FakeConfig {
	if len(w.created) == 0 && len(w.names) == 0 && len(w.health) == 0 {
		return c
	}
	cs := make([]FakeContainer, 0, len(c.Containers)+len(w.created))
	cs = append(cs, c.Containers...)
	cs = append(cs, w.created...)
	for i, fc := range cs {
		if to, ok := w.names[fc.ID]; ok {
			fc.Names = []string{"/" + to}
		}
		if h, ok := w.health[fc.ID]; ok {
			fc.Health = h
		}
		cs[i] = fc
	}
	c.Containers = cs
	return c
}
