// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Stop and Start of a swarm stack run against the fake swarm manager, over a stack the swarm deploy made.

// lifecycleStore is the stored side of a stack for a test: the last good revision and what is recorded
// about the stack being stopped.
type lifecycleStore struct {
	mu      sync.Mutex
	rev     stackops.Snapshot
	have    bool
	pins    map[string]string
	stopped map[string]bool
}

func (s *lifecycleStore) SetStopped(_ context.Context, stackID string, stopped bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped == nil {
		s.stopped = map[string]bool{}
	}
	s.stopped[stackID] = stopped
	return nil
}

func (s *lifecycleStore) isStopped(stackID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped[stackID]
}

func (s *lifecycleStore) LatestGoodRevision(context.Context, string) (stackops.Snapshot, bool, error) {
	return s.rev, s.have, nil
}

func (s *lifecycleStore) RevisionPins(context.Context, string) (map[string]string, error) {
	return s.pins, nil
}

// deployedStack deploys compose as a stack and returns it with the store its last good revision lives in.
func (r *swarmRig) deployedStack(t *testing.T, name, composeText string, files []stacks.File, vars ...stacks.Var) (stackops.Stack, *lifecycleStore) {
	t.Helper()
	st, in := checkedStackInput(t, name, composeText, files, vars...)
	res, job := r.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v %v, want it done", res, itemStates(job))
	}
	pins := map[string]string{}
	for svc, p := range res.Pins {
		pins[svc] = p.Ref
	}
	return st, &lifecycleStore{
		rev:  stackops.Snapshot{ID: "rev-1", StackID: st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood, Copy: in.Copy},
		have: true, pins: pins,
	}
}

// lifecycleRun runs a stop or a start as a job and returns its result and the job as it ended.
func (r *swarmRig) lifecycleRun(t *testing.T, mode string, st stackops.Stack, store stackops.LifecycleStore, caller stackops.Caller) (stackops.LifecycleResult, jobs.Job) {
	t.Helper()
	return r.lifecycleRunOn(t, mode, st, "", store, caller)
}

// lifecycleRunOn is lifecycleRun for a stack of a standalone host, whose node is named.
func (r *swarmRig) lifecycleRunOn(t *testing.T, mode string, st stackops.Stack, node string, store stackops.LifecycleStore, caller stackops.Caller) (stackops.LifecycleResult, jobs.Job) {
	t.Helper()
	id, res, done := startLifecycleJob(t, r, mode, st, node, store, caller)
	ended, err := r.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the "+mode+" function to return", done)
	return *res, ended
}

