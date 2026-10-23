// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// runningDeploy is a deploy that has started and that a test follows item by item.
type runningDeploy struct {
	rig   *swarmRig
	job   jobs.Job
	items map[string]jobs.Item
	ch    <-chan jobs.Event
	done  chan struct{}
	res   *stackops.DeployResult
}

// startDeploy starts DeploySwarm as a job and follows it. The caller moves the fake swarm and the clock on
// and reads the job with until, and ends with finish.
func (r *swarmRig) startDeploy(t *testing.T, in stackops.SwarmDeployInput) *runningDeploy {
	t.Helper()
	in = r.asCaller(t, in)
	d := &runningDeploy{rig: r, done: make(chan struct{}), res: new(stackops.DeployResult), items: map[string]jobs.Item{}}
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: r.envID, Owner: "user:test", Items: deployItems(),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		defer close(d.done)
		*d.res = r.deps.DeploySwarm(ctx, in, rep)
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, ch, cancel, err := r.hub.Subscribe(testsupport.Context(t), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	d.job, d.ch = job, ch
	for _, it := range snap.Items {
		d.items[it.ID] = it
	}
	return d
}

// until reads the job's events until the items satisfy cond, and fails naming what was awaited if the
// job ends first.
func (d *runningDeploy) until(t *testing.T, what string, cond func(items map[string]jobs.Item) bool) {
	t.Helper()
	ctx := testsupport.Context(t)
	for !cond(d.items) {
		select {
		case ev, ok := <-d.ch:
			if !ok {
				t.Fatalf("the job ended before %s; items: %v", what, d.states())
			}
			if ev.Item != nil {
				d.items[ev.Item.ID] = *ev.Item
			}
			if ev.Job != nil {
				for _, it := range ev.Job.Items {
					d.items[it.ID] = it
				}
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s; items: %v", what, d.states())
		}
	}
}

func (d *runningDeploy) states() map[string]string {
	out := map[string]string{}
	for id, it := range d.items {
		out[id] = it.State
		if it.Code != "" {
			out[id] += ":" + it.Code
		}
	}
	return out
}

// finish waits for the deploy to end and returns its result and the job as it ended.
func (d *runningDeploy) finish(t *testing.T) (stackops.DeployResult, jobs.Job) {
	t.Helper()
	ended, err := d.rig.hub.Wait(testsupport.Context(t), d.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the deploy function to return", d.done)
	return *d.res, ended
}

func TestDeploySwarmHoldsAServiceWorkingUntilItsTasksRun(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock = clk
	rig.fake.SetTaskState("tracer_web", "preparing", "")
	_, in := starterPlan(t, "tracer")
	d := rig.startDeploy(t, in)

	const row = "service:tracer_web"
	d.until(t, "the service row to count its tasks and say it pulls the image", func(items map[string]jobs.Item) bool {
		it := items[row]
		return it.Unit == "tasks" && it.Total == 1 && it.Result["phase"] == "pulling"
	})
	if it := d.items[row]; it.State != jobs.ItemRunning || it.Current != 0 {
		t.Errorf("row = %+v, want working with 0 of 1 tasks running", it)
	}
	d.until(t, "the task to appear under the row", func(items map[string]jobs.Item) bool {
		for _, it := range items {
			if it.Kind == stackops.KindTask && it.Parent == row && it.Result["state"] == "preparing" {
				return true
			}
		}
		return false
	})
	for _, it := range d.items {
		if it.Kind != stackops.KindTask {
			continue
		}
		if it.Label != "tracer_web.1" || it.Result["node"] != "mgr-1" {
			t.Errorf("task = %+v, want tracer_web.1 on mgr-1", it)
		}
	}

	rig.fake.SetTaskState("tracer_web", "", "")
	clk.Tick(t, stackops.PollInterval)
	res, job := d.finish(t)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	got := itemByID(t, job, row)
	if got.State != jobs.ItemDone || got.Result["settled"] != "true" || got.Result["running"] != "1" || got.Result["action"] != "created" {
		t.Errorf("row = %+v, want done, settled at 1 running, created", got)
	}
	for _, it := range job.Items {
		if it.Kind == stackops.KindTask && it.State != jobs.ItemDone {
			t.Errorf("task %s = %s, want done: it ends with its row", it.ID, it.State)
		}
	}
}

func TestDeploySwarmWaitsForOneTaskOnEachNodeOfAGlobalService(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.NodeRows = append(c.NodeRows, dockertest.FakeNode{ID: "node-2", Hostname: "wrk-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"})
	})
	_, in := stackInput(t, "mon", "services:\n  agent:\n    image: busybox:1.36\n    deploy:\n      mode: global\n", nil)
	res, job := rig.deploy(t, in)
	if !res.OK() {
		t.Fatalf("result = %+v %v", res, itemStates(job))
	}
	got := itemByID(t, job, "service:mon_agent")
	if got.Result["settled"] != "true" || got.Result["running"] != "2" || got.Result["desired"] != "2" || got.Total != 2 {
		t.Errorf("row = %+v, want settled at 2 of 2", got)
	}
}

func TestDeploySwarmDoesNotWaitForAJob(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "batch", "services:\n  once:\n    image: busybox:1.36\n    deploy:\n      mode: replicated-job\n", nil)
	res, job := rig.deploy(t, in)
	if !res.OK() {
		t.Fatalf("result = %+v %v", res, itemStates(job))
	}
	got := itemByID(t, job, "service:batch_once")
	if got.State != jobs.ItemDone || got.Result["settled"] == "true" || got.Result["action"] != "created" {
		t.Errorf("row = %+v, want done as created and not settled: a job runs to its end on its own", got)
	}
}

