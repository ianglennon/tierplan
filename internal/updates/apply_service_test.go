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
	cluster *envs.Clusters
	locks   *stackops.Locks
	runs    updates.RunStore
	applier *updates.Applier
	// failures is the memory of the digests updates failed with, shared by the applier and the service that judges.
	failures *updates.FailureStore
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
	failures := &updates.FailureStore{Q: st.Q, W: st.Write}
	usvc.Failures, usvc.UpdateRuns = failures, &runs
	return &applyRig{
		fake: fake, envID: env.ID, st: st, reg: rs, heads: heads, clock: clk, hub: hub, cluster: clusters, locks: locks, runs: runs, failures: failures,
		applier: &updates.Applier{
			Service: usvc, Runs: &runs, Locks: locks, Jobs: hub, Aggregator: agg, Resolver: registry.NewResolver(rs), Audit: aud,
			Clock: clk, ConvergeTimeout: 30 * time.Second, Failures: failures, Log: log,
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

// Every way the new tasks can fail puts the previous version back and ends the run rolled back with the reason in TierPlan's
// words, on a clock the test controls (plan 12-15, D-13; the rollback is plan 12-22's, D-12). The new version's tasks fail, the
// previous version's run: tasks are scripted for the writes whose image holds the new digest.
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
				r.fake.SetTaskStateWhileImage("web", dg('b'), tc.state, tc.text)
			} else {
				// A task that ended is not wanted by the swarm any more: it is in a slot with no task left that runs.
				var script []dockertest.FakeServiceTask
				for slot := 1; slot <= 3; slot++ {
					script = append(script, dockertest.FakeServiceTask{Slot: slot, State: tc.state, Desired: "shutdown", Err: tc.text})
				}
				r.fake.SetServiceTasksWhileImage("web", dg('b'), script...)
			}

			job := r.run(t, r.request("web", dg('b')))
			run := r.runRow(t, job)
			if run.State != updates.RunRolledBack || run.Reason != tc.reason {
				t.Errorf("run = %s %q, want rolled_back with %q", run.State, run.Reason, tc.reason)
			}
			if n := len(r.fake.ServiceUpdates()); n != 2 {
				t.Errorf("service updates = %d, want the one that was written and the one that put the previous version back", n)
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
		r.fake.SetTaskStateWhileImage("web", dg('b'), "preparing", "")
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
		if run := r.runRow(t, job); run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart {
			t.Errorf("run = %s %q, want rolled_back with didnt_start for a paused update", run.State, run.Reason)
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

// failureOf is the failure remembered for a service, and whether there is one.
func (r *applyRig) failureOf(t *testing.T, name string) (updates.Failure, bool) {
	t.Helper()
	all, err := r.failures.ForEnv(context.Background(), r.envID)
	if err != nil {
		t.Fatalf("read the failures: %v", err)
	}
	f, ok := all[updates.FailureKey("service", name)]
	return f, ok
}

// verdictOf is what the page would say about a service now.
func (r *applyRig) verdictOf(t *testing.T, name string) updates.Verdict {
	t.Helper()
	sv, found, err := r.applier.Service.Find(testsupport.Context(t), r.envID, "service", name)
	if err != nil || !found {
		t.Fatalf("Find(%s) = %v found %v", name, err, found)
	}
	return sv.Verdict
}

// An update whose new tasks keep failing is put back by TierPlan: the second write carries the previous digest, with a fresh
// registry header, the run ends rolled back, the digest that failed is remembered and the badge says so, and the failure and the
// rollback are each audited (plan 12-22, IMG-07, D-12, D-14, T-12-63).
func TestServiceUpdateFailureRollsBack(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-token-value-0123"
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	cred, err := r.reg.Create(testsupport.Context(t), registry.CreateInput{
		Name: "team", Type: registry.TypeGeneric, Host: "registry.test", Prefix: "team", Username: "bot", Secret: secret,
	}, "admin-1", nil)
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	r.heads.set("registry.test/team/web:1", answer{digest: dg('b'), credID: cred.ID})
	var script []dockertest.FakeServiceTask
	for slot := 1; slot <= 3; slot++ {
		script = append(script, dockertest.FakeServiceTask{Slot: slot, State: "rejected", Desired: "shutdown", Err: "No such image"})
	}
	r.fake.SetServiceTasksWhileImage("web", dg('b'), script...)

	job := r.run(t, r.request("web", dg('b')))
	run := r.runRow(t, job)
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart || run.FinishedAt.IsZero() {
		t.Fatalf("run = %s %q, want rolled_back with didnt_start", run.State, run.Reason)
	}

	sent := r.fake.ServiceUpdates()
	if len(sent) != 2 {
		t.Fatalf("service updates = %d, want the update and the rollback", len(sent))
	}
	want := swarmSpec("web", 'a')
	if !reflect.DeepEqual(sent[1].Spec, want) {
		t.Errorf("the rollback spec differs from the original in more than nothing:\n got %+v\nwant %+v", sent[1].Spec, want)
	}
	if got := sent[1].Spec.TaskTemplate.ContainerSpec.Image; got != "registry.test/team/web:1@"+dg('a') {
		t.Errorf("rollback image = %q, want the previous digest", got)
	}
	if sent[1].Auth == "" || sent[1].Query.Get("registryAuthFrom") != "" {
		t.Errorf("rollback auth = %q from %q, want the registry header again", sent[1].Auth, sent[1].Query.Get("registryAuthFrom"))
	}
	if sent[1].Version != "2" {
		t.Errorf("rollback version = %s, want the version the update left (2)", sent[1].Version)
	}

	states := map[string]string{}
	for _, it := range job.Items {
		states[it.ID] = it.State
	}
	if states["wait"] != jobs.ItemFailed || states["rollback"] != jobs.ItemDone || states["rollback_wait"] != jobs.ItemDone {
		t.Errorf("steps = %v, want wait failed and both rollback steps done", states)
	}

	f, ok := r.failureOf(t, "web")
	if !ok || f.Digest != dg('b') || f.Reason != updates.ReasonDidntStart || f.RollbackFailed || f.RunID != run.ID {
		t.Errorf("failure = %+v %v, want the new digest with didnt_start, not rollback_failed", f, ok)
	}
	v := r.verdictOf(t, "web")
	if v.State != updates.StateRolledBack || v.Failure == nil || v.Failure.Digest != dg('b') || v.FailureJob != job.ID {
		t.Errorf("verdict = %s %+v job %q, want rolled_back for the digest that failed, with its job", v.State, v.Failure, v.FailureJob)
	}

	failed, rolled := r.audits(t, "image.update_failed"), r.audits(t, "image.update_rolled_back")
	if len(failed) != 1 || failed[0]["reason"] != updates.ReasonDidntStart || failed[0]["_result"] != "failure" {
		t.Errorf("failure audit rows = %+v, want one with the reason", failed)
	}
	if len(rolled) != 1 || rolled[0]["_result"] != "success" || rolled[0]["workload"] != "web" || rolled[0]["from_digest"] != dg('a') || rolled[0]["to_digest"] != dg('b') || rolled[0]["reason"] != updates.ReasonDidntStart {
		t.Errorf("rollback audit rows = %+v, want one success for web", rolled)
	}
	if rows := r.audits(t, "image.updated"); len(rows) != 0 {
		t.Errorf("success audit rows = %+v, want none", rows)
	}
	// The credential went to the engine only (T-12-66).
	for what, v := range map[string]any{"job": job, "run": run, "failure audit": failed, "rollback audit": rolled} {
		text, _ := json.Marshal(v)
		if strings.Contains(string(text), secret) || strings.Contains(string(text), sent[1].Auth) {
			t.Errorf("the %s holds the registry credential: %s", what, text)
		}
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after the job ended")
	}
}

// failingTasks scripts the tasks of the service so those of any write whose image holds the digest are rejected.
func (r *applyRig) failingTasks(name, digest, text string) {
	var script []dockertest.FakeServiceTask
	for slot := 1; slot <= 3; slot++ {
		script = append(script, dockertest.FakeServiceTask{Slot: slot, State: "rejected", Desired: "shutdown", Err: text})
	}
	r.fake.SetServiceTasksWhileImage(name, digest, script...)
}

// A pull that fails has nothing to roll back: the run is failed, not rolled back, no write was made and no restore row exists, but
// the digest that could not be fetched is remembered (plan 12-22, D-13, D-14).
func TestPullFailureIsRecordedNotRolledBack(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: 500, Message: "registry exploded"}
	}, swarmService("web", 'a'))
	r.offer("web", dg('b'))

	job := r.run(t, r.request("web", dg('b')))
	run := r.runRow(t, job)
	if run.State != updates.RunFailed || run.Reason != updates.ReasonPullFailed {
		t.Fatalf("run = %s %q, want failed with pull_failed", run.State, run.Reason)
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want none: there was nothing to roll back", n)
	}
	f, ok := r.failureOf(t, "web")
	if !ok || f.Digest != dg('b') || f.Reason != updates.ReasonPullFailed || f.RollbackFailed {
		t.Errorf("failure = %+v %v, want the digest that failed to pull, not rollback_failed", f, ok)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != updates.ReasonPullFailed {
		t.Errorf("failure audit rows = %+v, want one with pull_failed", rows)
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 0 {
		t.Errorf("rollback audit rows = %+v, want none", rows)
	}
	for _, it := range job.Items {
		if it.ID == updates.ItemRollback || it.ID == updates.ItemRollbackWait {
			t.Errorf("job has the step %s, want no rollback steps on a failed pull", it.ID)
		}
	}
	if v := r.verdictOf(t, "web"); v.State != updates.StateRolledBack {
		t.Errorf("verdict = %s, want the digest that failed not offered again", v.State)
	}
}

// New tasks that never settle before the converge timeout are rolled back with the reason timeout, on a clock the test does not
// have to move (plan 12-22, D-13).
func TestTimeoutRollsBack(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	r.fake.SetTaskStateWhileImage("web", dg('b'), "preparing", "")

	job := r.run(t, r.request("web", dg('b')))
	run := r.runRow(t, job)
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonTimeout {
		t.Fatalf("run = %s %q, want rolled_back with timeout", run.State, run.Reason)
	}
	if sent := r.fake.ServiceUpdates(); len(sent) != 2 || sent[1].Spec.TaskTemplate.ContainerSpec.Image != "registry.test/team/web:1@"+dg('a') {
		t.Errorf("service updates = %+v, want the update and the previous digest written back", sent)
	}
	if f, ok := r.failureOf(t, "web"); !ok || f.Reason != updates.ReasonTimeout || f.Digest != dg('b') {
		t.Errorf("failure = %+v %v, want the new digest with timeout", f, ok)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != updates.ReasonTimeout {
		t.Errorf("failure audit rows = %+v", rows)
	}
}

// Where the image defines a health check, tasks that stay starting past the window are rolled back as unhealthy, and the health step
// is the one that failed and counts the window in seconds (plan 12-22, D-13, RESEARCH A2).
func TestUnhealthyRollsBack(t *testing.T) {
	t.Parallel()
	with := swarmService("checked", 'a')
	with.Spec.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "true"}, Interval: time.Second}
	r := newApplyRig(t, nil, with)
	r.offer("checked", dg('b'))
	r.fake.SetTaskStateWhileImage("checked", dg('b'), "starting", "")

	job := r.run(t, r.request("checked", dg('b')))
	run := r.runRow(t, job)
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonUnhealthy {
		t.Fatalf("run = %s %q, want rolled_back with unhealthy", run.State, run.Reason)
	}
	byID := map[string]jobs.Item{}
	for _, it := range job.Items {
		byID[it.ID] = it
	}
	if h := byID[updates.ItemHealth]; h.State != jobs.ItemFailed || h.Code != updates.ReasonUnhealthy || h.Unit != "seconds" || h.Total != 30 {
		t.Errorf("health step = %+v, want failed unhealthy counting a window of 30 seconds", h)
	}
	if byID[updates.ItemRollback].State != jobs.ItemDone || byID[updates.ItemRollbackWait].State != jobs.ItemDone {
		t.Errorf("rollback steps = %+v %+v, want both done", byID[updates.ItemRollback], byID[updates.ItemRollbackWait])
	}
	if f, ok := r.failureOf(t, "checked"); !ok || f.Reason != updates.ReasonUnhealthy {
		t.Errorf("failure = %+v %v, want unhealthy", f, ok)
	}

	// Without a health check the same wait is a timeout, not unhealthy.
	plain := newApplyRig(t, nil, swarmService("plain", 'a'))
	plain.offer("plain", dg('b'))
	plain.fake.SetTaskStateWhileImage("plain", dg('b'), "starting", "")
	if got := plain.runRow(t, plain.run(t, plain.request("plain", dg('b')))); got.State != updates.RunRolledBack || got.Reason != updates.ReasonTimeout {
		t.Errorf("a service with no health check whose tasks keep starting: run = %s %q, want rolled_back with timeout", got.State, got.Reason)
	}
}

// The person's own failure action may roll the service back before TierPlan looks: the restore then writes nothing, waits for the
// swarm's rollback, and the run still ends rolled back with its audit rows once (plan 12-22, D-12, RESEARCH Pitfall 10).
func TestEngineRolledBackFirst(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	r.clock.hold = true
	r.fake.SetTaskStateWhileImage("web", dg('b'), "preparing", "")
	t.Cleanup(func() { r.clock.mu.Lock(); r.clock.hold = false; r.clock.mu.Unlock() })

	id, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The first read found the new tasks starting; before the next, the swarm's failure action puts the previous spec back.
	testsupport.Await(t, "the wait to arm its first poll", r.clock.armed)
	if !r.fake.RollBackService("web") {
		t.Fatal("the fake could not roll the service back")
	}
	r.clock.fire(stackops.PollInterval)
	job, err := r.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}

	if n := len(r.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want only the update: the swarm already put the old image back", n)
	}
	run := r.runRow(t, job)
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart {
		t.Errorf("run = %s %q, want rolled_back with didnt_start", run.State, run.Reason)
	}
	if f, ok := r.failureOf(t, "web"); !ok || f.Digest != dg('b') || f.RollbackFailed {
		t.Errorf("failure = %+v %v, want the new digest, rolled back", f, ok)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 {
		t.Errorf("failure audit rows = %+v, want one", rows)
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 || rows[0]["_result"] != "success" {
		t.Errorf("rollback audit rows = %+v, want one success", rows)
	}
	for _, it := range job.Items {
		if (it.ID == updates.ItemRollback || it.ID == updates.ItemRollbackWait) && it.State != jobs.ItemDone {
			t.Errorf("step %s = %s, want done", it.ID, it.State)
		}
	}
}

// A restore the swarm refuses leaves the service as it is and says so: the run is rollback_failed, the failure is remembered as one,
// the badge says so and the audit log says the rollback failed (plan 12-22, D-14).
func TestRestoreRefusedIsRollbackFailed(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	r.clock.hold = true
	r.fake.SetTaskStateWhileImage("web", dg('b'), "preparing", "")
	t.Cleanup(func() { r.clock.mu.Lock(); r.clock.hold = false; r.clock.mu.Unlock() })

	id, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	testsupport.Await(t, "the wait to arm its first poll", r.clock.armed)
	// The write of the new version was taken; the swarm now refuses every other, and pauses the update.
	r.fake.RefuseServiceUpdates("web", "spec refused: engine words")
	r.fake.SetUpdateState("web", "paused", "update paused due to failure or early termination of task")
	r.clock.fire(stackops.PollInterval)
	job, err := r.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}

	run := r.runRow(t, job)
	if run.State != updates.RunRollbackFailed || run.Reason != updates.ReasonRestoreFailed {
		t.Fatalf("run = %s %q, want rollback_failed with restore_failed", run.State, run.Reason)
	}
	if n := len(r.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want only the update: the restore was refused", n)
	}
	f, ok := r.failureOf(t, "web")
	if !ok || !f.RollbackFailed || f.Digest != dg('b') || f.Reason != updates.ReasonDidntStart {
		t.Errorf("failure = %+v %v, want the new digest with rollback_failed and the reason the update failed", f, ok)
	}
	if v := r.verdictOf(t, "web"); v.State != updates.StateRollbackFailed {
		t.Errorf("verdict = %s, want rollback_failed", v.State)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != updates.ReasonDidntStart {
		t.Errorf("failure audit rows = %+v, want one with the reason the update failed", rows)
	}
	rows := r.audits(t, "image.update_rolled_back")
	if len(rows) != 1 || rows[0]["_result"] != "failure" || rows[0]["reason"] != updates.ReasonDidntStart {
		t.Errorf("rollback audit rows = %+v, want one failure", rows)
	}
	byID := map[string]jobs.Item{}
	for _, it := range job.Items {
		byID[it.ID] = it
	}
	if rb := byID[updates.ItemRollback]; rb.State != jobs.ItemFailed || rb.Code != updates.CodeRestoreRefused || strings.Contains(rb.Message+rb.Detail, "engine words") {
		t.Errorf("rollback step = %+v, want failed with TierPlan's own words", rb)
	}
	if byID[updates.ItemRollbackWait].State != jobs.ItemSkipped {
		t.Errorf("rollback_wait = %s, want skipped", byID[updates.ItemRollbackWait].State)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after the job ended")
	}
}

// Neither write touches the person's update_config (or any setting but the image): it is byte-equal before, after the update and
// after the restore (plan 12-22, T-12-64).
func TestUpdateConfigNeverOverwritten(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	r.failingTasks("web", dg('b'), "No such image")
	before, err := json.Marshal(swarmSpec("web", 'a').UpdateConfig)
	if err != nil {
		t.Fatal(err)
	}

	job := r.run(t, r.request("web", dg('b')))
	if run := r.runRow(t, job); run.State != updates.RunRolledBack {
		t.Fatalf("run = %s %q, want rolled_back", run.State, run.Reason)
	}
	sent := r.fake.ServiceUpdates()
	if len(sent) != 2 {
		t.Fatalf("service updates = %d, want 2", len(sent))
	}
	for i, u := range sent {
		after, err := json.Marshal(u.Spec.UpdateConfig)
		if err != nil || !bytes.Equal(before, after) {
			t.Errorf("write %d: update_config = %s, want %s", i+1, after, before)
		}
	}
	// The restore changes the image and nothing else: it is the original spec, field for field.
	if !reflect.DeepEqual(sent[1].Spec, swarmSpec("web", 'a')) {
		t.Errorf("the restore differs from the original spec:\n got %+v\nwant %+v", sent[1].Spec, swarmSpec("web", 'a'))
	}
}

// Somebody else changing the service while the update fails is left alone: the restore does not write an image over a third one
// (plan 12-22, T-12-64).
func TestRestoreLeavesAThirdImageAlone(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	r.clock.hold = true
	r.fake.SetTaskStateWhileImage("web", dg('b'), "preparing", "")
	t.Cleanup(func() { r.clock.mu.Lock(); r.clock.hold = false; r.clock.mu.Unlock() })

	id, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	testsupport.Await(t, "the wait to arm its first poll", r.clock.armed)
	// A person puts a third image on the service by hand.
	third := swarmSpec("web", 'a')
	third.TaskTemplate.ContainerSpec.Image = "registry.test/team/web:1@" + dg('c')
	if !r.fake.ReplaceServiceSpec("web", third) {
		t.Fatal("the fake could not replace the spec")
	}
	r.fake.SetUpdateState("web", "paused", "update paused due to failure or early termination of task")
	r.clock.fire(stackops.PollInterval)
	job, err := r.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want only the update: the third image is not written over", n)
	}
	if run := r.runRow(t, job); run.State != updates.RunFailed || run.Reason != updates.ReasonDidntStart {
		t.Errorf("run = %s %q, want failed with didnt_start: nothing was put back", run.State, run.Reason)
	}
	for _, it := range job.Items {
		if it.ID == updates.ItemRollback && it.Code != updates.CodeRestoreChanged {
			t.Errorf("rollback step = %+v, want %s", it, updates.CodeRestoreChanged)
		}
	}
}

// A good update forgets the failure the workload had, and the failure stops hiding anything once the tag points at another digest
// (plan 12-22, D-14).
func TestSuccessClearsFailure(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	if err := r.failures.Record(context.Background(), updates.Failure{
		EnvID: r.envID, Kind: "service", Workload: "web", Digest: dg('b'), Reason: updates.ReasonUnhealthy, RunID: "old-run", FailedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	// A failure of another workload of the same name and kind elsewhere is not touched.
	if err := r.failures.Record(context.Background(), updates.Failure{
		EnvID: r.envID, Kind: "service", Workload: "api", Digest: dg('b'), Reason: updates.ReasonUnhealthy, RunID: "old-run", FailedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	r.offer("web", dg('c'))

	job := r.run(t, r.request("web", dg('c')))
	if run := r.runRow(t, job); run.State != updates.RunDone {
		t.Fatalf("run = %s %q, want done", run.State, run.Reason)
	}
	if f, ok := r.failureOf(t, "web"); ok {
		t.Errorf("failure = %+v, want it forgotten after a good update", f)
	}
	if _, ok := r.failureOf(t, "api"); !ok {
		t.Errorf("the failure of another workload was forgotten")
	}
}

// A person can try a version that failed before, on purpose and one workload at a time: without force the update is refused with
// nothing changed, with force it runs to the digest that failed, and when it works the failure is forgotten and the badge reads up to
// date (plan 12-45, IMG-07, D-14). The version whose pull failed is remembered the same way, so a registry glitch is recoverable.
func TestUpdateAnywayClearsFailure(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{updates.ReasonUnhealthy, updates.ReasonPullFailed} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			r := newApplyRig(t, nil, swarmService("web", 'a'), swarmService("api", 'a'))
			for _, n := range []string{"web", "api"} {
				if err := r.failures.Record(context.Background(), updates.Failure{
					EnvID: r.envID, Kind: "service", Workload: n, Digest: dg('b'), Reason: reason, RunID: "old-run", FailedAt: t0,
				}); err != nil {
					t.Fatal(err)
				}
				r.offer(n, dg('b'))
			}

			// Without force the failed version is refused, with the failure at hand, and nothing is written or recorded.
			_, err := r.applier.Start(testsupport.Context(t), r.request("web", dg('b')))
			var prev *updates.PreviouslyFailedError
			if !errors.Is(err, updates.ErrPreviouslyFailed) || !errors.As(err, &prev) || prev.Failure == nil || prev.Failure.Reason != reason {
				t.Fatalf("Start without force = %v, want ErrPreviouslyFailed with the reason %s", err, reason)
			}
			if n := len(r.fake.ServiceUpdates()); n != 0 {
				t.Fatalf("service updates = %d, want none for a refused update", n)
			}
			if v := r.verdictOf(t, "web"); v.State != updates.StateRolledBack {
				t.Fatalf("verdict = %s, want rolled_back for the digest that failed", v.State)
			}
			if _, held := r.locks.Held(r.envID); held {
				t.Fatalf("the refused update still holds the lock")
			}

			// With force it runs to the digest the registry has, ends done, forgets the failure and reads up to date.
			req := r.request("web", dg('b'))
			req.Force = true
			job := r.run(t, req)
			run := r.runRow(t, job)
			if run.State != updates.RunDone {
				t.Fatalf("run = %s %q, want done", run.State, run.Reason)
			}
			sent := r.fake.ServiceUpdates()
			if len(sent) != 1 || sent[0].Spec.TaskTemplate.ContainerSpec.Image != "registry.test/team/web:1@"+dg('b') {
				t.Fatalf("service updates = %d, want one to the digest that failed", len(sent))
			}
			if f, ok := r.failureOf(t, "web"); ok {
				t.Errorf("failure = %+v, want it forgotten after the forced update worked", f)
			}
			// A server reads the cluster again when the engine's events say something changed; the rig has no events.
			if err := r.cluster.Refresh(testsupport.Context(t), r.envID); err != nil {
				t.Fatalf("read the cluster again: %v", err)
			}
			if v := r.verdictOf(t, "web"); v.State != updates.StateUpToDate {
				t.Errorf("verdict = %s running %s remote %s, want up_to_date", v.State, v.RunningDigest, v.RemoteDigest)
			}
			if _, ok := r.failureOf(t, "api"); !ok {
				t.Errorf("the failure of another workload was forgotten")
			}
			if rows := r.audits(t, "image.updated"); len(rows) != 1 || rows[0]["workload"] != "web" {
				t.Errorf("image.updated rows = %+v, want one for web", rows)
			}
		})
	}
}

// Force means nothing for a workload that has no failed update: it is an ordinary run, and a failure of another digest does not make
// force retry anything (plan 12-45).
func TestForceIgnoredWithoutAFailure(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('c'))
	// What failed was an older version; the registry has moved on, so this is an ordinary update.
	if err := r.failures.Record(context.Background(), updates.Failure{
		EnvID: r.envID, Kind: "service", Workload: "web", Digest: dg('b'), Reason: updates.ReasonTimeout, RunID: "old-run", FailedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	req := r.request("web", dg('c'))
	req.Force = true
	if run := r.runRow(t, r.run(t, req)); run.State != updates.RunDone {
		t.Fatalf("run = %s %q, want done", run.State, run.Reason)
	}
}
