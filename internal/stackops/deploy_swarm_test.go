// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The swarm deploy runs against a fake swarm manager behind the real monitor, aggregator and
// cluster reader.

type swarmRig struct {
	fake  *dockertest.Fake
	deps  stackops.Deps
	envID string
	hub   *jobs.Hub
	store *store.Store
	// addr is the environment's address reader, which a deploy reads the small stack subnet setting through.
	addr *envs.AddressSpaces
	// mon is the monitor of the environment, which a test stops to make the engine unreachable.
	mon *envs.Monitor
}

func newSwarmRig(t *testing.T, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) *swarmRig {
	t.Helper()
	cfg := dockertest.FakeConfig{
		Hostname: "mgr-1", Version: "29.1.0", SwarmState: "active", SwarmControl: true,
		NodeRows:    []dockertest.FakeNode{{ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, EngineVersion: "29.1.0", OS: "linux"}},
		ServiceRows: services,
		// The services this deploy writes settle at once; a test that wants one to be slow or stuck
		// moves it with Fake.SetTaskState.
		TasksFollowWrites: true,
	}
	if tune != nil {
		tune(&cfg)
	}
	fake := dockertest.NewFake(t, cfg)
	r := newRigOver(t, fake.SocketPath())
	r.fake = fake
	return r
}

// newRigOver is the rig over the engine behind a unix socket: the environment is the server's own local
// socket, as the product only ever builds one. fake is left nil; newSwarmRig sets it.
func newRigOver(t *testing.T, socket string) *swarmRig {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)
	mon := envs.NewMonitor(slog.New(slog.DiscardHandler), nil, nil)
	t.Cleanup(mon.StopAll)
	svc := envs.NewService(st, aud, mon, "unix://"+socket, "0123456789ab", time.Now)
	snaps := envs.NewSnapshots(mon, 5*time.Second, time.Now)
	agg := envs.NewAggregator(svc, snaps, nil, time.Now)
	t.Cleanup(agg.Close)
	clusters := envs.NewClusters(svc, snaps, envs.ClusterOptions{Now: time.Now})
	svc.WithClusters(clusters)
	env, err := svc.AddLocal(context.Background(), audit.SystemCLI, "", false, "", "")
	if err != nil {
		t.Fatalf("AddLocal: %v", err)
	}
	if _, err := mon.AwaitStatus(testsupport.Context(t), env.ID, func(s envs.Status) bool { return s.State == envs.StateOnline }); err != nil {
		t.Fatalf("waiting for the environment to come online: %v", err)
	}
	hub := jobs.New(jobs.Options{})
	t.Cleanup(hub.Close)
	addr := envs.NewAddressSpaces(svc, agg, clusters, envs.AddressSpaceOptions{Now: time.Now})
	return &swarmRig{
		envID: env.ID, hub: hub, store: st, addr: addr, mon: mon,
		deps: stackops.Deps{
			Aggregator: agg, Clusters: clusters, Checker: rbac.NewChecker(st), AddressSpaces: addr, Log: slog.New(slog.DiscardHandler),
			// The read-back of a swarm network waits on no clock: each wait ends at once.
			AllocationWait: dockerx.AllocationWait{After: func(time.Duration) <-chan time.Time {
				c := make(chan time.Time, 1)
				c <- time.Time{}
				return c
			}},
		},
	}
}

// stackInput loads a compose file for the swarm and returns the stack and the input of a deploy
// of it.
func stackInput(t *testing.T, name, compose string, files []stacks.File, vars ...stacks.Var) (stackops.Stack, stackops.SwarmDeployInput) {
	t.Helper()
	all := append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...)
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: name, Target: stacks.TargetSwarm, Copy: stacks.WorkingCopy{Files: all, Vars: vars},
	})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, v := range vars {
		values[v.Name] = v.Value
	}
	st := stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetSwarm}
	return st, stackops.SwarmDeployInput{
		Stack: st, Project: loaded.Project, Files: stacks.NewFileSet(all), Vars: values,
		Copy: stacks.WorkingCopy{Files: all, Vars: vars},
	}
}

// checkedStackInput is stackInput over the check pipeline: the project is the one stacks.Deployable
// returns, as the project of a stack that was deployed always is. A refusal fails the test; the callers
// of stackInput that feed the converter files the check would stop earlier keep using stackInput.
func checkedStackInput(t *testing.T, name, compose string, files []stacks.File, vars ...stacks.Var) (stackops.Stack, stackops.SwarmDeployInput) {
	t.Helper()
	all := append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...)
	loaded, err := stacks.Deployable(context.Background(), stacks.CheckInput{
		Name: name, Target: stacks.TargetSwarm, Copy: stacks.WorkingCopy{Files: all, Vars: vars},
	})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, v := range vars {
		values[v.Name] = v.Value
	}
	st := stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetSwarm}
	return st, stackops.SwarmDeployInput{
		Stack: st, Project: loaded.Project, Files: stacks.NewFileSet(all), Vars: values,
		Copy: stacks.WorkingCopy{Files: all, Vars: vars},
	}
}

// asCaller gives a deploy a caller when the test did not name one: an instance admin, who may deploy
// anything, so the policy check the deploy makes before it writes passes.
func (r *swarmRig) asCaller(t *testing.T, in stackops.SwarmDeployInput) stackops.SwarmDeployInput {
	t.Helper()
	if in.Caller.Principal == nil {
		in.Caller = stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"}
	}
	in.EnvID = r.envID
	return in
}

// starterPlan is the starter compose file as a stack and the input of its deploy.
func starterPlan(t *testing.T, name string) (stackops.Stack, stackops.SwarmDeployInput) {
	t.Helper()
	return stackInput(t, name, stacks.StarterCompose, nil)
}

// deployItems is the items a deploy job starts with, as the API starts it.
func deployItems() []jobs.ItemSpec { return stackops.DeployItemSpecs() }

// itemByID finds an item of a job.
func itemByID(t *testing.T, j jobs.Job, id string) jobs.Item {
	t.Helper()
	for _, it := range j.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("job has no item %q: %v", id, itemStates(j))
	return jobs.Item{}
}

// deploy runs DeploySwarm as a job and returns its result and the job as it ended.
func (r *swarmRig) deploy(t *testing.T, in stackops.SwarmDeployInput) (stackops.DeployResult, jobs.Job) {
	t.Helper()
	in = r.asCaller(t, in)
	var res stackops.DeployResult
	// The job reads as ended when its last item is finished, which DeploySwarm does before it
	// returns; res is therefore read only after the job function has closed done.
	done := make(chan struct{})
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: r.envID, Owner: "user:test",
		Items: deployItems(),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		defer close(done)
		res = r.deps.DeploySwarm(ctx, in, rep)
	})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := r.hub.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the deploy function to return", done)
	return res, ended
}

func itemStates(j jobs.Job) map[string]string {
	out := map[string]string{}
	for _, it := range j.Items {
		out[it.ID] = it.State
		if it.Code != "" {
			out[it.ID] += ":" + it.Code
		}
	}
	return out
}

func TestDeploySwarmCreatesTheNetworkAndTheServiceWithTheOwnershipLabels(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)

	if !res.OK() || res.Networks != 1 || res.Services != 1 || res.Updated != 0 {
		t.Fatalf("result = %+v, want one network and one service created", res)
	}
	if job.State != jobs.JobDone {
		t.Errorf("job = %s %v, want done", job.State, itemStates(job))
	}
	nets := rig.fake.NetworkCreates()
	if len(nets) != 1 || nets[0].Name != "tracer_default" || nets[0].Driver != "overlay" || nets[0].Labels["tierplan.stack.id"] != "stack-tracer" ||
		nets[0].Labels["com.docker.stack.namespace"] != "tracer" {
		t.Errorf("network creates = %+v", nets)
	}
	svcs := rig.fake.ServiceCreates()
	if len(svcs) != 1 {
		t.Fatalf("service creates = %+v, want one", svcs)
	}
	spec := svcs[0].Spec
	if spec.Name != "tracer_web" || spec.Labels["com.docker.stack.namespace"] != "tracer" || spec.Labels["tierplan.stack.id"] != "stack-tracer" ||
		spec.Labels["com.docker.stack.image"] != "nginx:1.27-alpine" || spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27-alpine@"+dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine") {
		t.Errorf("service spec = %+v", spec)
	}
	if got := rig.fake.Writes(); !slices.Equal(got, []string{"POST /networks/create", "POST /services/create"}) {
		t.Errorf("writes = %v, want the network then the service, each once", got)
	}
}

func TestDeploySwarmUpdatesAnOwnedServiceAtTheVersionItRead(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "tierplan.stack.id": "stack-tracer"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "tracer_default", Driver: "overlay", Scope: "swarm", Labels: owned}}
	}, dockertest.FakeService{ID: "svc-web", Name: "tracer_web", Mode: "replicated", Image: "nginx:1.26", Version: 7, Labels: owned})
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)

	if !res.OK() || res.Networks != 0 || res.Services != 0 || res.Updated != 1 {
		t.Fatalf("result = %+v, want one service updated and nothing created", res)
	}
	if job.State != jobs.JobDone {
		t.Errorf("job = %s %v", job.State, itemStates(job))
	}
	ups := rig.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].ServiceID != "svc-web" || ups[0].Version != "7" || ups[0].Spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27-alpine@"+dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine") {
		t.Errorf("updates = %+v, want one at version 7 with the new image pinned to its digest", ups)
	}
	if got := rig.fake.Writes(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /services/svc-web/update") {
		t.Errorf("writes = %v, want only the service update (the network exists)", got)
	}
}

func TestDeploySwarmRefusesANameAnotherStackOwns(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil, dockertest.FakeService{
		ID: "svc-x", Name: "tracer_web", Mode: "replicated", Image: "nginx:1", Version: 1,
		Labels: map[string]string{"com.docker.stack.namespace": "tracer"}, // made by docker stack deploy, not by TierPlan
	})
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)

	if res.Code != stackops.CodeNameClash {
		t.Fatalf("result = %+v, want stack_name_clash", res)
	}
	if got := itemStates(job); got[stackops.ItemCheck] != "failed:stack_name_clash" || got[stackops.ItemNetworks] != "skipped:not_run" || got[stackops.ItemServices] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a foreign stack must not be touched", w)
	}
}

func TestDeploySwarmStopsAtTheFirstServiceTheManagerRejects(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "secret not found: db-password" })
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)

	if res.Code != stackops.CodeServiceRejected || res.FailedService != "tracer_web" {
		t.Fatalf("result = %+v, want service_rejected at tracer_web", res)
	}
	got := itemStates(job)
	if got[stackops.ItemCheck] != "done" || got[stackops.ItemNetworks] != "done" || got[stackops.ItemServices] != "failed:service_rejected" {
		t.Errorf("items = %v", got)
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job state = %s, want partial", job.State)
	}
	for _, it := range job.Items {
		if strings.Contains(it.Message, "db-password") {
			t.Errorf("item %s message %q holds the manager's own words", it.ID, it.Message)
		}
	}
}

// Whether a failed deploy wrote decides whether an image update puts the stack back (WR-13). A manager that refuses the spec has
// changed nothing, so there is nothing to restore; one whose answer was lost may have taken it, so the stack is restored.
func TestDeploySwarmWhoseServiceWriteWasRefusedHasNotWritten(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "secret not found: db-password" })
	_, in := starterPlan(t, "tracer")
	res, _ := rig.deploy(t, in)

	if res.Code != stackops.CodeServiceRejected || res.Wrote || res.NeedsRestore() {
		t.Errorf("result = %+v, want service_rejected with nothing written and nothing to restore", res)
	}
}

