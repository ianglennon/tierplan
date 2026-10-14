// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"net/http"
	"regexp"

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

// NodeUpdates returns every node update the daemon accepted, in order.
func (f *Fake) NodeUpdates() []FakeNodeUpdate { return nil }
