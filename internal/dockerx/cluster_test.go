// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// attached is a swarm task attached to the networks by ID.
func attached(id, service, node string, desired, state swarm.TaskState, networks ...string) swarm.Task {
	t := swarm.Task{ID: id, ServiceID: service, NodeID: node, DesiredState: desired}
	t.Status.State = state
	for _, n := range networks {
		t.NetworksAttachments = append(t.NetworksAttachments, swarm.NetworkAttachment{Network: swarm.Network{ID: n}})
	}
	return t
}

// TestTaskNetworkRefsNonTerminal: a task that is still alive holds its attachments, whatever its
// service's spec says now; a task the swarm has finished with holds none. The networks come from
// the attachments and from the spec's targets, a name turned into the ID.
func TestTaskNetworkRefsNonTerminal(t *testing.T) {
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Networks: []dockertest.FakeNetwork{
			{ID: "ovl-id", Name: "ovl", Driver: "overlay", Scope: "swarm"},
			{ID: "d-id", Name: "d", Driver: "overlay", Scope: "swarm"},
		},
		ServiceRows: []dockertest.FakeService{{ID: "s1", Name: "web", Mode: "replicated", Running: 1, Desired: 1}},
		Tasks: []swarm.Task{
			// Dropped from the spec by an update, but still running and still attached.
			attached("alive", "s1", "node-2", swarm.TaskStateShutdown, swarm.TaskStateRunning, "d-id"),
			attached("starting", "s1", "node-1", swarm.TaskStateRunning, swarm.TaskStatePreparing, "ovl-id", "ovl-id"),
			attached("done", "s1", "node-1", swarm.TaskStateShutdown, swarm.TaskStateShutdown, "gone-id"),
			attached("failed", "s1", "node-1", swarm.TaskStateShutdown, swarm.TaskStateFailed, "gone-id"),
			attached("rejected", "s1", "node-1", swarm.TaskStateShutdown, swarm.TaskStateRejected, "gone-id"),
		},
	}
	byName := attached("byname", "s1", "node-3", swarm.TaskStateRunning, swarm.TaskStateRunning)
	byName.Spec.Networks = []swarm.NetworkAttachmentConfig{{Target: "ovl"}, {Target: "d-id"}}
	cfg.Tasks = append(cfg.Tasks, byName)
	fake := dockertest.NewFake(t, cfg)

	got, err := newClient(t, fake).TaskNetworkRefs(context.Background())
	if err != nil {
		t.Fatalf("TaskNetworkRefs: %v", err)
	}
	by := map[string]dockerx.TaskNetworkRef{}
	for _, r := range got {
		by[r.ID] = r
	}
	if len(by) != 3 {
		t.Fatalf("refs = %+v, want the three tasks that are alive and none of those the swarm finished with", got)
	}
	if a := by["alive"]; a.ServiceName != "web" || a.NodeID != "node-2" || a.State != "running" || !slices.Equal(a.Networks, []string{"d-id"}) {
		t.Errorf("alive = %+v", a)
	}
	if s := by["starting"]; s.State != "preparing" || !slices.Equal(s.Networks, []string{"ovl-id"}) {
		t.Errorf("starting = %+v, want its network once", s)
	}
	if b := by["byname"]; !slices.Equal(b.Networks, []string{"ovl-id", "d-id"}) {
		t.Errorf("byname = %+v, want the spec's name turned into the ID, then the ID", b)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Fatalf("a read wrote: %v", w)
	}
}

// A node list is believed only when it names the manager that answered it (T-02-68): the reaper
// and the enrolment check both ask this one function.
func TestListsAnswererNeedsTheAnsweringNodeInTheList(t *testing.T) {
	read := dockerx.ClusterRead{Nodes: []dockerx.NodeRow{{ID: "node-m1"}, {ID: "node-w1"}}}
	for _, tc := range []struct {
		name     string
		read     dockerx.ClusterRead
		answerer string
		want     bool
	}{
		{"the answerer is listed", read, "node-m1", true},
		{"the answerer is not listed", read, "node-m2", false},
		{"no answerer", read, "", false},
		{"a list without nodes", dockerx.ClusterRead{}, "node-m1", false},
	} {
		if got := tc.read.ListsAnswerer(tc.answerer); got != tc.want {
			t.Errorf("%s: ListsAnswerer(%q) = %v, want %v", tc.name, tc.answerer, got, tc.want)
		}
	}
}
