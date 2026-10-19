// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The swarm deploy runs against a fake swarm manager behind the real monitor, aggregator and
// cluster reader.

type swarmRig struct {
	fake  *dockertest.Fake
	deps  stackops.Deps
	envID string
	hub   *jobs.Hub
}

func newSwarmRig(t *testing.T, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) *swarmRig {
	t.Helper()
	cfg := dockertest.FakeConfig{
		Hostname: "mgr-1", Version: "29.1.0", SwarmState: "active", SwarmControl: true,
		NodeRows:    []dockertest.FakeNode{{ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, EngineVersion: "29.1.0", OS: "linux"}},
		ServiceRows: services,
	}
	if tune != nil {
		tune(&cfg)
	}
	fake := dockertest.NewFake(t, cfg)
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil)
	mon := envs.NewMonitor(slog.New(slog.DiscardHandler), nil, nil)
	t.Cleanup(mon.StopAll)
	svc := envs.NewService(st, aud, mon, "unix://"+fake.SocketPath(), "0123456789ab", time.Now)
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
	return &swarmRig{
		fake: fake, envID: env.ID, hub: hub,
		deps: stackops.Deps{Aggregator: agg, Clusters: clusters, Log: slog.New(slog.DiscardHandler)},
	}
}

func starterPlan(t *testing.T, name string) (stackops.Stack, convert.Plan) {
	t.Helper()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: name, Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(stacks.StarterCompose)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetSwarm}
	plan, err := convert.Convert(loaded.Project, convert.Options{StackName: name, StackID: st.ID})
	if err != nil {
		t.Fatal(err)
	}
	return st, plan
}

// deploy runs DeploySwarm as a job and returns its result and the job as it ended.
func (r *swarmRig) deploy(t *testing.T, in stackops.SwarmDeployInput) (stackops.DeployResult, jobs.Job) {
	t.Helper()
	in.EnvID = r.envID
	var res stackops.DeployResult
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: r.envID, Owner: "user:test",
		Items: []jobs.ItemSpec{{ID: stackops.ItemCheck}, {ID: stackops.ItemNetworks}, {ID: stackops.ItemServices}},
	}, func(ctx context.Context, rep *jobs.Reporter) { res = r.deps.DeploySwarm(ctx, in, rep) })
	if err != nil {
		t.Fatal(err)
	}
	ended, err := r.hub.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	st, plan := starterPlan(t, "tracer")
	res, job := rig.deploy(t, stackops.SwarmDeployInput{Stack: st, Plan: plan})

	if !res.OK() || res.Networks != 1 || res.Services != 1 || res.Updated != 0 {
		t.Fatalf("result = %+v, want one network and one service created", res)
	}
	if job.State != jobs.JobDone {
		t.Errorf("job = %s %v, want done", job.State, itemStates(job))
	}
	nets := rig.fake.NetworkCreates()
	if len(nets) != 1 || nets[0].Name != "tracer_default" || nets[0].Driver != "overlay" || nets[0].Labels["bayplan.stack.id"] != "stack-tracer" ||
		nets[0].Labels["com.docker.stack.namespace"] != "tracer" {
		t.Errorf("network creates = %+v", nets)
	}
	svcs := rig.fake.ServiceCreates()
	if len(svcs) != 1 {
		t.Fatalf("service creates = %+v, want one", svcs)
	}
	spec := svcs[0].Spec
	if spec.Name != "tracer_web" || spec.Labels["com.docker.stack.namespace"] != "tracer" || spec.Labels["bayplan.stack.id"] != "stack-tracer" ||
		spec.Labels["com.docker.stack.image"] != "nginx:1.27-alpine" || spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27-alpine" {
		t.Errorf("service spec = %+v", spec)
	}
	if got := rig.fake.Writes(); !slices.Equal(got, []string{"POST /networks/create", "POST /services/create"}) {
		t.Errorf("writes = %v, want the network then the service, each once", got)
	}
}

