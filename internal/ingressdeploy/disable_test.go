// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// steppingClock is a clock that moves only when something waits on it: After advances Now by the wait and answers at once,
// and hook, when set, runs on every wait with the number of waits so far, so a test changes the swarm between two polls of
// the deployer without a sleep.
type steppingClock struct {
	mu    sync.Mutex
	at    time.Time
	waits int
	hook  func(n int)
}

func newSteppingClock() *steppingClock {
	return &steppingClock{at: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *steppingClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.waits++
	n, at, hook := c.waits, c.at, c.hook
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	ch := make(chan time.Time, 1)
	ch <- at
	return ch
}

func (c *steppingClock) waited() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits
}

// kickRecorder records every KickEnv and what the swarm and the environment looked like at that instant.
type kickRecorder struct {
	mu    sync.Mutex
	calls []string
	// probe runs inside KickEnv; its answer is kept with the call.
	probe  func() string
	probes []string
}

func (k *kickRecorder) KickEnv(envID, code string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, envID+" "+code)
	if k.probe != nil {
		k.probes = append(k.probes, k.probe())
	}
}

// volEngine routes the calls for a node to that node's own fake engine, and fails the volume removal of a node a test marks.
type volEngine struct {
	ingressdeploy.Engine
	mu         sync.Mutex
	byAgent    map[string]*dockerx.Client
	failRemove map[string]bool
	// removes counts the volume removal calls that reached each node.
	removes map[string]int
}

func (e *volEngine) OnNode(ctx context.Context, envID, agentID string, fn func(*dockerx.Client) error) error {
	if c, ok := e.byAgent[agentID]; ok {
		return fn(c)
	}
	return e.Engine.OnNode(ctx, envID, agentID, fn)
}

func (e *volEngine) WriteOnNode(ctx context.Context, envID, agentID string, fn func(*dockerx.Client) error) error {
	e.mu.Lock()
	e.removes[agentID]++
	fail := e.failRemove[agentID]
	e.mu.Unlock()
	if fail {
		return errors.New("the node's engine did not answer")
	}
	if c, ok := e.byAgent[agentID]; ok {
		return fn(c)
	}
	return e.Engine.WriteOnNode(ctx, envID, agentID, fn)
}

func (e *volEngine) removeCalls(agentID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.removes[agentID]
}

// cacheVolume is the certificate cache volume a proxy node holds.
func cacheVolume() dockertest.FakeVolume {
	return dockertest.FakeVolume{Name: ingress.VolumeName, Driver: "local", Labels: ingress.ObjectLabels()}
}

// proxyTask is a running task of the proxy service, as the swarm keeps one after its service is removed.
func proxyTask() swarm.Task {
	return swarm.Task{
		ID: "task-proxy", ServiceID: "gone", NodeID: "node-1", DesiredState: swarm.TaskStateRunning,
		Annotations: swarm.Annotations{Labels: ingress.ObjectLabels()},
		Status:      swarm.TaskStatus{State: swarm.TaskStateRunning},
	}
}

// disableFix is a swarm of a manager and, when asked, a worker with its own engine, with ingress switched on.
type disableFix struct {
	*rig
	clock  *steppingClock
	eng    *volEngine
	kicker *kickRecorder
	// base is the fake manager's configuration, so a test can reconfigure it.
	base dockertest.FakeConfig
	// worker is the worker's engine and its fake.
	worker     *dockerx.Client
	workerFake *dockertest.Fake
	events     chan envs.HealthEvent
}

type disableOpts struct {
	worker bool
	// workerAway leaves the worker's agent disconnected at the time of the switch-off.
	workerAway bool
	// tasks leaves a running proxy task in the swarm until a test removes it.
	tasks bool
}

