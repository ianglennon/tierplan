// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
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
	store *store.Store
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
	return &swarmRig{
		envID: env.ID, hub: hub, store: st,
		deps: stackops.Deps{Aggregator: agg, Clusters: clusters, Checker: rbac.NewChecker(st), Log: slog.New(slog.DiscardHandler)},
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
		spec.Labels["com.docker.stack.image"] != "nginx:1.27-alpine" || spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27-alpine@"+dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine") {
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
		Labels: map[string]string{"com.docker.stack.namespace": "tracer"}, // made by docker stack deploy, not by Bayplan
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
	if n.Labels["team"] != "web" || n.Labels["com.docker.stack.namespace"] != "web" || n.Labels["bayplan.stack.id"] != "stack-web" {
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
	owned := map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web"}
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
	owned := map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web"}
	blog := map[string]string{"com.docker.stack.namespace": "blog", "bayplan.stack.id": "stack-blog"}
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

func (m *memPins) pinsOf(snapshotID string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stored[snapshotID]
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
	owned := map[string]string{"com.docker.stack.namespace": "site", "bayplan.stack.id": "stack-site", "com.docker.stack.image": "nginx:1.27"}
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
		if it.ID == stackops.ItemLookup && it.Message != "Bayplan can't reach registry.example:5000, and app needs a fresh lookup. Services that are already pinned don't need the registry." {
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
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "bayplan.stack.id": "stack-tracer"}
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
	owned := map[string]string{"com.docker.stack.namespace": "tracer", "bayplan.stack.id": "stack-tracer"}
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