func TestDeploySwarmWhoseServiceWriteLostItsAnswerMayHaveWritten(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.DropServiceWrites = true })
	_, in := starterPlan(t, "tracer")
	res, _ := rig.deploy(t, in)

	if res.OK() || res.Code == stackops.CodeServiceRejected || !res.Wrote || !res.NeedsRestore() {
		t.Errorf("result = %+v, want an engine failure that may have written and needs a restore", res)
	}
}

func TestDeploySwarmReportsAnAgentRefusalWithoutTheEnginesWords(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.PolicyDeny = true })
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeAgentPolicyDenied {
		t.Fatalf("result = %+v, want agent_policy_denied", res)
	}
	if got := itemStates(job); got[stackops.ItemCheck] != "failed:agent_policy_denied" {
		t.Errorf("items = %v", got)
	}
}

// A refusal the agent words as host policy (G-09-8b) ends as agent_host_policy, the API's wire code for
// it, with the sentence that names host access and the agent setting; the plain refusal above stays
// agent_policy_denied.
func TestDeploySwarmNamesAHostPolicyRefusal(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.ServiceCreateError = "agent_policy_denied: host_policy: POST /v1.56/services/create"
	})
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeAgentHostPolicy || res.FailedService != "tracer_web" {
		t.Fatalf("result = %+v, want agent_host_policy at tracer_web", res)
	}
	if got := itemStates(job); got[stackops.ItemServices] != "failed:agent_host_policy" {
		t.Errorf("items = %v", got)
	}
	row := itemByID(t, job, "service:tracer_web")
	if row.Code != stackops.CodeAgentHostPolicy || row.Message != stackops.CodeMessage(stackops.CodeAgentHostPolicy) {
		t.Errorf("service row = %+v, want the host-policy code and its sentence", row)
	}
	if !strings.Contains(row.Message, "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES") {
		t.Errorf("message = %q, want it to name the agent setting", row.Message)
	}
}

func TestDeploySwarmCallsOnDoneBeforeTheJobReadsAsFinished(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := starterPlan(t, "tracer")
	var seen atomic.Pointer[stackops.DeployResult]
	var stateInDone atomic.Value
	in = rig.asCaller(t, in)
	var jobID atomic.Value
	in.OnDone = func(res stackops.DeployResult) {
		seen.Store(&res)
		if id, ok := jobID.Load().(string); ok {
			if j, found := rig.hub.Get(id); found {
				stateInDone.Store(j.State)
			}
		}
	}
	started := make(chan struct{})
	job, err := rig.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: rig.envID, Owner: "user:test",
		Items: deployItems(),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		<-started
		rig.deps.DeploySwarm(ctx, in, rep)
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID.Store(job.ID)
	close(started)
	if _, err := rig.hub.Wait(testsupport.Context(t), job.ID); err != nil {
		t.Fatal(err)
	}
	if seen.Load() == nil {
		t.Fatal("OnDone was not called before the job read as finished")
	}
	if state, _ := stateInDone.Load().(string); state != jobs.JobRunning {
		t.Errorf("job state inside OnDone = %q, want running: the job must not read as finished before it is recorded", state)
	}
}

func TestDeploySwarmOfACancelledContextWritesNothingAndSaysCancelled(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := starterPlan(t, "tracer")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in = rig.asCaller(t, in)
	var res stackops.DeployResult
	done := make(chan struct{})
	job, err := rig.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: rig.envID, Owner: "user:test",
		Items: deployItems(),
	}, func(_ context.Context, r *jobs.Reporter) {
		defer close(done)
		res = rig.deps.DeploySwarm(ctx, in, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := rig.hub.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the deploy function to return", done)
	if res.Code != stackops.CodeCancelled {
		t.Errorf("result = %+v, want cancelled", res)
	}
	if got := itemStates(ended); got[stackops.ItemCheck] != "cancelled:cancelled" {
		t.Errorf("items = %v", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

// tokCompose is a stack with one service that uses one secret read from a stack file.
const tokCompose = `services:
  web:
    image: nginx:1.27-alpine
    secrets:
      - tok
secrets:
  tok:
    file: tok.txt
`

func tokFile(content string) []stacks.File {
	return []stacks.File{{Path: "tok.txt", Content: []byte(content), Secret: true}}
}

// secretNames lists the names of the swarm's secrets as a manager reports them now.
func (r *swarmRig) secretNames(t *testing.T) []string {
	t.Helper()
	var names []string
	err := r.deps.Clusters.ClusterRead(testsupport.Context(t), r.envID, func(c *dockerx.Client) error {
		rows, e := c.SecretList(testsupport.Context(t))
		for _, row := range rows {
			names = append(names, row.Name)
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestDeploySwarmCreatesAHashedSecretFromAStackFile(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", tokCompose, tokFile("s3cret"))
	res, job := rig.deploy(t, in)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	name, hash := stacks.ObjectName("web", "tok", []byte("s3cret"))
	creates := rig.fake.SecretCreates()
	if len(creates) != 1 {
		t.Fatalf("secret creates = %+v, want one", creates)
	}
	got := creates[0]
	wantLabels := map[string]string{
		"com.docker.stack.namespace": "web", "tierplan.stack.id": "stack-web",
		"tierplan.stack.object": "tok", "tierplan.stack.object.hash": hash,
	}
	if got.Name != name || got.DataBytes != len("s3cret") || !maps.Equal(got.Labels, wantLabels) {
		t.Errorf("secret create = %+v, want %s with labels %v", got, name, wantLabels)
	}
	if !slices.Equal(res.CreatedObjects, []string{name}) {
		t.Errorf("CreatedObjects = %v, want [%s]", res.CreatedObjects, name)
	}
	svcs := rig.fake.ServiceCreates()
	if len(svcs) != 1 {
		t.Fatalf("service creates = %+v", svcs)
	}
	refs := svcs[0].Spec.TaskTemplate.ContainerSpec.Secrets
	if len(refs) != 1 || refs[0].SecretName != name || refs[0].SecretID == "" {
		t.Errorf("the service's secret references = %+v, want the hashed name with the created ID", refs)
	}
	if got := itemStates(job); got[stackops.ItemObjects] != "done" || got[stackops.ItemCleanup] != "done" {
		t.Errorf("items = %v", got)
	}
}

func TestRedeployWithUnchangedSecretCreatesNothing(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", tokCompose, tokFile("s3cret"))
	if res, _ := rig.deploy(t, in); !res.OK() {
		t.Fatalf("first deploy = %+v", res)
	}
	before := len(rig.fake.Writes())

	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("second deploy = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	if n := len(rig.fake.SecretCreates()); n != 1 {
		t.Errorf("secret creates = %d, want still one", n)
	}
	writes := rig.fake.Writes()[before:]
	if len(writes) != 1 || !strings.HasPrefix(writes[0], "POST /services/") || !strings.Contains(writes[0], "/update") {
		t.Errorf("second deploy writes = %v, want only the service update: no secret create, no secret remove", writes)
	}
	if len(res.CreatedObjects) != 0 || len(res.RemovedObjects) != 0 || len(res.LeftInPlace) != 0 {
		t.Errorf("result = %+v, want no object created, removed or left", res)
	}
}

func TestChangedSecretRotatesAndRemovesTheOldOne(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, first := stackInput(t, "web", tokCompose, tokFile("one"))
	if res, _ := rig.deploy(t, first); !res.OK() {
		t.Fatalf("first deploy = %+v", res)
	}
	oldName, _ := stacks.ObjectName("web", "tok", []byte("one"))
	newName, _ := stacks.ObjectName("web", "tok", []byte("two"))
	before := len(rig.fake.Writes())

	_, second := stackInput(t, "web", tokCompose, tokFile("two"))
	res, job := rig.deploy(t, second)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("second deploy = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	if !slices.Equal(res.CreatedObjects, []string{newName}) || !slices.Equal(res.RemovedObjects, []string{oldName}) {
		t.Errorf("created %v removed %v, want [%s] and [%s]", res.CreatedObjects, res.RemovedObjects, newName, oldName)
	}
	// The new secret first, then the service moves to it, and only then the old secret goes.
	writes := rig.fake.Writes()[before:]
	if len(writes) != 3 || writes[0] != "POST /secrets/create" || !strings.Contains(writes[1], "/services/") || !strings.HasPrefix(writes[2], "DELETE /secrets/") {
		t.Errorf("second deploy writes = %v, want secret create, service update, secret remove", writes)
	}
	ups := rig.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("service updates = %+v", ups)
	}
	refs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets
	if len(refs) != 1 || refs[0].SecretName != newName {
		t.Errorf("the updated service's secret references = %+v, want %s", refs, newName)
	}
	if got := rig.secretNames(t); !slices.Equal(got, []string{newName}) {
		t.Errorf("secrets on the swarm = %v, want only %s: nothing left behind", got, newName)
	}
}

// poolsCompose is a stack whose network declares every setting a network create can carry.
const poolsCompose = `services:
  web:
    image: nginx:1.27-alpine
    networks: [backend]
networks:
  backend:
    driver: overlay
    attachable: true
    internal: true
    driver_opts:
      encrypted: "true"
    labels:
      team: web
    ipam:
      driver: default
      config:
        - subnet: 10.50.0.0/24
          gateway: 10.50.0.1
          ip_range: 10.50.0.128/25
        - subnet: 10.51.0.0/24
`

func TestStackNetworkKeepsEveryPool(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", poolsCompose, nil)
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	nets := rig.fake.NetworkCreates()
	if len(nets) != 1 {
		t.Fatalf("network creates = %+v, want one", nets)
	}
	n := nets[0]
	if n.Name != "web_backend" || n.Driver != "overlay" || !n.Internal || !n.Attachable || n.Options["encrypted"] != "true" {
		t.Errorf("network = %+v", n)
	}
	if n.Labels["team"] != "web" || n.Labels["com.docker.stack.namespace"] != "web" || n.Labels["tierplan.stack.id"] != "stack-web" {
		t.Errorf("labels = %v, want the file's own and the stack's", n.Labels)
	}
	if n.IPAM == nil || n.IPAM.Driver != "default" || len(n.IPAM.Config) != 2 {
		t.Fatalf("IPAM = %+v, want the default driver and both pools", n.IPAM)
	}
	first, second := n.IPAM.Config[0], n.IPAM.Config[1]
	if first.Subnet.String() != "10.50.0.0/24" || first.Gateway.String() != "10.50.0.1" || first.IPRange.String() != "10.50.0.128/25" || second.Subnet.String() != "10.51.0.0/24" {
		t.Errorf("pools = %+v and %+v", first, second)
	}
	if !slices.Equal(res.CreatedNetworks, []string{"web_backend"}) {
		t.Errorf("CreatedNetworks = %v", res.CreatedNetworks)
	}
}

func TestAnExistingStackNetworkIsNeverRecreatedEvenIfItsOptionsDiffer(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "web", "tierplan.stack.id": "stack-web"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		// The file now asks for two pools and an internal network; the one on the swarm has neither.
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "web_backend", Driver: "overlay", Scope: "swarm", Labels: owned, Subnets: []string{"10.99.0.0/24"}}}
	})
	_, in := stackInput(t, "web", poolsCompose, nil)
	res, _ := rig.deploy(t, in)
	if !res.OK() || len(res.CreatedNetworks) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := countWrites(rig.fake.Writes(), "POST /networks/create") + countWrites(rig.fake.Writes(), "DELETE /networks"); got != 0 {
		t.Errorf("writes = %v, an existing network must not be recreated or removed", rig.fake.Writes())
	}
}

const externalNetworkCompose = `services:
  web:
    image: nginx:1.27-alpine
    networks: [shared]
networks:
  shared:
    external: true
    name: org_shared
`

func TestExternalNetworkMustExist(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		networks []dockertest.FakeNetwork
		code     string
	}{
		{"missing", nil, stackops.CodeExternalMissing},
		{"local scope", []dockertest.FakeNetwork{{ID: "n9", Name: "org_shared", Driver: "bridge", Scope: "local"}}, stackops.CodeExternalNotSwarm},
	}
	for _, tc := range cases {
		rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.Networks = tc.networks })
		_, in := stackInput(t, "web", externalNetworkCompose, nil)
		res, job := rig.deploy(t, in)
		if res.Code != tc.code || res.FailedService != "org_shared" {
			t.Errorf("%s: result = %+v, want %s naming org_shared", tc.name, res, tc.code)
			continue
		}
		got := itemStates(job)
		if got[stackops.ItemCheck] != "done" || got[stackops.ItemNetworks] != "failed:"+tc.code || got[stackops.ItemObjects] != "skipped:not_run" {
			t.Errorf("%s: items = %v, want the networks step to stop the deploy", tc.name, got)
		}
		if w := rig.fake.Writes(); len(w) != 0 {
			t.Errorf("%s: writes = %v, nothing may be created for a stack whose external network is missing", tc.name, w)
		}
	}
}