func newDisableFix(t *testing.T, o disableOpts) *disableFix {
	t.Helper()
	f := &disableFix{clock: newSteppingClock(), kicker: &kickRecorder{}, events: make(chan envs.HealthEvent, 8)}
	eng := &volEngine{byAgent: map[string]*dockerx.Client{}, failRemove: map[string]bool{}, removes: map[string]int{}}
	f.eng = eng
	f.rig = newRig(t, rigOptions{
		config: func(c *dockertest.FakeConfig) {
			c.TasksFollowWrites = true
			c.Volumes = []dockertest.FakeVolume{cacheVolume()}
			if o.tasks {
				c.Tasks = []swarm.Task{proxyTask()}
			}
			f.base = *c
		},
		deps: func(d *ingressdeploy.Deps) {
			eng.Engine = d.Engine
			d.Engine = eng
			d.Kicker = f.kicker
			d.NodeRows = d.Tokens.(*ingresshub.Service)
			d.Clock = f.clock
			d.Events = func(ctx context.Context) (<-chan envs.HealthEvent, func()) {
				return f.events, func() {}
			}
		},
	})
	f.kicker.probe = func() string {
		_, has := f.service()
		st, err := f.istore.DeployState(testsupport.Context(t), f.envID)
		if err != nil {
			return "error: " + err.Error()
		}
		return "service=" + boolWord(has) + " enabled=" + boolWord(st.Enabled)
	}
	if o.worker {
		fake := dockertest.NewFake(t, dockertest.FakeConfig{Volumes: []dockertest.FakeVolume{cacheVolume()}})
		cli, err := dockerx.New(fake)
		if err != nil {
			t.Fatalf("dockerx.New: %v", err)
		}
		t.Cleanup(func() { _ = cli.Close() })
		f.worker, f.workerFake = cli, fake
		eng.byAgent[workerNode.AgentID] = cli
		w := workerNode
		w.Connected = !o.workerAway
		f.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes = append(i.Nodes, w) })
	}
	wantAllDone(t, f.enable(ingressdeploy.EnableRequest{}))
	return f
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// seesAll is a canView that sees every environment.
func seesAll(string) bool { return true }

// disable runs a switch-off and waits for its job to end.
func (f *disableFix) disable(canView func(string) bool) jobs.Job {
	f.t.Helper()
	id, err := f.svc.Disable(testsupport.Context(f.t), testActor, testOwner, f.envID, canView)
	if err != nil {
		f.t.Fatalf("Disable: %v", err)
	}
	return f.wait(id)
}

// hasVolume says whether the node's engine holds the cache volume.
func hasVolume(t *testing.T, c *dockerx.Client) bool {
	t.Helper()
	ok, err := c.VolumeExists(testsupport.Context(t), ingress.VolumeName)
	if err != nil {
		t.Fatalf("VolumeExists: %v", err)
	}
	return ok
}

// addNodeRow enrols a proxy node row as the enrolment would leave it.
func (f *disableFix) addNodeRow(name, swarmNode, status string) string {
	f.t.Helper()
	id := uuid.NewString()
	key := sha256.Sum256([]byte(name))
	if _, err := f.st.Write.Exec(`INSERT INTO ingress_nodes
		(id, env_id, name, swarm_node_id, public_key, status, created_at, revoked_at, revoked_by, revoke_reason, reenrol_blocked)
		VALUES (?, ?, ?, ?, ?, ?, 1, 0, '', '', 0)`, id, f.envID, name, swarmNode, key[:], status); err != nil {
		f.t.Fatalf("insert an ingress node: %v", err)
	}
	return id
}

