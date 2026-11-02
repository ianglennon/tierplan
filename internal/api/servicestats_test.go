// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
)

// The service, list and node stats streams (plan 06-33, RES-11, D-29, D-31). Docker is a
// FakeDaemon that is a swarm manager, with scripted stats behind the real monitor,
// aggregator, cluster reader and router; the samplers' ticker is the test's, so nothing
// here waits for a clock. Node node-3 has no agent: it is the worker whose agent is gone.

const (
	swWeb1 = "cont-w1"
	swWeb2 = "cont-w2"
	swWeb3 = "cont-w3"
	swWeb4 = "cont-w4"
	swDB1  = "cont-d1"
)

type swFix struct {
	*envFix
	env   envBody
	ticks *manualTicks
}

func swDBService() dockertest.FakeService {
	return dockertest.FakeService{ID: "svc-db", Name: "db", Mode: "replicated", Running: 1, Desired: 1, Image: scaleImage, Version: 2}
}

// swStats are the scripted stats of the containers that can be reached: w1 is one busy core
// (100 percent over a two second tick) with 200 MiB, w2 half a core with 150 MiB, d1 a fifth.
func swStats() map[string]dockertest.FakeStats {
	return map[string]dockertest.FakeStats{
		swWeb1: {
			OnlineCPUs: 4, CPUPerCall: 500_000_000, SystemPerCall: 2_000_000_000, MemUsage: 300 << 20, InactiveFile: 100 << 20, MemLimit: 64 << 30,
			NetRxPerCall: 2000, NetTxPerCall: 4000, BlkReadPerCall: 1000,
		},
		swWeb2: {
			OnlineCPUs: 4, CPUPerCall: 250_000_000, SystemPerCall: 2_000_000_000, MemUsage: 200 << 20, InactiveFile: 50 << 20, MemLimit: 64 << 30,
			NetRxPerCall: 4000, NetTxPerCall: 2000, BlkReadPerCall: 3000,
		},
		swWeb4: {
			OnlineCPUs: 4, CPUPerCall: 100_000_000, SystemPerCall: 2_000_000_000, MemUsage: 64 << 20, MemLimit: 64 << 30,
		},
		swDB1: {
			OnlineCPUs: 4, CPUPerCall: 100_000_000, SystemPerCall: 2_000_000_000, MemUsage: 100 << 20, MemLimit: 64 << 30,
			NetRxPerCall: 200, NetTxPerCall: 200,
		},
	}
}

// swTask is a running task of a service, in a slot, on a node, with a container and a
// memory limit (zero for none).
func swTask(id, service, node string, slot int, container string, limit int64) swarm.Task {
	t := tkRunning(id, service, node, slot, container, time.Minute)
	if limit > 0 {
		t.Spec.Resources = &swarm.ResourceRequirements{Limits: &swarm.Limit{MemoryBytes: limit}}
	}
	return t
}

// swTasks are the tasks of the fixture swarm: web has two tasks on the manager, one on the
// worker whose agent is gone and one that failed earlier; db has one on the manager.
func swTasks(limit int64) []swarm.Task {
	return []swarm.Task{
		swTask("w1", "svc-web", "node-1", 1, swWeb1, limit),
		swTask("w2", "svc-web", "node-1", 2, swWeb2, limit),
		swTask("w3", "svc-web", "node-3", 3, swWeb3, limit),
		tkExited("w0", "svc-web", "node-1", 1, "failed", "shutdown", 1, "task: non-zero exit (1)", 10*time.Minute),
		swTask("d1", "svc-db", "node-1", 1, swDB1, 0),
	}
}

func swConfig(tasks []swarm.Task) dockertest.FakeConfig {
	cfg := swarmManagerEngine(webService(), swDBService())
	cfg.SwarmNodeID, cfg.SwarmClusterID = "node-1", "cluster-1"
	cfg.NodeRows = []dockertest.FakeNode{
		{ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, EngineVersion: "29.1.0", OS: "linux", NanoCPUs: 4_000_000_000, MemoryBytes: 8 << 30},
		{ID: "node-3", Hostname: "wrk-ready", EngineVersion: "29.1.0", OS: "linux", NanoCPUs: 2_000_000_000, MemoryBytes: 4 << 30},
	}
	cfg.Tasks = tasks
	stats := swStats()
	for id := range stats {
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{
			ID: id, Names: []string{"/" + id}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours",
		})
	}
	cfg.Stats = stats
	return cfg
}

