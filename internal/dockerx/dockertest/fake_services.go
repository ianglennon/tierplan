// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"

	"github.com/moby/moby/api/types/swarm"
)

var _ = registerFakeHandler((*Fake).serveServiceInspect)
var _ = registerFakeHandler((*Fake).serveServiceUpdate)

// FakeServiceUpdate is one update the fake daemon accepted: the service it was for, the
// version and query it came with, the X-Registry-Auth header and the spec it carried.
type FakeServiceUpdate struct {
	ServiceID string
	Version   string
	Query     url.Values
	Auth      string
	Spec      swarm.ServiceSpec
}

// fakeServiceState is what accepted updates left a service with.
type fakeServiceState struct {
	version uint64
	spec    swarm.ServiceSpec
}

// outOfSequenceRefusal is the message a real manager gives an update whose version is
// not the service's current one (RESEARCH E8).
const outOfSequenceRefusal = "rpc error: code = Unknown desc = update out of sequence"

var (
	servicePath       = regexp.MustCompile(`^/services/([^/]+)$`)
	serviceUpdatePath = regexp.MustCompile(`^/services/([^/]+)/update$`)
)

// withServices returns c with each service the daemon accepted an update for showing the
// spec and version that update left. A replicated service's task counts follow its new
// replica count at once, where a real swarm converges over seconds. The slice is copied,
// so the Fake's own configuration is not touched.
func (c FakeConfig) withServices(state map[string]fakeServiceState) FakeConfig {
	if len(state) == 0 {
		return c
	}
	rows := slices.Clone(c.ServiceRows)
	for i, fs := range rows {
		st, ok := state[fs.ID]
		if !ok {
			continue
		}
		spec := st.spec
		fs.Spec, fs.Version = &spec, st.version
		if r := spec.Mode.Replicated; r != nil && r.Replicas != nil {
			fs.Desired, fs.Running = *r.Replicas, *r.Replicas
		}
		rows[i] = fs
	}
	c.ServiceRows = rows
	return c
}

// resolveService finds a service by its ID or its name.
func (c FakeConfig) resolveService(ref string) (FakeService, bool) {
	for _, fs := range c.ServiceRows {
		if fs.ID == ref {
			return fs, true
		}
	}
	for _, fs := range c.ServiceRows {
		if fs.Name == ref {
			return fs, true
		}
	}
	return FakeService{}, false
}

// clusterRefused answers the refusals a node that is not a manager, or whose swarm lost
// quorum, gives a cluster question, and reports whether it did.
func clusterRefused(w http.ResponseWriter, cfg FakeConfig) bool {
	if !cfg.swarmManager() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": workerRefusal})
		return true
	}
	if cfg.QuorumLost {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": quorumRefusal})
		return true
	}
	return false
}

// serveServiceInspect answers GET /services/{id}, by ID or by name. A service the swarm
// does not know is the 404 a real manager gives.
func (f *Fake) serveServiceInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := servicePath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	if clusterRefused(w, cfg) {
		return true
	}
	fs, ok := cfg.resolveService(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "service " + m[1] + " not found"})
		return true
	}
	for _, s := range cfg.serviceList(true) {
		if s.ID == fs.ID {
			writeJSON(w, http.StatusOK, s)
			return true
		}
	}
	return true
}

// serveServiceUpdate answers POST /services/{id}/update. An unknown service is the 404 a
// real manager gives, a version that is not the service's current one the 500 "update out
// of sequence". Otherwise the spec in the body becomes the service's, its version goes up
// by one and the update is kept for ServiceUpdates. The write is recorded whatever the
// answer.
func (f *Fake) serveServiceUpdate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost {
		return false
	}
	m := serviceUpdatePath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	// A held write is judged against the service as it is when the hold ends, as a real
	// manager judges the version when it gets to the update.
	cfg = f.config()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if cfg.WriteError != "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": cfg.WriteError})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	fs, ok := cfg.resolveService(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "service " + m[1] + " not found"})
		return true
	}
	var spec swarm.ServiceSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	current := max(fs.Version, 1)
	q := r.URL.Query()
	if q.Get("version") != strconv.FormatUint(current, 10) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": outOfSequenceRefusal})
		return true
	}
	f.mu.Lock()
	if f.svcState == nil {
		f.svcState = map[string]fakeServiceState{}
	}
	f.svcState[fs.ID] = fakeServiceState{version: current + 1, spec: spec}
	f.svcUpdates = append(f.svcUpdates, FakeServiceUpdate{
		ServiceID: fs.ID, Version: q.Get("version"), Query: q, Auth: r.Header.Get("X-Registry-Auth"), Spec: spec,
	})
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, swarm.ServiceUpdateResponse{})
	return true
}

// ServiceUpdates returns every service update the daemon accepted, in order.
func (f *Fake) ServiceUpdates() []FakeServiceUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.svcUpdates)
}

// BumpService makes the service look changed by somebody else: its version goes up by one
// without an update, so the next write that carries the old version is refused as out of
// sequence.
func (f *Fake) BumpService(ref string) bool {
	cfg := f.config()
	fs, ok := cfg.resolveService(ref)
	if !ok {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st, had := f.svcState[fs.ID]
	if !had {
		spec := swarm.ServiceSpec{}
		for _, s := range cfg.serviceList(false) {
			if s.ID == fs.ID {
				spec = s.Spec
			}
		}
		st = fakeServiceState{version: max(fs.Version, 1), spec: spec}
	}
	st.version++
	if f.svcState == nil {
		f.svcState = map[string]fakeServiceState{}
	}
	f.svcState[fs.ID] = st
	return true
}