func TestExternalNetworkOfSwarmScopeIsUsedAndNeverCreatedOrRemoved(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n9", Name: "org_shared", Driver: "overlay", Scope: "swarm"}}
	})
	_, in := stackInput(t, "web", externalNetworkCompose, nil)
	res, _ := rig.deploy(t, in)
	if !res.OK() || res.Networks != 0 {
		t.Fatalf("result = %+v", res)
	}
	if n := countWrites(rig.fake.Writes(), "POST /networks") + countWrites(rig.fake.Writes(), "DELETE /networks"); n != 0 {
		t.Errorf("writes = %v, an external network is never created or removed", rig.fake.Writes())
	}
	atts := rig.fake.ServiceCreates()[0].Spec.TaskTemplate.Networks
	if len(atts) != 1 || atts[0].Target != "org_shared" {
		t.Errorf("service networks = %+v, want org_shared", atts)
	}
}

// pruneRig is a swarm with the stack "web" already deployed with an old service the file no
// longer declares, a second stack and a service of nobody's stack.
func pruneRig(t *testing.T, updateError string) *swarmRig {
	t.Helper()
	owned := map[string]string{"com.docker.stack.namespace": "web", "tierplan.stack.id": "stack-web"}
	blog := map[string]string{"com.docker.stack.namespace": "blog", "tierplan.stack.id": "stack-blog"}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{
			{ID: "n1", Name: "web_default", Driver: "overlay", Scope: "swarm", Labels: owned},
			{ID: "n2", Name: "blog_default", Driver: "overlay", Scope: "swarm", Labels: blog},
		}
		c.Secrets = []dockertest.FakeSecret{{ID: "blogsec", Name: hashedName("blog", "tok", "x"), Labels: ownedLabels("blog", "stack-blog", "tok", "x")}}
	},
		dockertest.FakeService{ID: "svc-web", Name: "web_web", Mode: "replicated", Image: "nginx:1.26", Version: 7, Labels: owned, UpdateError: updateError},
		dockertest.FakeService{ID: "svc-old", Name: "web_old", Mode: "replicated", Image: "busybox", Version: 2, Labels: owned},
		dockertest.FakeService{ID: "svc-blog", Name: "blog_web", Mode: "replicated", Image: "nginx:1", Version: 3, Labels: blog},
		dockertest.FakeService{ID: "svc-loner", Name: "loner", Mode: "replicated", Image: "busybox", Version: 1},
	)
}

func TestUndeclaredServiceIsPrunedAfterUpdates(t *testing.T) {
	t.Parallel()
	rig := pruneRig(t, "")
	_, in := starterPlan(t, "web")
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	if !slices.Equal(res.RemovedServices, []string{"web_old"}) {
		t.Errorf("RemovedServices = %v, want [web_old]", res.RemovedServices)
	}
	writes := rig.fake.Writes()
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "POST /services/svc-web/update") || writes[1] != "DELETE /services/svc-old" {
		t.Errorf("writes = %v, want the declared service updated and then the old one removed", writes)
	}
}

func TestUndeclaredServiceIsKeptWhenAnUpdateFails(t *testing.T) {
	t.Parallel()
	rig := pruneRig(t, "the swarm rejected the update")
	_, in := starterPlan(t, "web")
	res, job := rig.deploy(t, in)
	if res.OK() {
		t.Fatalf("result = %+v, want the update to fail", res)
	}
	if n := countWrites(rig.fake.Writes(), "DELETE"); n != 0 {
		t.Errorf("writes = %v, nothing may be removed after a failed update", rig.fake.Writes())
	}
	if got := itemStates(job); got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
}

func TestOtherStacksAreNeverTouched(t *testing.T) {
	t.Parallel()
	rig := pruneRig(t, "")
	_, in := stackInput(t, "web", tokCompose, tokFile("one"))
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	for _, w := range rig.fake.Writes() {
		for _, other := range []string{"svc-blog", "svc-loner", "blogsec", "n2"} {
			if strings.Contains(w, other) {
				t.Errorf("write %q touches %s, which another stack or nobody owns", w, other)
			}
		}
	}
	if got := rig.secretNames(t); !slices.Contains(got, hashedName("blog", "tok", "x")) {
		t.Errorf("secrets = %v, the other stack's secret is gone", got)
	}
	if !slices.Equal(res.RemovedServices, []string{"web_old"}) {
		t.Errorf("RemovedServices = %v, want only this stack's undeclared service", res.RemovedServices)
	}
}

// The pins of a deploy (D-15, STK-09).

// pinCompose is a stack of one service, web, running image.
func pinCompose(image string) string { return "services:\n  web:\n    image: " + image + "\n" }

// digestOf is a valid digest made of one hex character.
func digestOf(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// memPins is a PinStore that keeps its pins in memory, for tests of the deploy that do not need
// a database: the stored pins it holds read back as the last good revision's.
type memPins struct {
	mu     sync.Mutex
	good   map[string]stacks.LivePin
	stored map[string]map[string]string
	// platforms are the platforms stored on each snapshot, by swarm service name.
	platforms map[string]map[string][]swarm.Platform
}

func (m *memPins) GoodPins(_ context.Context, _ string) (map[string]stacks.LivePin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.good), nil
}

func (m *memPins) SetPins(_ context.Context, snapshotID string, pins map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stored == nil {
		m.stored = map[string]map[string]string{}
	}
	m.stored[snapshotID] = maps.Clone(pins)
	return nil
}

func (m *memPins) SetPlatforms(_ context.Context, snapshotID string, platforms map[string][]swarm.Platform) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.platforms == nil {
		m.platforms = map[string]map[string][]swarm.Platform{}
	}
	m.platforms[snapshotID] = maps.Clone(platforms)
	return nil
}

func (m *memPins) platformsOf(snapshotID string) map[string][]swarm.Platform {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.platforms[snapshotID]
}

func (m *memPins) pinsOf(snapshotID string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stored[snapshotID]
}

// A deploy stores, with its revision, the platforms each service was sent with, so a roll back can send them again
// without asking a registry (G-08-4, P7 D-14, T-07-93).
func TestDeployStoresThePlatformsItSentWithTheRevision(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	store := &memPins{}
	_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
	in.Pins, in.RevisionID = store, "rev-1"

	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	want := []swarm.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
	creates := rig.fake.ServiceCreates()
	if len(creates) != 1 || !slices.Equal(creates[0].Spec.TaskTemplate.Placement.Platforms, want) {
		t.Fatalf("service creates = %+v, want one sent with %v", creates, want)
	}
	got := store.platformsOf("rev-1")
	if len(got) != 1 || !slices.Equal(got["site_web"], want) {
		t.Errorf("stored platforms = %v, want site_web sent with %v", got, want)
	}
	if got := store.pinsOf("rev-1"); !maps.Equal(got, map[string]string{"site_web": "nginx:1.27@" + digestOf("a")}) {
		t.Errorf("stored pins = %v, want the pinned reference of site_web beside the platforms", got)
	}
}

// siteWeb is the service of pinCompose in a stack named site that docker stack deploy (or TierPlan) left running the image
// ref, written as nginx:1.27, with no platforms: someone took them off outside TierPlan.
func siteWeb(ref string) dockertest.FakeService {
	owned := map[string]string{"com.docker.stack.namespace": "site", "tierplan.stack.id": "stack-site", stacks.LabelSwarmImage: "nginx:1.27"}
	return dockertest.FakeService{ID: "svc-web", Name: "site_web", Mode: "replicated", Image: ref, Version: 3, Labels: owned, Desired: 1, Running: 1}
}

// TestADeployPutsBackThePlatformsRecordedForAKeptDigest (G-08-4, P7 D-18, T-08-111): the platforms belong to the image,
// and what TierPlan recorded for a digest comes before what a service happens to run. A service that keeps its digest is
// sent the platforms the stack's last good revision recorded for that digest, so an outside change is put back; with
// nothing recorded it keeps the ones it runs; a record of another digest is never applied; and no registry is asked.
func TestADeployPutsBackThePlatformsRecordedForAKeptDigest(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}
	kept := "nginx:1.27@" + digestOf("a")
	updatedPlatforms := func(t *testing.T, rig *swarmRig) []swarm.Platform {
		t.Helper()
		ups := rig.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("service updates = %d, want 1", len(ups))
		}
		if pl := ups[0].Spec.TaskTemplate.Placement; pl != nil {
			return pl.Platforms
		}
		return nil
	}

	t.Run("a record for the same digest is put back", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil, siteWeb(kept))
		store := &memPins{good: map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.27", Image: kept, Platforms: []swarm.Platform{amd, arm}}}}
		_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins, in.RevisionID = store, "rev-2"
		res, job := rig.deploy(t, in)
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		if got := updatedPlatforms(t, rig); !slices.Equal(got, []swarm.Platform{amd, arm}) {
			t.Errorf("redeployed with platforms %v, want the recorded %v", got, []swarm.Platform{amd, arm})
		}
		if got := store.platformsOf("rev-2")["site_web"]; !slices.Equal(got, []swarm.Platform{amd, arm}) {
			t.Errorf("the revision records platforms %v, want the ones it sent", got)
		}
		if n := len(rig.fake.DistributionRefs()); n != 0 {
			t.Errorf("distribution lookups = %d, want none for a kept digest (P7 D-18)", n)
		}
	})

	t.Run("a record for another digest is ignored", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil, siteWeb(kept))
		other := "nginx:1.27@" + digestOf("b")
		store := &memPins{good: map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.27", Image: other, Platforms: []swarm.Platform{amd, arm}}}}
		_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins, in.RevisionID = store, "rev-2"
		res, job := rig.deploy(t, in)
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		if got := updatedPlatforms(t, rig); len(got) != 0 {
			t.Errorf("redeployed with platforms %v, want none: the record belongs to another digest (T-08-111)", got)
		}
		if n := len(rig.fake.DistributionRefs()); n != 0 {
			t.Errorf("distribution lookups = %d, want none", n)
		}
	})

	t.Run("nothing recorded keeps what the service runs", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil)
		rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
		store := &memPins{}
		_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins, in.RevisionID = store, "rev-1"
		if res, job := rig.deploy(t, in); !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("first deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		// The stack's last good revision holds the pin but no platforms (a revision from before they were recorded).
		store.good = map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.27", Image: kept}}
		in.RevisionID = "rev-2"
		if res, job := rig.deploy(t, in); !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("second deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		if got := updatedPlatforms(t, rig); !slices.Equal(got, []swarm.Platform{amd, arm}) {
			t.Errorf("redeployed with platforms %v, want the ones the service runs %v", got, []swarm.Platform{amd, arm})
		}
	})

	t.Run("a service the swarm lost is sent what its good revision recorded", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil)
		store := &memPins{good: map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.27", Image: kept, Platforms: []swarm.Platform{arm}}}}
		_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins, in.RevisionID = store, "rev-2"
		res, job := rig.deploy(t, in)
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		creates := rig.fake.ServiceCreates()
		if len(creates) != 1 || creates[0].Spec.TaskTemplate.Placement == nil || !slices.Equal(creates[0].Spec.TaskTemplate.Placement.Platforms, []swarm.Platform{arm}) {
			t.Fatalf("service creates = %+v, want one sent with %v", creates, []swarm.Platform{arm})
		}
		if n := len(rig.fake.DistributionRefs()); n != 0 {
			t.Errorf("distribution lookups = %d, want none", n)
		}
	})
}

