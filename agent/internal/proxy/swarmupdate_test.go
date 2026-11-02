// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package proxy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
)

// engineSwarmSpec is the swarm spec the fake engine reports in /info: every setting that is not the
// task history limit has a value that is not the zero value, so a forwarded spec that dropped one
// of them differs from it.
func engineSwarmSpec(limit int64) swarm.Spec {
	keep := uint64(2)
	return swarm.Spec{
		Annotations: swarm.Annotations{Name: "default", Labels: map[string]string{"env": "prod"}},
		Orchestration: swarm.OrchestrationConfig{
			TaskHistoryRetentionLimit: &limit,
		},
		Raft: swarm.RaftConfig{
			SnapshotInterval: 10000, KeepOldSnapshots: &keep, LogEntriesForSlowFollowers: 500,
			ElectionTick: 10, HeartbeatTick: 1,
		},
		Dispatcher: swarm.DispatcherConfig{HeartbeatPeriod: 5 * time.Second},
		CAConfig:   swarm.CAConfig{NodeCertExpiry: 90 * 24 * time.Hour},
		TaskDefaults: swarm.TaskDefaults{
			LogDriver: &swarm.Driver{Name: "json-file", Options: map[string]string{"max-size": "10m"}},
		},
		EncryptionConfig: swarm.EncryptionConfig{AutoLockManagers: false},
	}
}

// infoAnswer is what the fake engine answers GET /info with: a swarm manager with the given spec.
func infoAnswer(t *testing.T, spec swarm.Spec, manager bool) string {
	t.Helper()
	info := struct{ Swarm swarm.Info }{Swarm: swarm.Info{
		NodeID: "n1", LocalNodeState: swarm.LocalNodeStateActive, ControlAvailable: manager,
		Cluster: &swarm.ClusterInfo{ID: "c1", Meta: swarm.Meta{Version: swarm.Version{Index: 42}}, Spec: spec},
	}}
	return enc(t, info)
}

// swarmUpdateRequest is a swarm update as the server's client sends it.
func swarmUpdateRequest(t *testing.T, query string, spec swarm.Spec) string {
	t.Helper()
	return postJSON("POST", "/swarm/update"+query, enc(t, spec))
}

// updatesSeen is the swarm updates the engine received, whatever else it was asked.
func updatesSeen(f *fakeSocket) []string {
	var out []string
	for _, r := range f.requests() {
		if strings.Contains(r, " /swarm") || strings.Contains(r, "/swarm/") {
			out = append(out, r)
		}
	}
	return out
}

// engineSpecWithLimit is the spec a server would send to set the limit: the engine's spec read from
// /info with the one field changed.
func engineSpecWithLimit(limit int64) swarm.Spec {
	spec := engineSwarmSpec(5)
	spec.Orchestration.TaskHistoryRetentionLimit = &limit
	return spec
}

func TestSwarmUpdateForwardsTheOneField(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/info": infoAnswer(t, engineSwarmSpec(5), true)}
	h := Handler(sock.path, Policy{}, nil)

	resp := do(t, h, swarmUpdateRequest(t, "?version=42", engineSpecWithLimit(10)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	updates := updatesSeen(sock)
	if len(updates) != 1 || updates[0] != "POST /swarm/update?version=42" {
		t.Fatalf("the engine saw the swarm requests %v, want exactly POST /swarm/update?version=42", updates)
	}
	// The engine was asked its own spec once, by the agent, and nothing else.
	if got := sock.requests(); len(got) != 2 || got[0] != "GET /info" {
		t.Fatalf("the engine saw %v, want GET /info then the update", got)
	}
	var got, want swarm.Spec
	bodies := sock.bodiesSeen()
	if err := json.Unmarshal(bodies[len(bodies)-1], &got); err != nil {
		t.Fatalf("the forwarded body does not decode: %v: %s", err, bodies[len(bodies)-1])
	}
	want = engineSwarmSpec(10)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the engine received %s, want its own spec with the limit 10", bodies[len(bodies)-1])
	}
	if ct := sock.headersSeen()[1].Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q, want application/json", ct)
	}
}