// startLifecycleJob starts a stop or a start as a job without waiting for it. The result is filled in when
// done is closed. A caller with no principal is root. node names the host of a standalone stack, "" for a swarm.
func startLifecycleJob(t *testing.T, r *swarmRig, mode string, st stackops.Stack, node string, store stackops.LifecycleStore, caller stackops.Caller) (jobID string, res *stackops.LifecycleResult, done <-chan struct{}) {
	t.Helper()
	if caller.Principal == nil {
		caller = stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"}
	}
	res = new(stackops.LifecycleResult)
	fin := make(chan struct{})
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_" + mode, EnvID: r.envID, Owner: "user:test",
		Items: stackops.LifecycleItemSpecs(mode, st.Target),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		defer close(fin)
		in := stackops.LifecycleInput{EnvID: r.envID, NodeID: node, Stack: st, Caller: caller, Store: store}
		if mode == stackops.LifecycleStop {
			*res = r.deps.Stop(ctx, in, rep)
		} else {
			*res = r.deps.Start(ctx, in, rep)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return job.ID, res, fin
}

func rootCaller(t *testing.T, r *swarmRig) stackops.Caller {
	t.Helper()
	return stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"}
}

func (r *swarmRig) stop(t *testing.T, st stackops.Stack, store stackops.LifecycleStore) (stackops.LifecycleResult, jobs.Job) {
	t.Helper()
	return r.lifecycleRun(t, stackops.LifecycleStop, st, store, stackops.Caller{})
}

func (r *swarmRig) start(t *testing.T, st stackops.Stack, store stackops.LifecycleStore) (stackops.LifecycleResult, jobs.Job) {
	t.Helper()
	return r.lifecycleRun(t, stackops.LifecycleStart, st, store, stackops.Caller{})
}

// ownedNow reads the services the stack owns on the swarm.
func (r *swarmRig) ownedNow(t *testing.T, st stackops.Stack) map[string]dockerx.ServiceDetail {
	t.Helper()
	out := map[string]dockerx.ServiceDetail{}
	err := r.deps.Clusters.ClusterRead(testsupport.Context(t), r.envID, func(c *dockerx.Client) error {
		svcs, e := c.ServicesWithLabel(testsupport.Context(t), stacks.LabelSwarmNamespace, st.Name)
		for _, s := range svcs {
			out[s.Name] = s
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const lifecycleTwo = "services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      replicas: 3\n  api:\n    image: nginx:1.28-alpine\n"

func TestStopScalesEveryReplicatedServiceToZeroAndRemembersItsCountOnTheService(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, store := rig.deployedStack(t, "lifecycle", lifecycleTwo, nil)
	before := rig.ownedNow(t, st)

	res, job := rig.stop(t, st, store)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("stop = %+v %v, want it done", res, itemStates(job))
	}
	if want := []string{"lifecycle_api", "lifecycle_web"}; !slices.Equal(res.Scaled, want) || len(res.Removed) != 0 {
		t.Errorf("scaled = %v removed = %v, want %v and none", res.Scaled, res.Removed, want)
	}
	after := rig.ownedNow(t, st)
	for name, want := range map[string]string{"lifecycle_web": "3", "lifecycle_api": "1"} {
		got := after[name]
		if got.Replicas != 0 || got.Labels[stacks.LabelStoppedReplicas] != want {
			t.Errorf("%s: replicas %d label %q, want 0 and %q", name, got.Replicas, got.Labels[stacks.LabelStoppedReplicas], want)
		}
		if got.Image != before[name].Image {
			t.Errorf("%s: image %q, want %q kept", name, got.Image, before[name].Image)
		}
	}
	if !store.isStopped(st.ID) {
		t.Errorf("the stack is not recorded as stopped")
	}
	if it := itemByID(t, job, "service:lifecycle_web"); it.State != jobs.ItemDone || it.Result["action"] != "stopped" {
		t.Errorf("web row = %+v, want done as stopped", it)
	}
}

func TestStartScalesEachServiceBackToTheCountItRememberedWithTheSameImageAndRemovesTheLabel(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, store := rig.deployedStack(t, "lifecycle", lifecycleTwo, nil)
	before := rig.ownedNow(t, st)
	if res, _ := rig.stop(t, st, store); !res.OK() {
		t.Fatalf("stop = %+v", res)
	}

	res, job := rig.start(t, st, store)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("start = %+v %v, want it done", res, itemStates(job))
	}
	if len(res.Recreated) != 0 || !slices.Equal(res.Scaled, []string{"lifecycle_api", "lifecycle_web"}) {
		t.Errorf("scaled = %v recreated = %v, want both scaled and none recreated", res.Scaled, res.Recreated)
	}
	after := rig.ownedNow(t, st)
	for name, want := range map[string]uint64{"lifecycle_web": 3, "lifecycle_api": 1} {
		got := after[name]
		if got.Replicas != want {
			t.Errorf("%s: replicas %d, want %d", name, got.Replicas, want)
		}
		if _, left := got.Labels[stacks.LabelStoppedReplicas]; left {
			t.Errorf("%s still carries the stopped-replicas label", name)
		}
		if got.Image != before[name].Image {
			t.Errorf("%s: image %q, want the digest %q it had", name, got.Image, before[name].Image)
		}
	}
	if store.isStopped(st.ID) {
		t.Errorf("the stack is still recorded as stopped")
	}
}

func TestStartEndsAServiceTheSwarmWillNotStartAgainAsTasksFailed(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, store := rig.deployedStack(t, "lifecycle", lifecycleTwo, nil)
	if res, _ := rig.stop(t, st, store); !res.OK() {
		t.Fatalf("stop = %+v", res)
	}
	clk := newStepClock()
	rig.deps.Clock = clk
	dead := dockertest.FakeServiceTask{Slot: 1, State: "failed", Desired: "shutdown", Err: "task: non-zero exit (1)"}
	rig.fake.SetServiceTasks("lifecycle_api", dead, dead)

	id, res, done := startLifecycleJob(t, rig, stackops.LifecycleStart, st, "", store, stackops.Caller{})
	// One poll interval is the second read; a start that waited out the converge timeout would not end.
	clk.Tick(t, stackops.PollInterval)
	job, err := rig.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the start function to return", done)

	if res.Code != stackops.CodeTasksFailed || res.FailedService != "lifecycle_api" {
		t.Fatalf("start = %+v, want tasks failed at lifecycle_api", *res)
	}
	row := itemByID(t, job, "service:lifecycle_api")
	if row.State != jobs.ItemFailed || row.Code != stackops.CodeTasksFailed || !strings.Contains(row.Detail, "non-zero exit (1)") {
		t.Errorf("api row = %+v, want failed as tasks_failed with the swarm's words", row)
	}
}

func TestStartEndsAServiceWhoseNetworkHasNoFreeAddressesAsNetworkFull(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, store := rig.deployedStack(t, "lifecycle", lifecycleTwo, nil)
	if res, _ := rig.stop(t, st, store); !res.OK() {
		t.Fatalf("stop = %+v", res)
	}
	clk := newStepClock()
	rig.deps.Clock = clk
	rig.fake.SetNetworkUsage("lifecycle_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 256, Available: 0})
	rig.fake.SetServiceTasks("lifecycle_web",
		dockertest.FakeServiceTask{Slot: 3, State: "new", Desired: "running", Message: "created"},
		dockertest.FakeServiceTask{Slot: 2, State: "running"},
		dockertest.FakeServiceTask{Slot: 1, State: "running"},
	)

	id, res, done := startLifecycleJob(t, rig, stackops.LifecycleStart, st, "", store, stackops.Caller{})
	// Eight polls are sixteen seconds: past the hold of fifteen, which each tick proves the wait outlasted.
	for range 8 {
		clk.Tick(t, stackops.PollInterval)
	}
	job, err := rig.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the start function to return", done)

	if res.Code != stackops.CodeNetworkFull || res.FailedService != "lifecycle_web" {
		t.Fatalf("start = %+v, want network full at lifecycle_web", *res)
	}
	row := itemByID(t, job, "service:lifecycle_web")
	if row.State != jobs.ItemFailed || row.Code != stackops.CodeNetworkFull ||
		row.Result["network"] != "lifecycle_default" || row.Result["subnet"] != "10.0.2.0/24" {
		t.Errorf("web row = %+v, want failed as network_full naming lifecycle_default and 10.0.2.0/24", row)
	}
}

// UAT G-07-8 under the real-manager shape: the service Start scales up names its network by ID, and the
// manager that is read holds the network locally with room in its own count.
func TestStartEndsAServiceWhoseNetworkIsFullWhenTheManagerItReadsHoldsTheNetworkLocally(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.NetworkTargetsByID = true })
	st, store := rig.deployedStack(t, "lifecycle", lifecycleTwo, nil)
	if res, _ := rig.stop(t, st, store); !res.OK() {
		t.Fatalf("stop = %+v", res)
	}
	clk := newStepClock()
	rig.deps.Clock = clk
	rig.fake.SetNetworkUsage("lifecycle_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 256, Available: 0})
	rig.fake.SetNetworkLocalUsage("lifecycle_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 40, Available: 216})
	rig.fake.SetServiceTasks("lifecycle_web",
		dockertest.FakeServiceTask{Slot: 3, State: "new", Desired: "running", Message: "created"},
		dockertest.FakeServiceTask{Slot: 2, State: "running"},
		dockertest.FakeServiceTask{Slot: 1, State: "running"},
	)

	id, res, done := startLifecycleJob(t, rig, stackops.LifecycleStart, st, "", store, stackops.Caller{})
	for range 8 {
		clk.Tick(t, stackops.PollInterval)
	}
	job, err := rig.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the start function to return", done)

	if res.Code != stackops.CodeNetworkFull || res.FailedService != "lifecycle_web" {
		t.Fatalf("start = %+v, want network full at lifecycle_web", *res)
	}
	row := itemByID(t, job, "service:lifecycle_web")
	if row.State != jobs.ItemFailed || row.Code != stackops.CodeNetworkFull ||
		row.Result["network"] != "lifecycle_default" || row.Result["subnet"] != "10.0.2.0/24" {
		t.Errorf("web row = %+v, want failed as network_full naming lifecycle_default and 10.0.2.0/24", row)
	}
	requireNetworkTargetsByID(t, rig, "lifecycle_web", "lifecycle_default")
}

func TestStopAndStartNeverRemoveANetworkVolumeSecretOrConfig(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    volumes: [data:/data]\n    secrets: [tok]\n    configs: [cfg]\n" +
		"volumes:\n  data: {}\nsecrets:\n  tok:\n    file: tok.txt\nconfigs:\n  cfg:\n    file: cfg.txt\n"
	files := []stacks.File{{Path: "tok.txt", Content: []byte("s3cret")}, {Path: "cfg.txt", Content: []byte("config")}}
	st, store := rig.deployedStack(t, "keeps", compose, files)
	mark := len(rig.fake.Writes())

	if res, _ := rig.stop(t, st, store); !res.OK() {
		t.Fatalf("stop = %+v", res)
	}
	if res, _ := rig.start(t, st, store); !res.OK() {
		t.Fatalf("start = %+v", res)
	}

	for _, w := range rig.fake.Writes()[mark:] {
		for _, noun := range []string{"/networks", "/volumes", "/secrets", "/configs"} {
			if strings.Contains(w, noun) {
				t.Errorf("a stop and a start wrote %q: they must never touch a network, volume, secret or config", w)
			}
		}
	}
}

// seeded is a replicated service the stack st owns on the fake swarm, with a count and the digest it runs.
func seeded(st stackops.Stack, short string, replicas uint64, labels map[string]string) dockertest.FakeService {
	l := map[string]string{stacks.LabelSwarmNamespace: st.Name, stacks.LabelStackID: st.ID, stacks.LabelStackImage: "nginx:1.27-alpine"}
	for k, v := range labels {
		l[k] = v
	}
	return dockertest.FakeService{
		ID: "svc-" + short, Name: st.Name + "_" + short, Mode: "replicated", Running: replicas, Desired: replicas, Replicas: replicas,
		Image: "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64), Labels: l, Version: 3,
	}
}

func TestStopOfAStackStoppedInPartKeepsTheCountsAlreadyRemembered(t *testing.T) {
	t.Parallel()
	st := stackops.Stack{ID: "stack-partial", Name: "partial", Target: stacks.TargetSwarm}
	// A first stop got as far as api, which remembers 4. web still runs 3.
	rig := newSwarmRig(t, nil,
		seeded(st, "api", 0, map[string]string{stacks.LabelStoppedReplicas: "4"}),
		seeded(st, "web", 3, nil),
	)
	store := &lifecycleStore{}

	res, job := rig.stop(t, st, store)

	if !res.OK() || !slices.Equal(res.Scaled, []string{"partial_web"}) {
		t.Fatalf("stop = %+v %v, want only web scaled", res, itemStates(job))
	}
	if updates := rig.fake.ServiceUpdates(); len(updates) != 1 || updates[0].ServiceID != "svc-web" {
		t.Errorf("updates = %+v, want a single write, for web", updates)
	}
	after := rig.ownedNow(t, st)
	if got := after["partial_api"].Labels[stacks.LabelStoppedReplicas]; got != "4" {
		t.Errorf("api remembers %q after a second stop, want 4 untouched", got)
	}
	if got := after["partial_web"].Labels[stacks.LabelStoppedReplicas]; got != "3" {
		t.Errorf("web remembers %q, want 3", got)
	}
	if !store.isStopped(st.ID) {
		t.Errorf("the stack is not recorded as stopped once everything stopped")
	}
}

func TestStopThatFailsOnOneServiceStopsThereAndLeavesWhatStoppedStopped(t *testing.T) {
	t.Parallel()
	st := stackops.Stack{ID: "stack-failing", Name: "failing", Target: stacks.TargetSwarm}
	web := seeded(st, "web", 3, nil)
	web.UpdateError = "no capacity"
	// api sorts before web, so it is stopped first.
	rig := newSwarmRig(t, nil, seeded(st, "api", 1, nil), web)
	store := &lifecycleStore{}

	res, job := rig.stop(t, st, store)

	if res.OK() || res.FailedService != "failing_web" || res.Code != stackops.CodeServiceRejected {
		t.Fatalf("stop = %+v, want it failed at web as rejected", res)
	}
	if job.State == jobs.JobDone {
		t.Errorf("job = %s %v, want it not done", job.State, itemStates(job))
	}
	if !slices.Equal(res.Scaled, []string{"failing_api"}) {
		t.Errorf("scaled = %v, want api stopped before the failure", res.Scaled)
	}
	if store.isStopped(st.ID) {
		t.Errorf("a stop that failed recorded the stack as stopped")
	}
	if got := rig.ownedNow(t, st)["failing_api"]; got.Replicas != 0 {
		t.Errorf("api replicas = %d, want what stopped to stay stopped", got.Replicas)
	}
}

const withGlobal = "services:\n  web:\n    image: nginx:1.27-alpine\n  agent:\n    image: nginx:1.28-alpine\n    deploy:\n      mode: global\n"

func TestStopRemovesAGlobalServiceAndStartCreatesItAgainFromTheLastRevisionWithItsPinnedImage(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, store := rig.deployedStack(t, "globals", withGlobal, nil)
	pinned := store.pins["globals_agent"]
	if !strings.Contains(pinned, "@sha256:") {
		t.Fatalf("the revision pinned %q, want a digest", pinned)
	}

	stopRes, _ := rig.stop(t, st, store)
	if !stopRes.OK() || !slices.Equal(stopRes.Removed, []string{"globals_agent"}) {
		t.Fatalf("stop = %+v, want the global service removed", stopRes)
	}
	if _, gone := rig.ownedNow(t, st)["globals_agent"]; gone {
		t.Fatalf("the global service is still there after a stop")
	}
	creates := len(rig.fake.ServiceCreates())

	check, err := rig.deps.CheckStart(testsupport.Context(t), rig.envID, st, store, rootCaller(t, rig))
	if err != nil || !check.Recreates || check.Policy.Refused {
		t.Fatalf("CheckStart = %+v, %v, want a start that recreates and is allowed", check, err)
	}
	res, job := rig.start(t, st, store)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("start = %+v %v, want it done", res, itemStates(job))
	}
	if !slices.Equal(res.Recreated, []string{"globals_agent"}) {
		t.Errorf("recreated = %v, want the global service", res.Recreated)
	}
	made := rig.fake.ServiceCreates()[creates:]
	if len(made) != 1 {
		t.Fatalf("start created %d services, want 1", len(made))
	}
	if got := made[0].Spec.TaskTemplate.ContainerSpec.Image; got != pinned {
		t.Errorf("recreated image = %q, want the pinned %q", got, pinned)
	}
	if made[0].Spec.Mode.Global == nil {
		t.Errorf("recreated mode is not global: %+v", made[0].Spec.Mode)
	}
	if check, _ := rig.deps.CheckStart(testsupport.Context(t), rig.envID, st, store, rootCaller(t, rig)); check.Recreates {
		t.Errorf("CheckStart recreates after the service was made again")
	}
}

func TestStartOfAStoppedStackThatUsesAnEnvFileRecreatesItsGlobalServiceWithTheFilesValues(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n  agent:\n    image: nginx:1.28-alpine\n    env_file: app.env\n    deploy:\n      mode: global\n"
	st, store := rig.deployedStack(t, "envglobal", compose, []stacks.File{{Path: "app.env", Content: []byte("FROM_FILE=1\n")}})
	if stopRes, _ := rig.stop(t, st, store); !stopRes.OK() || !slices.Equal(stopRes.Removed, []string{"envglobal_agent"}) {
		t.Fatalf("stop = %+v, want the global service removed", stopRes)
	}
	creates := len(rig.fake.ServiceCreates())

	res, job := rig.start(t, st, store)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("start = %+v %v, want it done", res, itemStates(job))
	}
	if !slices.Equal(res.Recreated, []string{"envglobal_agent"}) {
		t.Errorf("recreated = %v, want the global service", res.Recreated)
	}
	made := rig.fake.ServiceCreates()[creates:]
	if len(made) != 1 {
		t.Fatalf("start created %d services, want 1", len(made))
	}
	if env := made[0].Spec.TaskTemplate.ContainerSpec.Env; !slices.Contains(env, "FROM_FILE=1") {
		t.Errorf("recreated env = %v, want FROM_FILE=1 from the env file", env)
	}
}

func TestStartMakingAServiceAgainIsJudgedByTheHostAccessPolicyForTheCaller(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	compose := "services:\n  agent:\n    image: nginx:1.28-alpine\n    deploy:\n      mode: global\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"
	st, store := rig.deployedStack(t, "hostaccess", compose, nil)
	if res, _ := rig.stop(t, st, store); !res.OK() {
		t.Fatalf("stop = %+v", res)
	}
	creates := len(rig.fake.ServiceCreates())

	operator := stackops.Caller{Principal: rig.principal(t, "operator-1", "operator"), Actor: "operator-1"}
	res, job := rig.lifecycleRun(t, stackops.LifecycleStart, st, store, operator)

	if res.OK() || res.Code != stackops.CodeNeedsAdmin {
		t.Fatalf("start = %+v %v, want it refused as needs_admin", res, itemStates(job))
	}
	if got := len(rig.fake.ServiceCreates()); got != creates {
		t.Errorf("a refused start created %d services", got-creates)
	}
}

func TestStartOfAStackThatWasNeverDeployedHasNoVersionToBringBack(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, _ := stackInput(t, "never", lifecycleTwo, nil)

	res, job := rig.start(t, st, &lifecycleStore{})

	if res.OK() || res.Code != stackops.CodeNoRevision {
		t.Fatalf("start = %+v %v, want no_revision", res, itemStates(job))
	}
}

func TestStartLeavesAServiceOfAnotherStackAlone(t *testing.T) {
	t.Parallel()
	foreign := dockertest.FakeService{
		ID: "svc-foreign", Name: "shared_web", Mode: "replicated", Running: 2, Desired: 2, Replicas: 2,
		Image:  "nginx:1",
		Labels: map[string]string{stacks.LabelSwarmNamespace: "shared", stacks.LabelStackID: "someone-else", stacks.LabelStoppedReplicas: "5"},
	}
	rig := newSwarmRig(t, nil, foreign)
	st := stackops.Stack{ID: "mine", Name: "shared", Target: stacks.TargetSwarm}
	store := &lifecycleStore{}

	stopRes, _ := rig.stop(t, st, store)
	startRes, _ := rig.start(t, st, store)

	if len(stopRes.Scaled)+len(stopRes.Removed) != 0 {
		t.Errorf("stop touched %+v of another stack's service", stopRes)
	}
	if len(startRes.Scaled) != 0 {
		t.Errorf("start touched %+v of another stack's service", startRes)
	}
	if len(rig.fake.ServiceUpdates()) != 0 {
		t.Errorf("an update was sent for a service the stack does not own")
	}
}

func TestReplicasSpecChangesTheCountAndTheLabelAndNothingElse(t *testing.T) {
	t.Parallel()
	spec := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "s_web", Labels: map[string]string{"keep": "me"}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "nginx@sha256:" + strings.Repeat("a", 64)}},
	}
	three := uint64(3)
	spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &three}
	raw := mustJSON(t, spec)

	stopped, err := stackops.ReplicasSpec(raw, 0, "3")
	if err != nil {
		t.Fatal(err)
	}
	var got swarm.ServiceSpec
	mustUnmarshal(t, stopped, &got)
	if *got.Mode.Replicated.Replicas != 0 || got.Labels[stacks.LabelStoppedReplicas] != "3" || got.Labels["keep"] != "me" {
		t.Errorf("stopped spec = %+v", got)
	}
	if got.TaskTemplate.ContainerSpec.Image != spec.TaskTemplate.ContainerSpec.Image || got.Name != "s_web" {
		t.Errorf("the stopped spec changed more than the count and the label: %+v", got)
	}

	back, err := stackops.ReplicasSpec(stopped, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	var again swarm.ServiceSpec
	mustUnmarshal(t, back, &again)
	if _, left := again.Labels[stacks.LabelStoppedReplicas]; left || *again.Mode.Replicated.Replicas != 3 || again.Labels["keep"] != "me" {
		t.Errorf("restored spec = %+v", again)
	}
}