// TestARollBackDeploySendsThePlatformsItsRevisionStored (G-08-4, P7 D-14, T-07-93, T-08-101): a roll back sends the
// digests and the platforms its revision stored and asks no registry. A revision from before platforms were stored
// keeps what the service runs when it runs that revision's image, and sends none when it runs another.
func TestARollBackDeploySendsThePlatformsItsRevisionStored(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}
	older := "nginx:1.27@" + digestOf("a")
	// run deploys revision 2 of the stack, which looks nginx:1.27 up as digest runs (so the service runs both
	// platforms), and then rolls back to the image of revision 1 with the given record of its platforms.
	run := func(t *testing.T, runs string, record map[string][]swarm.Platform) (*swarmRig, *memPins, int) {
		t.Helper()
		rig := newSwarmRig(t, nil)
		rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf(runs))
		store := &memPins{}
		_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins, in.RevisionID = store, "rev-2"
		if res, job := rig.deploy(t, in); !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy of revision 2 = %+v job %s %v", res, job.State, itemStates(job))
		}
		lookups := len(rig.fake.DistributionRefs())
		in.RevisionID, in.Rollback = "rev-3", true
		in.Reviewed = map[string]string{"site_web": older}
		in.RestoredPlatforms = record
		if res, job := rig.deploy(t, in); !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("roll back = %+v job %s %v", res, job.State, itemStates(job))
		}
		return rig, store, lookups
	}
	sent := func(t *testing.T, rig *swarmRig) swarm.ServiceSpec {
		t.Helper()
		ups := rig.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("service updates = %d, want 1", len(ups))
		}
		return ups[0].Spec
	}
	platformsOf := func(spec swarm.ServiceSpec) []swarm.Platform {
		if pl := spec.TaskTemplate.Placement; pl != nil {
			return pl.Platforms
		}
		return nil
	}

	t.Run("the revision's own platforms replace the running ones", func(t *testing.T) {
		t.Parallel()
		rig, store, lookups := run(t, "b", map[string][]swarm.Platform{"site_web": {amd}})
		spec := sent(t, rig)
		if got := spec.TaskTemplate.ContainerSpec.Image; got != older {
			t.Errorf("rolled back to image %q, want %q", got, older)
		}
		if got := platformsOf(spec); !slices.Equal(got, []swarm.Platform{amd}) {
			t.Errorf("rolled back with platforms %v, want the revision's %v", got, []swarm.Platform{amd})
		}
		if got := store.platformsOf("rev-3")["site_web"]; !slices.Equal(got, []swarm.Platform{amd}) {
			t.Errorf("the roll back revision records platforms %v, want %v", got, []swarm.Platform{amd})
		}
		if n := len(rig.fake.DistributionRefs()); n != lookups {
			t.Errorf("distribution lookups = %d, want %d: a roll back asks no registry (T-07-93)", n, lookups)
		}
	})

	t.Run("a revision with no record keeps what the service runs of the same digest", func(t *testing.T) {
		t.Parallel()
		rig, _, lookups := run(t, "a", nil)
		if got := platformsOf(sent(t, rig)); !slices.Equal(got, []swarm.Platform{amd, arm}) {
			t.Errorf("rolled back with platforms %v, want the running %v", got, []swarm.Platform{amd, arm})
		}
		if n := len(rig.fake.DistributionRefs()); n != lookups {
			t.Errorf("distribution lookups = %d, want %d", n, lookups)
		}
	})

	t.Run("a revision with no record sends none when the service runs another digest", func(t *testing.T) {
		t.Parallel()
		rig, _, lookups := run(t, "b", nil)
		if got := platformsOf(sent(t, rig)); len(got) != 0 {
			t.Errorf("rolled back with platforms %v, want none: the service ran another image (T-08-97)", got)
		}
		if n := len(rig.fake.DistributionRefs()); n != lookups {
			t.Errorf("distribution lookups = %d, want %d", n, lookups)
		}
	})

	t.Run("a record for another service is not applied", func(t *testing.T) {
		t.Parallel()
		rig, _, _ := run(t, "b", map[string][]swarm.Platform{"site_db": {arm}})
		if got := platformsOf(sent(t, rig)); len(got) != 0 {
			t.Errorf("rolled back with platforms %v, want none", got)
		}
	})
}

func TestRedeployKeepsTheRunningDigestAfterTheTagMoved(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	store := &memPins{}
	_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
	in.Pins, in.RevisionID = store, "rev-1"

	first, job := rig.deploy(t, in)
	if !first.OK() || job.State != jobs.JobDone {
		t.Fatalf("first deploy = %+v, job = %s %v", first, job.State, itemStates(job))
	}
	creates := rig.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27@"+digestOf("a") {
		t.Fatalf("service creates = %+v, want one at nginx:1.27@sha256:aaa", creates)
	}
	if got := store.pinsOf("rev-1"); !maps.Equal(got, map[string]string{"site_web": "nginx:1.27@" + digestOf("a")}) {
		t.Errorf("stored pins = %v, want the pinned reference of site_web", got)
	}
	if pin := first.Pins["site_web"]; pin.Action != stacks.PinResolve || pin.Ref != "nginx:1.27@"+digestOf("a") {
		t.Errorf("first pin = %+v, want a resolve", pin)
	}
	lookups := len(rig.fake.DistributionRefs())

	// The tag moves in the registry; nothing in the file changes.
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("b"))
	in.RevisionID = "rev-2"
	second, job := rig.deploy(t, in)
	if !second.OK() || job.State != jobs.JobDone {
		t.Fatalf("second deploy = %+v, job = %s %v", second, job.State, itemStates(job))
	}
	if n := len(rig.fake.ServiceCreates()); n != 1 {
		t.Errorf("service creates = %d, want still one: an unchanged stack sends no ServiceCreate", n)
	}
	ups := rig.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("service updates = %+v, want one", ups)
	}
	if got := ups[0].Spec.TaskTemplate.ContainerSpec.Image; got != "nginx:1.27@"+digestOf("a") {
		t.Errorf("update image = %q, want the running digest nginx:1.27@sha256:aaa, not the moved tag", got)
	}
	if !reflect.DeepEqual(creates[0].Spec.TaskTemplate, ups[0].Spec.TaskTemplate) {
		t.Errorf("the redeploy's task template differs from the first deploy's:\n create %+v\n update %+v", creates[0].Spec.TaskTemplate, ups[0].Spec.TaskTemplate)
	}
	if pin := second.Pins["site_web"]; pin.Action != stacks.PinKeep {
		t.Errorf("second pin = %+v, want keep", pin)
	}
	if n := len(rig.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution lookups = %d, want still %d: a kept digest needs no registry", n, lookups)
	}
	if got := store.pinsOf("rev-2"); !maps.Equal(got, map[string]string{"site_web": "nginx:1.27@" + digestOf("a")}) {
		t.Errorf("second revision's pins = %v", got)
	}
}

func TestChangedReferenceResolvesANewDigest(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.28", digestOf("c"))
	_, first := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
	if res, _ := rig.deploy(t, first); !res.OK() {
		t.Fatalf("first deploy = %+v", res)
	}
	lookups := len(rig.fake.DistributionRefs())

	_, second := stackInput(t, "site", pinCompose("nginx:1.28"), nil)
	res, job := rig.deploy(t, second)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("second deploy = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	ups := rig.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].Spec.TaskTemplate.ContainerSpec.Image != "nginx:1.28@"+digestOf("c") {
		t.Fatalf("service updates = %+v, want one at nginx:1.28@sha256:ccc", ups)
	}
	if got := ups[0].Spec.Labels["com.docker.stack.image"]; got != "nginx:1.28" {
		t.Errorf("image label = %q, want the text as written", got)
	}
	if refs := rig.fake.DistributionRefs()[lookups:]; !slices.Equal(refs, []string{"docker.io/library/nginx:1.28"}) {
		t.Errorf("lookups of the second deploy = %v, want only the new reference", refs)
	}
}

func TestADeployKeepsTheForceUpdateCounterOfARunningService(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "site", "tierplan.stack.id": "stack-site", "com.docker.stack.image": "nginx:1.27"}
	live := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "site_web", Labels: owned},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: "nginx:1.27@" + digestOf("a")}, ForceUpdate: 3,
		},
	}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "site_default", Driver: "overlay", Scope: "swarm", Labels: owned}}
	}, dockertest.FakeService{ID: "svc-web", Name: "site_web", Mode: "replicated", Version: 4, Labels: owned, Spec: &live})
	_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
	res, _ := rig.deploy(t, in)
	if !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	ups := rig.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v", ups)
	}
	if got := ups[0].Spec.TaskTemplate.ForceUpdate; got != 3 {
		t.Errorf("ForceUpdate = %d, want the running counter 3: sending 0 would replace every task", got)
	}
	if got := ups[0].Spec.TaskTemplate.ContainerSpec.Image; got != "nginx:1.27@"+digestOf("a") {
		t.Errorf("image = %q, want the running digest", got)
	}
	if n := len(rig.fake.DistributionRefs()); n != 0 {
		t.Errorf("distribution lookups = %d, want none for a kept digest", n)
	}
}

func TestADigestInTheFileIsDeployedAsWrittenWithoutALookup(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	text := "nginx:1.27@" + digestOf("d")
	_, in := stackInput(t, "site", pinCompose(text), nil)
	res, _ := rig.deploy(t, in)
	if !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	if got := rig.fake.ServiceCreates()[0].Spec.TaskTemplate.ContainerSpec.Image; got != text {
		t.Errorf("image = %q, want %q as written", got, text)
	}
	if n := len(rig.fake.DistributionRefs()); n != 0 {
		t.Errorf("distribution lookups = %d, want none", n)
	}
}

