// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest

import (
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/swarm"
)

var _ = registerFakeHandler((*Fake).serveNodeInspect)

var nodePath = regexp.MustCompile(`^/nodes/([^/]+)$`)

// serveNodeInspect answers GET /nodes/{id}. A node the swarm does not know is the 404 a
// real manager gives, and a worker or a swarm without quorum answers as it does for the
// list.
func (f *Fake) serveNodeInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := nodePath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	if clusterRefused(w, cfg) {
		return true
	}
	for _, n := range cfg.nodeList() {
		if n.ID == m[1] {
			writeJSON(w, http.StatusOK, n)
			return true
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "node " + m[1] + " not found"})
	return true
}

// FakeNodeUpdate is one node update the fake daemon accepted: the node, the version and the
// spec the request carried.
type FakeNodeUpdate struct {
	NodeID  string
	Version string
	Spec    swarm.NodeSpec
}

// fakeNodeState is what accepted updates left a node with: its version and the spec fields an
// update can change.
type fakeNodeState struct {
	version uint64
	spec    swarm.NodeSpec
}

var nodeUpdatePath = regexp.MustCompile(`^/nodes/([^/]+)/update$`)

var _ = registerFakeHandler((*Fake).serveNodeUpdate)

// withNodes returns c with each node the daemon accepted an update for showing the version,
// role, availability and labels that update left. The slice is copied, so the Fake's own
// configuration is not touched.
func (c FakeConfig) withNodes(state map[string]fakeNodeState) FakeConfig {
	if len(state) == 0 || len(c.NodeRows) == 0 {
		return c
	}
	rows := slices.Clone(c.NodeRows)
	for i, fn := range rows {
		st, ok := state[fn.ID]
		if !ok {
			continue
		}
		fn.Version = st.version
		fn.Role, fn.Availability = string(st.spec.Role), string(st.spec.Availability)
		fn.Labels = maps.Clone(st.spec.Labels)
		rows[i] = fn
	}
	c.NodeRows = rows
	return c
}

// serveNodeUpdate answers POST /nodes/{id}/update. An unknown node is the 404 a real manager
// gives, a version that is not the node's current one the 500 "update out of sequence", and a
// configured NodeUpdateError its own refusal. Otherwise the spec in the body becomes the node's,
// its version goes up by one and the update is kept for NodeUpdates. The write is recorded
// whatever the answer.
func (f *Fake) serveNodeUpdate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost {
		return false
	}
	m := nodeUpdatePath.FindStringSubmatch(p)
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
	if cfg.NodeUpdateError != "" {
		code := http.StatusInternalServerError
		if strings.Contains(cfg.NodeUpdateError, "last manager") {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]string{"message": cfg.NodeUpdateError})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	var node swarm.Node
	for _, n := range cfg.nodeList() {
		if n.ID == m[1] {
			node = n
		}
	}
	if node.ID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "node " + m[1] + " not found"})
		return true
	}
	var spec swarm.NodeSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	q := r.URL.Query()
	if q.Get("version") != strconv.FormatUint(node.Version.Index, 10) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": outOfSequenceRefusal})
		return true
	}
	f.mu.Lock()
	if f.nodeState == nil {
		f.nodeState = map[string]fakeNodeState{}
	}
	f.nodeState[node.ID] = fakeNodeState{version: node.Version.Index + 1, spec: spec}
	f.nodeUpdates = append(f.nodeUpdates, FakeNodeUpdate{NodeID: node.ID, Version: q.Get("version"), Spec: spec})
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{})
	return true
}

// NodeUpdates returns every node update the daemon accepted, in order.
func (f *Fake) NodeUpdates() []FakeNodeUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.nodeUpdates)
}

// BumpNode makes the node look changed by somebody else: its version goes up by one without an
// update, so the next write that carries the old version is refused as out of sequence.
func (f *Fake) BumpNode(id string) bool {
	cfg := f.config()
	for _, n := range cfg.nodeList() {
		if n.ID != id {
			continue
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.nodeState == nil {
			f.nodeState = map[string]fakeNodeState{}
		}
		f.nodeState[id] = fakeNodeState{version: n.Version.Index + 1, spec: n.Spec}
		return true
	}
	return false
}

var _ = registerFakeHandler((*Fake).serveNodeRemove)

// withoutNodes returns c without the nodes removed through the daemon. The slice is copied, so
// the Fake's own configuration is not touched.
func (c FakeConfig) withoutNodes(gone map[string]bool) FakeConfig {
	if len(gone) == 0 || len(c.NodeRows) == 0 {
		return c
	}
	c.NodeRows = slices.DeleteFunc(slices.Clone(c.NodeRows), func(n FakeNode) bool { return gone[n.ID] })
	return c
}

// serveNodeRemove answers DELETE /nodes/{id}. A node the swarm does not know is the 404 a real
// manager gives, and a node that is not down the 400 it gives (RESEARCH E14); otherwise the node
// leaves every list. The write is recorded whatever the answer, with its query, so a test can see
// that force is never sent.
func (f *Fake) serveNodeRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := nodePath.FindStringSubmatch(p)
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
	var node swarm.Node
	for _, n := range cfg.nodeList() {
		if n.ID == m[1] {
			node = n
		}
	}
	if node.ID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "node " + m[1] + " not found"})
		return true
	}
	if node.Status.State != swarm.NodeStateDown && r.URL.Query().Get("force") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "rpc error: code = FailedPrecondition desc = node " + node.ID + " is not down and can't be removed"})
		return true
	}
	f.mu.Lock()
	if f.nodeGone == nil {
		f.nodeGone = map[string]bool{}
	}
	f.nodeGone[node.ID] = true
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	return true
}