func TestStandaloneStopAndStartRunTheSDKOverTheLastRevisionsProject(t *testing.T) {
	t.Parallel()
	st := stackops.Stack{ID: "stack-standalone", Name: "plain", Target: stacks.TargetStandalone}
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    secrets: [tok]\nsecrets:\n  tok:\n    file: tok.txt\n"
	files := []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}, {Path: "tok.txt", Content: []byte("s3cret")}}
	pinned := "nginx:1.27-alpine@sha256:" + strings.Repeat("c", 64)
	store := &lifecycleStore{
		rev:  stackops.Snapshot{ID: "rev-1", StackID: st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood, Copy: stacks.WorkingCopy{Files: files}},
		have: true, pins: map[string]string{"web": pinned},
	}
	var calls []string
	var sawSecret bool
	rig := newSwarmRig(t, nil)
	rig.deps.Writable = func(context.Context, string, string) error { return nil }
	rig.deps.StopProject = func(_ context.Context, _ *dockerx.Client, p *types.Project) error {
		calls = append(calls, "stop "+p.Name)
		return nil
	}
	rig.deps.StartProject = func(_ context.Context, _ *dockerx.Client, p *types.Project) error {
		calls = append(calls, "start "+p.Name)
		sawSecret = strings.Contains(p.Services["web"].Image, "@sha256:") && p.Environment[stacks.SecretEnvPrefix+"tok"] == "s3cret"
		return nil
	}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	run := func(mode string) (stackops.LifecycleResult, jobs.Job) {
		t.Helper()
		return rig.lifecycleRunOn(t, mode, st, node, store, stackops.Caller{})
	}

	stopRes, stopJob := run(stackops.LifecycleStop)
	startRes, startJob := run(stackops.LifecycleStart)

	if !stopRes.OK() || stopJob.State != jobs.JobDone || !startRes.OK() || startJob.State != jobs.JobDone {
		t.Fatalf("stop = %+v %v, start = %+v %v, want both done", stopRes, itemStates(stopJob), startRes, itemStates(startJob))
	}
	if !slices.Equal(calls, []string{"stop plain", "start plain"}) {
		t.Errorf("SDK calls = %v, want a stop then a start of the project", calls)
	}
	if !sawSecret {
		t.Errorf("the project a start hands the SDK lacks the pinned image or the stack file's secret content")
	}
	if store.isStopped(st.ID) {
		t.Errorf("the stack is recorded as stopped after a start")
	}
}