const twoServices = `services:
  api:
    image: busybox:1.36
  web:
    image: nginx:1.27-alpine
`

func TestDeploySwarmTimesOutAServiceThatDoesNotSettleAndStopsThere(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock, rig.deps.ConvergeTimeout = clk, time.Minute
	rig.fake.SetTaskState("web_api", "rejected", "No such image: busybox:1.36")
	_, in := stackInput(t, "web", twoServices, nil)
	d := rig.startDeploy(t, in)
	// Thirty polls of two seconds reach the minute; the poll after the last finds it passed.
	for range 30 {
		clk.Tick(t, stackops.PollInterval)
	}
	res, job := d.finish(t)

	if res.Code != stackops.CodeConvergeTimeout || res.FailedService != "web_api" {
		t.Fatalf("result = %+v, want a timeout at web_api", res)
	}
	api := itemByID(t, job, "service:web_api")
	if api.State != jobs.ItemFailed || api.Code != stackops.CodeConvergeTimeout || api.Result["minutes"] != "1" || !strings.Contains(api.Detail, "No such image") {
		t.Errorf("api row = %+v, want failed as a timeout after 1 minute with the swarm's words", api)
	}
	if web := itemByID(t, job, "service:web_web"); web.State != jobs.ItemDone || web.Result["settled"] != "true" {
		t.Errorf("web row = %+v, want done: it settled on its own", web)
	}
	got := itemStates(job)
	if got[stackops.ItemServices] != "failed:converge_timeout" || got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("steps = %v, want Deploy services failed and the cleanup not run", got)
	}
	var tasks int
	for _, it := range job.Items {
		if it.Kind == stackops.KindTask {
			tasks++
			if it.State != jobs.ItemDone {
				t.Errorf("task %s = %s, want done", it.ID, it.State)
			}
		}
	}
	if tasks == 0 {
		t.Error("no task item for the rejected task: the console cannot show why")
	}
	if ups := rig.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("service updates = %d, want none: a timeout writes nothing", len(ups))
	}
}