func TestADeployStopsAtTheLookupWhenAnImageCannotBePinned(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: 404, Message: "manifest unknown"}
	})
	_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeImageUnresolved || res.FailedService != "site_web" {
		t.Fatalf("result = %+v, want image_unresolved at site_web", res)
	}
	got := itemStates(job)
	if got[stackops.ItemCheck] != "done" || got[stackops.ItemLookup] != "failed:image_unresolved" || got[stackops.ItemNetworks] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a deploy that cannot pin an image writes nothing", w)
	}
	for _, it := range job.Items {
		if it.ID == stackops.ItemLookup && !strings.Contains(it.Message, "docker.io") {
			t.Errorf("lookup message = %q, want it to name the registry", it.Message)
		}
	}
}

// A deploy that cannot pin an image, a local image, an update of chosen services and a service
// the swarm lost (D-16, D-18, A7).

const twoServiceCompose = "services:\n  web:\n    image: nginx:1.27\n  cache:\n    image: redis:7\n"

// updatesByService is the specs the fake received as updates, by service name.
func updatesByService(f *dockertest.Fake) map[string]string {
	out := map[string]string{}
	for _, u := range f.ServiceUpdates() {
		out[u.Spec.Name] = u.Spec.TaskTemplate.ContainerSpec.Image
	}
	return out
}

func TestPinDeployStopsAtTheLookupNamingTheHostAndTheServiceAndWritesNothing(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	_, first := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
	if res, _ := rig.deploy(t, first); !res.OK() {
		t.Fatalf("first deploy = %+v", res)
	}
	rig.fake.SetDistributionFailure("registry.example:5000/app:1", 500, `dial tcp: lookup registry.example: no such host`)
	writes := len(rig.fake.Writes())

	// The file now also names a new service on the registry that is down.
	_, second := stackInput(t, "site", "services:\n  web:\n    image: nginx:1.27\n  app:\n    image: registry.example:5000/app:1\n", nil)
	res, job := rig.deploy(t, second)

	if res.Code != stackops.CodeRegistryUnreachable || res.FailedService != "site_app" {
		t.Fatalf("result = %+v, want registry_unreachable at site_app", res)
	}
	got := itemStates(job)
	if got[stackops.ItemCheck] != "done" || got[stackops.ItemLookup] != "failed:registry_unreachable" || got[stackops.ItemNetworks] != "skipped:not_run" {
		t.Errorf("items = %v, want the deploy to stop at the lookup", got)
	}
	for _, it := range job.Items {
		if it.ID == stackops.ItemLookup && it.Message != "TierPlan can't reach registry.example:5000, and app needs a fresh lookup. Services that are already pinned don't need the registry." {
			t.Errorf("lookup message = %q", it.Message)
		}
	}
	if n := len(rig.fake.Writes()); n != writes {
		t.Errorf("writes grew from %d to %d, a deploy that cannot pin writes nothing", writes, n)
	}
	if res.Pins["site_web"].Action != stacks.PinKeep {
		t.Errorf("pins = %+v, the pinned service needs no registry", res.Pins)
	}
	if len(res.Problems) != 1 || res.Problems[0].File != stacks.ComposeFile || res.Problems[0].Line != 5 {
		t.Errorf("problems = %+v, want one at the image line of app (compose.yaml:5)", res.Problems)
	}
}

func TestPinDeployOfALocalOnlyImageSendsItsImageIDAndWarns(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Images = []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{localImageRef}}}
	})
	rig.fake.SetDistributionFailure("docker.io/library/myapp:1", 404, "pull access denied for myapp")
	store := &memPins{}
	_, in := stackInput(t, "site", "services:\n  app:\n    image: myapp:1\n", nil)
	in.Pins, in.RevisionID = store, "rev-1"
	res, job := rig.deploy(t, in)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	creates := rig.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Spec.TaskTemplate.ContainerSpec.Image != localImageID || creates[0].Spec.Labels["com.docker.stack.image"] != localImageRef {
		t.Fatalf("service creates = %+v, want the image ID with the text as the label", creates)
	}
	if len(res.Problems) != 1 || res.Problems[0].Code != stackops.CodeLocalImage || res.Problems[0].Severity != stacks.SeverityWarning {
		t.Errorf("problems = %+v, want one local-image warning", res.Problems)
	}
	if got := store.pinsOf("rev-1"); !maps.Equal(got, map[string]string{"site_app": localImageID}) {
		t.Errorf("stored pins = %v", got)
	}
}

func TestPinUpdateModeLooksUpOnlyTheSelectedServices(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	rig.fake.SetDistributionDigest("docker.io/library/redis:7", digestOf("c"))
	_, in := stackInput(t, "site", twoServiceCompose, nil)
	if res, _ := rig.deploy(t, in); !res.OK() {
		t.Fatalf("first deploy = %+v", res)
	}
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("b"))
	rig.fake.SetDistributionDigest("docker.io/library/redis:7", digestOf("d"))
	lookups := len(rig.fake.DistributionRefs())

	in.Update = map[string]bool{"web": true}
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("update = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	got := updatesByService(rig.fake)
	if got["site_web"] != "nginx:1.27@"+digestOf("b") || got["site_cache"] != "redis:7@"+digestOf("c") {
		t.Errorf("updates = %v, want web moved to the newest digest and cache kept on its own", got)
	}
	if refs := rig.fake.DistributionRefs()[lookups:]; !slices.Equal(refs, []string{"docker.io/library/nginx:1.27"}) {
		t.Errorf("lookups of the update = %v, want only the selected service", refs)
	}
}

func TestPinAServiceTheSwarmLostKeepsTheLastGoodRevisionsDigestWithoutARegistryCall(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("b")) // the tag has moved since
	store := &memPins{good: map[string]stacks.LivePin{
		"site_web": {AsWritten: "nginx:1.27", Image: "nginx:1.27@" + digestOf("a")},
	}}
	_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
	in.Pins, in.RevisionID = store, "rev-2"

	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	creates := rig.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27@"+digestOf("a") {
		t.Fatalf("service creates = %+v, want the digest the last good revision ran", creates)
	}
	if n := len(rig.fake.DistributionRefs()); n != 0 {
		t.Errorf("distribution lookups = %d, want none", n)
	}
	if got := store.pinsOf("rev-2"); !maps.Equal(got, map[string]string{"site_web": "nginx:1.27@" + digestOf("a")}) {
		t.Errorf("stored pins = %v", got)
	}
}

func TestPinAChangedTextIgnoresTheStoredPinOfAServiceTheSwarmLost(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.28", digestOf("c"))
	store := &memPins{good: map[string]stacks.LivePin{
		"site_web": {AsWritten: "nginx:1.27", Image: "nginx:1.27@" + digestOf("a")},
	}}
	_, in := stackInput(t, "site", pinCompose("nginx:1.28"), nil)
	in.Pins, in.RevisionID = store, "rev-2"
	if res, _ := rig.deploy(t, in); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	if got := rig.fake.ServiceCreates()[0].Spec.TaskTemplate.ContainerSpec.Image; got != "nginx:1.28@"+digestOf("c") {
		t.Errorf("image = %q, want the new text looked up", got)
	}
}

const consoleCompose = `services:
  web:
    image: nginx:1.27-alpine
    configs:
      - source: conf
        target: /etc/nginx/conf.d/site.conf
  api:
    image: busybox:1.36
configs:
  conf:
    content: "listen 80;"
`

func TestDeploySwarmAddsAnItemForEachNetworkObjectAndServiceWithTheConsoleLabels(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", consoleCompose, nil)
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job = %s %v", res, job.State, itemStates(job))
	}
	full := hashedName("web", "conf", "listen 80;")
	for _, want := range []struct{ id, label, parent, kind string }{
		{"network:web_default", "Create network web_default", stackops.ItemNetworks, stackops.KindStep},
		{"config:" + full, "Create config conf", stackops.ItemObjects, stackops.KindStep},
		{"service:web_api", "api", stackops.ItemServices, stackops.KindService},
		{"service:web_web", "web", stackops.ItemServices, stackops.KindService},
	} {
		it := itemByID(t, job, want.id)
		if it.Label != want.label || it.Parent != want.parent || it.Kind != want.kind || it.State != jobs.ItemDone {
			t.Errorf("item %s = %+v, want label %q under %s as a %s, done", want.id, it, want.label, want.parent, want.kind)
		}
	}
	if got := itemByID(t, job, "service:web_web").Result["action"]; got != "created" {
		t.Errorf("service result = %v, want created", got)
	}
	// The spine keeps its words and kinds: groups are not lines of the step list.
	for id, want := range map[string]string{
		stackops.ItemCheck: "Check the stack", stackops.ItemLookup: "Look up images", stackops.ItemServices: "Deploy services",
		stackops.ItemCleanup: "Remove services no longer in the stack",
	} {
		if it := itemByID(t, job, id); it.Label != want || it.Kind != stackops.KindStep {
			t.Errorf("item %s = %q (%s), want the step %q", id, it.Label, it.Kind, want)
		}
	}
	if it := itemByID(t, job, stackops.ItemNetworks); it.Kind != stackops.KindGroup {
		t.Errorf("networks kind = %q, want a group", it.Kind)
	}
}

func TestDeploySwarmAddsNoStepForANetworkThatExistsAndCallsAnOwnedServiceUpdated(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "tierplan.stack.id": "stack-tracer"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "tracer_default", Driver: "overlay", Scope: "swarm", Labels: owned}}
	}, dockertest.FakeService{ID: "svc-web", Name: "tracer_web", Mode: "replicated", Image: "nginx:1.26", Version: 7, Labels: owned})
	_, in := starterPlan(t, "tracer")
	_, job := rig.deploy(t, in)
	for _, it := range job.Items {
		if strings.HasPrefix(it.ID, "network:") {
			t.Errorf("item %s was added for a network that already exists", it.ID)
		}
	}
	if got := itemByID(t, job, "service:tracer_web").Result["action"]; got != "updated" {
		t.Errorf("service result = %v, want updated", got)
	}
}

func TestDeploySwarmEndsTheServiceThatFailedAndLeavesTheRestNotRun(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "no such image" })
	_, in := stackInput(t, "web", consoleCompose, nil)
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeServiceRejected {
		t.Fatalf("result = %+v, want service_rejected", res)
	}
	got := itemStates(job)
	first, second := res.FailedService, "service:web_web"
	if first == "web_web" {
		second = "service:web_api"
	}
	if got["service:"+first] != "failed:service_rejected" || got[second] != "skipped:not_run" {
		t.Errorf("service rows = %v, want %s failed and %s not run", got, first, second)
	}
	if got["network:web_default"] != "done" {
		t.Errorf("network step = %q, want done: it was created before the failure", got["network:web_default"])
	}
}

// The words of the console (plan 07-17, STK-07): what the deploy wrote, and what Docker said.

