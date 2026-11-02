// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/proto/testsupport"
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

// janitorSwarm is the swarm of newSwarmFix with the given boxes on each node (by hostname), the
// tasks the swarm wants running listed by both managers, and tune applied last.
func janitorSwarm(t *testing.T, boxes map[string][]taskBox, tune func(host string, cfg *dockertest.FakeConfig), wanted ...swarm.Task) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		for _, b := range boxes[host] {
			cfg.Containers = append(cfg.Containers, b.fake())
		}
		if strings.HasPrefix(host, "mgr-") {
			cfg.Tasks = append(cfg.Tasks, wanted...)
		}
		if tune != nil {
			tune(host, cfg)
		}
	})
}

// onWorker puts boxes on the worker, wrk-1.
func onWorker(boxes []taskBox) map[string][]taskBox { return map[string][]taskBox{"wrk-1": boxes} }

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
	s := janitorSwarm(t, onWorker(boxes), nil, wantedTask("task-web-live", "web-live"))
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

func TestJanitorPreviewOfflineNode(t *testing.T) {
	t.Parallel()
	boxes := map[string][]taskBox{"wrk-1": exitedSlot(5)}
	for i := range 4 {
		boxes["mgr-1"] = append(boxes["mgr-1"], taskBox{id: fmt.Sprintf("m-x%d", i+1), service: "web", serviceID: "svc-web", slot: "2", state: "exited", ageIdx: i, finishedAgo: 48 * time.Hour})
	}
	s := janitorSwarm(t, boxes, nil)
	s.srv.Hub.Kick(s.w.id, tunnel.DisconnectClosed)
	if _, err := s.srv.Hub.AwaitSession(testsupport.Context(t), s.w.id, func(sess *tunnel.Session) bool { return sess == nil }); err != nil {
		t.Fatalf("the worker's session never went: %v", err)
	}
	rec, got := s.previewJanitor(t)
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.code, rec.body)
	}
	if !slices.Equal(got.WithheldNodes, []string{"wrk-1"}) || got.NodesChecked != 2 {
		t.Errorf("withheld = %v checked = %d, want wrk-1 named and the two managers counted", got.WithheldNodes, got.NodesChecked)
	}
	var names []string
	for _, it := range got.Items {
		names = append(names, it.Ref+"@"+it.Node)
	}
	if !slices.Equal(names, []string{"m-x1@mgr-1", "m-x2@mgr-1"}) {
		t.Fatalf("items = %v, want only the managers' two, none of the missing worker's", names)
	}
	for _, it := range append(got.Items, got.NotRemoved...) {
		if it.Node == "wrk-1" {
			t.Errorf("%+v is from a node that did not report", it)
		}
	}
}

func TestJanitorPreviewEstimate(t *testing.T) {
	t.Parallel()
	boxes := exitedSlot(5)
	boxes[0].size, boxes[1].size = 1000, 234 // the third is not measured
	s := janitorSwarm(t, onWorker(boxes), nil)
	rec, got := s.previewJanitor(t)
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.code, rec.body)
	}
	if got.Summary.EstBytes == nil || *got.Summary.EstBytes != 1234 {
		t.Fatalf("estBytes = %v, want the sum of the sizes that are known, 1234", got.Summary.EstBytes)
	}
	if len(got.Items) != 3 || got.Items[0].SizeBytes == nil || *got.Items[0].SizeBytes != 1000 || got.Items[2].SizeBytes != nil {
		t.Fatalf("items = %+v, want sizes 1000, 234 and null", got.Items)
	}
	// A body whose sizes are all unknown says null, which JSON shows as null and not 0.
	none := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	rec = none.req(t, http.MethodPost, "/api/env/"+none.env.ID+"/janitor/preview", "")
	if !strings.Contains(string(rec.body), `"estBytes":null`) {
		t.Fatalf("body = %s, want estBytes null", rec.body)
	}
}