func TestDeploySwarmFailsAServiceWhoseTasksTheSwarmWillNotStartAgainAtTheSecondRead(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock = clk
	const words = "task: non-zero exit (1)"
	rig.fake.SetServiceTasks("web_api",
		dockertest.FakeServiceTask{Slot: 1, State: "failed", Desired: "shutdown", Err: words},
		dockertest.FakeServiceTask{Slot: 1, State: "failed", Desired: "shutdown", Err: words},
	)
	_, in := stackInput(t, "web", twoServices, nil)
	d := rig.startDeploy(t, in)
	// One poll interval is the second read. A wait that ran to the converge timeout would arm timer after
	// timer, and the deploy would not end.
	clk.Tick(t, stackops.PollInterval)
	res, job := d.finish(t)

	if res.Code != stackops.CodeTasksFailed || res.FailedService != "web_api" {
		t.Fatalf("result = %+v, want tasks failed at web_api", res)
	}
	api := itemByID(t, job, "service:web_api")
	if api.State != jobs.ItemFailed || api.Code != stackops.CodeTasksFailed || api.Message == "" ||
		api.Message != stackops.CodeMessage(stackops.CodeTasksFailed) || !strings.Contains(api.Detail, "non-zero exit (1)") || api.Result["action"] != "created" {
		t.Errorf("api row = %+v, want failed as tasks_failed with the server's sentence and the swarm's words", api)
	}
	if web := itemByID(t, job, "service:web_web"); web.State != jobs.ItemDone || web.Result["settled"] != "true" {
		t.Errorf("web row = %+v, want done: it settled on its own", web)
	}
	got := itemStates(job)
	if got[stackops.ItemServices] != "failed:tasks_failed" || got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("steps = %v, want Deploy services failed and the cleanup not run", got)
	}
	var tasks int
	for _, it := range job.Items {
		if it.Kind == stackops.KindTask && it.Parent == "service:web_api" {
			tasks++
			if it.State != jobs.ItemDone {
				t.Errorf("task %s = %s, want done", it.ID, it.State)
			}
		}
	}
	if tasks == 0 {
		t.Error("no task item under the web_api row: the console cannot show why")
	}
	if ups := rig.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("service updates = %d, want none: nothing is rolled back", len(ups))
	}
}

const wideAndAPI = "services:\n  wide:\n    image: busybox:1.36\n    deploy:\n      replicas: 3\n  api:\n    image: nginx:1.27-alpine\n"

func TestDeploySwarmFailsAServiceWhoseNetworkHasNoFreeAddressesNamingTheNetworkAndSubnet(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock = clk
	// The shape of UAT G-07-5: a task stays new with nothing in its error while the subnet has no
	// address left, and no update is rolling out.
	rig.fake.SetNetworkUsage("web_default", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 256, Available: 0})
	rig.fake.SetServiceTasks("web_wide",
		dockertest.FakeServiceTask{Slot: 3, State: "new", Desired: "running", Message: "created"},
		dockertest.FakeServiceTask{Slot: 2, State: "running"},
		dockertest.FakeServiceTask{Slot: 1, State: "running"},
	)
	_, in := stackInput(t, "web", wideAndAPI, nil)
	d := rig.startDeploy(t, in)
	// Eight polls of two seconds are sixteen: each tick proves the wait armed again, so it did not
	// end before the fifteen-second hold.
	for range 8 {
		clk.Tick(t, stackops.PollInterval)
	}
	res, job := d.finish(t)

	if res.Code != stackops.CodeNetworkFull || res.FailedService != "web_wide" {
		t.Fatalf("result = %+v, want network_full at web_wide", res)
	}
	wide := itemByID(t, job, "service:web_wide")
	if wide.State != jobs.ItemFailed || wide.Code != stackops.CodeNetworkFull || wide.Message != stackops.CodeMessage(stackops.CodeNetworkFull) ||
		wide.Message == "" || wide.Result["network"] != "web_default" || wide.Result["subnet"] != "10.0.1.0/24" || wide.Result["action"] != "created" {
		t.Errorf("wide row = %+v, want failed as network_full naming web_default and 10.0.1.0/24", wide)
	}
	if api := itemByID(t, job, "service:web_api"); api.State != jobs.ItemDone || api.Result["settled"] != "true" {
		t.Errorf("api row = %+v, want done: it settled on its own", api)
	}
	got := itemStates(job)
	if got[stackops.ItemServices] != "failed:network_full" || got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("steps = %v, want Deploy services failed and the cleanup not run", got)
	}
	if ups := rig.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("service updates = %d, want none: nothing is rolled back", len(ups))
	}
}