func newSwFix(t *testing.T, tasks []swarm.Task) *swFix {
	t.Helper()
	f := newEnvFix(t, swConfig(tasks))
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	ticks := newManualTicks()
	f.statsHub = api.NewStatsHub(f.agg, api.StatsHubOptions{NewTicker: ticks.newTicker, Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(f.statsHub.Close)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return &swFix{envFix: f, env: e, ticks: ticks}
}

func (f *swFix) open(t *testing.T, path string) *sseConn {
	t.Helper()
	return openSSE(t, f.authFixture, "/api/env/"+f.env.ID+"/"+path, f.cookie, "")
}

type swTaskRef struct {
	ID     string `json:"id"`
	Slot   int    `json:"slot"`
	Node   string `json:"node"`
	NodeID string `json:"nodeId"`
}

type swTasksEvent struct {
	Tasks      []swTaskRef `json:"tasks"`
	LimitBytes *int64      `json:"limitBytes"`
}

type swTotal struct {
	CPU      *float64 `json:"cpu"`
	Mem      uint64   `json:"mem"`
	NetRx    *float64 `json:"netRx"`
	NetTx    *float64 `json:"netTx"`
	BlkRead  *float64 `json:"blkRead"`
	BlkWrite *float64 `json:"blkWrite"`
	Counted  int      `json:"counted"`
	Silent   int      `json:"silent"`
}

type swTaskSample struct {
	ID     string   `json:"id"`
	CPU    *float64 `json:"cpu"`
	Mem    *uint64  `json:"mem"`
	NetRx  *float64 `json:"netRx"`
	NetTx  *float64 `json:"netTx"`
	NoData bool     `json:"noData"`
}

type swSampleEvent struct {
	T     int64          `json:"t"`
	Total swTotal        `json:"total"`
	Tasks []swTaskSample `json:"tasks"`
}

func decodeEvent[T any](t *testing.T, c *sseConn, name string) T {
	t.Helper()
	fr := nextEvent(t, c, name)
	var v T
	if err := json.Unmarshal([]byte(fr.data), &v); err != nil {
		t.Fatalf("%s %q: %v", name, fr.data, err)
	}
	return v
}

func near(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-6 }

func idsOf(tasks []swTaskRef) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	slices.Sort(out)
	return out
}

func TestServiceStatsStreamSumsTheRunningTasksAndLeavesOutASilentNode(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	c := f.open(t, "services/svc-web/stats/stream")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	ready := decodeEvent[swTasksEvent](t, c, "ready")
	if got, want := idsOf(ready.Tasks), []string{"w1", "w2", "w3"}; !slices.Equal(got, want) {
		t.Fatalf("ready tasks = %v, want the running ones %v (the task that failed earlier is not one)", got, want)
	}
	for _, tk := range ready.Tasks {
		if tk.ID == "w3" && (tk.Node != "wrk-ready" || tk.NodeID != "node-3" || tk.Slot != 3) {
			t.Errorf("task w3 = %+v, want its node named and its slot", tk)
		}
	}
	if ready.LimitBytes != nil {
		t.Errorf("limitBytes = %d, want none: no task has a memory limit", *ready.LimitBytes)
	}

	first := decodeEvent[swSampleEvent](t, c, "sample")
	if first.Total.CPU != nil || first.Total.Counted != 2 || first.Total.Silent != 1 {
		t.Errorf("first round total = %+v, want no CPU yet and two counted, one silent", first.Total)
	}
	f.ticks.fire()
	got := decodeEvent[swSampleEvent](t, c, "sample")
	tot := got.Total
	if !near(tot.CPU, 150) || tot.Mem != (200<<20)+(150<<20) || !near(tot.NetRx, 3000) || !near(tot.NetTx, 3000) || !near(tot.BlkRead, 2000) || !near(tot.BlkWrite, 0) {
		t.Errorf("total = %+v, want CPU 150, memory 350 MiB, 3000 B/s each way on the network and 2000 B/s of reads", tot)
	}
	if tot.Counted != 2 || tot.Silent != 1 {
		t.Errorf("counted = %d, silent = %d, want 2 and 1", tot.Counted, tot.Silent)
	}
	byID := map[string]swTaskSample{}
	for _, s := range got.Tasks {
		byID[s.ID] = s
	}
	if len(byID) != 3 || !near(byID["w1"].CPU, 100) || !near(byID["w2"].CPU, 50) || byID["w1"].NoData || byID["w2"].NoData {
		t.Errorf("tasks = %+v, want w1 at 100%%, w2 at 50%%", got.Tasks)
	}
	if w3 := byID["w3"]; !w3.NoData || w3.CPU != nil || w3.Mem != nil {
		t.Errorf("w3 = %+v, want no data: its node has no agent", w3)
	}
	if n := f.fake.StatsCalls(swWeb3); n != 0 {
		t.Errorf("%d stats requests for a task on a node nothing reaches, want none", n)
	}
}

func TestServiceStatsStreamSendsALimitOnlyWhenEveryRunningTaskSharesIt(t *testing.T) {
	t.Parallel()
	same := newSwFix(t, swTasks(256<<20))
	ready := decodeEvent[swTasksEvent](t, same.open(t, "services/svc-web/stats/stream"), "ready")
	if ready.LimitBytes == nil || *ready.LimitBytes != 256<<20 {
		t.Errorf("limitBytes = %v, want 256 MiB: every running task has it", ready.LimitBytes)
	}

	tasks := swTasks(256 << 20)
	tasks[1] = swTask("w2", "svc-web", "node-1", 2, swWeb2, 512<<20)
	differ := newSwFix(t, tasks)
	if r := decodeEvent[swTasksEvent](t, differ.open(t, "services/svc-web/stats/stream"), "ready"); r.LimitBytes != nil {
		t.Errorf("limitBytes = %d, want none: the tasks do not agree", *r.LimitBytes)
	}

	tasks = swTasks(256 << 20)
	tasks[1] = swTask("w2", "svc-web", "node-1", 2, swWeb2, 0)
	missing := newSwFix(t, tasks)
	if r := decodeEvent[swTasksEvent](t, missing.open(t, "services/svc-web/stats/stream"), "ready"); r.LimitBytes != nil {
		t.Errorf("limitBytes = %d, want none: one task has no limit", *r.LimitBytes)
	}
}

func TestServiceStatsStreamPicksUpNewTasksAndDropsStoppedOnes(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	c := f.open(t, "services/svc-web/stats/stream")
	decodeEvent[swTasksEvent](t, c, "ready")
	decodeEvent[swSampleEvent](t, c, "sample")

	// w2 stops and a new task, w4, takes its slot.
	stopped := tkExited("w2", "svc-web", "node-1", 2, "shutdown", "shutdown", 0, "", 20*time.Second)
	f.fake.Reconfigure(swConfig([]swarm.Task{
		swTask("w1", "svc-web", "node-1", 1, swWeb1, 0),
		stopped,
		swTask("w3", "svc-web", "node-3", 3, swWeb3, 0),
		swTask("w4", "svc-web", "node-1", 2, swWeb4, 0),
	}))
	f.ticks.fire()
	joined := decodeEvent[swTasksEvent](t, c, "tasks")
	if got, want := idsOf(joined.Tasks), []string{"w1", "w3", "w4"}; !slices.Equal(got, want) {
		t.Fatalf("tasks after the refresh = %v, want %v", got, want)
	}

	f.ticks.fire()
	for range 5 {
		s := decodeEvent[swSampleEvent](t, c, "sample")
		var ids []string
		for _, ts := range s.Tasks {
			ids = append(ids, ts.ID)
		}
		slices.Sort(ids)
		if slices.Equal(ids, []string{"w1", "w3", "w4"}) {
			if s.Total.Counted != 2 || s.Total.Silent != 1 {
				t.Errorf("total = %+v, want w1 and w4 counted, w3 silent; the stopped task left the totals", s.Total)
			}
			return
		}
	}
	t.Fatal("no round carried the new task and not the stopped one")
}

func TestServiceStatsStreamOfAnUnknownServiceIs404AndOfANonsenseIDIs422(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	c := f.open(t, "services/nope/stats/stream")
	c.awaitEnd(t, "the 404 was answered")
	if c.status0() != http.StatusNotFound || !strings.Contains(c.bodyString(), `"type":"service_not_found"`) {
		t.Errorf("status = %d body = %s, want 404 service_not_found", c.status0(), c.bodyString())
	}
	bad := f.open(t, "services/"+url2(" ;x")+"/stats/stream")
	bad.awaitEnd(t, "the 422 was answered")
	if bad.status0() != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 for an id that is not one", bad.status0())
	}
}

