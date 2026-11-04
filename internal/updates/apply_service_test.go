// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The update of a plain swarm service (plan 12-15, IMG-06, D-05, D-07, D-09, D-12, D-13). Docker is a fake swarm manager behind the real
// monitor, aggregator and cluster reader; the registry is a scripted Heads; the applier, its run store, the job hub, the
// operation lock and the audit log are the real ones.

// waitClock is the time the convergence wait runs on. Until a test says otherwise a wait is answered at once and moves the
// clock on by what it asked for, so a timeout is reached without a sleep; with hold set, a wait is answered only when the
// test advances the clock, so the test decides what the service does between two polls.
type waitClock struct {
	mu     sync.Mutex
	now    time.Time
	hold   bool
	timers []waitTimer
	armed  chan struct{}
}

type waitTimer struct {
	at time.Time
	ch chan time.Time
}

func newWaitClock() *waitClock {
	return &waitClock{now: t0, armed: make(chan struct{}, 4096)}
}

func (c *waitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *waitClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if !c.hold {
		c.now = c.now.Add(d)
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, waitTimer{at: c.now.Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// fire moves the clock on by d and answers the polls that are due.
func (c *waitClock) fire(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, tm := range c.timers {
		if !tm.at.After(c.now) {
			tm.ch <- c.now
			continue
		}
		kept = append(kept, tm)
	}
	c.timers = kept
}

// applyRig is an environment of one swarm manager and an applier over it.
type applyRig struct {
	fake    *dockertest.Fake
	envID   string
	st      *store.Store
	reg     *registry.Store
	heads   *fakeHeads
	clock   *waitClock
	hub     *jobs.Hub
	locks   *stackops.Locks
	runs    updates.RunStore
	applier *updates.Applier
}

// newApplyRig builds the rig over a fake manager whose tasks follow the writes made to it. tune changes the manager's configuration
// before it starts.
func newApplyRig(t *testing.T, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) *applyRig {
	t.Helper()
	cfg := dockertest.FakeConfig{
		Hostname: "mgr-1", Version: "29.1.0", SwarmState: "active", SwarmControl: true, SwarmNodeID: "node-1",
		NodeRows:    []dockertest.FakeNode{{ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, EngineVersion: "29.1.0", OS: "linux"}},
		ServiceRows: services, TasksFollowWrites: true,
	}
	if tune != nil {
		tune(&cfg)
	}
	fake := dockertest.NewFake(t, cfg)
	return newApplyRigOn(t, "unix://"+fake.SocketPath(), fake)
}

// newApplyRigOn builds the rig over the engine at host, a local socket: the fake's, or a real engine's. fake is nil for a real one.
func newApplyRigOn(t *testing.T, host string, fake *dockertest.Fake) *applyRig {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	aud := audit.NewWriter(st, v, log, nil)
	mon := envs.NewMonitor(log, nil, nil)
	t.Cleanup(mon.StopAll)
	svc := envs.NewService(st, aud, mon, host, "0123456789ab", time.Now)
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

	clk := newWaitClock()
	rs := registry.NewStore(st, v, func() time.Time { return t0 })
	sql := updates.SQLStore{Q: st.Q, W: st.Write}
	heads := newHeads()
	runIndex := updates.NewRunIndex()
	chk := &updates.Checker{Heads: heads, Store: sql, Runs: runIndex, Now: func() time.Time { return t0 }, Log: log}
	usvc := &updates.Service{
		Store: sql, Resolver: registry.NewResolver(rs), Clusters: clusters, Aggregator: agg, Checker: chk, Runs: runIndex,
		Now: func() time.Time { return t0 },
	}
	hub := jobs.New(jobs.Options{})
	t.Cleanup(hub.Close)
	locks := stackops.NewLocks()
	runs := updates.RunStore{Q: st.Q, W: st.Write}
	return &applyRig{
		fake: fake, envID: env.ID, st: st, reg: rs, heads: heads, clock: clk, hub: hub, locks: locks, runs: runs,
		applier: &updates.Applier{
			Service: usvc, Runs: &runs, Locks: locks, Jobs: hub, Aggregator: agg, Resolver: registry.NewResolver(rs), Audit: aud,
			Clock: clk, ConvergeTimeout: 30 * time.Second, Log: log,
		},
	}
}

// swarmSpec is a service with the settings an update must keep: replicas, labels, environment, an update configuration, limits,
// a placement, a mount and a published port. It runs digest running of registry.test/team/<name>:1.
func swarmSpec(name string, running byte) swarm.ServiceSpec {
	replicas := uint64(3)
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{"team": "core", "tier": "web"}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: fmt.Sprintf("registry.test/team/%s:1@%s", name, dg(running)), Env: []string{"MODE=prod", "WORKERS=4"},
				Labels:  map[string]string{"app": name},
				Command: []string{"serve"},
				Mounts:  []mount.Mount{{Type: mount.TypeVolume, Source: "data", Target: "/srv/data"}},
			},
			Resources: &swarm.ResourceRequirements{Limits: &swarm.Limit{NanoCPUs: 500000000, MemoryBytes: 1 << 28}},
			Placement: &swarm.Placement{Constraints: []string{"node.role==manager"}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism: 2, Order: swarm.UpdateOrderStartFirst, FailureAction: swarm.UpdateFailureActionRollback, Delay: 5 * time.Second,
		},
		EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP, Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080}}},
	}
}