// requireNetworkTargetsByID fails the test unless the service names the network by its full ID, as a
// real manager stores it, so a case cannot pass through the name path by accident.
func requireNetworkTargetsByID(t *testing.T, r *swarmRig, service, network string) {
	t.Helper()
	ctx := testsupport.Context(t)
	if err := r.deps.Clusters.ClusterRead(ctx, r.envID, func(c *dockerx.Client) error {
		nd, _, err := c.NetworkDetail(ctx, network, false)
		if err != nil {
			return err
		}
		sd, _, err := c.ServiceInspect(ctx, service)
		if err != nil {
			return err
		}
		if len(sd.NetworkIDs) != 1 || sd.NetworkIDs[0] != nd.ID || nd.ID == network || len(nd.ID) != 64 {
			t.Errorf("service %s names networks %v, want its full ID %q of %s", service, sd.NetworkIDs, nd.ID, network)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// UAT G-07-8: the manager that is read holds a local instance of the network, so Engine 29 answers an
// inspect by full ID with that node's own count, and the service names its network by ID as a real
// manager stores it. Only the swarm's count says the subnet is full.
func TestDeploySwarmFailsAServiceWhoseNetworkIsFullWhenTheManagerItReadsHoldsTheNetworkLocally(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.NetworkTargetsByID = true })
	clk := newStepClock()
	rig.deps.Clock = clk
	rig.fake.SetNetworkUsage("web_default", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 256, Available: 0})
	rig.fake.SetNetworkLocalUsage("web_default", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 46, Available: 210})
	rig.fake.SetServiceTasks("web_wide",
		dockertest.FakeServiceTask{Slot: 3, State: "new", Desired: "running", Message: "created"},
		dockertest.FakeServiceTask{Slot: 2, State: "running"},
		dockertest.FakeServiceTask{Slot: 1, State: "running"},
	)
	_, in := stackInput(t, "web", wideAndAPI, nil)
	d := rig.startDeploy(t, in)
	for range 8 {
		clk.Tick(t, stackops.PollInterval)
	}
	res, job := d.finish(t)

	if res.Code != stackops.CodeNetworkFull || res.FailedService != "web_wide" {
		t.Fatalf("result = %+v, want network_full at web_wide", res)
	}
	wide := itemByID(t, job, "service:web_wide")
	if wide.State != jobs.ItemFailed || wide.Code != stackops.CodeNetworkFull || wide.Message != stackops.CodeMessage(stackops.CodeNetworkFull) ||
		wide.Message == "" || wide.Result["network"] != "web_default" || wide.Result["subnet"] != "10.0.1.0/24" {
		t.Errorf("wide row = %+v, want failed as network_full naming web_default and 10.0.1.0/24", wide)
	}
	if api := itemByID(t, job, "service:web_api"); api.State != jobs.ItemDone || api.Result["settled"] != "true" {
		t.Errorf("api row = %+v, want done: it settled on its own", api)
	}
	got := itemStates(job)
	if got[stackops.ItemServices] != "failed:network_full" || got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("steps = %v, want Deploy services failed and the cleanup not run", got)
	}
	if ups := rig.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("service updates = %d, want none: nothing is rolled back", len(ups))
	}
	requireNetworkTargetsByID(t, rig, "web_wide", "web_default")
}

func TestDeploySwarmTimesOutWithHowManyTasksWereStillWaitingForAnAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		usage []dockertest.FakeSubnetUsage
	}{
		{"an engine that reports no address usage", nil},
		{"a network with room", []dockertest.FakeSubnetUsage{{Subnet: "10.0.1.0/24", InUse: 200, Available: 53}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSwarmRig(t, nil)
			clk := newStepClock()
			rig.deps.Clock, rig.deps.ConvergeTimeout = clk, time.Minute
			if tc.usage != nil {
				rig.fake.SetNetworkUsage("web_default", tc.usage...)
			}
			rig.fake.SetServiceTasks("web_wide",
				dockertest.FakeServiceTask{Slot: 3, State: "new", Desired: "running", Message: "created"},
				dockertest.FakeServiceTask{Slot: 3, State: "rejected", Desired: "shutdown", Err: "node is missing network attachments, ip addresses may be exhausted"},
				dockertest.FakeServiceTask{Slot: 2, State: "running"},
				dockertest.FakeServiceTask{Slot: 1, State: "running"},
			)
			_, in := stackInput(t, "web", wideAndAPI, nil)
			d := rig.startDeploy(t, in)
			for range 30 {
				clk.Tick(t, stackops.PollInterval)
			}
			res, job := d.finish(t)

			if res.Code != stackops.CodeConvergeTimeout || res.FailedService != "web_wide" {
				t.Fatalf("result = %+v, want a timeout at web_wide", res)
			}
			wide := itemByID(t, job, "service:web_wide")
			if wide.Result["minutes"] != "1" || wide.Result["waiting"] != "1" || !strings.Contains(wide.Detail, "ip addresses may be exhausted") {
				t.Errorf("wide row = %+v, want 1 minute, 1 task waiting and the swarm's words", wide)
			}
		})
	}
}