func url2(s string) string { return strings.NewReplacer(" ", "%20", ";", "%3B").Replace(s) }

func TestServiceStatsStreamSharesItsSamplersWithTheContainerStream(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	one := f.open(t, "containers/"+swWeb1+"/stats/stream?nodeId="+f.env.ID)
	decodeEvent[statsReady](t, one, "ready")
	decodeEvent[statsSampleEvent](t, one, "sample")

	c := f.open(t, "services/svc-web/stats/stream")
	decodeEvent[swTasksEvent](t, c, "ready")
	// The service's round waits for w1 to report again, since the container stream's sampler
	// had already taken its first sample.
	f.ticks.fire()
	decodeEvent[swSampleEvent](t, c, "sample")
	decodeEvent[statsSampleEvent](t, one, "sample")
	if n := f.statsHub.Samplers(); n != 2 {
		t.Errorf("%d samplers, want 2: w1 is watched twice and w2 once, w3 is not reachable", n)
	}
	before := f.fake.StatsCalls(swWeb1)
	f.ticks.fire()
	decodeEvent[swSampleEvent](t, c, "sample")
	decodeEvent[statsSampleEvent](t, one, "sample")
	if got := f.fake.StatsCalls(swWeb1) - before; got != 1 {
		t.Errorf("%d requests for w1 in one tick, want 1 for both viewers", got)
	}
}

