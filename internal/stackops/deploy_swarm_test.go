// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
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
	return st, stackops.SwarmDeployInput{Stack: st, Project: loaded.Project, Files: stacks.NewFileSet(all), Vars: values}
}

// starterPlan is the starter compose file as a stack and the input of its deploy.
func starterPlan(t *testing.T, name string) (stackops.Stack, stackops.SwarmDeployInput) {
	t.Helper()
	return stackInput(t, name, stacks.StarterCompose, nil)
}

// deployItems is the items of a deploy job.
func deployItems() []jobs.ItemSpec {
	var items []jobs.ItemSpec
	for _, id := range stackops.DeployItems() {
		items = append(items, jobs.ItemSpec{ID: id})
	}
	return items
}

// deploy runs DeploySwarm as a job and returns its result and the job as it ended.
func (r *swarmRig) deploy(t *testing.T, in stackops.SwarmDeployInput) (stackops.DeployResult, jobs.Job) {
	t.Helper()
	in.EnvID = r.envID
	var res stackops.DeployResult
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: r.envID, Owner: "user:test",
		Items: deployItems(),
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
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)

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
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)

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
	_, in := starterPlan(t, "tracer")
	res, job := rig.deploy(t, in)

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

func TestDeploySwarmCallsOnDoneBeforeTheJobReadsAsFinished(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := starterPlan(t, "tracer")
	var seen atomic.Pointer[stackops.DeployResult]
	var stateInDone atomic.Value
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
	in.EnvID = rig.envID
	var res stackops.DeployResult
	job, err := rig.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: rig.envID, Owner: "user:test",
		Items: deployItems(),
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
		"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web",
		"bayplan.stack.object": "tok", "bayplan.stack.object.hash": hash,
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
