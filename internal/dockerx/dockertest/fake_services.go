// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/swarm"
)

var _ = registerFakeHandler((*Fake).serveServiceInspect)
var _ = registerFakeHandler((*Fake).serveServiceUpdate)
var _ = registerFakeHandler((*Fake).serveServiceCreate)
var _ = registerFakeHandler((*Fake).serveServiceRemove)

// FakeServiceUpdate is one update the fake daemon accepted: the service it was for, the
// version and query it came with, the X-Registry-Auth header and the spec it carried.
type FakeServiceUpdate struct {
	ServiceID string
	Version   string
	Query     url.Values
	Auth      string
	Spec      swarm.ServiceSpec
}

// FakeServiceCreate is one service create the fake daemon accepted: the query and the
// X-Registry-Auth header it came with and the spec it carried.
type FakeServiceCreate struct {
	Query url.Values
	Auth  string
	Spec  swarm.ServiceSpec
}

// fakeServiceState is what accepted updates left a service with: its version, its spec, the
// spec before the last update and the update status a rollback left.
type fakeServiceState struct {
	version  uint64
	spec     swarm.ServiceSpec
	previous *swarm.ServiceSpec
	// updateState and updateMessage are the UpdateStatus a rollback started, empty for none.
	updateState, updateMessage string
}

// rollbackMessage is the message a manager gives the update status of a rollback an operator
// asked for.
const rollbackMessage = "manually requested rollback"

// previousSpecRefusal is what a manager answers a rollback of a service that has never been
// updated.
const previousSpecRefusal = "service does not have a previous spec"

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
		if st.previous != nil {
			prev := *st.previous
			fs.PreviousSpec = &prev
		}
		if st.updateState != "" {
			fs.UpdateState, fs.UpdateMessage = st.updateState, st.updateMessage
		}
		if r := spec.Mode.Replicated; r != nil && r.Replicas != nil {
			fs.Desired, fs.Running = *r.Replicas, *r.Replicas
		}
		rows[i] = fs
	}
	c.ServiceRows = rows
	return c
}

// virtualIPs gives the listed service s its endpoint's virtual IPs: the networks ServiceVIPNetworks
// names for it, else the ones the row set itself (FakeService.VirtualIPs), else one per network its
// current spec names, as a manager does for a service in virtual-IP mode. The ID of a network a
// spec names by name is the daemon's.
func (c FakeConfig) virtualIPs(fs FakeService, s *swarm.Service) {
	ids, ok := c.ServiceVIPNetworks[fs.Name]
	if !ok {
		ids, ok = c.ServiceVIPNetworks[fs.ID]
	}
	if ok {
		s.Endpoint.VirtualIPs = nil
	} else if len(fs.VirtualIPs) > 0 {
		return
	} else {
		for _, a := range s.Spec.TaskTemplate.Networks {
			id := a.Target
			if n, found := c.resolveNetwork(id); found {
				id = n.ID
			}
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		if !slices.ContainsFunc(s.Endpoint.VirtualIPs, func(v swarm.EndpointVirtualIP) bool { return v.NetworkID == id }) {
			s.Endpoint.VirtualIPs = append(s.Endpoint.VirtualIPs, swarm.EndpointVirtualIP{NetworkID: id})
		}
	}
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
	if fs.UpdateError != "" && q.Get("rollback") != "previous" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": fs.UpdateError})
		return true
	}
	var now swarm.ServiceSpec
	for _, s := range cfg.serviceList(false) {
		if s.ID == fs.ID {
			now = s.Spec
		}
	}
	stored := spec
	if cfg.NetworkTargetsByID {
		stored.TaskTemplate.Networks = cfg.networkTargetsByID(spec.TaskTemplate.Networks)
	}
	next := fakeServiceState{version: current + 1, spec: stored, previous: &now}
	if q.Get("rollback") == "previous" {
		// A rollback ignores the spec in the body: the previous spec becomes the service's and
		// the one it replaces becomes the previous one, so a second rollback toggles back.
		switch {
		case fs.RollbackError != "":
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": fs.RollbackError})
			return true
		case fs.PreviousSpec == nil:
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": previousSpecRefusal})
			return true
		}
		next.spec = *fs.PreviousSpec
		next.updateState, next.updateMessage = "rollback_started", rollbackMessage
	}
	f.mu.Lock()
	if f.svcState == nil {
		f.svcState = map[string]fakeServiceState{}
	}
	f.svcState[fs.ID] = next
	f.svcUpdates = append(f.svcUpdates, FakeServiceUpdate{
		ServiceID: fs.ID, Version: q.Get("version"), Query: q, Auth: r.Header.Get("X-Registry-Auth"), Spec: spec,
	})
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, swarm.ServiceUpdateResponse{Warnings: fs.UpdateWarnings})
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