// standaloneStopStart stops and starts a standalone stack whose last good revision holds files, and
// returns the project the SDK stand-in received on the start. Both jobs must be done.
func standaloneStopStart(t *testing.T, files []stacks.File) *types.Project {
	t.Helper()
	st := stackops.Stack{ID: "stack-standalone", Name: "plain", Target: stacks.TargetStandalone}
	store := &lifecycleStore{
		rev:  stackops.Snapshot{ID: "rev-1", StackID: st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood, Copy: stacks.WorkingCopy{Files: files}},
		have: true, pins: map[string]string{"web": "nginx:1.27-alpine@sha256:" + strings.Repeat("c", 64)},
	}
	var started *types.Project
	rig := newSwarmRig(t, nil)
	rig.deps.Writable = func(context.Context, string, string) error { return nil }
	rig.deps.StopProject = func(context.Context, *dockerx.Client, *types.Project) error { return nil }
	rig.deps.StartProject = func(_ context.Context, _ *dockerx.Client, p *types.Project) error {
		started = p
		return nil
	}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	stopRes, stopJob := rig.lifecycleRunOn(t, stackops.LifecycleStop, st, node, store, stackops.Caller{})
	startRes, startJob := rig.lifecycleRunOn(t, stackops.LifecycleStart, st, node, store, stackops.Caller{})
	if !stopRes.OK() || stopJob.State != jobs.JobDone || !startRes.OK() || startJob.State != jobs.JobDone {
		t.Fatalf("stop = %+v %v, start = %+v %v, want both done", stopRes, itemStates(stopJob), startRes, itemStates(startJob))
	}
	if started == nil {
		t.Fatal("the SDK was never asked to start the project")
	}
	return started
}

