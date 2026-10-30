// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package selfmaint

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/system"

	"github.com/ianglennon/coaming/proto/testsupport"
)

// fakeEngine is a small Docker Engine API on a Unix socket: enough of the
// container, image, service and info endpoints for self-maintenance, and a record
// of every request so a test can say "no Docker call was made" or "exactly these
// writes". It keeps its state in memory and mirrors what the real engine
// answers, using the engine's own API types.
type fakeEngine struct {
	t    *testing.T
	sock string

	mu    sync.Mutex
	calls []string // "METHOD /path?query", version prefix removed

	swarmControl bool
	service      string // raw JSON of GET /services/{id}; "" means 404
	serviceID    string
	servicePosts []servicePost
	serviceErr   int // when not zero, POST /services/{id}/update answers this status

	// removals: what DELETE of a service, secret or volume was asked for, in order.
	serviceDeletes, secretDeletes, volumeDeletes []string
	// volumeBusy is how many DELETE /volumes/{name} answer 409 (in use) before one works.
	volumeBusy int
	// volumeErr, when not zero, makes DELETE /volumes/{name} answer this status always.
	volumeErr int

	containers map[string]*container.InspectResponse
	nextID     int
	creates    []createdContainer
	connects   []connectCall
	images     map[string]image.InspectResponse
	pullErr    bool
	pulled     []string

	// onStart runs, with the lock held, when a container is started.
	onStart func(f *fakeEngine, c *container.InspectResponse)
	// onInspect runs, with the lock held, when a container that exists is inspected,
	// before the answer is written. A test sets it before the code under test starts,
	// since it is read under the lock only once requests arrive.
	onInspect func(f *fakeEngine, c *container.InspectResponse)
	// failStart makes starting the container with this ID fail.
	failStart map[string]bool
}

type servicePost struct {
	ID      string
	Version string
	Body    []byte
}

type createdContainer struct {
	Name string
	ID   string
	Req  container.CreateRequest
}

type connectCall struct {
	Network, Container string
	Endpoint           *network.EndpointSettings
}

var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	// A short path: the limit of a Unix socket address is about 100 bytes, and a
	// test name makes t.TempDir() longer than that.
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeEngine{
		t: t, sock: filepath.Join(dir, "d.sock"),
		containers: map[string]*container.InspectResponse{},
		images:     map[string]image.InspectResponse{},
		failStart:  map[string]bool{},
	}
	l, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: testsupport.Deadline}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// callList returns the requests so far.
func (f *fakeEngine) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// writes returns the requests that change something: every method but GET.
func (f *fakeEngine) writes() []string {
	var out []string
	for _, c := range f.callList() {
		if !strings.HasPrefix(c, "GET ") && !strings.HasPrefix(c, "HEAD ") {
			out = append(out, c)
		}
	}
	return out
}

// addContainer registers a container as the engine would inspect it.
func (f *fakeEngine) addContainer(c container.InspectResponse) *container.InspectResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	cc := c
	f.containers[c.ID] = &cc
	return &cc
}