type swServiceTotal struct {
	ID         string   `json:"id"`
	CPU        *float64 `json:"cpu"`
	Mem        uint64   `json:"mem"`
	NetRx      *float64 `json:"netRx"`
	NetTx      *float64 `json:"netTx"`
	Counted    int      `json:"counted"`
	Silent     int      `json:"silent"`
	LimitBytes *int64   `json:"limitBytes"`
}

type swServicesEvent struct {
	Services []swServiceTotal `json:"services"`
}

type swContainerSample struct {
	ID      string   `json:"id"`
	CPU     *float64 `json:"cpu"`
	Mem     *uint64  `json:"mem"`
	NetRx   *float64 `json:"netRx"`
	NetTx   *float64 `json:"netTx"`
	NoData  bool     `json:"noData"`
	Stopped bool     `json:"stopped"`
}

type swContainersEvent struct {
	Containers []swContainerSample `json:"containers"`
}

func TestListStatsStreamsServicesSendOneTotalPerServicePerTick(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(256<<20))
	c := f.open(t, "services/stats/stream?ids=svc-web,svc-db")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	nextEvent(t, c, "ready")
	decodeEvent[swServicesEvent](t, c, "sample")
	f.ticks.fire()
	got := decodeEvent[swServicesEvent](t, c, "sample")
	byID := map[string]swServiceTotal{}
	for _, s := range got.Services {
		byID[s.ID] = s
	}
	web, db := byID["svc-web"], byID["svc-db"]
	if len(byID) != 2 || !near(web.CPU, 150) || web.Mem != (350<<20) || web.Counted != 2 || web.Silent != 1 {
		t.Errorf("web = %+v, want CPU 150, 350 MiB, two counted and one silent", web)
	}
	if web.LimitBytes == nil || *web.LimitBytes != 256<<20 {
		t.Errorf("web limit = %v, want 256 MiB: every running task has it", web.LimitBytes)
	}
	if !near(db.CPU, 20) || db.Mem != 100<<20 || db.Counted != 1 || db.Silent != 0 || db.LimitBytes != nil {
		t.Errorf("db = %+v, want CPU 20, 100 MiB, one counted, no limit", db)
	}
}

func TestListStatsStreamsContainersSendOneSamplePerContainerAndMarkUnreachableOnes(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	ids := f.env.ID + "|" + swWeb1 + "," + f.env.ID + "|" + swWeb2 + ",node-elsewhere|" + swWeb1
	c := f.open(t, "containers/stats/stream?ids="+strings.ReplaceAll(ids, "|", "%7C"))
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	nextEvent(t, c, "ready")
	decodeEvent[swContainersEvent](t, c, "sample")
	f.ticks.fire()
	got := decodeEvent[swContainersEvent](t, c, "sample")
	byID := map[string]swContainerSample{}
	for _, s := range got.Containers {
		byID[s.ID] = s
	}
	w1, w2, away := byID[f.env.ID+"|"+swWeb1], byID[f.env.ID+"|"+swWeb2], byID["node-elsewhere|"+swWeb1]
	if len(byID) != 3 || !near(w1.CPU, 100) || w1.Mem == nil || *w1.Mem != 200<<20 || !near(w2.CPU, 50) {
		t.Errorf("containers = %+v, want w1 at 100%% with 200 MiB and w2 at 50%%", got.Containers)
	}
	if !away.NoData || away.CPU != nil {
		t.Errorf("a container named on a node that is not one of this environment's = %+v, want no data (T-06-97)", away)
	}
	// Two readings of w1 so far, the first and the tick's, however many streams ask for it; the
	// node that is not this environment's was refused before any engine was asked.
	if n := f.fake.StatsCalls(swWeb1); n != 2 {
		t.Errorf("%d stats requests for w1, want 2: one per round, and none for the node that is not this environment's", n)
	}
}

