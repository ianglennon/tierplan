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

// muxClock is the clock of a deployer under test: it answers as the clock a test last chose, so a test that switches off with a
// stepping clock can then run the wipe loop on a clock it fires by hand.
type muxClock struct {
	mu  sync.Mutex
	cur stackops.Clock
}

func (m *muxClock) set(c stackops.Clock) {
	m.mu.Lock()
	m.cur = c
	m.mu.Unlock()
}

func (m *muxClock) get() stackops.Clock {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

func (m *muxClock) Now() time.Time                         { return m.get().Now() }
func (m *muxClock) After(d time.Duration) <-chan time.Time { return m.get().After(d) }

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
	mux    *muxClock
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
	// config adjusts the fake manager's swarm and deps the deployer's dependencies, after the fixture has set its own.
	config func(*dockertest.FakeConfig)
	deps   func(*ingressdeploy.Deps)
}

func newDisableFix(t *testing.T, o disableOpts) *disableFix {
	t.Helper()
	f := &disableFix{clock: newSteppingClock(), kicker: &kickRecorder{}, events: make(chan envs.HealthEvent, 8)}
	f.mux = &muxClock{cur: f.clock}
	eng := &volEngine{byAgent: map[string]*dockerx.Client{}, failRemove: map[string]bool{}, removes: map[string]int{}}
	f.eng = eng
	f.rig = newRig(t, rigOptions{
		config: func(c *dockertest.FakeConfig) {
			c.TasksFollowWrites = true
			c.Volumes = []dockertest.FakeVolume{cacheVolume()}
			if o.tasks {
				c.Tasks = []swarm.Task{proxyTask()}
			}
			if o.config != nil {
				o.config(c)
			}
			f.base = *c
		},
		deps: func(d *ingressdeploy.Deps) {
			eng.Engine = d.Engine
			d.Engine = eng
			d.Kicker = f.kicker
			rows, ok := d.Tokens.(*ingresshub.Service)
			if !ok {
				t.Fatalf("the tokens are %T, want the hub service", d.Tokens)
			}
			d.NodeRows = rows
			d.Nodes = rows
			d.Clock = f.mux
			d.Events = func(ctx context.Context) (<-chan envs.HealthEvent, func()) {
				return f.events, func() {}
			}
			if o.deps != nil {
				o.deps(d)
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
	want := []string{"virtual_ip", "switch_off", "service", "volumes", "network", "secret", "token", "nodes"}
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

	// The token is disabled with its row and audit kept; the active node row is gone and audited, and the revoked one stays: it
	// is what keeps that host out when ingress is switched on again (WR-03).
	tok, err := f.tokens.TokenStatus(ctx, f.envID)
	if err != nil || !tok.Exists || !tok.Disabled {
		t.Errorf("TokenStatus = %+v, %v; want a disabled token that still exists", tok, err)
	}
	if rows, err := f.tokens.ListNodes(ctx, f.envID); err != nil || len(rows) != 1 || rows[0].Name != "edge-b" || rows[0].Status != "revoked" {
		t.Errorf("ListNodes = %+v, %v; want only the revoked edge-b", rows, err)
	}
	for action, want := range map[string]int{
		"ingress.disabled": 1, "ingress.join_token_disabled": 1, "ingress.node_removed": 1, "ingress.wipe_queued": 0,
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

// TestDisableRefusedWhileRoutesLive: a running stack's claim and a stopped stack's each refuse a switch-off and its preview
// with ingress_routes_live, the items read hostname, path and stack ordered by hostname then path, and a claim in another
// environment does not refuse.
func TestDisableRefusedWhileRoutesLive(t *testing.T) {
	f := newDisableFix(t, disableOpts{})
	other := f.otherEnv()
	f.seedRoute(other, "stack-x", "elsewhere", "x.other.example.org", "/", false)

	// Another environment's claim does not stand in the way.
	if prev, err := f.svc.PreviewDisable(testsupport.Context(t), f.envID, seesAll); err != nil || len(prev.Findings) != 0 {
		t.Fatalf("PreviewDisable = %+v, %v; want nothing in the way of another environment's claim", prev, err)
	}

	f.seedRoute(f.envID, "stack-1", "web", "b.apps.example.com", "/api", false)
	f.seedRoute(f.envID, "stack-1", "web", "b.apps.example.com", "/", false)
	f.seedRoute(f.envID, "stack-2", "batch", "a.apps.example.com", "/z", true)
	got := f.wantRefused(seesAll, ingressdeploy.CodeRoutesLive)
	want := []string{"a.apps.example.com/z (stack batch)", "b.apps.example.com/ (stack web)", "b.apps.example.com/api (stack web)"}
	if !slices.Equal(got.Items, want) {
		t.Errorf("items = %q, want %q", got.Items, want)
	}
	if !strings.Contains(got.Message, "Remove the labels and redeploy, or remove the stacks") || ingressdeploy.CodeRoutesLive != "ingress_routes_live" {
		t.Errorf("message = %q, code = %q", got.Message, ingressdeploy.CodeRoutesLive)
	}
	if n := f.auditCount("ingress.node_removed"); n != 0 || len(f.kicker.calls) != 0 {
		t.Errorf("a refusal kicked %v and removed %d nodes", f.kicker.calls, n)
	}
}

// TestDisableRefusalIsRedacted: the stack's name appears for a canView that sees the environment; for one that does not, and
// for none, the item carries the redacted wording and neither the stack's nor the environment's name appears anywhere.
func TestDisableRefusalIsRedacted(t *testing.T) {
	f := newDisableFix(t, disableOpts{})
	f.seedRoute(f.envID, "stack-1", "secret-shop", "shop.apps.example.com", "/", false)

	seen := f.wantRefused(seesAll, ingressdeploy.CodeRoutesLive)
	if want := []string{"shop.apps.example.com/ (stack secret-shop)"}; !slices.Equal(seen.Items, want) {
		t.Errorf("items for a caller who sees the environment = %q, want %q", seen.Items, want)
	}

	redacted := ingress.DescribeHolder(ingress.ClaimInfo{
		Claim: ingress.Claim{EnvID: f.envID, Hostname: "shop.apps.example.com", OwnerKind: ingress.OwnerStack, StackID: "stack-1"},
	}, nil)
	for name, canView := range map[string]func(string) bool{
		"a canView that sees nothing": func(string) bool { return false },
		"no canView":                  nil,
	} {
		got := f.wantRefused(canView, ingressdeploy.CodeRoutesLive)
		all := got.Message + " " + strings.Join(got.Items, " ")
		for _, leak := range []string{"secret-shop", "prod", "stack-1", f.envID} {
			if strings.Contains(all, leak) {
				t.Errorf("%s: the refusal %q names %q", name, all, leak)
			}
		}
		if len(got.Items) != 1 || !strings.Contains(got.Items[0], redacted) || !strings.HasPrefix(got.Items[0], "shop.apps.example.com/") {
			t.Errorf("%s: items = %q, want the route followed by %q", name, got.Items, redacted)
		}
	}
}

// TestDisableRefusedWhilePaused: an environment a restore paused refuses the preview and the switch-off with ingress_paused
// and starts no job, even with a route live: the pause is reported first.
func TestDisableRefusedWhilePaused(t *testing.T) {
	f := newDisableFix(t, disableOpts{})
	if _, err := f.istore.SetPaused(testsupport.Context(t), f.envID, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	f.seedRoute(f.envID, "stack-1", "web", "b.apps.example.com", "/", false)
	got := f.wantRefused(seesAll, ingressdeploy.CodePaused)
	if got.Code != "ingress_paused" {
		t.Errorf("code = %q", got.Code)
	}
	if want := "Ingress cannot be switched off while a restore has it paused. Resume it first."; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
}

// TestDisableEdges: with every node reachable nothing is unreachable; an environment with no nodes still loses its service,
// network and secret; and a second switch-off after a complete one is done with nothing left to remove or record.
func TestDisableEdges(t *testing.T) {
	t.Run("every node reachable", func(t *testing.T) {
		f := newDisableFix(t, disableOpts{worker: true})
		prev, err := f.svc.PreviewDisable(testsupport.Context(t), f.envID, seesAll)
		if err != nil || len(prev.Unreachable) != 0 {
			t.Fatalf("PreviewDisable = %+v, %v; want no unreachable node", prev, err)
		}
		job := f.disable(seesAll)
		wantDisableDone(t, job)
		if it := itemOf(job, ingressdeploy.ItemVolumes); it.Result["unreachable"] != "" || it.Result["pending"] != "0" {
			t.Errorf("volumes result = %v, want an empty unreachable list", it.Result)
		}
		if p, _ := f.istore.PendingWipes(testsupport.Context(t), f.envID); len(p) != 0 {
			t.Errorf("pending wipes = %+v, want none", p)
		}
	})

	t.Run("no nodes", func(t *testing.T) {
		f := newDisableFix(t, disableOpts{})
		f.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes = nil })
		job := f.disable(seesAll)
		wantDisableDone(t, job)
		if _, ok := f.service(); ok {
			t.Error("the service is still there")
		}
		if _, ok := f.network(); ok {
			t.Error("the network is still there")
		}
		if len(f.secrets()) != 0 {
			t.Error("a secret is still there")
		}
		if it := itemOf(job, ingressdeploy.ItemVolumes); it.Result["wiped"] != "0" {
			t.Errorf("volumes result = %v, want none wiped", it.Result)
		}
	})

	t.Run("again after a complete one", func(t *testing.T) {
		f := newDisableFix(t, disableOpts{})
		wantDisableDone(t, f.disable(seesAll))
		writes := len(f.fake.Writes())
		job := f.disable(seesAll)
		wantDisableDone(t, job)
		if n := len(f.fake.Writes()) - writes; n != 0 {
			t.Errorf("a repeat wrote %v to the swarm, want nothing to remove", f.fake.Writes()[writes:])
		}
		for action, want := range map[string]int{"ingress.disabled": 1, "ingress.join_token_disabled": 1, "ingress.wipe_queued": 0} {
			if got := f.auditCount(action); got != want {
				t.Errorf("%s audit rows = %d after a repeat, want %d", action, got, want)
			}
		}
	})
}

// TestDisableTasksThatNeverGoFailTheServiceStepAndARerunFinishes: proxy tasks still running after DisableTaskDeadline end the
// service item failed with disable_service_failed and the items after it not run, with ingress already off and the lock free;
// once they are gone the same call finishes the rest.
func TestDisableTasksThatNeverGoFailTheServiceStepAndARerunFinishes(t *testing.T) {
	f := newDisableFix(t, disableOpts{tasks: true})
	job := f.disable(seesAll)
	if job.State == jobs.JobDone {
		t.Fatalf("the job ended done with a task still running: %+v", job.Items)
	}
	if it := itemOf(job, ingressdeploy.ItemSwitchOff); it.State != jobs.ItemDone {
		t.Errorf("switch_off = %q, want done", it.State)
	}
	if it := itemOf(job, ingressdeploy.ItemService); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeDisableServiceFailed {
		t.Errorf("service = %q %q, want failed with disable_service_failed", it.State, it.Code)
	}
	for _, id := range []string{"volumes", "network", "secret", "token", "nodes"} {
		if it := itemOf(job, id); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
			t.Errorf("%s = %q %q, want skipped and not_run", id, it.State, it.Code)
		}
	}
	if !hasVolume(t, f.cli) {
		t.Error("the cache volume was wiped under a task that still ran")
	}
	// Two minutes of the injected clock, polled every PollInterval, and no real wait.
	if got, max := f.clock.waited(), int(ingressdeploy.DisableTaskDeadline/stackops.PollInterval)+1; got < max-1 || got > max {
		t.Errorf("waited %d times, want about %d", got, max)
	}

	f.fake.Reconfigure(func() dockertest.FakeConfig { c := f.base; c.Tasks = nil; return c }())
	wantDisableDone(t, f.disable(seesAll))
	if hasVolume(t, f.cli) {
		t.Error("the rerun left the cache volume")
	}
}

// TestDisableQueuesUnreachableWipes: with the worker's agent disconnected the switch-off finishes, names the worker as not
// wiped and queues one pending wipe with one audit row; a second run with the agent still away keeps it queued, audits it
// once and removes nothing twice; a node whose removal fails is queued as well.
func TestDisableQueuesUnreachableWipes(t *testing.T) {
	f := newDisableFix(t, disableOpts{worker: true, workerAway: true})
	ctx := testsupport.Context(t)
	prev, err := f.svc.PreviewDisable(ctx, f.envID, seesAll)
	if err != nil || !slices.Equal(prev.Unreachable, []string{workerNode.Name}) {
		t.Fatalf("PreviewDisable unreachable = %v, %v; want the worker", prev.Unreachable, err)
	}

	job := f.disable(seesAll)
	wantDisableDone(t, job)
	it := itemOf(job, ingressdeploy.ItemVolumes)
	if it.Result["unreachable"] != workerNode.Name || it.Result["pending"] != "1" || it.Result["wiped"] != "1" {
		t.Fatalf("volumes result = %v, want the worker named as not wiped", it.Result)
	}
	if hasVolume(t, f.cli) {
		t.Error("the manager's volume was left")
	}
	if !hasVolume(t, f.worker) {
		t.Error("the unreachable worker's volume vanished without a wipe")
	}
	pending, err := f.istore.PendingWipes(ctx, f.envID)
	if err != nil || len(pending) != 1 || pending[0].SwarmNodeID != workerNode.SwarmNodeID || pending[0].Volume != ingress.VolumeName {
		t.Fatalf("pending wipes = %+v, %v; want the worker's cache volume", pending, err)
	}
	if got := f.auditCount("ingress.wipe_queued"); got != 1 {
		t.Fatalf("ingress.wipe_queued audit rows = %d, want 1", got)
	}
	if n := f.eng.removeCalls(workerNode.AgentID); n != 0 {
		t.Errorf("%d removals were sent to a node whose agent is away", n)
	}
	managerRemoves := f.eng.removeCalls("agent-1")

	// Run again with the agent still away: still one queued wipe and one audit row, and the manager's volume is not removed twice.
	wantDisableDone(t, f.disable(seesAll))
	if p, _ := f.istore.PendingWipes(ctx, f.envID); len(p) != 1 {
		t.Errorf("pending wipes after a rerun = %+v, want still one", p)
	}
	if got := f.auditCount("ingress.wipe_queued"); got != 1 {
		t.Errorf("ingress.wipe_queued audit rows after a rerun = %d, want 1", got)
	}
	if got := f.eng.removeCalls("agent-1"); got != managerRemoves {
		t.Errorf("the rerun sent %d more removals to the manager, want none: its volume is gone", got-managerRemoves)
	}

	// The worker is back but its removal fails: queued again, which changes nothing.
	f.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[1].Connected = true })
	f.eng.failRemove[workerNode.AgentID] = true
	job = f.disable(seesAll)
	if it := itemOf(job, ingressdeploy.ItemVolumes); it.Result["unreachable"] != workerNode.Name {
		t.Errorf("volumes result after a failed removal = %v, want the worker named", it.Result)
	}
	if p, _ := f.istore.PendingWipes(ctx, f.envID); len(p) != 1 {
		t.Errorf("pending wipes after a failed removal = %+v, want one", p)
	}
	if !hasVolume(t, f.worker) {
		t.Error("the worker's volume is gone though its removal failed")
	}

	// The removal works now: the rerun wipes it, clears the entry and records the wipe.
	f.eng.failRemove[workerNode.AgentID] = false
	job = f.disable(seesAll)
	if it := itemOf(job, ingressdeploy.ItemVolumes); it.Result["unreachable"] != "" || it.Result["wiped"] != "2" {
		t.Errorf("volumes result = %v, want both wiped", it.Result)
	}
	if p, _ := f.istore.PendingWipes(ctx, f.envID); len(p) != 0 {
		t.Errorf("pending wipes = %+v, want none after the wipe", p)
	}
	if hasVolume(t, f.worker) {
		t.Error("the worker's volume is still there")
	}
	if got := f.auditCount("ingress.wipe_done"); got != 1 {
		t.Errorf("ingress.wipe_done audit rows = %d, want 1", got)
	}
}