func TestJanitorPreviewIdempotentReadOnly(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(6)), nil)
	rec1, first := s.previewJanitor(t)
	rec2, second := s.previewJanitor(t)
	if rec1.code != http.StatusOK || rec2.code != http.StatusOK {
		t.Fatalf("previews: %d %d", rec1.code, rec2.code)
	}
	if len(first.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(first.Items))
	}
	first.PreviewedAt, second.PreviewedAt = "", ""
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two previews of an unchanged environment differ:\n%+v\n%+v", first, second)
	}
	for name, a := range map[string]*agentConn{"mgr-1": s.m1, "mgr-2": s.m2, "wrk-1": s.w} {
		if w := a.fake.Writes(); len(w) != 0 {
			t.Errorf("%s served writes %v: a preview only reads", name, w)
		}
	}
}

func TestJanitorPreviewIgnoresLock(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	holder := stackops.Holder{StackID: "stk-1", StackName: "shop", JobID: "job-1", Kind: "deploy", By: "ada", Started: janitorNow}
	release, _, ok := s.stackLocks.TryAcquire(s.env.ID, holder)
	if !ok {
		t.Fatal("the lock was not free")
	}
	defer release()
	rec, got := s.previewJanitor(t)
	if rec.code != http.StatusOK || len(got.Items) != 3 {
		t.Fatalf("preview under a deploy: %d %s, want 200 and the three items", rec.code, rec.body)
	}
	if held, ok := s.stackLocks.Held(s.env.ID); !ok || held != holder {
		t.Fatalf("held = %+v %v, want the deploy still holding the lock", held, ok)
	}
}

func TestJanitorPreviewAccess(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	path := "/api/env/" + s.env.ID + "/janitor/preview"
	as := func(user, role string) *httptest.ResponseRecorder {
		s.addUser(t, user, false)
		if role != "" {
			s.addEnvRole(t, user, s.env.ID, role)
		}
		cookie, csrf := s.signIn(t, user, false)
		return s.call(t, authReq{method: http.MethodPost, path: path, cookie: cookie, csrf: csrf})
	}
	if rec := as("vera", "viewer"); rec.Code != http.StatusOK {
		t.Fatalf("a viewer: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := as("nobody", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a user with no access: %d %s, want 404 as every environment route answers", rec.Code, rec.Body)
	}
	if rec := s.call(t, authReq{method: http.MethodPost, path: path}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d, want 401", rec.Code)
	}
}

func TestJanitorPreviewOrder(t *testing.T) {
	t.Parallel()
	boxes := map[string][]taskBox{"wrk-1": exitedSlot(4), "mgr-2": nil}
	// Slot 2 sorts after slot 1 by name; the manager sorts before the worker by node.
	for i := range 3 {
		boxes["wrk-1"] = append(boxes["wrk-1"], taskBox{id: fmt.Sprintf("web-y%d", i+1), service: "web", serviceID: "svc-web", slot: "2", state: "exited", ageIdx: i, finishedAgo: 48 * time.Hour})
		boxes["mgr-2"] = append(boxes["mgr-2"], taskBox{id: fmt.Sprintf("api-z%d", i+1), service: "api", serviceID: "svc-api", slot: "1", state: "exited", ageIdx: i, finishedAgo: 48 * time.Hour})
	}
	s := janitorSwarm(t, boxes, func(host string, cfg *dockertest.FakeConfig) {
		if strings.HasPrefix(host, "mgr-") {
			cfg.ServiceRows = append(cfg.ServiceRows, dockertest.FakeService{ID: "svc-api", Name: "api", Mode: "replicated", Running: 1, Desired: 1})
		}
	})
	rec, got := s.previewJanitor(t)
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.code, rec.body)
	}
	var order []string
	for _, it := range got.Items {
		order = append(order, it.Node+"/"+it.Ref)
	}
	want := []string{"mgr-2/api-z1", "wrk-1/web-x1", "wrk-1/web-x2", "wrk-1/web-y1"}
	if !slices.Equal(order, want) {
		t.Fatalf("items = %v, want %v (type, node, name, ID)", order, want)
	}
	again, _ := s.previewJanitor(t)
	if !reflect.DeepEqual(decode[janitorPreview](t, again).Items, got.Items) {
		t.Fatalf("the order changed between two previews")
	}
}
