// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// The typed task read (plan 06-07, RES-04, D-17).

var taskT0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func fakeTask(id, service, node string, slot int, state, desired string, age time.Duration) swarm.Task {
	t := swarm.Task{ID: id, ServiceID: service, NodeID: node, Slot: slot, DesiredState: swarm.TaskState(desired)}
	t.CreatedAt = taskT0.Add(-age)
	t.UpdatedAt = taskT0.Add(-age + time.Second)
	t.Status = swarm.TaskStatus{Timestamp: taskT0.Add(-age + 2*time.Second), State: swarm.TaskState(state), Message: state}
	return t
}

func taskFake(t *testing.T, tasks ...swarm.Task) *dockerx.Client {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1",
		NodeRows:    []dockertest.FakeNode{{ID: "node-a", Hostname: "alpha", Role: "manager"}, {ID: "node-b", Hostname: "beta"}},
		ServiceRows: []dockertest.FakeService{{ID: "web-id", Name: "web", Mode: "replicated"}, {ID: "db-id", Name: "db", Mode: "replicated"}},
		Tasks:       tasks,
	})
	return newClient(t, fake)
}

func TestTasksOfAServiceComeBackTypedNewestFirst(t *testing.T) {
	failed := fakeTask("t-old", "web-id", "node-a", 1, "failed", "shutdown", 10*time.Minute)
	failed.Status.Err = "task: non-zero exit (1)"
	failed.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c-old", ExitCode: 1}
	live := fakeTask("t-new", "web-id", "node-b", 1, "running", "running", time.Minute)
	live.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c-new"}
	pending := fakeTask("t-pend", "web-id", "", 2, "pending", "running", 30*time.Second)
	other := fakeTask("t-db", "db-id", "node-a", 1, "running", "running", time.Minute)
	c := taskFake(t, failed, live, other, pending)

	rows, err := c.Tasks(context.Background(), dockerx.TaskFilter{ServiceIDs: []string{"web-id"}})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if want := []string{"t-pend", "t-new", "t-old"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v (the service's own tasks, newest first)", ids, want)
	}
	byID := map[string]dockerx.TaskRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	old := byID["t-old"]
	if old.ServiceID != "web-id" || old.NodeID != "node-a" || old.Slot != 1 || old.State != "failed" || old.DesiredState != "shutdown" ||
		old.Err != "task: non-zero exit (1)" || old.ContainerID != "c-old" || old.ExitCode == nil || *old.ExitCode != 1 {
		t.Errorf("failed task = %+v", old)
	}
	if !old.CreatedAt.Equal(taskT0.Add(-10*time.Minute)) || !old.UpdatedAt.Equal(taskT0.Add(-10*time.Minute+time.Second)) || !old.StateSince.Equal(taskT0.Add(-10*time.Minute+2*time.Second)) {
		t.Errorf("times = %v %v %v", old.CreatedAt, old.UpdatedAt, old.StateSince)
	}
	if old.CreatedAt.Location() != time.UTC {
		t.Errorf("created at is not UTC: %v", old.CreatedAt.Location())
	}
	if p := byID["t-pend"]; p.ExitCode != nil || p.ContainerID != "" || p.NodeID != "" {
		t.Errorf("a task with no container status = %+v, want a nil exit code and no container", p)
	}
	if n := byID["t-new"]; n.ExitCode == nil || *n.ExitCode != 0 || n.ContainerID != "c-new" {
		t.Errorf("a running task = %+v, want container c-new and an exit code of 0", n)
	}
}

func TestTasksCanBeNarrowedByNodeAndDesiredState(t *testing.T) {
	c := taskFake(t,
		fakeTask("t1", "web-id", "node-a", 1, "failed", "shutdown", 3*time.Minute),
		fakeTask("t2", "web-id", "node-a", 1, "running", "running", time.Minute),
		fakeTask("t3", "web-id", "node-b", 2, "running", "running", time.Minute),
	)
	rows, err := c.Tasks(context.Background(), dockerx.TaskFilter{ServiceIDs: []string{"web-id"}, DesiredState: "shutdown"})
	if err != nil || len(rows) != 1 || rows[0].ID != "t1" {
		t.Fatalf("desired-state shutdown = %+v, %v; want t1 only", rows, err)
	}
	rows, err = c.Tasks(context.Background(), dockerx.TaskFilter{NodeIDs: []string{"node-b"}})
	if err != nil || len(rows) != 1 || rows[0].ID != "t3" {
		t.Fatalf("node-b = %+v, %v; want t3 only", rows, err)
	}
	rows, err = c.Tasks(context.Background(), dockerx.TaskFilter{IDs: []string{"t2"}})
	if err != nil || len(rows) != 1 || rows[0].ID != "t2" {
		t.Fatalf("id t2 = %+v, %v; want t2 only", rows, err)
	}
}

func TestTasksRefusesAFilterThatNamesNothingWithoutCallingTheEngine(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{SwarmState: "active", SwarmControl: true, Tasks: []swarm.Task{fakeTask("t1", "web-id", "node-a", 1, "running", "running", time.Minute)}})
	c := newClient(t, fake)
	before := len(fake.Requests())
	for _, f := range []dockerx.TaskFilter{{}, {ServiceIDs: []string{}}, {ServiceIDs: []string{""}}, {NodeIDs: []string{" "}}} {
		rows, err := c.Tasks(context.Background(), f)
		if !errors.Is(err, dockerx.ErrUnfilteredTasks) || rows != nil {
			t.Errorf("Tasks(%+v) = %v, %v; want ErrUnfilteredTasks", f, rows, err)
		}
	}
	if after := len(fake.Requests()); after != before {
		t.Errorf("a refused read still sent %d request(s)", after-before)
	}
}