// outputOf reads the output log of a job as plain lines.
func outputOf(t *testing.T, rig *swarmRig, j jobs.Job) []string {
	t.Helper()
	lines, ok := rig.hub.Output(j.ID)
	if !ok {
		t.Fatalf("the hub has no job %s", j.ID)
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

// writesThenRunning puts the lines that say a service is running after the others, sorted: the
// services are waited for side by side, so the order they settle in is not the file's.
func writesThenRunning(lines []string) []string {
	var writes, running []string
	for _, l := range lines {
		if strings.HasSuffix(l, " is running") {
			running = append(running, l)
		} else {
			writes = append(writes, l)
		}
	}
	slices.Sort(running)
	return append(writes, running...)
}

func TestDeploySwarmWritesOneOutputLineForEachWrite(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", consoleCompose, nil)
	_, job := rig.deploy(t, in)
	full := hashedName("web", "conf", "listen 80;")
	want := []string{"Created network web_default", "Created config " + full, "Created service web_api", "Created service web_web",
		"Service web_api is running", "Service web_web is running"}
	if got := writesThenRunning(outputOf(t, rig, job)); !slices.Equal(got, want) {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestDeploySwarmWritesUpdatedAndRemovedLines(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "tierplan.stack.id": "stack-tracer"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "tracer_default", Driver: "overlay", Scope: "swarm", Labels: owned}}
	},
		dockertest.FakeService{ID: "svc-web", Name: "tracer_web", Mode: "replicated", Image: "nginx:1.26", Version: 7, Labels: owned},
		dockertest.FakeService{ID: "svc-old", Name: "tracer_old", Mode: "replicated", Image: "busybox:1", Version: 3, Labels: owned},
	)
	_, in := starterPlan(t, "tracer")
	_, job := rig.deploy(t, in)
	want := []string{"Updated service tracer_web", "Service tracer_web is running", "Removed service tracer_old"}
	if got := outputOf(t, rig, job); !slices.Equal(got, want) {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestDeploySwarmPutsDockersWordsOnTheFailedServiceMaskedAndCleaned(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-token-value"
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.ServiceCreateError = "rpc error: code = InvalidArgument\x1b[0m desc = bad env TOKEN=" + secret
	})
	const compose = "services:\n  api:\n    image: busybox:1.36\n    environment:\n      TOKEN: ${TOKEN}\n"
	vars := []stacks.Var{{Name: "TOKEN", Value: secret, Secret: true}}
	_, in := stackInput(t, "web", compose, nil, vars...)
	in.Masker = stacks.NewMasker(vars, nil)
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeServiceRejected {
		t.Fatalf("result = %+v, want service_rejected", res)
	}
	row := itemByID(t, job, "service:web_api")
	const want = "rpc error: code = InvalidArgument[0m desc = bad env TOKEN=<hidden>"
	if row.State != jobs.ItemFailed || row.Code != stackops.CodeServiceRejected || !strings.Contains(row.Detail, want) {
		t.Errorf("service row = %+v, want it failed with Docker's words (masked, cleaned) in its detail", row)
	}
	if row.Message != stackops.CodeMessage(stackops.CodeServiceRejected) {
		t.Errorf("service message = %q, want the sentence for the code, never Docker's words", row.Message)
	}
	if got := itemByID(t, job, stackops.ItemServices).Detail; got != "" {
		t.Errorf("services step detail = %q, want none: the row carries Docker's words", got)
	}
	out := strings.Join(outputOf(t, rig, job), "\n")
	if !strings.Contains(out, "Failed service web_api: ") || !strings.Contains(out, "TOKEN=<hidden>") {
		t.Errorf("output = %q, want a failure line with Docker's words, masked", out)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out+string(raw), secret) {
		t.Error("the secret reached the job or its output")
	}
}

func TestDeploySwarmMasksTheWordsOfAFailedSecretToo(t *testing.T) {
	t.Parallel()
	const secret = "correct-horse-battery-staple"
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.WriteError = "engine said: " + secret })
	vars := []stacks.Var{}
	files := tokFile(secret)
	_, in := stackInput(t, "web", tokCompose, files, vars...)
	in.Masker = stacks.NewMasker(vars, files)
	res, job := rig.deploy(t, in)
	if res.OK() {
		t.Fatal("a deploy against a failing engine succeeded")
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(outputOf(t, rig, job), "\n")
	if strings.Contains(out+string(raw), secret) {
		t.Errorf("the secret reached the job or its output: %s\n%s", raw, out)
	}
	var detail string
	for _, it := range job.Items {
		if strings.HasPrefix(it.ID, "secret:") {
			detail = it.Detail
		}
	}
	if !strings.Contains(detail, "engine said: <hidden>") || strings.Contains(detail, "unix://") {
		t.Errorf("secret detail = %q, want the engine's words with the secret hidden and no address of ours", detail)
	}
}

func TestDeploySwarmSkipsTheStepsAndServicesAfterAFailure(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "no such image" })
	_, in := stackInput(t, "web", consoleCompose, nil)
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodeServiceRejected {
		t.Fatalf("result = %+v", res)
	}
	got := itemStates(job)
	first, second := res.FailedService, "service:web_web"
	if first == "web_web" {
		second = "service:web_api"
	}
	if got["service:"+first] != "failed:service_rejected" || got[second] != "skipped:not_run" || got[stackops.ItemCleanup] != "skipped:not_run" {
		t.Errorf("items = %v, want %s failed, %s and the cleanup skipped", got, first, second)
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job = %s, want partial: the check and the network were done", job.State)
	}
}

// Just before a deploy writes anything it judges the host access of the stack again, for the person
// who asked, on the values it will send (plan 07-18, T-07-86): the review was made earlier, and a
// grant can be gone by now.