// seedRoute claims host for a stack of the environment (a new one named stack unless it exists) with one route at path.
// A stopped stack keeps its claim.
func (f *disableFix) seedRoute(envID, stackID, stack, host, path string, stopped bool) {
	f.t.Helper()
	exec := func(q string, args ...any) {
		f.t.Helper()
		if _, err := f.st.Write.Exec(q, args...); err != nil {
			f.t.Fatalf("seed %q: %v", q, err)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM stacks WHERE id = ?`, stackID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	if n == 0 {
		stop := 0
		if stopped {
			stop = 1
		}
		exec(`INSERT INTO stacks (id, env_id, name, target, created_by, created_at, stopped) VALUES (?, ?, ?, 'swarm', 'tester', 1, ?)`, stackID, envID, stack, stop)
	}
	domain := uuid.NewString()
	suffix := host[strings.Index(host, ".")+1:]
	var existing string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(MAX(id), '') FROM ingress_domains WHERE env_id = ? AND suffix = ?`, envID, suffix).Scan(&existing); err != nil {
		f.t.Fatal(err)
	}
	if existing == "" {
		exec(`INSERT INTO ingress_domains (id, env_id, suffix, issuer, created_by, created_at, updated_at) VALUES (?, ?, ?, 'staging', 'tester', 1, 1)`, domain, envID, suffix)
	} else {
		domain = existing
	}
	var claimed int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_hostnames WHERE hostname = ?`, host).Scan(&claimed); err != nil {
		f.t.Fatal(err)
	}
	if claimed == 0 {
		exec(`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES (?, ?, ?, 'stack', ?, 'alice', 1)`, host, envID, domain, stackID)
	}
	exec(`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, created_at, updated_at) VALUES (?, ?, ?, '', ?, 'web', 8080, 1, 1)`,
		uuid.NewString(), envID, host, path)
}

// otherEnv adds a second environment to the install and returns its ID.
func (f *disableFix) otherEnv() string {
	f.t.Helper()
	id := uuid.NewString()
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		VALUES (?, 'agent', 'staging', '', 1, 1, 1, 'swarm')`, id); err != nil {
		f.t.Fatalf("insert environment: %v", err)
	}
	return id
}