func TestStandaloneStopAndStartOfAStackThatUsesAnEnvFile(t *testing.T) {
	t.Parallel()
	project := standaloneStopStart(t, []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("services:\n  web:\n    image: nginx:1.27-alpine\n    env_file: app.env\n")},
		{Path: "app.env", Content: []byte("FROM_FILE=1\n")},
	})
	svc := project.Services["web"]
	if v := svc.Environment["FROM_FILE"]; v == nil || *v != "1" || len(svc.EnvFiles) != 0 {
		t.Errorf("service = environment %v, env files %+v, want FROM_FILE=1 and no env files", svc.Environment, svc.EnvFiles)
	}
}

func TestStandaloneStopAndStartOfAServiceWithBuildBesideItsImage(t *testing.T) {
	t.Parallel()
	project := standaloneStopStart(t, []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("services:\n  web:\n    image: nginx:1.27-alpine\n    build: .\n")},
	})
	if project.Services["web"].Build != nil {
		t.Error("the project handed to the SDK keeps a build section")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// TestStopStartAdoptedStack: an adopted stack stops and starts before its first deploy, when its services carry the
// namespace alone, as it does after (plan 08-18, D-14). A service of the namespace that is not the stack's is left
// alone: one that another stack's ID names, and, once the first deploy is done, one that carries no ID.
func TestStopStartAdoptedStack(t *testing.T) {
	t.Parallel()
	alien := dockertest.FakeService{
		ID: "svc-alien", Name: "shop_alien", Mode: "replicated", Image: "nginx:1", Running: 2, Desired: 2, Replicas: 2,
		Labels: map[string]string{stacks.LabelSwarmNamespace: "shop", stacks.LabelStackID: "someone-else"},
	}
	var base dockertest.FakeConfig
	a := adoptRig(t, corpusRigTuned(t, "deploy.yaml", nil, func(c *dockertest.FakeConfig) { base = *c }))
	rig, st := a.rig, a.stack
	// The other stack's service appears after the adoption (one there at adoption would be taken for the stale ID of
	// the stack's own, D-11).
	base.ServiceRows = append(slices.Clone(base.ServiceRows), alien)
	rig.fake.Reconfigure(base)
	if !st.Adoption.Pending {
		t.Fatalf("the stack is not pending its first deploy: %+v", st.Adoption)
	}
	before := rig.ownedNow(t, st)

	stopRes, stopJob := rig.stop(t, st, a.repo)

	if !stopRes.OK() || stopJob.State != jobs.JobDone {
		t.Fatalf("stop = %+v %v, want it done", stopRes, itemStates(stopJob))
	}
	if len(stopRes.Scaled) == 0 {
		t.Fatalf("stop scaled %v, want every replicated service of the adopted stack", stopRes.Scaled)
	}
	for name, svc := range rig.ownedNow(t, st) {
		if name == "shop_alien" {
			if svc.Replicas != 2 || svc.Labels[stacks.LabelStoppedReplicas] != "" {
				t.Errorf("the other stack's service was changed: replicas %d labels %v", svc.Replicas, svc.Labels)
			}
			continue
		}
		if before[name].Replicas == 0 {
			continue // a global service is removed, not scaled
		}
		if svc.Replicas != 0 || svc.Labels[stacks.LabelStoppedReplicas] != strconv.FormatUint(before[name].Replicas, 10) {
			t.Errorf("%s: replicas %d label %q after stop, want 0 and %d remembered", name, svc.Replicas, svc.Labels[stacks.LabelStoppedReplicas], before[name].Replicas)
		}
	}

	startRes, startJob := rig.start(t, st, a.repo)

	if !startRes.OK() || startJob.State != jobs.JobDone {
		t.Fatalf("start = %+v %v, want it done", startRes, itemStates(startJob))
	}
	for name, svc := range rig.ownedNow(t, st) {
		if name == "shop_alien" {
			continue
		}
		if svc.Replicas != before[name].Replicas {
			t.Errorf("%s: replicas %d after start, want the %d it had", name, svc.Replicas, before[name].Replicas)
		}
		if _, left := svc.Labels[stacks.LabelStoppedReplicas]; left {
			t.Errorf("%s still carries the stopped-replicas label after start", name)
		}
	}
	for _, u := range rig.fake.ServiceUpdates() {
		if u.ServiceID == "svc-alien" {
			t.Errorf("an update was sent for the other stack's service: %+v", u)
		}
	}

	// Once the first deploy is done, the services that carry no ID are not the stack's.
	if err := a.repo.ClearAdoptionPending(testsupport.Context(t), st.ID); err != nil {
		t.Fatal(err)
	}
	done, err := a.repo.Stack(testsupport.Context(t), rig.envID, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	updates := len(rig.fake.ServiceUpdates())
	res, _ := rig.stop(t, done, a.repo)
	if len(res.Scaled)+len(res.Removed) != 0 || len(rig.fake.ServiceUpdates()) != updates {
		t.Errorf("stop after the first deploy touched %+v: a service with no ID is another owner's", res)
	}
}

// TestStartSendsThePlatformsTheDeployedRevisionStoredAndAsksNoRegistry (G-08-4, T-07-93): a start that makes a service
// again sends the platforms the last good revision recorded for it, which here differ from the ones the service had, and
// asks no registry.
func TestStartSendsThePlatformsTheDeployedRevisionStoredAndAsksNoRegistry(t *testing.T) {
	t.Parallel()
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}
	rig := newSwarmRig(t, nil)
	st, store := rig.deployedStack(t, "globals", withGlobal, nil)
	store.rev.Platforms = map[string][]swarm.Platform{"globals_agent": {arm}}
	if res, _ := rig.stop(t, st, store); !res.OK() {
		t.Fatalf("stop = %+v", res)
	}
	creates, lookups := len(rig.fake.ServiceCreates()), len(rig.fake.DistributionRefs())

	res, job := rig.start(t, st, store)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("start = %+v %v, want it done", res, itemStates(job))
	}
	made := rig.fake.ServiceCreates()[creates:]
	if len(made) != 1 {
		t.Fatalf("start created %d services, want 1", len(made))
	}
	pl := made[0].Spec.TaskTemplate.Placement
	if pl == nil || !slices.Equal(pl.Platforms, []swarm.Platform{arm}) {
		t.Errorf("recreated placement = %+v, want the platforms the revision stored, [linux/arm64]", pl)
	}
	if n := len(rig.fake.DistributionRefs()); n != lookups {
		t.Errorf("a start asked a registry %d times", n-lookups)
	}
}