func (f *fakeEngine) find(ref string) *container.InspectResponse {
	if c, ok := f.containers[ref]; ok {
		return c
	}
	for id, c := range f.containers {
		if strings.HasPrefix(id, ref) || c.Name == "/"+strings.TrimPrefix(ref, "/") {
			return c
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such " + what})
}

func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	p := versionPrefix.ReplaceAllString(r.URL.Path, "")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+p+queryOf(r.URL))

	switch {
	case p == "/_ping":
		w.Header().Set("API-Version", "1.50")
		w.Header().Set("Ostype", "linux")
		_, _ = io.WriteString(w, "OK")
	case p == "/info" && r.Method == http.MethodGet:
		var info system.Info
		info.Swarm = swarm.Info{LocalNodeState: swarm.LocalNodeStateActive, ControlAvailable: f.swarmControl}
		writeJSON(w, http.StatusOK, info)

	case p == "/containers/json" && r.Method == http.MethodGet:
		f.list(w, r)
	case p == "/containers/create" && r.Method == http.MethodPost:
		f.create(w, r, body)
	case strings.HasPrefix(p, "/containers/"):
		f.containerOp(w, r, strings.TrimPrefix(p, "/containers/"))

	case strings.HasPrefix(p, "/networks/") && strings.HasSuffix(p, "/connect") && r.Method == http.MethodPost:
		var req struct {
			Container      string
			EndpointConfig *network.EndpointSettings
		}
		_ = json.Unmarshal(body, &req)
		f.connects = append(f.connects, connectCall{Network: strings.TrimSuffix(strings.TrimPrefix(p, "/networks/"), "/connect"), Container: req.Container, Endpoint: req.EndpointConfig})
		w.WriteHeader(http.StatusOK)

	case p == "/images/create" && r.Method == http.MethodPost:
		q := r.URL.Query()
		f.pulled = append(f.pulled, q.Get("fromImage")+":"+q.Get("tag"))
		if f.pullErr {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "pull access denied"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"Pulling"}`+"\n"+`{"status":"Status: Downloaded newer image"}`+"\n")
	case strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json") && r.Method == http.MethodGet:
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
		if im, ok := f.images[ref]; ok {
			writeJSON(w, http.StatusOK, im)
			return
		}
		notFound(w, "image: "+ref)

	case strings.HasPrefix(p, "/services/") && r.Method == http.MethodGet:
		if f.service == "" || strings.TrimPrefix(p, "/services/") != f.serviceID {
			notFound(w, "service")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.service)
	case strings.HasPrefix(p, "/services/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(p, "/services/")
		if f.service == "" || id != f.serviceID {
			notFound(w, "service")
			return
		}
		f.serviceDeletes = append(f.serviceDeletes, id)
		f.service = ""
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(p, "/secrets/") && r.Method == http.MethodDelete:
		f.secretDeletes = append(f.secretDeletes, strings.TrimPrefix(p, "/secrets/"))
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/volumes/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(p, "/volumes/")
		switch {
		case f.volumeErr != 0:
			writeJSON(w, f.volumeErr, map[string]string{"message": "volume failure"})
		case f.volumeBusy > 0:
			f.volumeBusy--
			writeJSON(w, http.StatusConflict, map[string]string{"message": "remove " + name + ": volume is in use"})
		default:
			f.volumeDeletes = append(f.volumeDeletes, name)
			w.WriteHeader(http.StatusNoContent)
		}
	case strings.HasPrefix(p, "/services/") && strings.HasSuffix(p, "/update") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/services/"), "/update")
		f.servicePosts = append(f.servicePosts, servicePost{ID: id, Version: r.URL.Query().Get("version"), Body: body})
		if f.serviceErr != 0 {
			writeJSON(w, f.serviceErr, map[string]string{"message": "update out of sequence"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"Warnings": nil})
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]string{"message": "the fake engine does not serve " + r.Method + " " + p})
	}
}

func queryOf(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

func (f *fakeEngine) list(w http.ResponseWriter, r *http.Request) {
	var filters map[string]map[string]bool
	_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
	var out []container.Summary
	for _, c := range f.containers {
		if c.State == nil || !c.State.Running {
			continue
		}
		ok := true
		for l := range filters["label"] {
			k, v, has := strings.Cut(l, "=")
			got, present := c.Config.Labels[k]
			if !present || (has && got != v) {
				ok = false
			}
		}
		if ok {
			out = append(out, container.Summary{ID: c.ID, Names: []string{c.Name}, Labels: c.Config.Labels, State: "running"})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeEngine) create(w http.ResponseWriter, r *http.Request, body []byte) {
	var req container.CreateRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Config == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad create request"})
		return
	}
	name := r.URL.Query().Get("name")
	if name != "" {
		for _, c := range f.containers {
			if c.Name == "/"+name {
				writeJSON(w, http.StatusConflict, map[string]string{"message": "Conflict. The container name \"/" + name + "\" is already in use"})
				return
			}
		}
	}
	f.nextID++
	id := fmt.Sprintf("%064x", f.nextID)
	if name == "" {
		name = fmt.Sprintf("generated_%d", f.nextID)
	}
	f.containers[id] = &container.InspectResponse{
		ID: id, Name: "/" + name, Image: "sha256:new", State: &container.State{Status: "created"},
		Config: req.Config, HostConfig: req.HostConfig,
	}
	f.creates = append(f.creates, createdContainer{Name: name, ID: id, Req: req})
	writeJSON(w, http.StatusCreated, container.CreateResponse{ID: id})
}

func (f *fakeEngine) containerOp(w http.ResponseWriter, r *http.Request, rest string) {
	ref, op, _ := strings.Cut(rest, "/")
	c := f.find(ref)
	switch {
	case op == "json" && r.Method == http.MethodGet:
		if c == nil {
			notFound(w, "container: "+ref)
			return
		}
		// onInspect is set before the code under test starts; it is read here under f.mu.
		if f.onInspect != nil {
			f.onInspect(f, c)
		}
		writeJSON(w, http.StatusOK, c)
	case op == "" && r.Method == http.MethodDelete:
		if c == nil {
			notFound(w, "container: "+ref)
			return
		}
		delete(f.containers, c.ID)
		w.WriteHeader(http.StatusNoContent)
	case op == "rename" && r.Method == http.MethodPost:
		if c == nil {
			notFound(w, "container: "+ref)
			return
		}
		nn := r.URL.Query().Get("name")
		for _, o := range f.containers {
			if o.Name == "/"+nn && o != c {
				writeJSON(w, http.StatusConflict, map[string]string{"message": "Conflict. The container name \"/" + nn + "\" is already in use"})
				return
			}
		}
		c.Name = "/" + nn
		w.WriteHeader(http.StatusNoContent)
	case op == "stop" && r.Method == http.MethodPost:
		if c == nil {
			notFound(w, "container: "+ref)
			return
		}
		c.State = &container.State{Status: "exited", Running: false}
		w.WriteHeader(http.StatusNoContent)
	case op == "start" && r.Method == http.MethodPost:
		if c == nil {
			notFound(w, "container: "+ref)
			return
		}
		if f.failStart[c.ID] {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "cannot start"})
			return
		}
		c.State = &container.State{Status: "running", Running: true, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if f.onStart != nil {
			f.onStart(f, c)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]string{"message": "the fake engine does not serve " + r.Method + " /containers/" + rest})
	}
}