// The 07-49 fallback holds under the real-manager shape: the manager holds the network locally and the
// service names it by ID, yet a swarm with room, or an engine that reports no usage, is not a full network.
func TestDeploySwarmKeepsWaitingWhenTheSwarmHasRoomThoughTheManagerHoldsTheNetwork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		swarm []dockertest.FakeSubnetUsage
		local []dockertest.FakeSubnetUsage
	}{
		{"the swarm has room",
			[]dockertest.FakeSubnetUsage{{Subnet: "10.0.1.0/24", InUse: 200, Available: 53}},
			[]dockertest.FakeSubnetUsage{{Subnet: "10.0.1.0/24", InUse: 46, Available: 210}}},
		{"an engine that reports no address usage", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.NetworkTargetsByID = true })
			clk := newStepClock()
			rig.deps.Clock, rig.deps.ConvergeTimeout = clk, time.Minute
			if tc.swarm != nil {
				rig.fake.SetNetworkUsage("web_default", tc.swarm...)
				rig.fake.SetNetworkLocalUsage("web_default", tc.local...)
			}
			rig.fake.SetServiceTasks("web_wide",
				dockertest.FakeServiceTask{Slot: 3, State: "new", Desired: "running", Message: "created"},
				dockertest.FakeServiceTask{Slot: 3, State: "rejected", Desired: "shutdown", Err: "node is missing network attachments, ip addresses may be exhausted"},
				dockertest.FakeServiceTask{Slot: 2, State: "running"},
				dockertest.FakeServiceTask{Slot: 1, State: "running"},
			)
			_, in := stackInput(t, "web", wideAndAPI, nil)
			d := rig.startDeploy(t, in)
			for range 30 {
				clk.Tick(t, stackops.PollInterval)
			}
			res, job := d.finish(t)

			if res.Code != stackops.CodeConvergeTimeout || res.FailedService != "web_wide" {
				t.Fatalf("result = %+v, want a timeout at web_wide", res)
			}
			wide := itemByID(t, job, "service:web_wide")
			if wide.Result["minutes"] != "1" || wide.Result["waiting"] != "1" || !strings.Contains(wide.Detail, "ip addresses may be exhausted") {
				t.Errorf("wide row = %+v, want 1 minute, 1 task waiting and the swarm's words", wide)
			}
			requireNetworkTargetsByID(t, rig, "web_wide", "web_default")
		})
	}
}

func TestDeploySwarmWaitsThroughARestartDelay(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock = clk
	rig.fake.SetServiceTasks("web_api",
		dockertest.FakeServiceTask{Slot: 1, State: "ready", Desired: "ready"},
		dockertest.FakeServiceTask{Slot: 1, State: "failed", Desired: "shutdown", Err: "task: non-zero exit (1)"},
	)
	_, in := stackInput(t, "web", twoServices, nil)
	d := rig.startDeploy(t, in)
	// Each tick is a read that found the service not settled, and not at its end: the wait armed again.
	for range 4 {
		clk.Tick(t, stackops.PollInterval)
	}
	// The wait is blocked on its fifth timer, so the swarm can move on without racing a read.
	testsupport.Await(t, "the wait to arm its fifth poll", clk.armed)
	rig.fake.SetServiceTasks("web_api", dockertest.FakeServiceTask{Slot: 1, State: "running"})
	clk.Advance(stackops.PollInterval)
	res, job := d.finish(t)

	if !res.OK() {
		t.Fatalf("result = %+v, want the deploy to wait for the replacement and settle", res)
	}
	if api := itemByID(t, job, "service:web_api"); api.State != jobs.ItemDone || api.Result["settled"] != "true" {
		t.Errorf("api row = %+v, want done and settled", api)
	}
}

func TestDeploySwarmDoesNotFailASlotTheSwarmRefillsAfterTheWrite(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock = clk
	dead := dockertest.FakeServiceTask{Slot: 1, State: "failed", Desired: "shutdown", Err: "task: non-zero exit (1)"}
	rig.fake.SetServiceTasks("web_api", dead, dead)
	_, in := stackInput(t, "web", twoServices, nil)
	d := rig.startDeploy(t, in)
	d.until(t, "the first read of web_api to be shown", func(items map[string]jobs.Item) bool {
		return items["service:web_api"].Unit == "tasks"
	})
	// The swarm adds a task to the dead slot just after the write; the wait has not read again.
	rig.fake.SetServiceTasks("web_api", dockertest.FakeServiceTask{Slot: 1, State: "running"}, dead, dead)
	clk.Tick(t, stackops.PollInterval)
	res, _ := d.finish(t)

	if !res.OK() {
		t.Fatalf("result = %+v, want the deploy to settle: one exhausted read is not the end", res)
	}
}

