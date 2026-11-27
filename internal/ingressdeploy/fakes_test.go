// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// testServerVersion is the version the deployer under test says it is; the proxy image follows it.
const testServerVersion = "1.2.3"

// testClusterID is the swarm cluster ID of the fake swarm.
const testClusterID = "cluster-1"

var (
	testActor = audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}
	testOwner = "user:u-admin"
)

// fixedClock is a clock that stands still: the deployer only stamps a holder with it.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time                       { return c.at }
func (c fixedClock) After(time.Duration) <-chan time.Time { return nil }

// gate holds the first manager write of an engine until the test lets it go.
type gate struct {
	once    sync.Once
	entered chan struct{}
	proceed chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan struct{}), proceed: make(chan struct{})} }

// fakeEngine runs every call against the one fake swarm manager.
type fakeEngine struct {
	cli  *dockerx.Client
	gate *gate
	// failReadAfterWrite makes the first ClusterRead after each write fail, as a manager that stops answering between a
	// pass's write and its read does.
	failReadAfterWrite, failNextRead atomic.Bool
}

func (e *fakeEngine) WriteOnManager(ctx context.Context, _ string, fn func(*dockerx.Client) error) error {
	if e.failReadAfterWrite.Load() {
		defer e.failNextRead.Store(true)
	}
	if g := e.gate; g != nil {
		g.once.Do(func() {
			close(g.entered)
			select {
			case <-g.proceed:
			case <-ctx.Done():
			}
		})
	}
	return fn(e.cli)
}

func (e *fakeEngine) WriteOnNode(_ context.Context, _, _ string, fn func(*dockerx.Client) error) error {
	return fn(e.cli)
}

func (e *fakeEngine) ClusterRead(_ context.Context, _ string, fn func(*dockerx.Client) error) error {
	if e.failNextRead.CompareAndSwap(true, false) {
		return errors.New("the manager did not answer")
	}
	return fn(e.cli)
}

func (e *fakeEngine) OnNode(_ context.Context, _, _ string, fn func(*dockerx.Client) error) error {
	return fn(e.cli)
}

// fakeEnvs reports the environment a test sets.
type fakeEnvs struct {
	mu   sync.Mutex
	info ingressdeploy.EnvInfo
}

func (f *fakeEnvs) Info(_ context.Context, envID string) (ingressdeploy.EnvInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.info
	i.ID = envID
	return i, nil
}

func (f *fakeEnvs) set(fn func(*ingressdeploy.EnvInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.info)
}

// fakeStacks lists the stacks a test sets.
type fakeStacks struct{ refs []ingressdeploy.StackRef }

func (f fakeStacks) WithRoutes(context.Context, string) ([]ingressdeploy.StackRef, error) {
	return f.refs, nil
}

// fakeAddressing hands out blocks of 10.0.0.0/8 and counts the networks of the fake swarm as occupied.
type fakeAddressing struct {
	cli *dockerx.Client
	mu  sync.Mutex
}

func (a *fakeAddressing) LockAllocation(string) func() {
	a.mu.Lock()
	return a.mu.Unlock
}

func (a *fakeAddressing) AllocationInputs(ctx context.Context, _ string, _ bool) (envs.AllocationInputs, error) {
	rows, err := a.cli.Networks(ctx)
	if err != nil {
		return envs.AllocationInputs{}, err
	}
	in := envs.AllocationInputs{Pools: []netspace.Pool{{Base: netip.MustParsePrefix("10.0.0.0/8"), Size: 24}}}
	for _, r := range rows {
		for _, s := range r.Subnets {
			if p, err := netip.ParsePrefix(s); err == nil {
				in.Occupied = append(in.Occupied, p)
			}
		}
	}
	return in, nil
}

// fakeRegistry chooses one header for every image.
type fakeRegistry struct{ header string }

func (f fakeRegistry) AuthFor(context.Context, string, registry.Ref) (string, *registry.Candidate, error) {
	return f.header, nil, nil
}

// rig is one fake swarm, one database and the deployer over them.
type rig struct {
	t      *testing.T
	fake   *dockertest.Fake
	cli    *dockerx.Client
	eng    *fakeEngine
	envs   *fakeEnvs
	stacks *fakeStacks
	st     *store.Store
	tokens *ingresshub.Service
	istore *ingressstore.Store
	hub    *jobs.Hub
	locks  *stackops.Locks
	svc    *ingressdeploy.Service
	envID  string
}

// rigOptions tune a rig.
type rigOptions struct {
	// config adjusts the fake swarm.
	config func(*dockertest.FakeConfig)
	// deps adjusts the deployer's dependencies.
	deps func(*ingressdeploy.Deps)
	// standalone makes the environment a standalone host.
	standalone bool
}