// swarmService is a service made of swarmSpec.
func swarmService(name string, running byte) dockertest.FakeService {
	spec := swarmSpec(name, running)
	return dockertest.FakeService{
		ID: "svc-" + name, Name: name, Mode: "replicated", Running: 3, Desired: 3, Version: 1, Image: spec.TaskTemplate.ContainerSpec.Image,
		Labels: spec.Annotations.Labels, Spec: &spec,
	}
}

// offer scripts the registry's answer for team/<name>:1 to be digest. The applier checks again at the click, so this is only what
// the page would have shown.
func (r *applyRig) offer(name string, digest string) {
	r.heads.set("registry.test/team/"+name+":1", answer{digest: digest})
}

// request is the update of a service as an Operator asks for it, confirmed at the digest.
func (r *applyRig) request(name, confirmed string) updates.Request {
	req := updates.Request{
		EnvID: r.envID, EnvName: "Local", Kind: "service", Name: name,
		Actor: audit.Actor{Type: audit.ActorUser, ID: "user-1", Name: "olive"}, Owner: "user:user-1",
	}
	if confirmed != "" {
		req.Expect = map[string]string{name: confirmed}
	}
	return req
}

// run asks for an update and waits for the job to end.
func (r *applyRig) run(t *testing.T, req updates.Request) jobs.Job {
	t.Helper()
	id, err := r.applier.Start(testsupport.Context(t), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job, err := r.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatalf("waiting for the job: %v", err)
	}
	return job
}

func (r *applyRig) runRow(t *testing.T, job jobs.Job) updates.Run {
	t.Helper()
	run, err := r.runs.Get(context.Background(), job.Meta[updates.MetaRunID])
	if err != nil {
		t.Fatalf("read the run: %v", err)
	}
	return run
}