func TestDeploySwarmUpdatesAnOwnedServiceAtTheVersionItRead(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "bayplan.stack.id": "stack-tracer"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "tracer_default", Driver: "overlay", Scope: "swarm", Labels: owned}}
	}, dockertest.FakeService{ID: "svc-web", Name: "tracer_web", Mode: "replicated", Image: "nginx:1.26", Version: 7, Labels: owned})
	st, plan := starterPlan(t, "tracer")
	res, job := rig.deploy(t, stackops.SwarmDeployInput{Stack: st, Plan: plan})

	if !res.OK() || res.Networks != 0 || res.Services != 0 || res.Updated != 1 {
		t.Fatalf("result = %+v, want one service updated and nothing created", res)
	}
	if job.State != jobs.JobDone {
		t.Errorf("job = %s %v", job.State, itemStates(job))
	}
	ups := rig.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].ServiceID != "svc-web" || ups[0].Version != "7" || ups[0].Spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27-alpine" {
		t.Errorf("updates = %+v, want one at version 7 with the new image", ups)
	}
	if got := rig.fake.Writes(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /services/svc-web/update") {
		t.Errorf("writes = %v, want only the service update (the network exists)", got)
	}
}

func TestDeploySwarmRefusesANameAnotherStackOwns(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil, dockertest.FakeService{
		ID: "svc-x", Name: "tracer_web", Mode: "replicated", Image: "nginx:1", Version: 1,
		Labels: map[string]string{"com.docker.stack.namespace": "tracer"}, // made by docker stack deploy, not by Bayplan
	})
	st, plan := starterPlan(t, "tracer")
	res, job := rig.deploy(t, stackops.SwarmDeployInput{Stack: st, Plan: plan})

	if res.Code != stackops.CodeNameClash {
		t.Fatalf("result = %+v, want stack_name_clash", res)
	}
	if got := itemStates(job); got[stackops.ItemCheck] != "failed:stack_name_clash" || got[stackops.ItemNetworks] != "cancelled:not_run" || got[stackops.ItemServices] != "cancelled:not_run" {
		t.Errorf("items = %v", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a foreign stack must not be touched", w)
	}
}

func TestDeploySwarmStopsAtTheFirstServiceTheManagerRejects(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "secret not found: db-password" })
	st, plan := starterPlan(t, "tracer")
	res, job := rig.deploy(t, stackops.SwarmDeployInput{Stack: st, Plan: plan})

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

func TestDeploySwarmReportsAnAgentRefusalWithoutTheEnginesWords(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.PolicyDeny = true })
	st, plan := starterPlan(t, "tracer")
	res, job := rig.deploy(t, stackops.SwarmDeployInput{Stack: st, Plan: plan})
	if res.Code != stackops.CodeAgentPolicyDenied {
		t.Fatalf("result = %+v, want agent_policy_denied", res)
	}
	if got := itemStates(job); got[stackops.ItemCheck] != "failed:agent_policy_denied" {
		t.Errorf("items = %v", got)
	}
}

func TestDeploySwarmCallsOnDoneBeforeTheJobReadsAsFinished(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	st, plan := starterPlan(t, "tracer")
	var seen atomic.Pointer[stackops.DeployResult]
	var stateInDone atomic.Value
	in := stackops.SwarmDeployInput{Stack: st, Plan: plan}
	in.EnvID = rig.envID
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
		Items: []jobs.ItemSpec{{ID: stackops.ItemCheck}, {ID: stackops.ItemNetworks}, {ID: stackops.ItemServices}},
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
	st, plan := starterPlan(t, "tracer")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := stackops.SwarmDeployInput{EnvID: rig.envID, Stack: st, Plan: plan}
	var res stackops.DeployResult
	job, err := rig.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: rig.envID, Owner: "user:test",
		Items: []jobs.ItemSpec{{ID: stackops.ItemCheck}, {ID: stackops.ItemNetworks}, {ID: stackops.ItemServices}},
	}, func(_ context.Context, r *jobs.Reporter) { res = rig.deps.DeploySwarm(ctx, in, r) })
	if err != nil {
		t.Fatal(err)
	}
	ended, err := rig.hub.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
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