func TestDeploySwarmChecksTheHostAccessPolicyAgainBeforeItWritesAnything(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	admin := rig.principal(t, "adm", "admin")
	_, in := stackInput(t, "web", privilegedCompose, nil)
	in.Caller = stackops.Caller{Principal: admin, Actor: "adm"}

	// The review passes for an env Admin...
	plan := rig.review(t, "web", privilegedCompose, nil)
	if got := needsAdmin(plan); len(got) != 0 {
		t.Fatalf("an env Admin was refused at the review: %+v", got)
	}
	// ...and the grant is gone before the job runs.
	if _, err := rig.store.Write.ExecContext(testsupport.Context(t), `UPDATE role_assignments SET role_id = 'operator' WHERE principal_id = 'adm'`); err != nil {
		t.Fatal(err)
	}
	res, job := rig.deploy(t, in)

	if res.Code != stackops.CodeNeedsAdmin {
		t.Fatalf("result = %+v, want the policy to refuse it with needs_admin", res)
	}
	check := itemByID(t, job, stackops.ItemCheck)
	if check.State != jobs.ItemFailed || check.Code != stackops.CodeNeedsAdmin || !strings.Contains(check.Detail, "privileged: true") {
		t.Errorf("check = %+v, want it failed with the setting named", check)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if got := itemStates(job); got[stackops.ItemLookup] != "skipped:not_run" || got[stackops.ItemServices] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
}

func TestDeploySwarmAllowsWhatTheCallerMayDeployAndNothingElse(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, clean := stackInput(t, "web", pinCompose("nginx:1.27-alpine"), nil)
	clean.Caller = stackops.Caller{Principal: rig.principal(t, "op", "operator"), Actor: "op"}
	if res, job := rig.deploy(t, clean); !res.OK() {
		t.Fatalf("an Operator's clean stack: %+v %v", res, itemStates(job))
	}
}

func TestDeploySwarmFailsClosedWhenThePolicyCannotBeApplied(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.deps.Checker = nil
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)
	if res.Code != stackops.CodePolicyUnavailable {
		t.Fatalf("result = %+v, want policy_unavailable: an unjudged stack is not an allowed one", res)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
	if got := itemStates(job)[stackops.ItemCheck]; got != "failed:policy_unavailable" {
		t.Errorf("check = %q", got)
	}
}

// adopted is a corpus stack docker stack deploy made, adopted into the rig's store: the stack as stored (pending its
// first deploy), its draft and the repo that holds them (plan 08-16, D-16).
type adopted struct {
	rig   *swarmRig
	repo  stackops.Repo
	stack stackops.Stack
	draft stackops.Snapshot
}

// adoptCorpus stands the corpus file deploy.yaml on a fake swarm as the CLI left it, with the services edited by
// edit (nil for none), and adopts it.
func adoptCorpus(t *testing.T, edit func(short string, spec *swarm.ServiceSpec)) *adopted {
	t.Helper()
	return adoptRig(t, corpusRig(t, "deploy.yaml", edit))
}

// adoptCorpusStale is adoptCorpus for a stack TierPlan made once for a record this instance does not hold: its
// services and their containers, and its networks, carry the tierplan.stack.id staleID (D-11).
func adoptCorpusStale(t *testing.T, staleID string) *adopted {
	t.Helper()
	edit := func(_ string, spec *swarm.ServiceSpec) {
		spec.Labels = maps.Clone(spec.Labels)
		spec.Labels[stacks.LabelStackID] = staleID
		cs := *spec.TaskTemplate.ContainerSpec
		cs.Labels = maps.Clone(cs.Labels)
		if cs.Labels == nil {
			cs.Labels = map[string]string{}
		}
		cs.Labels[stacks.LabelStackID] = staleID
		spec.TaskTemplate.ContainerSpec = &cs
	}
	tune := func(c *dockertest.FakeConfig) {
		for i := range c.Networks {
			c.Networks[i].Labels = maps.Clone(c.Networks[i].Labels)
			c.Networks[i].Labels[stacks.LabelStackID] = staleID
		}
	}
	return adoptRig(t, corpusRigTuned(t, "deploy.yaml", edit, tune))
}

// adoptRig adopts the stack named shop that rig holds.
func adoptRig(t *testing.T, rig *swarmRig) *adopted {
	t.Helper()
	return adoptRigOn(t, rig, stacks.TargetSwarm)
}

// adoptRigOn adopts the stack named shop that rig holds, a swarm stack or a Compose project of a standalone host.
func adoptRigOn(t *testing.T, rig *swarmRig, target stacks.Target) *adopted {
	t.Helper()
	repo := adoptRepo(t, rig)
	st, err := rig.adopt(t, repo, target, "shop", stackops.SelfMarks{})
	if err != nil {
		t.Fatalf("AdoptCommit: %v", err)
	}
	draft, err := repo.Draft(testsupport.Context(t), st.ID)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	return &adopted{rig: rig, repo: repo, stack: st, draft: draft}
}

// review reviews the adopted stack's draft as an instance admin does, with the stack as the store holds it now.
func (a *adopted) review(t *testing.T) stackops.Review {
	t.Helper()
	ctx := testsupport.Context(t)
	st, err := a.repo.Stack(ctx, a.rig.envID, a.stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := a.rig.deps.Review(ctx, stackops.ReviewInput{
		EnvID: a.rig.envID, Stack: st, Copy: a.draft.Copy, DraftVersion: a.draft.Version, Pins: a.repo, Revisions: a.repo,
		Caller: stackops.Caller{Principal: a.rig.principal(t, "root", ""), Actor: "root"},
	})
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}

// deployInput is the input of the adopted stack's first deploy, with the images its review chose.
func (a *adopted) deployInput(t *testing.T, rv stackops.Review) stackops.SwarmDeployInput {
	t.Helper()
	ctx := testsupport.Context(t)
	st, err := a.repo.Stack(ctx, a.rig.envID, a.stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: a.draft.Copy})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for _, v := range a.draft.Copy.Vars {
		vars[v.Name] = v.Value
	}
	return stackops.SwarmDeployInput{
		Stack: st, Project: loaded.Project, Files: stacks.NewFileSet(a.draft.Copy.Files), Vars: vars, Copy: a.draft.Copy,
		Pins: a.repo, Reviewed: rv.Pins,
	}
}

// writesNamed filters the writes the fake saw to those whose request line contains any of the words.
func writesNamed(fake *dockertest.Fake, words ...string) []string {
	var out []string
	for _, w := range fake.Writes() {
		for _, word := range words {
			if strings.Contains(w, word) {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

// TestFirstDeployOfAnAdoptedSwarmStack: the first deploy sends one service update per service carrying TierPlan's
// ID, and creates, removes or conflicts with no network, secret or config that was there at adoption (T-08-61).
func TestFirstDeployOfAnAdoptedSwarmStack(t *testing.T) {
	t.Parallel()
	a := adoptCorpus(t, nil)
	rv := a.review(t)
	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v, want the first review of an adopted stack to refuse nothing", rv.Problems)
	}
	before := len(a.rig.fake.Writes())

	res, job := a.rig.deploy(t, a.deployInput(t, rv))
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
	}

	if res.Networks != 0 || len(res.CreatedObjects) != 0 || len(res.RemovedServices) != 0 || len(res.RemovedObjects) != 0 || res.Services != 0 {
		t.Errorf("deploy created networks %v objects %v services %d and removed services %v objects %v, want nothing but updates",
			res.CreatedNetworks, res.CreatedObjects, res.Services, res.RemovedServices, res.RemovedObjects)
	}
	if res.Updated != 3 {
		t.Errorf("updated = %d, want one update per service (3)", res.Updated)
	}
	writes := a.rig.fake.Writes()[before:]
	if got := writesNamed(a.rig.fake, "/networks", "/secrets", "/configs"); len(got) != 0 {
		t.Errorf("the deploy wrote %v to networks, secrets or configs", got)
	}
	updates := 0
	for _, w := range writes {
		if strings.Contains(w, "/services/") && strings.Contains(w, "/update") {
			updates++
		}
		if strings.Contains(w, "DELETE") {
			t.Errorf("the deploy removed something: %s", w)
		}
	}
	if updates != 3 {
		t.Errorf("service updates = %d (%v), want 3", updates, writes)
	}
	for _, u := range a.rig.fake.ServiceUpdates() {
		spec := u.Spec
		if spec.Labels[stacks.LabelStackID] != a.stack.ID || spec.TaskTemplate.ContainerSpec.Labels[stacks.LabelStackID] != a.stack.ID {
			t.Errorf("service %s was updated without TierPlan's ID on the service and its containers: %v", spec.Name, spec.Labels)
		}
	}

	// The deploy is the whole of the difference: a second review lists no change, before and after the record of the
	// adoption is cleared (the API clears it when the deploy ends good).
	if still := a.review(t); len(still.Services) != 0 || still.Adoption == nil || still.Adoption.OwnershipOnly != 0 {
		t.Errorf("review of the deployed stack while the adoption is pending = services %+v adoption %+v, want no service", still.Services, still.Adoption)
	}
	if err := a.repo.ClearAdoptionPending(testsupport.Context(t), a.stack.ID); err != nil {
		t.Fatal(err)
	}
	again := a.review(t)
	var changed []string
	for _, o := range again.Objects {
		if o.Change != stackops.ChangeExternal {
			changed = append(changed, o.Change+" "+o.Kind+" "+o.Name)
		}
	}
	if len(again.Services) != 0 || len(changed) != 0 || len(again.Problems) != 0 || again.Adoption != nil {
		t.Errorf("review after the first deploy = services %+v objects %v problems %+v adoption %+v, want no change", again.Services, changed, again.Problems, again.Adoption)
	}
}

// TestFirstDeployRelabelsAStaleID: a service that carried a stale tierplan.stack.id at adoption gets the new
// stack's ID, and its network, which keeps the stale label, is neither a conflict nor touched (D-11).
func TestFirstDeployRelabelsAStaleID(t *testing.T) {
	t.Parallel()
	const stale = "stale-id"
	a := adoptCorpusStale(t, stale)
	rv := a.review(t)
	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v, want a stale ID to be no name clash", rv.Problems)
	}

	res, _ := a.rig.deploy(t, a.deployInput(t, rv))
	if !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	if res.Code == stackops.CodeNetworkConflict || res.Networks != 0 {
		t.Errorf("deploy = %+v, want the stale-labelled networks left as they are", res)
	}
	for _, u := range a.rig.fake.ServiceUpdates() {
		spec := u.Spec
		if spec.Labels[stacks.LabelStackID] != a.stack.ID {
			t.Errorf("service %s carries ID %q after the deploy, want %q", spec.Name, spec.Labels[stacks.LabelStackID], a.stack.ID)
		}
	}
	if got := writesNamed(a.rig.fake, "/networks"); len(got) != 0 {
		t.Errorf("the deploy wrote to networks: %v", got)
	}
}

// livePlatforms is an edit of a service as docker stack deploy left it: the platforms of its image, which no compose file
// says (G-08-4).
func livePlatforms(platforms ...swarm.Platform) func(string, *swarm.ServiceSpec) {
	return func(_ string, spec *swarm.ServiceSpec) {
		pl := swarm.Placement{}
		if spec.TaskTemplate.Placement != nil {
			pl = *spec.TaskTemplate.Placement
		}
		pl.Platforms = slices.Clone(platforms)
		spec.TaskTemplate.Placement = &pl
	}
}

// TestFirstReviewAndDeployOfAnAdoptedStackKeepItsPlatforms (G-08-4, D-14, D-16): the stack docker stack deploy made holds
// the platforms of each image. Its first review lists every service as an ownership-label recreation and nothing in the
// placement, and its first deploy sends the services with those platforms; a stack that runs none is sent none.
func TestFirstReviewAndDeployOfAnAdoptedStackKeepItsPlatforms(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}

	t.Run("platforms the CLI wrote", func(t *testing.T) {
		t.Parallel()
		a := adoptCorpus(t, livePlatforms(amd, arm))
		rv := a.review(t)
		if len(rv.Problems) != 0 {
			t.Fatalf("problems = %+v, want none", rv.Problems)
		}
		if len(rv.Services) != 3 || rv.Summary.OwnershipOnly != 3 {
			t.Fatalf("services = %d, ownership-only = %d, want every service listed as an ownership-label recreation (3)", len(rv.Services), rv.Summary.OwnershipOnly)
		}
		for _, p := range gapPaths(t, rv.Services) {
			if strings.Contains(p, "Placement") {
				t.Errorf("the review lists %s, want the platforms the services run kept", p)
			}
		}
		for _, s := range rv.Services {
			if !s.OwnershipOnly {
				t.Errorf("service %s is not ownership-only", s.Short)
			}
		}

		res, job := a.rig.deploy(t, a.deployInput(t, rv))
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		ups := a.rig.fake.ServiceUpdates()
		if len(ups) != 3 {
			t.Fatalf("service updates = %d, want 3", len(ups))
		}
		for _, u := range ups {
			pl := u.Spec.TaskTemplate.Placement
			if pl == nil || !slices.Equal(pl.Platforms, []swarm.Platform{amd, arm}) {
				t.Errorf("service %s was updated with placement %+v, want the platforms linux/amd64 and linux/arm64", u.Spec.Name, pl)
			}
		}
		if still := a.review(t); len(still.Services) != 0 {
			t.Errorf("review after the first deploy = %+v, want no change", still.Services)
		}
	})

	t.Run("no platforms", func(t *testing.T) {
		t.Parallel()
		a := adoptCorpus(t, nil)
		rv := a.review(t)
		res, job := a.rig.deploy(t, a.deployInput(t, rv))
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		for _, u := range a.rig.fake.ServiceUpdates() {
			if pl := u.Spec.TaskTemplate.Placement; pl != nil && len(pl.Platforms) != 0 {
				t.Errorf("service %s was updated with platforms %+v, want none for a stack that runs none", u.Spec.Name, pl.Platforms)
			}
		}
	})
}

// TestADeployThatLooksAnImageUpSendsThePlatformsOfTheImage (G-08-4, user decision 2026-10-27): docker stack deploy sends
// each service with the platforms of its image, and so does TierPlan, filled from the answer of the lookup that pinned
// the digest (the fake's registry answers linux/amd64 and linux/arm64/v8 and two unknown/unknown entries). A redeploy
// with no edit sends the same platforms and asks no registry (P7 D-18); an image pinned to a local ID has none.
func TestADeployThatLooksAnImageUpSendsThePlatformsOfTheImage(t *testing.T) {
	t.Parallel()
	want := []swarm.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}

	t.Run("a new service, a redeploy and a review", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil)
		store := &memPins{}
		_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins, in.RevisionID = store, "rev-1"

		// The review of a service that is not there yet lists it with the platforms it would be sent.
		first := rig.review(t, "site", pinCompose("nginx:1.27"), nil)
		if len(first.Services) != 1 || first.Services[0].Change != "added" || !strings.Contains(string(first.Services[0].Next), `"Platforms":[{"Architecture":"amd64","OS":"linux"},{"Architecture":"arm64","OS":"linux"}]`) {
			t.Fatalf("review of a new service = %+v, want it added with the platforms of its image", first.Services)
		}

		res, job := rig.deploy(t, in)
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		creates := rig.fake.ServiceCreates()
		if len(creates) != 1 {
			t.Fatalf("service creates = %d, want 1", len(creates))
		}
		if pl := creates[0].Spec.TaskTemplate.Placement; pl == nil || !slices.Equal(pl.Platforms, want) {
			t.Fatalf("created with placement %+v, want the platforms %v", pl, want)
		}
		lookups := len(rig.fake.DistributionRefs())

		if rv := rig.review(t, "site", pinCompose("nginx:1.27"), nil); len(rv.Services) != 0 {
			t.Errorf("review right after the deploy = %+v, want no change", rv.Services)
		}

		in.RevisionID = "rev-2"
		again, job := rig.deploy(t, in)
		if !again.OK() || job.State != jobs.JobDone {
			t.Fatalf("redeploy = %+v job %s %v", again, job.State, itemStates(job))
		}
		ups := rig.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("service updates = %d, want 1", len(ups))
		}
		if pl := ups[0].Spec.TaskTemplate.Placement; pl == nil || !slices.Equal(pl.Platforms, want) {
			t.Errorf("redeployed with placement %+v, want the same platforms %v", pl, want)
		}
		if n := len(rig.fake.DistributionRefs()); n != lookups {
			t.Errorf("distribution lookups = %d, want still %d: a kept digest is never looked up again (T-08-108)", n, lookups)
		}
	})

	t.Run("a local image", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
			c.Images = []dockertest.FakeImage{{ID: localImageID, RepoTags: []string{localImageRef}}}
		})
		rig.fake.SetDistributionFailure("docker.io/library/myapp:1", 404, "pull access denied for myapp")
		_, in := stackInput(t, "site", "services:\n  app:\n    image: myapp:1\n", nil)
		res, job := rig.deploy(t, in)
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		creates := rig.fake.ServiceCreates()
		if len(creates) != 1 {
			t.Fatalf("service creates = %d, want 1", len(creates))
		}
		if pl := creates[0].Spec.TaskTemplate.Placement; pl != nil && len(pl.Platforms) != 0 {
			t.Errorf("created with platforms %+v, want none for a local image", pl.Platforms)
		}
	})
}

// reviewedDeploy is a deploy of the compose text that follows a review: it carries exactly the images the review chose.
func reviewedDeploy(t *testing.T, rig *swarmRig, name, compose string, rv stackops.Review) stackops.SwarmDeployInput {
	t.Helper()
	_, in := stackInput(t, name, compose, nil)
	in.Pins, in.RevisionID = &memPins{}, "rev-1"
	in.Reviewed = rv.Pins
	return in
}