func TestDeploySwarmDoesNotFailAServiceWhoseOnlyDeadSlotIsAboveItsReplicaCount(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock = clk
	// A stale failed slot 4 beside slots 1 and 2: the swarm has not made slot 3 yet.
	rig.fake.SetServiceTasks("web_wide",
		dockertest.FakeServiceTask{Slot: 4, State: "failed", Desired: "shutdown", Err: "task: non-zero exit (1)"},
		dockertest.FakeServiceTask{Slot: 2, State: "running"},
		dockertest.FakeServiceTask{Slot: 1, State: "running"},
	)
	_, in := stackInput(t, "web", wideAndAPI, nil)
	d := rig.startDeploy(t, in)
	// The second read of the wait. Counting slot 4 as dead, it would be the second exhausted read.
	clk.Tick(t, stackops.PollInterval)
	select {
	case <-clk.armed:
	case <-d.done:
		t.Fatalf("the deploy ended before the swarm made slot 3: a dead slot above the replica count must not end it; result = %+v", *d.res)
	case <-testsupport.Context(t).Done():
		t.Fatal("timed out waiting for the wait to arm its third poll")
	}
	// The swarm answers the write: it makes slot 3 and deletes the stale slot 4.
	rig.fake.SetServiceTasks("web_wide",
		dockertest.FakeServiceTask{Slot: 3, State: "running"},
		dockertest.FakeServiceTask{Slot: 2, State: "running"},
		dockertest.FakeServiceTask{Slot: 1, State: "running"},
	)
	clk.Advance(stackops.PollInterval)
	res, job := d.finish(t)

	if !res.OK() {
		t.Fatalf("result = %+v, want the deploy to settle: a dead slot above the replica count must not end it", res)
	}
	if wide := itemByID(t, job, "service:web_wide"); wide.State != jobs.ItemDone || wide.Result["settled"] != "true" {
		t.Errorf("wide row = %+v, want done and settled: a dead slot above the replica count must not end the deploy", wide)
	}
}

func TestDeploySwarmStopsWhenTheSwarmRollsTheUpdateBack(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "bayplan.stack.id": "stack-tracer"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "tracer_default", Driver: "overlay", Scope: "swarm", Labels: owned}}
	}, dockertest.FakeService{ID: "svc-web", Name: "tracer_web", Mode: "replicated", Image: "nginx:1.26", Version: 7, Labels: owned, Desired: 1, Running: 1})
	clk := newStepClock()
	rig.deps.Clock = clk
	rig.fake.SetTaskState("tracer_web", "preparing", "")
	_, in := starterPlan(t, "tracer")
	d := rig.startDeploy(t, in)

	d.until(t, "the row to count its tasks", func(items map[string]jobs.Item) bool { return items["service:tracer_web"].Unit == "tasks" })
	rig.fake.SetUpdateState("tracer_web", "rollback_completed", "update rolled back: task failed")
	clk.Tick(t, stackops.PollInterval)
	res, job := d.finish(t)

	if res.Code != stackops.CodeUpdateStopped || res.FailedService != "tracer_web" {
		t.Fatalf("result = %+v, want the update stopped at tracer_web", res)
	}
	row := itemByID(t, job, "service:tracer_web")
	if row.State != jobs.ItemFailed || row.Code != stackops.CodeUpdateStopped || !strings.Contains(row.Detail, "update rolled back") {
		t.Errorf("row = %+v, want failed with the swarm's words", row)
	}
}

func TestDeploySwarmIgnoresTheUpdateStatusAnEarlierDeployLeft(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "bayplan.stack.id": "stack-tracer"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "tracer_default", Driver: "overlay", Scope: "swarm", Labels: owned}}
	}, dockertest.FakeService{ID: "svc-web", Name: "tracer_web", Mode: "replicated", Image: "nginx:1.26", Version: 7, Labels: owned,
		UpdateState: "rollback_completed", UpdateMessage: "rolled back by an earlier deploy"})
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)
	if !res.OK() {
		t.Fatalf("result = %+v %v, want a deploy that is not stopped by a status it did not make", res, itemStates(job))
	}
}