// audits are the audit rows of one action, oldest first.
func (r *applyRig) audits(t *testing.T, action string) []map[string]string {
	t.Helper()
	rows, err := r.st.Read.Query(`SELECT result, COALESCE(reason, ''), COALESCE(details, '{}') FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]string
	for rows.Next() {
		var result, reason, raw string
		if err := rows.Scan(&result, &reason, &raw); err != nil {
			t.Fatal(err)
		}
		d := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		d["_result"], d["_reason"] = result, reason
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// An update changes the image of a service and nothing else: the spec that is sent is the spec that was read, field for field,
// with the image alone replaced by the one the server pinned (plan 12-15, D-07, T-12-40).
func TestSpecKeptButImage(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))

	job := r.run(t, r.request("web", dg('b')))
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s %+v, want done", job.State, job.Items)
	}
	sent := r.fake.ServiceUpdates()
	if len(sent) != 1 {
		t.Fatalf("service updates = %d, want 1", len(sent))
	}
	want := swarmSpec("web", 'a')
	want.TaskTemplate.ContainerSpec.Image = "registry.test/team/web:1@" + dg('b')
	if !reflect.DeepEqual(sent[0].Spec, want) {
		t.Errorf("the spec sent differs from the old spec in more than its image:\n got %+v\nwant %+v", sent[0].Spec, want)
	}
	if got := sent[0].Spec.UpdateConfig; got == nil || got.Parallelism != 2 || got.Order != swarm.UpdateOrderStartFirst || got.Delay != 5*time.Second {
		t.Errorf("update_config = %+v, want the person's own", got)
	}
	// The image is kept as it was written, and only its digest changed.
	if got := sent[0].Spec.TaskTemplate.ContainerSpec.Image; got != "registry.test/team/web:1@"+dg('b') {
		t.Errorf("image = %q", got)
	}
}

// A digest the request carries is only compared, never deployed, and a digest that is not the registry's is refused as moved
// without a lock, a run or a write (plan 12-15, D-05, T-12-37).
func TestUpdateDeploysTheDigestTheServerReadAndRefusesAMovedOne(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('c'))

	_, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
	var moved *updates.MovedError
	if !errors.As(err, &moved) || len(moved.Items) != 1 || moved.Items[0].Service != "web" || moved.Items[0].ToDigest != dg('c') || moved.Items[0].FromDigest != dg('a') {
		t.Fatalf("Start = %v, want a MovedError naming web, a to c", err)
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want none", n)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after a refusal")
	}
	if runs, err := r.runs.Running(context.Background(), r.envID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v, want none", runs, err)
	}

	// Confirmed at what the registry says now, the update deploys that digest.
	job := r.run(t, r.request("web", dg('c')))
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s, want done", job.State)
	}
	if got := r.fake.ServiceUpdates()[0].Spec.TaskTemplate.ContainerSpec.Image; got != "registry.test/team/web:1@"+dg('c') {
		t.Errorf("image deployed = %q, want the digest the registry gave", got)
	}
}

// The credential a service is updated with is chosen at the click: the pull and the write both carry it, and with none
// the write says the manager is to use the service's own (plan 12-15, D-07, T-12-39).
func TestUpdateSendsFreshAuth(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-token-value-0123"

	t.Run("a matching credential", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil, swarmService("web", 'a'))
		cred, err := r.reg.Create(testsupport.Context(t), registry.CreateInput{
			Name: "team", Type: registry.TypeGeneric, Host: "registry.test", Prefix: "team", Username: "bot", Secret: secret,
		}, "admin-1", nil)
		if err != nil {
			t.Fatalf("create credential: %v", err)
		}
		r.heads.set("registry.test/team/web:1", answer{digest: dg('b'), credID: cred.ID})

		job := r.run(t, r.request("web", dg('b')))
		if job.State != jobs.JobDone {
			t.Fatalf("job = %s %+v, want done", job.State, job.Items)
		}
		sent := r.fake.ServiceUpdates()
		if len(sent) != 1 || sent[0].Auth == "" {
			t.Fatalf("updates = %+v, want one carrying the registry header", sent)
		}
		if got := sent[0].Query.Get("registryAuthFrom"); got != "" {
			t.Errorf("registryAuthFrom = %q, want none when a credential is sent", got)
		}
		raw, err := base64.URLEncoding.DecodeString(sent[0].Auth)
		if err != nil || !strings.Contains(string(raw), `"username":"bot"`) || !strings.Contains(string(raw), secret) {
			t.Errorf("header = %q %v, want the stored credential", raw, err)
		}
		pulls := r.fake.PullAuth()
		if len(pulls) == 0 || pulls[0] == "" {
			t.Errorf("pull headers = %v, want the pull to carry the credential too", pulls)
		}
		// The header goes to the engine and nowhere else (T-12-39).
		run := r.runRow(t, job)
		for what, v := range map[string]any{"job": job, "run": run, "audit": r.audits(t, "image.updated")} {
			text, _ := json.Marshal(v)
			if strings.Contains(string(text), secret) || strings.Contains(string(text), sent[0].Auth) {
				t.Errorf("the %s holds the registry credential: %s", what, text)
			}
		}
		if out, _ := r.hub.Output(job.ID); strings.Contains(fmt.Sprint(out), secret) {
			t.Errorf("the job output holds the registry credential")
		}
	})

	t.Run("no credential", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil, swarmService("web", 'a'))
		r.offer("web", dg('b'))

		if job := r.run(t, r.request("web", dg('b'))); job.State != jobs.JobDone {
			t.Fatalf("job = %s, want done", job.State)
		}
		sent := r.fake.ServiceUpdates()
		if len(sent) != 1 || sent[0].Auth != "" || sent[0].Query.Get("registryAuthFrom") != "spec" {
			t.Errorf("updates = %+v, want one with no header that takes the credential from the spec", sent)
		}
		for _, a := range r.fake.PullAuth() {
			if a != "" {
				t.Errorf("a pull carried a header: %q", a)
			}
		}
	})
}

// A pull that fails ends the update before the service is touched: the run is failed with its reason, nothing was written and
// the failure is audited (plan 12-15, D-13).
func TestPullFailureChangesNothing(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: 500, Message: "registry exploded"}
	}, swarmService("web", 'a'))
	r.offer("web", dg('b'))

	job := r.run(t, r.request("web", dg('b')))
	if job.State == jobs.JobDone {
		t.Fatalf("job = done, want it to have failed")
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want none after a failed pull", n)
	}
	run := r.runRow(t, job)
	if run.State != updates.RunFailed || run.Reason != updates.ReasonPullFailed || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v, want failed with pull_failed", run)
	}
	states := map[string]string{}
	for _, it := range job.Items {
		states[it.ID] = it.State
	}
	if states["check"] != jobs.ItemDone || states["pull"] != jobs.ItemFailed || states["update"] != jobs.ItemSkipped || states["wait"] != jobs.ItemSkipped {
		t.Errorf("steps = %v, want check done, pull failed, the rest skipped", states)
	}
	rows := r.audits(t, "image.update_failed")
	if len(rows) != 1 || rows[0]["reason"] != updates.ReasonPullFailed || rows[0]["workload"] != "web" || rows[0]["_result"] != "failure" {
		t.Errorf("failure audit rows = %+v, want one with the reason", rows)
	}
	if rows := r.audits(t, "image.updated"); len(rows) != 0 {
		t.Errorf("success audit rows = %+v, want none", rows)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after the job ended")
	}
}

// Every way the new tasks can fail ends the run failed with a reason in TierPlan's words, on a clock the test controls
// (plan 12-15, D-13).
func TestFailureReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, state, text, reason, code string
	}{
		{"tasks rejected", "rejected", "No such image: registry.test/team/web", updates.ReasonDidntStart, updates.ReasonDidntStart},
		{"tasks failed", "failed", "task: non-zero exit (1)", updates.ReasonDidntStart, updates.ReasonDidntStart},
		{"tasks unhealthy", "failed", "task: non-zero exit (1): dockerexec: unhealthy container", updates.ReasonUnhealthy, updates.ReasonUnhealthy},
		{"never settles", "preparing", "", updates.ReasonTimeout, updates.ReasonTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newApplyRig(t, nil, swarmService("web", 'a'))
			r.offer("web", dg('b'))
			if tc.state == "preparing" {
				r.fake.SetTaskState("web", tc.state, tc.text)
			} else {
				// A task that ended is not wanted by the swarm any more: it is in a slot with no task left that runs.
				var script []dockertest.FakeServiceTask
				for slot := 1; slot <= 3; slot++ {
					script = append(script, dockertest.FakeServiceTask{Slot: slot, State: tc.state, Desired: "shutdown", Err: tc.text})
				}
				r.fake.SetServiceTasks("web", script...)
			}

			job := r.run(t, r.request("web", dg('b')))
			run := r.runRow(t, job)
			if run.State != updates.RunFailed || run.Reason != tc.reason {
				t.Errorf("run = %s %q, want failed with %q", run.State, run.Reason, tc.reason)
			}
			if n := len(r.fake.ServiceUpdates()); n != 1 {
				t.Errorf("service updates = %d, want the one that was written", n)
			}
			var failed jobs.Item
			for _, it := range job.Items {
				if it.State == jobs.ItemFailed {
					failed = it
				}
			}
			if failed.ID != updates.ItemWait || failed.Code != tc.code {
				t.Errorf("failed step = %+v, want wait with %q", failed, tc.code)
			}
			rows := r.audits(t, "image.update_failed")
			if len(rows) != 1 || rows[0]["reason"] != tc.reason {
				t.Errorf("failure audit rows = %+v, want one with %q", rows, tc.reason)
			}
			if _, held := r.locks.Held(r.envID); held {
				t.Errorf("the lock is still held after the job ended")
			}
		})
	}

	t.Run("the swarm stops the update", func(t *testing.T) {
		t.Parallel()
		r := newApplyRig(t, nil, swarmService("web", 'a'))
		r.offer("web", dg('b'))
		r.clock.hold = true
		r.fake.SetTaskState("web", "preparing", "")
		t.Cleanup(func() { r.clock.mu.Lock(); r.clock.hold = false; r.clock.mu.Unlock() })

		id, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		// The first read has been made and the service is not settled, and the wait is on its next poll: the swarm pauses the
		// update before it comes, so the second read sees it.
		testsupport.Await(t, "the wait to arm its first poll", r.clock.armed)
		r.fake.SetUpdateState("web", "paused", "update paused due to failure or early termination of task")
		r.clock.fire(stackops.PollInterval)
		job, err := r.hub.Wait(testsupport.Context(t), id)
		if err != nil {
			t.Fatal(err)
		}
		if run := r.runRow(t, job); run.State != updates.RunFailed || run.Reason != updates.ReasonDidntStart {
			t.Errorf("run = %s %q, want failed with didnt_start for a paused update", run.State, run.Reason)
		}
	})
}

// The services this path does not update are refused with a typed reason, before anything is taken or written (plan 12-15, D-09).
func TestNotUpdatableKinds(t *testing.T) {
	t.Parallel()
	managed := swarmService("managed", 'a')
	managed.Spec.Annotations.Labels = map[string]string{stacks.LabelStackID: "stack-1"}
	managed.Labels = managed.Spec.Annotations.Labels
	agent := dockertest.FakeService{
		ID: "svc-agent", Name: "tierplan-agent", Mode: "global", Version: 1, Image: "registry.test/team/agent:1@" + dg('a'),
	}
	job := dockertest.FakeService{
		ID: "svc-job", Name: "migrate", Mode: "replicated-job", Version: 1, Image: "registry.test/team/migrate:1@" + dg('a'),
	}
	r := newApplyRig(t, nil, managed, agent, job, swarmService("web", 'a'))
	r.applier.ManagedStack = func(_ context.Context, _, id string) (bool, error) { return id == "stack-1", nil }
	r.offer("web", dg('b'))

	for name, code := range map[string]string{
		"tierplan-agent": updates.NotUpdatableAgent, "migrate": updates.NotUpdatableJobMode, "managed": updates.NotUpdatableManaged,
	} {
		_, err := r.applier.Start(testsupport.Context(t), r.request(name, ""))
		var nu *updates.NotUpdatableError
		if !errors.As(err, &nu) || nu.Code != code {
			t.Errorf("Start(%s) = %v, want not updatable: %s", name, err, code)
		}
	}
	if _, err := r.applier.Start(testsupport.Context(t), r.request("nobody", "")); !errors.Is(err, updates.ErrWorkloadGone) {
		t.Errorf("Start(nobody) = %v, want ErrWorkloadGone", err)
	}
	req := r.request("web", "")
	req.Kind = "stack" // a stack is updated through a revision of its own, never by the applier's Start
	if _, err := r.applier.Start(testsupport.Context(t), req); !errors.Is(err, updates.ErrUnsupportedKind) {
		t.Errorf("Start(stack) = %v, want ErrUnsupportedKind", err)
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d after refusals, want none", n)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after refusals")
	}
	if runs, err := r.runs.Running(context.Background(), r.envID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v, want none", runs, err)
	}
}

// A service of a stack that was only discovered, or whose TierPlan stack no longer exists, is a plain service and is updated
// through the plain path (plan 12-15, D-09).
func TestUnmanagedStackServiceIsPlain(t *testing.T) {
	t.Parallel()
	discovered := swarmService("discovered", 'a')
	discovered.Spec.Annotations.Labels = map[string]string{"com.docker.stack.namespace": "shop"}
	discovered.Labels = discovered.Spec.Annotations.Labels
	orphan := swarmService("orphan", 'a')
	orphan.Spec.Annotations.Labels = map[string]string{stacks.LabelStackID: "ghost"}
	orphan.Labels = orphan.Spec.Annotations.Labels
	r := newApplyRig(t, nil, discovered, orphan)
	r.applier.ManagedStack = func(context.Context, string, string) (bool, error) { return false, nil }
	for _, name := range []string{"discovered", "orphan"} {
		r.offer(name, dg('b'))
		if job := r.run(t, r.request(name, dg('b'))); job.State != jobs.JobDone {
			t.Errorf("update of %s = %s %+v, want done", name, job.State, job.Items)
		}
	}
	if n := len(r.fake.ServiceUpdates()); n != 2 {
		t.Errorf("service updates = %d, want 2", n)
	}
}

// The wait for a healthy check is a step of its own, present only when the image defines a check, and it ends with the wait
// (plan 12-15, UI-SPEC "Steps (plain service)").
func TestHealthStepOnlyWhenTheImageHasACheck(t *testing.T) {
	t.Parallel()
	with := swarmService("checked", 'a')
	with.Spec.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "true"}, Interval: time.Second}
	r := newApplyRig(t, nil, with, swarmService("plain", 'a'))
	for _, name := range []string{"checked", "plain"} {
		r.offer(name, dg('b'))
	}
	steps := func(name string) []string {
		job := r.run(t, r.request(name, dg('b')))
		if job.State != jobs.JobDone {
			t.Fatalf("update of %s = %s %+v, want done", name, job.State, job.Items)
		}
		var ids []string
		for _, it := range job.Items {
			if it.State != jobs.ItemDone {
				t.Errorf("%s: step %s = %s, want done", name, it.ID, it.State)
			}
			ids = append(ids, it.ID)
		}
		return ids
	}
	if got := steps("checked"); !reflect.DeepEqual(got, []string{"check", "pull", "update", "wait", "health"}) {
		t.Errorf("steps of an image with a check = %v", got)
	}
	if got := steps("plain"); !reflect.DeepEqual(got, []string{"check", "pull", "update", "wait"}) {
		t.Errorf("steps of an image with none = %v", got)
	}
}