// withoutServices returns c without the services removed through the daemon. The slice is
// copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withoutServices(gone map[string]bool) FakeConfig {
	if len(gone) == 0 {
		return c
	}
	kept := make([]FakeService, 0, len(c.ServiceRows))
	for _, fs := range c.ServiceRows {
		if !gone[fs.ID] {
			kept = append(kept, fs)
		}
	}
	c.ServiceRows = kept
	return c
}

// serveServiceRemove answers DELETE /services/{id}, by ID or by name. A service the swarm
// does not know is the 404 a real manager gives; otherwise it leaves every list. The write is
// recorded whatever the answer.
func (f *Fake) serveServiceRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := servicePath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
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
	f.mu.Lock()
	if f.svcGone == nil {
		f.svcGone = map[string]bool{}
	}
	f.svcGone[fs.ID] = true
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	return true
}

// withCreatedServices returns c with the services created through the daemon listed after its own.
// The slice is copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withCreatedServices(created []FakeService) FakeConfig {
	if len(created) == 0 {
		return c
	}
	c.ServiceRows = append(slices.Clone(c.ServiceRows), created...)
	return c
}

// serveServiceCreate answers POST /services/create. It refuses a name a service already has with
// the 409 a real manager gives (RESEARCH E16) and the configured ServiceCreateError with a 400. An
// accepted create joins the lists at version 1 and is kept for ServiceCreates. The write is recorded
// whatever the answer.
func (f *Fake) serveServiceCreate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/services/create" {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
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
	var spec swarm.ServiceSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	for _, fs := range cfg.ServiceRows {
		if fs.Name == spec.Name {
			writeJSON(w, http.StatusConflict, map[string]string{
				"message": "rpc error: code = AlreadyExists desc = name conflicts with an existing object: service " + spec.Name + " already exists"})
			return true
		}
	}
	if cfg.ServiceCreateError != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": cfg.ServiceCreateError})
		return true
	}
	stored := spec
	if cfg.NetworkTargetsByID {
		stored.TaskTemplate.Networks = cfg.networkTargetsByID(spec.TaskTemplate.Networks)
	}
	row := FakeService{Name: spec.Name, Version: 1, Spec: &stored}
	switch {
	case spec.Mode.Global != nil:
		row.Mode = "global"
	case spec.Mode.ReplicatedJob != nil:
		row.Mode = "replicated-job"
	case spec.Mode.GlobalJob != nil:
		row.Mode = "global-job"
	default:
		row.Mode = "replicated"
		if rep := spec.Mode.Replicated; rep != nil && rep.Replicas != nil {
			row.Desired, row.Running = *rep.Replicas, *rep.Replicas
		}
	}
	q := r.URL.Query()
	f.mu.Lock()
	row.ID = fmt.Sprintf("svcnew%02d", len(f.svcCreated)+1)
	f.svcCreated = append(f.svcCreated, row)
	f.svcCreates = append(f.svcCreates, FakeServiceCreate{Query: q, Auth: r.Header.Get("X-Registry-Auth"), Spec: spec})
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, swarm.ServiceCreateResponse{ID: row.ID, Warnings: cfg.ServiceCreateWarnings})
	return true
}

// ServiceCreates returns every service create the daemon accepted, in order.
func (f *Fake) ServiceCreates() []FakeServiceCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.svcCreates)
}

// labelFilter reads the "label" terms of a list request's filters query (a JSON map of term to
// values, as the engine client encodes it) and returns a test of a label set: every term must
// match, a "key=value" term on both, a bare "key" on the key alone. Without a label term it
// accepts everything, as a daemon does.
func labelFilter(rawFilters string) func(map[string]string) bool {
	var f map[string]map[string]bool
	if rawFilters == "" || json.Unmarshal([]byte(rawFilters), &f) != nil || len(f["label"]) == 0 {
		return func(map[string]string) bool { return true }
	}
	terms := f["label"]
	return func(labels map[string]string) bool {
		for term, on := range terms {
			if !on {
				continue
			}
			key, value, hasValue := strings.Cut(term, "=")
			got, ok := labels[key]
			if !ok || (hasValue && got != value) {
				return false
			}
		}
		return true
	}
}

// filterServicesByLabel keeps the services whose own labels pass the request's label filter.
func filterServicesByLabel(in []swarm.Service, rawFilters string) []swarm.Service {
	keep := labelFilter(rawFilters)
	out := make([]swarm.Service, 0, len(in))
	for _, s := range in {
		if keep(s.Spec.Labels) {
			out = append(out, s)
		}
	}
	return out
}