func TestTaskHistoryLimitIsReadFromTheClusterSpec(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1", HistoryLimit: 12})
	got, err := newClient(t, fake).TaskHistoryLimit(context.Background())
	if err != nil || got != 12 {
		t.Fatalf("TaskHistoryLimit = %d, %v; want 12", got, err)
	}
}

func TestTaskHistoryLimitIsFiveWhenTheClusterSetsNone(t *testing.T) {
	for name, cfg := range map[string]dockertest.FakeConfig{
		"a cluster that never set it": {SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1"},
		"an engine with no cluster":   {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := newClient(t, dockertest.NewFake(t, cfg)).TaskHistoryLimit(context.Background())
			if err != nil || got != 5 {
				t.Fatalf("TaskHistoryLimit = %d, %v; want 5", got, err)
			}
		})
	}
}

// The hash of a task's spec against the hash of its service's template (plan 06-09, RES-02,
// RESEARCH E10): a task is on the new version when the two are equal.

func specTask(id, image, runtime string) swarm.Task {
	t := fakeTask(id, "web-id", "node-a", 1, "running", "running", time.Minute)
	t.Spec = swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: image, Env: []string{"A=1", "B=2"}}, Runtime: swarm.RuntimeType(runtime)}
	return t
}

func TestTaskSpecHashIgnoresRuntimeAndMatchesTheTemplateOfTheSameSpec(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1",
		NodeRows: []dockertest.FakeNode{{ID: "node-a", Hostname: "alpha", Role: "manager"}},
		ServiceRows: []dockertest.FakeService{{
			ID: "web-id", Name: "web", Mode: "replicated",
			Spec: &swarm.ServiceSpec{
				Annotations: swarm.Annotations{Name: "web"},
				TaskTemplate: swarm.TaskSpec{
					ContainerSpec: &swarm.ContainerSpec{Image: "img:2", Env: []string{"A=1", "B=2"}},
				},
			},
		}},
		Tasks: []swarm.Task{specTask("t-new", "img:2", "container"), specTask("t-new-bare", "img:2", ""), specTask("t-old", "img:1", "container")},
	})
	c := newClient(t, fake)

	detail, _, err := c.ServiceInspect(context.Background(), "web-id")
	if err != nil {
		t.Fatalf("ServiceInspect: %v", err)
	}
	if detail.TemplateHash == "" {
		t.Fatal("the service has no TemplateHash")
	}
	rows, err := c.Tasks(context.Background(), dockerx.TaskFilter{ServiceIDs: []string{"web-id"}})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	hash := map[string]string{}
	for _, r := range rows {
		hash[r.ID] = r.SpecHash
	}
	if hash["t-new"] == "" || hash["t-new"] != hash["t-new-bare"] {
		t.Errorf("Runtime changed the hash: %q vs %q", hash["t-new"], hash["t-new-bare"])
	}
	if hash["t-new"] != detail.TemplateHash {
		t.Errorf("a task on the new spec hashes to %q, the template to %q", hash["t-new"], detail.TemplateHash)
	}
	if hash["t-old"] == detail.TemplateHash {
		t.Errorf("a task on the old spec hashes like the template: %q", hash["t-old"])
	}
}
