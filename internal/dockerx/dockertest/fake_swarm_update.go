// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

	"github.com/moby/moby/api/types/swarm"
)

var _ = registerFakeHandler((*Fake).serveSwarmUpdate)

// FakeSwarmUpdate is one swarm update the fake daemon accepted: the version and the whole query
// string the request carried, and the spec its body held.
type FakeSwarmUpdate struct {
	Version string
	// RawQuery is the request's query as it arrived, so a test sees every key the client sent
	// (a rotate flag, for one).
	RawQuery string
	Spec     swarm.Spec
}

// fakeSwarmState is what the accepted swarm updates left the swarm with: its version and its task
// history limit.
type fakeSwarmState struct {
	version uint64
	limit   *int64
}

// withSwarm returns c reporting the version and the limit the accepted swarm updates left. The
// pointer is shared with the Fake's state, which is only ever replaced, never changed.
func (c FakeConfig) withSwarm(st *fakeSwarmState) FakeConfig {
	if st == nil {
		return c
	}
	c.SwarmVersion = st.version
	c.swarmLimit = st.limit
	return c
}

// swarmIndex is the version the swarm has now.
func (c FakeConfig) swarmIndex() uint64 { return max(c.SwarmVersion, 1) }

// serveSwarmUpdate answers POST /swarm/update. A worker, a swarm without quorum and a refusing
// agent answer as they do for every other cluster write. A version that is not the swarm's own is
// the 500 "update out of sequence". Otherwise the body's task history limit becomes the swarm's,
// its version goes up by one and the update is kept for SwarmUpdates. The write is recorded
// whatever the answer.
func (f *Fake) serveSwarmUpdate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/swarm/update" {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	cfg = f.config()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny || cfg.SwarmUpdateDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if clusterRefused(w, cfg) {
		return true
	}
	var spec swarm.Spec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.swarmVersionLocked()
	if cfg.SwarmChangedUnderUpdate {
		// Somebody else changed the swarm after the client read its version.
		now++
		f.swarmState = &fakeSwarmState{version: now, limit: f.swarmLimitLocked()}
	}
	q := r.URL.Query()
	if q.Get("version") != strconv.FormatUint(now, 10) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": outOfSequenceRefusal})
		return true
	}
	f.swarmState = &fakeSwarmState{version: now + 1, limit: spec.Orchestration.TaskHistoryRetentionLimit}
	f.swarmUpdates = append(f.swarmUpdates, FakeSwarmUpdate{Version: q.Get("version"), RawQuery: r.URL.RawQuery, Spec: spec})
	writeJSON(w, http.StatusOK, map[string]string{})
	return true
}

// swarmVersionLocked is the swarm's version now; the caller holds f.mu.
func (f *Fake) swarmVersionLocked() uint64 {
	if f.swarmState != nil {
		return f.swarmState.version
	}
	return f.cfg.swarmIndex()
}

// swarmLimitLocked is the limit an accepted update left, nil when none did; the caller holds f.mu.
func (f *Fake) swarmLimitLocked() *int64 {
	if f.swarmState != nil {
		return f.swarmState.limit
	}
	return nil
}

// SwarmUpdates returns every swarm update the daemon accepted, in order.
func (f *Fake) SwarmUpdates() []FakeSwarmUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.swarmUpdates)
}