// TestAReviewedDeploySendsThePlatformsItsReviewFound (G-08-4, user decision 2026-10-27): the deploy the UI starts after a
// review carries references only. It sends each looked-up image with the platforms the review showed, read from the
// server's memo of the review's answers and so without a second registry call; when the server no longer holds them (it
// restarted between the two), it asks the registry once for the platforms of the reviewed digest, and a registry that
// cannot answer stops the deploy for that service as any lookup a deploy needs does (P7 D-18).
func TestAReviewedDeploySendsThePlatformsItsReviewFound(t *testing.T) {
	t.Parallel()
	want := []swarm.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
	compose := pinCompose("nginx:1.27")

	t.Run("the memo of the review answers", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil)
		rig.deps.Platforms = stackops.NewPlatformMemo()
		rv := rig.review(t, "site", compose, nil)
		reviewed := rig.fake.DistributionRefs()
		if len(reviewed) != 1 {
			t.Fatalf("review lookups = %v, want 1", reviewed)
		}

		res, job := rig.deploy(t, reviewedDeploy(t, rig, "site", compose, rv))
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		creates := rig.fake.ServiceCreates()
		if len(creates) != 1 {
			t.Fatalf("service creates = %d, want 1", len(creates))
		}
		if pl := creates[0].Spec.TaskTemplate.Placement; pl == nil || !slices.Equal(pl.Platforms, want) {
			t.Errorf("created with placement %+v, want the platforms the review found %v", pl, want)
		}
		if refs := rig.fake.DistributionRefs(); !slices.Equal(refs, reviewed) {
			t.Errorf("distribution lookups = %v, want only the review's %v: the deploy asks no registry while the server holds the answer", refs, reviewed)
		}
	})

	t.Run("no memo: one lookup of the reviewed digest", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil)
		rv := rig.review(t, "site", compose, nil)
		before := len(rig.fake.DistributionRefs())
		pin := rv.Pins["site_web"]
		if !strings.Contains(pin, "@sha256:") {
			t.Fatalf("reviewed pins = %v, want a pinned reference", rv.Pins)
		}

		res, job := rig.deploy(t, reviewedDeploy(t, rig, "site", compose, rv))
		if !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		extra := rig.fake.DistributionRefs()[before:]
		if len(extra) != 1 || !strings.HasSuffix(extra[0], pin[strings.Index(pin, "@"):]) || !strings.HasPrefix(extra[0], "docker.io/library/nginx:1.27@") {
			t.Errorf("the deploy's lookups = %v, want exactly one, for the reviewed reference %s", extra, pin)
		}
		creates := rig.fake.ServiceCreates()
		if len(creates) != 1 {
			t.Fatalf("service creates = %d, want 1", len(creates))
		}
		if pl := creates[0].Spec.TaskTemplate.Placement; pl == nil || !slices.Equal(pl.Platforms, want) {
			t.Errorf("created with placement %+v, want the platforms %v", pl, want)
		}
	})

	t.Run("no memo and a registry that cannot answer", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name          string
			status        int
			message, code string
		}{
			{"unreachable", 500, "dial tcp: lookup registry.example: no such host", stackops.CodeRegistryUnreachable},
			{"unresolved", 404, "manifest unknown", stackops.CodeImageUnresolved},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				rig := newSwarmRig(t, nil)
				rv := rig.review(t, "site", compose, nil)
				pin := rv.Pins["site_web"]
				rig.fake.SetDistributionFailure("docker.io/library/nginx:1.27"+pin[strings.Index(pin, "@"):], tc.status, tc.message)
				writes := len(rig.fake.Writes())

				res, job := rig.deploy(t, reviewedDeploy(t, rig, "site", compose, rv))
				if res.OK() || res.Code != tc.code || res.FailedService != "site_web" {
					t.Fatalf("deploy = %+v, want code %s at service site_web", res, tc.code)
				}
				if it := itemByID(t, job, stackops.ItemLookup); it.State != jobs.ItemFailed {
					t.Errorf("lookup item = %+v, want it failed", it)
				}
				if got := writesNamed(rig.fake, "/services"); len(got) != 0 || len(rig.fake.Writes()) != writes {
					t.Errorf("the deploy wrote %v after a failed lookup, want nothing", rig.fake.Writes()[writes:])
				}
			})
		}
	})
}

// TestAReviewedDeployOfAnImageWithNoPlatformsAsksNoRegistryAgain (WR-01): a digest whose lookup found no platform is
// remembered as that answer, so the deploy that follows the review asks no registry for it and sends no platform.
func TestAReviewedDeployOfAnImageWithNoPlatformsAsksNoRegistryAgain(t *testing.T) {
	t.Parallel()
	compose := pinCompose("nginx:1.27")
	rig := newSwarmRig(t, nil)
	rig.deps.Platforms = stackops.NewPlatformMemo()
	rig.fake.SetDistributionNoPlatforms("docker.io/library/nginx:1.27")
	rv := rig.review(t, "site", compose, nil)
	reviewed := rig.fake.DistributionRefs()
	if len(reviewed) != 1 {
		t.Fatalf("review lookups = %v, want 1", reviewed)
	}

	res, job := rig.deploy(t, reviewedDeploy(t, rig, "site", compose, rv))
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
	}
	if refs := rig.fake.DistributionRefs(); !slices.Equal(refs, reviewed) {
		t.Errorf("distribution lookups = %v, want only the review's %v: an answer of none is remembered too", refs, reviewed)
	}
	creates := rig.fake.ServiceCreates()
	if len(creates) != 1 {
		t.Fatalf("service creates = %d, want 1", len(creates))
	}
	if pl := creates[0].Spec.TaskTemplate.Placement; pl != nil && len(pl.Platforms) != 0 {
		t.Errorf("created with platforms %+v, want none", pl.Platforms)
	}
}

// TestAReviewedDeployOfKeptDigestsAsksNoRegistry (G-08-4): an adopted CLI stack's first deploy keeps the digests its
// services run, so it asks no registry for platforms, memo or not, and sends the platforms they run with.
func TestAReviewedDeployOfKeptDigestsAsksNoRegistry(t *testing.T) {
	t.Parallel()
	amd, arm := swarm.Platform{OS: "linux", Architecture: "amd64"}, swarm.Platform{OS: "linux", Architecture: "arm64"}
	a := adoptCorpus(t, livePlatforms(amd, arm))
	rv := a.review(t)
	before := len(a.rig.fake.DistributionRefs())

	res, job := a.rig.deploy(t, a.deployInput(t, rv))
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
	}
	if n := len(a.rig.fake.DistributionRefs()); n != before {
		t.Errorf("distribution lookups = %d, want still %d: kept digests never ask the registry", n, before)
	}
	for _, u := range a.rig.fake.ServiceUpdates() {
		if pl := u.Spec.TaskTemplate.Placement; pl == nil || !slices.Equal(pl.Platforms, []swarm.Platform{amd, arm}) {
			t.Errorf("service %s was updated with placement %+v, want the platforms it runs", u.Spec.Name, pl)
		}
	}
}

// unallocatedCompose is a stack whose network asks for a subnet the swarm will not allocate.
const unallocatedCompose = `
services:
  web:
    image: nginx:1.27-alpine
    networks: [back]
networks:
  back:
    ipam:
      config:
        - subnet: 10.70.0.0/24
`

// stackNetworks lists the names of the networks of the stack the swarm holds now, as a manager reports them.
func (r *swarmRig) stackNetworks(t *testing.T, stack string) []string {
	t.Helper()
	var names []string
	err := r.deps.Clusters.ClusterRead(testsupport.Context(t), r.envID, func(c *dockerx.Client) error {
		rows, e := c.NetworksWithLabel(testsupport.Context(t), stacks.LabelSwarmNamespace, stack)
		for _, row := range rows {
			names = append(names, row.Name)
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestDeploySwarmNetworkNotAllocated(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.SwarmClusterID = "cluster-1"
		c.SwarmUnallocated = map[string]int{"10.70.0.0/24": -1}
	})
	_, in := stackInput(t, "shop", unallocatedCompose, nil)
	res, job := rig.deploy(t, in)

	if res.OK() || res.Code != stackops.CodeNetworkNotAllocated || res.FailedService != "shop_back" {
		t.Fatalf("result = %+v, want a failure with code %s naming shop_back", res, stackops.CodeNetworkNotAllocated)
	}
	if got := itemStates(job)[stackops.ItemNetworks]; got != "failed:network_not_allocated" {
		t.Errorf("networks item = %q (%v), want failed:network_not_allocated", got, itemStates(job))
	}
	if n := rig.stackNetworks(t, "shop"); len(n) != 0 {
		t.Errorf("networks left = %v, want none: the stub is removed again", n)
	}
	if got := rig.fake.ServiceCreates(); len(got) != 0 {
		t.Errorf("service creates = %d, want none: no service is made against a stub", len(got))
	}
	if countWrites(rig.fake.Writes(), "POST /services/") != 0 {
		t.Errorf("writes = %v, want no service write", rig.fake.Writes())
	}
	if countWrites(rig.fake.Writes(), "DELETE /networks/") != 1 {
		t.Errorf("writes = %v, want the stub removed once, by ID", rig.fake.Writes())
	}
	if len(res.CreatedNetworks) != 0 || len(res.LeftInPlace) != 0 {
		t.Errorf("CreatedNetworks = %v, LeftInPlace = %v, want both empty", res.CreatedNetworks, res.LeftInPlace)
	}
	if m := stackops.CodeMessage(stackops.CodeNetworkNotAllocated); !strings.Contains(m, "didn't give a network of the stack an address range") || !strings.Contains(m, "address pools are full") {
		t.Errorf("message = %q", m)
	}
}

func TestDeploySwarmNetworkStubLeftInPlace(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.SwarmClusterID = "cluster-1"
		c.SwarmUnallocated = map[string]int{"10.70.0.0/24": -1}
		c.RemoveErrors = map[string]string{"shop_back": "network is busy"}
	})
	_, in := stackInput(t, "shop", unallocatedCompose, nil)
	res, job := rig.deploy(t, in)

	if res.OK() || res.Code != stackops.CodeNetworkNotAllocated || res.FailedService != "shop_back" {
		t.Fatalf("result = %+v, want code %s naming shop_back", res, stackops.CodeNetworkNotAllocated)
	}
	if !slices.Equal(res.CreatedNetworks, []string{"shop_back"}) {
		t.Errorf("CreatedNetworks = %v, want the stub handed to the cleanup as a created network", res.CreatedNetworks)
	}
	if !slices.Equal(res.LeftInPlace, []string{"shop_back"}) || len(res.CleanedUp) != 0 {
		t.Errorf("LeftInPlace = %v, CleanedUp = %v, want the stub named as left in place", res.LeftInPlace, res.CleanedUp)
	}
	if n := countWrites(rig.fake.Writes(), "DELETE /networks/"); n != 2 {
		t.Errorf("network removals = %d, want 2: the read-back's and the cleanup's second try", n)
	}
	if n := rig.stackNetworks(t, "shop"); !slices.Equal(n, []string{"shop_back"}) {
		t.Errorf("networks = %v, want the stub still there", n)
	}
	if countWrites(rig.fake.Writes(), "POST /services/") != 0 || len(rig.fake.ServiceCreates()) != 0 {
		t.Errorf("writes = %v, want no service write", rig.fake.Writes())
	}
	if got := itemStates(job)[stackops.ItemNetworks]; got != "failed:network_not_allocated" {
		t.Errorf("networks item = %q, want failed:network_not_allocated", got)
	}
}
