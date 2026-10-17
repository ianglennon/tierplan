// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// A service's tasks, grouped by slot with the history Docker keeps (plan 06-07, RES-04, D-17,
// RESEARCH E11 to E13, Pitfall 12).

var tkT0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

type tkItem struct {
	ID           string `json:"id"`
	Slot         int    `json:"slot"`
	NodeID       string `json:"nodeId"`
	Node         string `json:"node"`
	NodeKnown    bool   `json:"nodeKnown"`
	TargetNodeID string `json:"targetNodeId"`
	State        string `json:"state"`
	DockerState  string `json:"dockerState"`
	DesiredState string `json:"desiredState"`
	Error        string `json:"error"`
	Message      string `json:"message"`
	ExitCode     *int   `json:"exitCode"`
	ContainerID  string `json:"containerId"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	StateSince   string `json:"stateSince"`
}

type tkSlot struct {
	Slot    int      `json:"slot"`
	Node    string   `json:"node"`
	Current tkItem   `json:"current"`
	History []tkItem `json:"history"`
}

type tkBody struct {
	Slots     []tkSlot `json:"slots"`
	Retention int      `json:"retention"`
	Total     int      `json:"total"`
	Global    bool     `json:"global"`
}

// tkTask builds a task that was created age before tkT0.
func tkTask(id, service, node string, slot int, state, desired string, age time.Duration) swarm.Task {
	t := swarm.Task{ID: id, ServiceID: service, NodeID: node, Slot: slot, DesiredState: swarm.TaskState(desired)}
	t.CreatedAt = tkT0.Add(-age)
	t.UpdatedAt = tkT0.Add(-age + time.Second)
	t.Status = swarm.TaskStatus{Timestamp: tkT0.Add(-age + 2*time.Second), State: swarm.TaskState(state), Message: state}
	return t
}

func tkRunning(id, service, node string, slot int, container string, age time.Duration) swarm.Task {
	t := tkTask(id, service, node, slot, "running", "running", age)
	t.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: container}
	return t
}

func tkExited(id, service, node string, slot int, state, desired string, exit int, errText string, age time.Duration) swarm.Task {
	t := tkTask(id, service, node, slot, state, desired, age)
	t.Status.Err = errText
	t.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c-" + id, ExitCode: exit}
	return t
}

// tkNodes are the nodes of the fixture swarm: the manager Bayplan's local socket is, a
// ready worker, a worker that is down and one that is disconnected.
func tkNodes() []dockertest.FakeNode {
	return []dockertest.FakeNode{
		{ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, EngineVersion: "29.1.0", OS: "linux"},
		{ID: "node-2", Hostname: "wrk-down", State: "down", EngineVersion: "29.1.0", OS: "linux"},
		{ID: "node-3", Hostname: "wrk-ready", EngineVersion: "29.1.0", OS: "linux"},
		{ID: "node-4", Hostname: "wrk-away", State: "disconnected", EngineVersion: "29.1.0", OS: "linux"},
	}
}

// tkEnv is a local environment whose socket is node-1 of a swarm of four nodes.
func tkEnv(t *testing.T, limit int, services []dockertest.FakeService, tasks ...swarm.Task) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(services...)
	cfg.SwarmNodeID, cfg.SwarmClusterID = "node-1", "cluster-1"
	cfg.NodeRows = tkNodes()
	cfg.Tasks = tasks
	cfg.HistoryLimit = limit
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

func tkGet(t *testing.T, f *envFix, e envBody, service string) tkBody {
	t.Helper()
	rec := f.get(t, e, "services/"+service+"/tasks")
	if rec.code != http.StatusOK {
		t.Fatalf("GET tasks of %s: %d %s", service, rec.code, rec.body)
	}
	return decode[tkBody](t, rec)
}

func tkSlotOf(t *testing.T, b tkBody, slot int) tkSlot {
	t.Helper()
	for _, s := range b.Slots {
		if s.Slot == slot {
			return s
		}
	}
	t.Fatalf("no slot %d in %+v", slot, b.Slots)
	return tkSlot{}
}

func TestServiceTasksGroupBySlotWithHistory(t *testing.T) {
	t.Parallel()
	web := webService()
	f, e := tkEnv(t, 7, []dockertest.FakeService{web},
		tkRunning("s1-now", "svc-web", "node-1", 1, "cont-1", time.Minute),
		tkExited("s1-failed", "svc-web", "node-1", 1, "failed", "shutdown", 1, "task: non-zero exit (1)", 5*time.Minute),
		tkExited("s1-rolled", "svc-web", "node-3", 1, "shutdown", "shutdown", 137, "", 20*time.Minute),
		tkRunning("s2-now", "svc-web", "node-3", 2, "cont-2", 2*time.Minute),
		tkRunning("other", "svc-other", "node-1", 1, "cont-x", time.Minute),
	)
	b := tkGet(t, f, e, "svc-web")
	if b.Global || b.Retention != 7 || b.Total != 4 || len(b.Slots) != 2 {
		t.Fatalf("body = %+v, want 2 slots, 4 tasks, retention 7, replicated (another service's task is not in it)", b)
	}
	if b.Slots[0].Slot != 1 || b.Slots[1].Slot != 2 {
		t.Fatalf("slots are %d, %d; want 1, 2 (a replicated service by slot number)", b.Slots[0].Slot, b.Slots[1].Slot)
	}
	s1 := b.Slots[0]
	if s1.Current.ID != "s1-now" || s1.Node != "mgr-1" || s1.Current.State != "running" || s1.Current.ContainerID != "cont-1" {
		t.Errorf("slot 1 current = %+v on %q", s1.Current, s1.Node)
	}
	if len(s1.History) != 2 || s1.History[0].ID != "s1-failed" || s1.History[1].ID != "s1-rolled" {
		t.Fatalf("slot 1 history = %+v, want the failed then the rolled task, newest first", s1.History)
	}
	failed := s1.History[0]
	if failed.State != "failed" || failed.DesiredState != "shutdown" || failed.Error != "task: non-zero exit (1)" || failed.ExitCode == nil || *failed.ExitCode != 1 {
		t.Errorf("failed task = %+v", failed)
	}
	if rolled := s1.History[1]; rolled.ExitCode == nil || *rolled.ExitCode != 137 || rolled.Node != "wrk-ready" {
		t.Errorf("rolled task = %+v, want exit 137 on wrk-ready", rolled)
	}
	if s1.Current.CreatedAt != "2026-10-14T08:59:00Z" || s1.Current.StateSince != "2026-10-14T08:59:02Z" || s1.Current.UpdatedAt != "2026-10-14T08:59:01Z" {
		t.Errorf("times = %q %q %q, want RFC 3339 UTC", s1.Current.CreatedAt, s1.Current.UpdatedAt, s1.Current.StateSince)
	}
	// A running task has no exit code to show.
	if s1.Current.ExitCode != nil {
		t.Errorf("a running task has exit code %d, want null", *s1.Current.ExitCode)
	}
	// The local socket is node-1, so its tasks can link to the environment's own node ID; a
	// node nothing is connected to cannot be opened.
	if s1.Current.TargetNodeID != e.ID {
		t.Errorf("targetNodeId = %q, want the environment ID %q for the local socket's node", s1.Current.TargetNodeID, e.ID)
	}
	if s2 := b.Slots[1]; s2.Current.TargetNodeID != "" || !s2.Current.NodeKnown || s2.Current.Node != "wrk-ready" {
		t.Errorf("slot 2 = %+v, want a known node nothing reaches (no targetNodeId)", s2.Current)
	}
}

func TestServiceTasksWithNoHistoryHaveAnEmptyHistoryList(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 0, []dockertest.FakeService{webService()},
		tkRunning("only", "svc-web", "node-1", 1, "cont-1", time.Minute))
	rec := f.get(t, e, "services/svc-web/tasks")
	if rec.code != http.StatusOK || !strings.Contains(string(rec.body), `"history":[]`) {
		t.Fatalf("answer = %d %s, want history as an empty list, never null", rec.code, rec.body)
	}
	if b := decode[tkBody](t, rec); b.Retention != 5 {
		t.Errorf("retention = %d, want 5 when the cluster sets none", b.Retention)
	}
}

func TestServiceTasksOfAServiceScaledToZeroHaveNoSlots(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, []dockertest.FakeService{webService()})
	rec := f.get(t, e, "services/svc-web/tasks")
	if rec.code != http.StatusOK || !strings.Contains(string(rec.body), `"slots":[]`) {
		t.Fatalf("answer = %d %s, want slots as an empty list", rec.code, rec.body)
	}
	if b := decode[tkBody](t, rec); b.Total != 0 {
		t.Errorf("total = %d", b.Total)
	}
}

func TestServiceTasksOnADownNodeAreUnknown(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, []dockertest.FakeService{webService()},
		tkRunning("on-down", "svc-web", "node-2", 1, "cont-1", time.Minute),
		tkTask("on-away", "svc-web", "node-4", 2, "assigned", "running", time.Minute),
		tkExited("done-on-down", "svc-web", "node-2", 3, "complete", "shutdown", 0, "", time.Minute),
		tkRunning("healthy", "svc-web", "node-3", 4, "cont-4", time.Minute),
	)
	b := tkGet(t, f, e, "svc-web")
	for slot, want := range map[int]string{1: "unknown", 2: "unknown", 3: "complete", 4: "running"} {
		if got := tkSlotOf(t, b, slot).Current.State; got != want {
			t.Errorf("slot %d state = %q, want %q", slot, got, want)
		}
	}
	down := tkSlotOf(t, b, 1).Current
	if down.DockerState != "running" || down.Node != "wrk-down" || !down.NodeKnown {
		t.Errorf("task on the down node = %+v, want Docker's own word kept in dockerState", down)
	}
}

func TestServiceTasksOfARemovedNode(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, []dockertest.FakeService{webService()},
		tkRunning("on-gone", "svc-web", "node-gone", 1, "cont-1", time.Minute),
		tkTask("unplaced", "svc-web", "", 2, "pending", "running", time.Minute),
	)
	b := tkGet(t, f, e, "svc-web")
	gone := tkSlotOf(t, b, 1)
	if gone.Current.NodeKnown || gone.Current.NodeID != "node-gone" || gone.Current.Node != "" || gone.Node != "" {
		t.Errorf("task of a removed node = %+v on %q, want nodeKnown false, the swarm's ID kept and no name", gone.Current, gone.Node)
	}
	if gone.Current.State != "unknown" || gone.Current.TargetNodeID != "" {
		t.Errorf("state %q target %q; a removed node's running task is unknown and cannot be opened", gone.Current.State, gone.Current.TargetNodeID)
	}
	un := tkSlotOf(t, b, 2).Current
	if un.NodeID != "" || un.Node != "" || !un.NodeKnown || un.State != "pending" {
		t.Errorf("task with no node = %+v, want no node, nothing missing and its own state", un)
	}
}

func TestServiceTasksOfAGlobalServiceAreOneRowPerNodeSortedByNodeName(t *testing.T) {
	t.Parallel()
	agentish := dockertest.FakeService{ID: "svc-glob", Name: "node-exporter", Mode: "global", Running: 2, Desired: 2}
	f, e := tkEnv(t, 5, []dockertest.FakeService{agentish},
		tkRunning("g-ready", "svc-glob", "node-3", 0, "c3", time.Minute),
		tkRunning("g-mgr", "svc-glob", "node-1", 0, "c1", time.Minute),
		tkExited("g-mgr-old", "svc-glob", "node-1", 0, "failed", "shutdown", 2, "boom", 10*time.Minute),
	)
	b := tkGet(t, f, e, "node-exporter")
	if !b.Global || len(b.Slots) != 2 || b.Total != 3 {
		t.Fatalf("body = %+v, want global with two nodes and three tasks", b)
	}
	if b.Slots[0].Node != "mgr-1" || b.Slots[1].Node != "wrk-ready" {
		t.Errorf("rows are %q, %q; want sorted by node name", b.Slots[0].Node, b.Slots[1].Node)
	}
	if first := b.Slots[0]; first.Current.ID != "g-mgr" || len(first.History) != 1 || first.History[0].ID != "g-mgr-old" {
		t.Errorf("mgr-1 row = %+v, want the live task with the failed one in its history", first)
	}
}

func TestServiceTasksCleanText(t *testing.T) {
	t.Parallel()
	bad := tkExited("bad", "svc-web", "node-1", 1, "failed", "shutdown", 1, "\x1b[31mboom\x00", 5*time.Minute)
	bad.Status.Message = "started\tok\nthen \xff\xfe died"
	long := tkExited("long", "svc-web", "node-1", 2, "failed", "shutdown", 1, strings.Repeat("é", 3000), 5*time.Minute)
	f, e := tkEnv(t, 5, []dockertest.FakeService{webService()}, bad, long)
	b := tkGet(t, f, e, "svc-web")

	got := tkSlotOf(t, b, 1).Current
	if got.Error != "[31mboom" {
		t.Errorf("error = %q, want the control characters gone", got.Error)
	}
	if want := "started ok then �� died"; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
	for _, s := range []string{got.Error, got.Message} {
		for _, r := range s {
			if r < ' ' || r == 0x7f {
				t.Errorf("%q still holds control character %U", s, r)
			}
		}
	}
	capped := tkSlotOf(t, b, 2).Current.Error
	if len(capped) > 2048 || len(capped) < 2046 || strings.ContainsRune(capped, '�') {
		t.Errorf("a long error is %d bytes with a replacement character %v, want at most 2048 cut on a character boundary", len(capped), strings.ContainsRune(capped, '�'))
	}
}

func TestServiceTasksOfAnUnknownServiceAreNotFound(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, []dockertest.FakeService{webService()})
	rec := f.get(t, e, "services/nope/tasks")
	if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Fatalf("answer = %d %s (%s), want 404 service_not_found", rec.code, rec.body, rec.ptyp)
	}
}

func TestServiceTasksOfAStandaloneHostIsNotASwarm(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "solo", Version: "29.1.0"})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "services/web/tasks")
	if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Fatalf("answer = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}

func TestServiceTasksNeedAGrantAndAreServedToAViewer(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, []dockertest.FakeService{webService()},
		tkRunning("t", "svc-web", "node-1", 1, "cont-1", time.Minute))
	get := f.asRole(t, e, "tk-viewer", "viewer")
	if rec := get("services/svc-web/tasks"); rec.Code != http.StatusOK {
		t.Fatalf("a Viewer's read = %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := get("services/svc-web/tasks").Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}