// newRig builds a swarm of one manager whose ingress network holds 10.0.0.0/24, so the first free /22 is
// 10.0.4.0/22, and a deployer over it.
func newRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: testClusterID,
		Networks: []dockertest.FakeNetwork{
			{ID: "ingress-net", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.0.0.0/24"}},
		},
	}
	if o.config != nil {
		o.config(&cfg)
	}
	fake := dockertest.NewFake(t, cfg)
	cli, err := dockerx.New(fake)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	log := slog.New(slog.DiscardHandler)
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	now := func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }
	aud := audit.NewWriter(st, v, log, now)
	envType := "swarm"
	if o.standalone {
		envType = "standalone"
	}
	envID := uuid.NewString()
	if _, err := st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		VALUES (?, 'agent', ?, '', 1, 1, 1, ?)`, envID, "prod", envType); err != nil {
		t.Fatalf("insert environment: %v", err)
	}
	hub := jobs.New(jobs.Options{})
	t.Cleanup(hub.Close)
	r := &rig{
		t: t, fake: fake, cli: cli, eng: &fakeEngine{cli: cli}, stacks: &fakeStacks{}, st: st,
		tokens: ingresshub.NewService(st, aud, now), istore: ingressstore.New(ingressstore.Deps{St: st, Vault: v, Now: now}),
		hub: hub, locks: stackops.NewLocks(), envID: envID,
	}
	r.envs = &fakeEnvs{info: ingressdeploy.EnvInfo{
		Name: "prod", Swarm: !o.standalone, ClusterID: testClusterID, ManagerConnected: true,
		Nodes: []ingressdeploy.NodeInfo{{SwarmNodeID: "node-1", AgentID: "agent-1", Name: "manager-1", Role: "manager", Connected: true}},
	}}
	deps := ingressdeploy.Deps{
		Engine: r.eng, Envs: r.envs, Stacks: r.stacks, Store: r.istore, Tokens: r.tokens, Jobs: hub, Locks: r.locks, Audit: aud,
		AddressSpaces: &fakeAddressing{cli: cli},
		AllocationWait: dockerx.AllocationWait{After: func(time.Duration) <-chan time.Time {
			c := make(chan time.Time, 1)
			c <- time.Time{}
			return c
		}},
		ServerVersion: testServerVersion,
		URL:           func() string { return "wss://tierplan.example:7480/ingress/connect" },
		Pins:          func() []string { return []string{"sha256/AAAA", "sha256/BBBB"} },
		Clock:         fixedClock{at: now()}, Log: log,
	}
	if o.deps != nil {
		o.deps(&deps)
	}
	r.svc = ingressdeploy.New(deps)
	return r
}

// enable starts an enable and waits for its job to end. The job is returned whatever its outcome.
func (r *rig) enable(req ingressdeploy.EnableRequest) jobs.Job {
	r.t.Helper()
	id, err := r.svc.Enable(testsupport.Context(r.t), testActor, testOwner, r.envID, req)
	if err != nil {
		r.t.Fatalf("Enable: %v", err)
	}
	return r.wait(id)
}

// wait waits for a job to end by the hub's own signal.
func (r *rig) wait(id string) jobs.Job {
	r.t.Helper()
	job, err := r.hub.Wait(testsupport.Context(r.t), id)
	if err != nil {
		r.t.Fatalf("waiting for job %s: %v", id, err)
	}
	return job
}

// secrets, network and service read what the fake swarm holds of the ingress.
func (r *rig) secrets() []dockerx.SecretRow {
	r.t.Helper()
	rows, err := r.cli.SecretList(testsupport.Context(r.t))
	if err != nil {
		r.t.Fatalf("SecretList: %v", err)
	}
	return rows
}

func (r *rig) network() (dockerx.NetworkRow, bool) {
	r.t.Helper()
	rows, err := r.cli.Networks(testsupport.Context(r.t))
	if err != nil {
		r.t.Fatalf("Networks: %v", err)
	}
	for _, n := range rows {
		if n.Name == ingress.NetworkName {
			return n, true
		}
	}
	return dockerx.NetworkRow{}, false
}

func (r *rig) service() (dockerx.ServiceDetail, bool) {
	r.t.Helper()
	d, _, err := r.cli.ServiceInspect(testsupport.Context(r.t), ingress.ServiceName)
	if err != nil {
		return dockerx.ServiceDetail{}, false
	}
	return d, true
}

// itemState is the state and code of one item of a job.
func itemOf(job jobs.Job, id string) jobs.Item {
	for _, it := range job.Items {
		if it.ID == id {
			return it
		}
	}
	return jobs.Item{ID: id, State: "missing"}
}

// auditCount is the number of audit rows of an action.
func (r *rig) auditCount(action string) int {
	r.t.Helper()
	var n int
	if err := r.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		r.t.Fatalf("count audit rows: %v", err)
	}
	return n
}