// wantDisableDone fails the test unless every item of the disable job ended done, in the order the job lists them.
func wantDisableDone(t *testing.T, job jobs.Job) {
	t.Helper()
	want := []string{"switch_off", "service", "volumes", "network", "secret", "token", "nodes"}
	var got []string
	for _, it := range job.Items {
		got = append(got, it.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if job.Kind != ingressdeploy.JobKindDisable || ingressdeploy.JobKindDisable != "ingress_disable" {
		t.Errorf("job kind = %q, want ingress_disable", job.Kind)
	}
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, it := range job.Items {
		if it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", it.ID, it.State, it.Code)
		}
	}
}

// wantRefused runs Disable and its preview and fails unless both refuse with the one finding code; it returns that finding
// as the preview reports it, and checks that the refusal started no job and changed nothing.
func (f *disableFix) wantRefused(canView func(string) bool, code string) ingressdeploy.Finding {
	f.t.Helper()
	ctx := testsupport.Context(f.t)
	prev, err := f.svc.PreviewDisable(ctx, f.envID, canView)
	if err != nil {
		f.t.Fatalf("PreviewDisable: %v", err)
	}
	if len(prev.Findings) != 1 || prev.Findings[0].Code != code || prev.Findings[0].Severity != ingressdeploy.SeverityError {
		f.t.Fatalf("preview findings = %+v, want exactly %s as an error", prev.Findings, code)
	}
	id, err := f.svc.Disable(ctx, testActor, testOwner, f.envID, canView)
	var refused *ingressdeploy.DisableRefusedError
	if !errors.As(err, &refused) || id != "" {
		f.t.Fatalf("Disable = %q, %v; want a *DisableRefusedError and no job", id, err)
	}
	if len(refused.Findings) != 1 || refused.Findings[0].Code != code {
		f.t.Errorf("Disable's findings = %+v, want %s", refused.Findings, code)
	}
	if _, ok := f.service(); !ok {
		f.t.Error("a refused switch-off removed the proxy service")
	}
	if st, _ := f.istore.DeployState(ctx, f.envID); !st.Enabled {
		f.t.Error("a refused switch-off turned ingress off")
	}
	if got := f.auditCount("ingress.disabled"); got != 0 {
		f.t.Errorf("a refused switch-off audited ingress.disabled %d times", got)
	}
	// The lock the refusal took was given back.
	release, held, ok := f.locks.TryAcquire(f.envID, stackops.Holder{Kind: stackops.HolderKindIngress})
	if !ok {
		f.t.Fatalf("the operation lock is still held after a refusal: %+v", held)
	}
	release()
	return prev.Findings[0]
}

// TestDisableRemovesEverything is the tracer: a switch-off of a swarm of two nodes stops delivery, closes the proxies'
// sessions before the service goes, removes the service once its tasks are gone, the cache volume on both nodes, the network,
// the join token secrets, disables the token and removes the node rows, each in its item and in that order.
func TestDisableRemovesEverything(t *testing.T) {
	f := newDisableFix(t, disableOpts{worker: true, tasks: true})
	ctx := testsupport.Context(t)
	f.addNodeRow("edge-a", "node-1", "active")
	f.addNodeRow("edge-b", "node-2", "revoked")
	// The proxy's task outlives the service for three polls, as a node takes a moment to stop its container.
	f.clock.hook = func(n int) {
		if n == 3 {
			cfg := f.base
			cfg.Tasks = nil
			f.fake.Reconfigure(cfg)
		}
	}
	prev, err := f.svc.PreviewDisable(ctx, f.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	if len(prev.Findings) != 0 || prev.Removes.Service != ingress.ServiceName || prev.Removes.Network != ingress.NetworkName ||
		len(prev.Removes.Secrets) != 1 || len(prev.Removes.Volumes) != 2 || len(prev.Unreachable) != 0 {
		t.Fatalf("preview = %+v, want nothing in the way, the service, the network, one secret, two volumes and none unreachable", prev)
	}
	writes := len(f.fake.Writes())

	job := f.disable(seesAll)
	wantDisableDone(t, job)

	// Delivery stopped and the sessions were closed while the service was still there.
	if st, err := f.istore.DeployState(ctx, f.envID); err != nil || st.Enabled {
		t.Fatalf("DeployState = %+v, %v; want ingress off", st, err)
	}
	if got := f.kicker.calls; len(got) != 1 || got[0] != f.envID+" ingress_disabled" {
		t.Fatalf("KickEnv calls = %v, want exactly one for the environment", got)
	}
	if got := f.kicker.probes; len(got) != 1 || got[0] != "service=yes enabled=no" {
		t.Errorf("at the kick the swarm was %v, want the service still there and ingress already off", got)
	}
	if f.clock.waited() < 3 {
		t.Errorf("the switch-off waited %d times, want it to have waited for the task to go", f.clock.waited())
	}

	// Everything TierPlan made is gone, on every node.
	if _, ok := f.service(); ok {
		t.Error("the proxy service is still there")
	}
	if _, ok := f.network(); ok {
		t.Error("the ingress network is still there")
	}
	if s := f.secrets(); len(s) != 0 {
		t.Errorf("secrets = %+v, want none", s)
	}
	if hasVolume(t, f.cli) || hasVolume(t, f.worker) {
		t.Error("a cache volume was left on a node")
	}
	if it := itemOf(job, ingressdeploy.ItemVolumes); it.Result["wiped"] != "2" || it.Result["unreachable"] != "" {
		t.Errorf("the volumes item's result = %v, want two wiped and none unreachable", it.Result)
	}
	if n := len(f.fake.Writes()) - writes; n == 0 {
		t.Error("the switch-off wrote nothing to the swarm")
	}

	// The token is disabled with its row and audit kept; the node rows are gone, each audited.
	tok, err := f.tokens.TokenStatus(ctx, f.envID)
	if err != nil || !tok.Exists || !tok.Disabled {
		t.Errorf("TokenStatus = %+v, %v; want a disabled token that still exists", tok, err)
	}
	if rows, err := f.tokens.ListNodes(ctx, f.envID); err != nil || len(rows) != 0 {
		t.Errorf("ListNodes = %+v, %v; want none", rows, err)
	}
	for action, want := range map[string]int{
		"ingress.disabled": 1, "ingress.join_token_disabled": 1, "ingress.node_removed": 2, "ingress.wipe_queued": 0,
	} {
		if got := f.auditCount(action); got != want {
			t.Errorf("%s audit rows = %d, want %d", action, got, want)
		}
	}

	// The operation lock is free: it was given back before the job ended.
	release, held, ok := f.locks.TryAcquire(f.envID, stackops.Holder{Kind: stackops.HolderKindIngress})
	if !ok {
		t.Fatalf("the operation lock is still held after the job: %+v", held)
	}
	release()
}