func TestListStatsStreamsRefuseMoreThanAHundredIDsAndNoneAtAll(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	var services, containers []string
	for i := range 101 {
		services = append(services, fmt.Sprintf("svc-%d", i))
		containers = append(containers, fmt.Sprintf("%s%%7Cc%d", f.env.ID, i))
	}
	for name, path := range map[string]string{
		"services":   "services/stats/stream?ids=" + strings.Join(services, ","),
		"containers": "containers/stats/stream?ids=" + strings.Join(containers, ","),
	} {
		c := f.open(t, path)
		c.awaitEnd(t, name+" answered")
		if c.status0() != http.StatusUnprocessableEntity || !strings.Contains(c.bodyString(), `"type":"too_many_ids"`) {
			t.Errorf("%s: status = %d body = %s, want 422 too_many_ids", name, c.status0(), c.bodyString())
		}
	}
	// A hundred is allowed.
	ok := f.open(t, "services/stats/stream?ids="+strings.Join(services[:100], ","))
	if ok.status0() != http.StatusOK {
		t.Errorf("100 ids: status = %d: %s", ok.status0(), ok.bodyString())
	}
	for name, path := range map[string]string{
		"services none":        "services/stats/stream",
		"services empty":       "services/stats/stream?ids=",
		"services bad":         "services/stats/stream?ids=a%20b",
		"containers none":      "containers/stats/stream",
		"containers no node":   "containers/stats/stream?ids=c1",
		"containers bad":       "containers/stats/stream?ids=n%7C%3B",
		"containers empty one": "containers/stats/stream?ids=n%7C",
	} {
		c := f.open(t, path)
		c.awaitEnd(t, name+" answered")
		if c.status0() != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d body = %s, want 422", name, c.status0(), c.bodyString())
		}
	}
}

type swNodeTotal struct {
	ID           string   `json:"id"`
	CPU          *float64 `json:"cpu"`
	Mem          uint64   `json:"mem"`
	Cpus         float64  `json:"cpus"`
	MemoryBytes  int64    `json:"memoryBytes"`
	Counted      int      `json:"counted"`
	Silent       int      `json:"silent"`
	NotReporting bool     `json:"notReporting"`
}

type swNodesEvent struct {
	Nodes []swNodeTotal `json:"nodes"`
}

func TestNodeStatsStreamSumsTheTasksOfEachNodeAndNamesTheOnesThatAreNotReporting(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	c := f.open(t, "nodes/stats/stream")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	nextEvent(t, c, "ready")
	decodeEvent[swNodesEvent](t, c, "sample")
	f.ticks.fire()
	got := decodeEvent[swNodesEvent](t, c, "sample")
	byID := map[string]swNodeTotal{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
	}
	mgr, wrk := byID["node-1"], byID["node-3"]
	if len(byID) != 2 || !near(mgr.CPU, 170) || mgr.Mem != (200<<20)+(150<<20)+(100<<20) || mgr.Counted != 3 || mgr.Silent != 0 || mgr.NotReporting {
		t.Errorf("node-1 = %+v, want the three tasks on it: CPU 170, 450 MiB", mgr)
	}
	if mgr.Cpus != 4 || mgr.MemoryBytes != 8<<30 {
		t.Errorf("node-1 capacity = %v CPUs, %d bytes, want 4 and 8 GiB", mgr.Cpus, mgr.MemoryBytes)
	}
	if !wrk.NotReporting || wrk.Silent != 1 || wrk.Counted != 0 || wrk.Cpus != 2 {
		t.Errorf("node-3 = %+v, want not reporting: its agent is gone", wrk)
	}
}

func TestStatsAggregateStreamsNeedASignedInUser(t *testing.T) {
	t.Parallel()
	f := newSwFix(t, swTasks(0))
	for _, path := range []string{
		"services/svc-web/stats/stream", "services/stats/stream?ids=svc-web", "containers/stats/stream?ids=a%7Cb", "nodes/stats/stream",
	} {
		c := openSSE(t, f.authFixture, "/api/env/"+f.env.ID+"/"+path, "", "")
		c.awaitEnd(t, path+" answered")
		if c.status0() != http.StatusUnauthorized {
			t.Errorf("%s without a session: status = %d, want 401", path, c.status0())
		}
	}
}
