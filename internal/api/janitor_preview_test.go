// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/janitor"
)

// The janitor preview (plan 11-01, JAN-08, D-14). Docker is a fake daemon behind each node of a
// swarm of two managers and a worker, reached through real agent sessions; the router, policy,
// aggregator and cluster reader are the real ones, and the planner is the production one.

type janitorReason struct {
	Code             string `json:"code"`
	Service          string `json:"service"`
	Slot             string `json:"slot"`
	Node             string `json:"node"`
	Rank             int    `json:"rank"`
	AgeSeconds       int64  `json:"ageSeconds"`
	RemainingSeconds int64  `json:"remainingSeconds"`
}

type janitorItem struct {
	Type      string        `json:"type"`
	NodeID    string        `json:"nodeId"`
	Node      string        `json:"node"`
	Ref       string        `json:"ref"`
	Name      string        `json:"name"`
	Class     string        `json:"class"`
	Reason    janitorReason `json:"reason"`
	SizeBytes *int64        `json:"sizeBytes"`
}

type janitorPreview struct {
	PreviewedAt  string `json:"previewedAt"`
	NodesChecked int    `json:"nodesChecked"`
	Summary      struct {
		Containers int    `json:"containers"`
		Networks   int    `json:"networks"`
		Images     int    `json:"images"`
		EstBytes   *int64 `json:"estBytes"`
	} `json:"summary"`
	Items            []janitorItem `json:"items"`
	NotRemoved       []janitorItem `json:"notRemoved"`
	WithheldNodes    []string      `json:"withheldNodes"`
	NetworksWithheld bool          `json:"networksWithheld"`
}

// janitorNow is the clock of the api fixtures (authFixture.clk). Every container of these tests
// is dated against it.
var janitorNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// taskBox describes one swarm task container of the fake worker.
type taskBox struct {
	id, service, serviceID, slot string
	state                        string
	// ageIdx orders the containers of a group: the larger, the newer.
	ageIdx int
	// finishedAgo is how long before janitorNow the container stopped.
	finishedAgo time.Duration
	size        int64
	keep        string
}

// fake makes the FakeContainer of a task box.
func (b taskBox) fake() dockertest.FakeContainer {
	taskID := "task-" + b.id
	labels := map[string]string{
		"com.docker.swarm.service.id":   b.serviceID,
		"com.docker.swarm.service.name": b.service,
		"com.docker.swarm.task.id":      taskID,
		"com.docker.swarm.task.name":    b.service + "." + b.slot + "." + taskID,
		"com.docker.swarm.node.id":      "node-wrk-1",
	}
	if b.keep != "" {
		labels["tierplan.keep"] = b.keep
	}
	created := janitorNow.Add(-72 * time.Hour).Add(time.Duration(b.ageIdx) * time.Hour)
	fc := dockertest.FakeContainer{
		ID: b.id, Names: []string{"/" + b.service + "." + b.slot + "." + taskID}, Image: "nginx:1", State: b.state,
		Created: created.Unix(), Labels: labels,
	}
	if b.state == "exited" {
		fc.Inspect = dockertest.FakeInspect{
			StartedAt:  created.Format(time.RFC3339Nano),
			FinishedAt: janitorNow.Add(-b.finishedAgo).Format(time.RFC3339Nano),
			SizeRw:     b.size,
		}
	}
	return fc
}

// janitorSwarm is the swarm of newSwarmFix with the given boxes on the worker, the web service
// listed by both managers, and the tasks the swarm wants running.
func janitorSwarm(t *testing.T, boxes []taskBox, wanted ...swarm.Task) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		switch {
		case host == "wrk-1":
			for _, b := range boxes {
				cfg.Containers = append(cfg.Containers, b.fake())
			}
		case strings.HasPrefix(host, "mgr-"):
			cfg.Tasks = append(cfg.Tasks, wanted...)
		}
	})
}

// exitedSlot makes n exited task containers of slot 1 of service web, finished two days ago, the
// first the oldest.
func exitedSlot(n int) []taskBox {
	var out []taskBox
	for i := range n {
		out = append(out, taskBox{id: fmt.Sprintf("web-x%d", i+1), service: "web", serviceID: "svc-web", slot: "1", state: "exited", ageIdx: i, finishedAgo: 48 * time.Hour})
	}
	return out
}

func wantedTask(id, containerID string) swarm.Task {
	return swarm.Task{
		ID: id, ServiceID: "svc-web", NodeID: "node-wrk-1", Slot: 1, DesiredState: swarm.TaskStateRunning,
		Status: swarm.TaskStatus{State: swarm.TaskStateRunning, ContainerStatus: &swarm.ContainerStatus{ContainerID: containerID}},
	}
}

func (s *swarmFix) previewJanitor(t *testing.T) (*envRec, janitorPreview) {
	t.Helper()
	rec := s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/janitor/preview", "")
	var got janitorPreview
	if rec.code == http.StatusOK {
		got = decode[janitorPreview](t, rec)
	}
	return rec, got
}

func TestJanitorPreviewTaskContainers(t *testing.T) {
	t.Parallel()
	boxes := append(exitedSlot(5), taskBox{id: "web-live", service: "web", serviceID: "svc-web", slot: "1", state: "running", ageIdx: 9})
	s := janitorSwarm(t, boxes, wantedTask("task-web-live", "web-live"))
	rec, got := s.previewJanitor(t)
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.code, rec.body)
	}
	if len(got.Items) != 3 || got.Summary.Containers != 3 || got.Summary.Networks != 0 || got.Summary.Images != 0 {
		t.Fatalf("items = %+v summary = %+v, want the 3 oldest exited containers of slot 1", got.Items, got.Summary)
	}
	for i, it := range got.Items {
		want := fmt.Sprintf("web-x%d", i+1)
		if it.Type != "container" || it.Ref != want || it.Node != "wrk-1" || it.Class != "removable" ||
			it.Reason.Code != "task_beyond_keep" || it.Reason.Slot != "1" || it.Reason.Service != "web" || it.Reason.Rank != i+1 {
			t.Errorf("item %d = %+v, want %s removable task_beyond_keep slot 1 rank %d", i, it, want, i+1)
		}
	}
	if len(got.NotRemoved) != 0 {
		t.Errorf("notRemoved = %+v, want none: the newest two are simply not the janitor's business", got.NotRemoved)
	}
	if got.NodesChecked != 3 || len(got.WithheldNodes) != 0 || got.NetworksWithheld {
		t.Errorf("nodesChecked/withheld = %d/%v/%v, want 3 and none", got.NodesChecked, got.WithheldNodes, got.NetworksWithheld)
	}
	if got.Summary.EstBytes != nil {
		t.Errorf("estBytes = %d, want null: no size is known", *got.Summary.EstBytes)
	}
}

func TestJanitorReasonEnumMatchesThePlanner(t *testing.T) {
	t.Parallel()
	field, ok := reflect.TypeOf(api.JanitorReasonBody{}).FieldByName("Code")
	if !ok {
		t.Fatal("JanitorReasonBody has no Code")
	}
	var want []string
	for _, c := range janitor.Reasons() {
		want = append(want, string(c))
	}
	if got := strings.Split(field.Tag.Get("enum"), ","); !slices.Equal(got, want) {
		t.Fatalf("the enum of the reason code is\n%v\nwant the planner's codes\n%v", got, want)
	}
}
