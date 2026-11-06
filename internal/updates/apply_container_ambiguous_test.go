// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// A container is named, and Docker names are unique on a host, not across the nodes of a swarm. An update that names a container
// which two nodes hold is refused instead of taking the first of them (WR-07). The rig has one engine, so the second node is a
// snapshot that holds a twin of the named containers on a node of its own.

// twinSnap is the rig's aggregator with a twin of each named container added on a second node.
type twinSnap struct {
	inner updates.Snapshotter
	names map[string]bool
	// labels are put on the twins, so a twin can be a task container.
	labels map[string]string
}

func (s twinSnap) Snapshot(ctx context.Context, envID string) (envs.Aggregate, envs.Health, error) {
	agg, h, err := s.inner.Snapshot(ctx, envID)
	if err != nil {
		return agg, h, err
	}
	rows := append([]envs.ContainerRow{}, agg.Containers...)
	for _, c := range agg.Containers {
		if !s.names[strings.TrimPrefix(c.Name, "/")] {
			continue
		}
		twin := c
		twin.ID = strings.Repeat("e", 64)
		twin.Node, twin.NodeID = "worker-2", "node-2"
		if s.labels != nil {
			twin.Labels = s.labels
		}
		rows = append(rows, twin)
	}
	agg.Containers = rows
	return agg, h, nil
}

// withTwins makes the rig's environment hold a second container of each name on another node.
func (r *applyRig) withTwins(names ...string) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	r.applier.Service.Aggregator = twinSnap{inner: r.applier.Service.Aggregator, names: set}
}

func wantAmbiguous(t *testing.T, what string, err error) {
	t.Helper()
	var nu *updates.NotUpdatableError
	if !errors.As(err, &nu) || nu.Code != updates.NotUpdatableAmbiguousName || nu.Name != "app" {
		t.Errorf("%s = %v, want not updatable: %s, naming app", what, err, updates.NotUpdatableAmbiguousName)
	}
}

// soloContainer is a second container of the rig, a plain one under a name no other node holds.
func soloContainer() dockertest.FakeContainer {
	c := appContainer("running")
	c.ID = strings.Repeat("d", 64)
	c.Names = []string{"/solo"}
	return c
}

// A name two nodes hold is refused whatever reaches it: the update, its preview and the lookup the API makes first. Nothing is
// written to any node, no lock is taken and no run is recorded.
func TestContainerNameOnTwoNodesIsRefused(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("running"))
	r.offer("app", dg('b'))
	r.withTwins("app")

	_, err := r.applier.Start(testsupport.Context(t), r.containerRequest("app", ""))
	wantAmbiguous(t, "Start", err)
	_, err = r.applier.FindContainer(testsupport.Context(t), r.envID, "app")
	wantAmbiguous(t, "FindContainer", err)

	p, err := r.applier.Preview(testsupport.Context(t), r.envID, "container", "app")
	if err != nil || p.Offered || p.Refusal != updates.NotUpdatableAmbiguousName || len(p.Items) != 0 {
		t.Errorf("Preview = %+v %v, want no update offered, refused: %s", p, err, updates.NotUpdatableAmbiguousName)
	}

	if got := writeKinds(r.fake.Writes()); len(got) != 0 {
		t.Errorf("the engine was asked %v, want nothing", got)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after the refusal")
	}
	if runs, err := r.runs.Running(context.Background(), r.envID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v, want none", runs, err)
	}
}

// A name that is on one node only is updated as before, whatever the other nodes hold under other names.
func TestContainerNameOnOneNodeIsUpdated(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("running"), soloContainer())
	r.offer("app", dg('b'))
	r.withTwins("solo")

	job := r.run(t, r.containerRequest("app", ""))
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s, want done", job.State)
	}
	p, err := r.applier.Preview(testsupport.Context(t), r.envID, "container", "app")
	if err != nil || p.Refusal == updates.NotUpdatableAmbiguousName {
		t.Errorf("Preview = %+v %v, want no ambiguity for a name one node holds", p, err)
	}
}

// Task containers of a swarm service are not what the refusal is for: they are refused on their own account, however many nodes
// hold the name.
func TestTaskContainerNamesAreNotAmbiguous(t *testing.T) {
	t.Parallel()
	task := appContainer("running")
	task.Labels = map[string]string{"com.docker.swarm.service.id": "svc1"}
	r := containerRig(t, nil, task)
	r.offer("app", dg('b'))
	r.applier.Service.Aggregator = twinSnap{inner: r.applier.Service.Aggregator, names: map[string]bool{"app": true}, labels: task.Labels}

	_, err := r.applier.Start(testsupport.Context(t), r.containerRequest("app", ""))
	var nu *updates.NotUpdatableError
	if !errors.As(err, &nu) || nu.Code != updates.NotUpdatableTaskContainer {
		t.Errorf("Start = %v, want not updatable: %s", err, updates.NotUpdatableTaskContainer)
	}
}

// In a bulk update the item with an ambiguous name is refused, nothing is written for it, and the other items go on.
func TestBulkRefusesAContainerNameOnTwoNodes(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("running"), soloContainer())
	r.offer("app", dg('b'))
	r.withTwins("app")

	req := r.bulkRequest("", "app", "solo")
	for i := range req.Items {
		req.Items[i].Kind = "container"
	}
	job, skipped := r.runBulk(t, req)
	if len(skipped) != 1 || skipped[0].Name != "app" || skipped[0].Reason != updates.BulkNotUpdatable || skipped[0].Code != updates.NotUpdatableAmbiguousName {
		t.Errorf("skipped = %+v, want app refused: %s", skipped, updates.NotUpdatableAmbiguousName)
	}
	if job.State != jobs.JobDone {
		t.Errorf("job = %s, want done: solo is updated", job.State)
	}
	for _, w := range r.fake.Writes() {
		if strings.Contains(w, appID) {
			t.Errorf("the engine was written for the ambiguous container: %s", w)
		}
	}
	if got := r.containersNow(t); !strings.HasSuffix(got["app"], appID) || !strings.HasPrefix(got["app"], "running") {
		t.Errorf("app is %q, want it untouched and running", got["app"])
	}
	if len(writeKinds(r.fake.Writes())) == 0 {
		t.Errorf("solo was not updated: the engine was asked for nothing")
	}
}
