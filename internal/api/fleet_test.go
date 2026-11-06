// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The fleet views (plan 12-26, ENV-10, D-17): what is failing across the environments a person
// may read, and the four tiles of the Overview. The environments are real (so the grants are),
// and what each one answers with is a fake, keyed by environment ID, that also remembers which
// environments it was asked about.

type fleetSnap struct {
	agg    envs.Aggregate
	health envs.Health
	err    error
}

type fleetTally struct {
	t   updates.Tally
	err error
}

type fleetSpace struct {
	v   envs.AddressSpace
	err error
}

type fleetFake struct {
	mu       sync.Mutex
	read     map[string]int
	snap     map[string]fleetSnap
	swarm    map[string]bool
	state    map[string]envs.ClusterState
	policies map[string]*envs.PolicyCache
	tally    map[string]fleetTally
	space    map[string]fleetSpace
	verdicts map[string]fleetVerdicts
	work     map[string]fleetWork
	// checked is the environments each CheckFleet was asked about, in order; checkRes is what it answers.
	checked  [][]string
	checkRes map[string]updates.CheckResult
	// wait, when set, is called by every Snapshot before it answers.
	wait         func(ctx context.Context) error
	active, peak int
}

func newFleetFake() *fleetFake {
	return &fleetFake{
		read: map[string]int{}, snap: map[string]fleetSnap{}, swarm: map[string]bool{}, state: map[string]envs.ClusterState{},
		policies: map[string]*envs.PolicyCache{}, tally: map[string]fleetTally{}, space: map[string]fleetSpace{}, verdicts: map[string]fleetVerdicts{}, work: map[string]fleetWork{}, checkRes: map[string]updates.CheckResult{},
	}
}

func (f *fleetFake) touch(id string) {
	f.mu.Lock()
	f.read[id]++
	f.mu.Unlock()
}

// asked is the IDs of the environments anything was read of, sorted.
func (f *fleetFake) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.read {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (f *fleetFake) Snapshot(ctx context.Context, id string) (envs.Aggregate, envs.Health, error) {
	f.touch(id)
	f.mu.Lock()
	f.active++
	f.peak = max(f.peak, f.active)
	wait := f.wait
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if wait != nil {
		if err := wait(ctx); err != nil {
			return envs.Aggregate{}, envs.Health{}, err
		}
	}
	s, ok := f.snap[id]
	if !ok {
		return envs.Aggregate{}, envs.Health{}, errors.New("nothing set for " + id)
	}
	return s.agg, s.health, s.err
}

func (f *fleetFake) IsSwarm(_ context.Context, id string) (bool, error) {
	f.touch(id)
	return f.swarm[id], nil
}

func (f *fleetFake) State(_ context.Context, id string) (envs.ClusterState, error) {
	f.touch(id)
	return f.state[id], nil
}

func (f *fleetFake) Policies(id string) *envs.PolicyCache {
	f.touch(id)
	if c, ok := f.policies[id]; ok {
		return c
	}
	return envs.NewPolicyCache()
}

func (f *fleetFake) Tally(_ context.Context, id string) (updates.Tally, error) {
	f.touch(id)
	t := f.tally[id]
	return t.t, t.err
}

func (f *fleetFake) EnvVerdicts(_ context.Context, id string) ([]updates.WorkloadVerdict, updates.Coverage, error) {
	f.touch(id)
	v := f.verdicts[id]
	return v.list, v.cov, v.err
}

func (f *fleetFake) Workloads(_ context.Context, id string) ([]updates.Workload, error) {
	f.touch(id)
	w := f.work[id]
	return w.list, w.err
}

func (f *fleetFake) CheckFleet(_ context.Context, workloads map[string][]updates.Workload) (map[string]updates.CheckResult, error) {
	ids := make([]string, 0, len(workloads))
	for id := range workloads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	f.mu.Lock()
	f.checked = append(f.checked, ids)
	f.mu.Unlock()
	out := map[string]updates.CheckResult{}
	for _, id := range ids {
		out[id] = f.checkRes[id]
	}
	return out, nil
}

func (f *fleetFake) Read(_ context.Context, id string) (envs.AddressSpace, error) {
	f.touch(id)
	s := f.space[id]
	return s.v, s.err
}

var fleetNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// online makes id a reporting environment.
func (f *fleetFake) online(id string) {
	f.snap[id] = fleetSnap{
		agg:    envs.Aggregate{Containers: []envs.ContainerRow{}, NotReporting: []string{}, FetchedAt: fleetNow},
		health: envs.Health{State: envs.StateOnline},
	}
}

// down makes id an environment that cannot be read, in the given state.
func (f *fleetFake) down(id string, state envs.State) {
	h := envs.Health{State: state}
	f.snap[id] = fleetSnap{err: envs.ErrNotOnline{Health: h}, health: h}
}

// swarmWith makes id a reporting swarm with these services and failed tasks.
func (f *fleetFake) swarmWith(id string, services []dockerx.ServiceRow, failed ...dockerx.FailedTaskRow) {
	f.online(id)
	f.swarm[id] = true
	f.state[id] = envs.ClusterState{
		Read:      dockerx.ClusterRead{Services: services, FailedTasks: failed},
		FetchedAt: fleetNow, AttemptedAt: fleetNow,
	}
}

func degraded(id, name string, running, desired uint64) dockerx.ServiceRow {
	return dockerx.ServiceRow{ID: id, Name: name, Mode: "replicated", Running: running, Desired: desired, Stack: "shop"}
}

type fleetFailingResp struct {
	Items []struct {
		Kind      string  `json:"kind"`
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		EnvID     string  `json:"envId"`
		EnvName   string  `json:"envName"`
		Stack     string  `json:"stack"`
		Node      string  `json:"node"`
		State     string  `json:"state"`
		Running   *uint64 `json:"running"`
		Desired   *uint64 `json:"desired"`
		Since     string  `json:"since"`
		LastError string  `json:"lastError"`
	} `json:"items"`
	NotIncluded []fleetNotIncluded `json:"notIncluded"`
	ReadAt      string             `json:"readAt"`
}

type fleetNotIncluded struct {
	EnvID   string `json:"envId"`
	EnvName string `json:"envName"`
	Reason  string `json:"reason"`
	Nodes   int    `json:"nodes"`
}

type fleetFix struct {
	*agentFix
	fake *fleetFake
	envs map[string]string
}

// newFleetFix creates one environment per name and registers the fleet routes over a fake.
func newFleetFix(t *testing.T, names ...string) *fleetFix {
	t.Helper()
	f := newAgentFix(t)
	fix := &fleetFix{agentFix: f, fake: newFleetFake(), envs: map[string]string{}}
	for _, n := range names {
		fix.envs[n] = f.srv.NewEnvironment(t, n).ID
	}
	f.fleet = &api.FleetDeps{
		Aggregator: fix.fake, Clusters: fix.fake, Updates: fix.fake, AddressSpaces: fix.fake,
		Now: func() time.Time { return fleetNow }, EnvTimeout: testFleetTimeout,
	}
	return fix
}

// testFleetTimeout is long enough for a loaded machine, short enough for the timeout test to be quick.
const testFleetTimeout = 20 * time.Second

// rebuild registers the fleet routes. Call it after the fake is set up.
func (f *fleetFix) rebuild(t *testing.T) { f.h = f.build(t) }

func (f *fleetFix) id(name string) string { return f.envs[name] }

func (f *fleetFix) failing(t *testing.T, as, query string) fleetFailingResp {
	t.Helper()
	rec := f.fleetGet(t, as, "/api/fleet/failing"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET /api/fleet/failing%s as %q = %d %s", query, as, rec.code, rec.body)
	}
	var r fleetFailingResp
	if err := json.Unmarshal(rec.body, &r); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return r
}

func (f *fleetFix) fleetGet(t *testing.T, as, path string) *envRec {
	t.Helper()
	if as == "" {
		return f.req(t, http.MethodGet, path, "")
	}
	return f.asUser(t, as, http.MethodGet, path, "")
}

func TestFleetFailingNamesOffline(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	f.fake.swarmWith(alpha, []dockerx.ServiceRow{degraded("s1", "web", 1, 2), degraded("s2", "db", 1, 1)},
		dockerx.FailedTaskRow{ID: "t1", ServiceID: "s1", At: fleetNow.Add(-time.Minute), Err: "task: non-zero exit (1)"})
	f.fake.down(beta, envs.StateOffline)
	f.rebuild(t)

	got := f.failing(t, "", "")
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want the one degraded service", got.Items)
	}
	it := got.Items[0]
	if it.Kind != "service" || it.Name != "web" || it.EnvID != alpha || it.EnvName != "Alpha" || it.Stack != "shop" ||
		it.State != "1 of 2 running" || it.Running == nil || *it.Running != 1 || it.Desired == nil || *it.Desired != 2 ||
		it.Since != fleetNow.Add(-time.Minute).Format(time.RFC3339) || it.LastError != "task: non-zero exit (1)" {
		t.Errorf("item = %+v", it)
	}
	if want := []fleetNotIncluded{{EnvID: beta, EnvName: "Beta", Reason: "offline"}}; !slices.Equal(got.NotIncluded, want) {
		t.Errorf("notIncluded = %+v, want %+v", got.NotIncluded, want)
	}
}

func TestFleetFailingListsAStandaloneHostsContainers(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	alpha := f.id("Alpha")
	f.fake.online(alpha)
	f.fake.snap[alpha] = fleetSnap{health: envs.Health{State: envs.StateOnline}, agg: envs.Aggregate{
		NotReporting: []string{}, FetchedAt: fleetNow,
		Containers: []envs.ContainerRow{
			{ContainerRow: dockerx.ContainerRow{ID: "c1", Name: "api", State: "exited", Status: "Exited (137) 3 minutes ago", Labels: map[string]string{"com.docker.compose.project": "shop"}}, Node: "host-1", NodeID: "n1"},
			{ContainerRow: dockerx.ContainerRow{ID: "c2", Name: "ok", State: "running", Status: "Up 2 hours"}, Node: "host-1", NodeID: "n1"},
		},
	}}
	cache := envs.NewPolicyCache()
	_, _ = envs.FailingContainers(context.Background(), []dockerx.ContainerRow{{ID: "c1", State: "exited", Status: "Exited (137) 3 minutes ago"}, {ID: "c2", State: "running"}}, cache,
		func(_ context.Context, ids []string) (map[string]dockerx.RestartPolicy, error) {
			out := map[string]dockerx.RestartPolicy{}
			for _, id := range ids {
				out[id] = dockerx.RestartPolicy{Name: "always"}
			}
			return out, nil
		})
	f.fake.policies[alpha] = cache
	f.fake.state[alpha] = envs.ClusterState{FetchedAt: fleetNow, AttemptedAt: fleetNow}
	f.rebuild(t)

	got := f.failing(t, "", "")
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want the exited container", got.Items)
	}
	it := got.Items[0]
	if it.Kind != "container" || it.Name != "api" || it.Stack != "shop" || it.Node != "host-1" || it.State != "exited" ||
		it.Running != nil || it.Desired != nil || it.LastError != "Exited with code 137" ||
		it.Since != fleetNow.Add(-3*time.Minute).Format(time.RFC3339) {
		t.Errorf("item = %+v", it)
	}
}

func TestFleetFailingNamesAnEnvironmentWhoseNodesDidNotAllReport(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	alpha := f.id("Alpha")
	f.fake.swarmWith(alpha, []dockerx.ServiceRow{degraded("s1", "web", 0, 1)})
	s := f.fake.snap[alpha]
	s.agg.NotReporting = []string{"node-2", "node-3"}
	f.fake.snap[alpha] = s
	f.rebuild(t)

	got := f.failing(t, "", "")
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want the rows of the nodes that did report", got.Items)
	}
	want := []fleetNotIncluded{{EnvID: alpha, EnvName: "Alpha", Reason: "nodes_not_reporting", Nodes: 2}}
	if !slices.Equal(got.NotIncluded, want) {
		t.Errorf("notIncluded = %+v, want %+v", got.NotIncluded, want)
	}
}

func TestFleetFailingNamesThePendingAndTheSilentEnvironments(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma", "Delta")
	f.fake.down(f.id("Alpha"), envs.StatePending)
	f.fake.down(f.id("Beta"), envs.StateReconnecting)
	f.fake.down(f.id("Gamma"), envs.StateRevoked)
	f.fake.online(f.id("Delta"))
	f.fake.swarm[f.id("Delta")] = true
	f.fake.state[f.id("Delta")] = envs.ClusterState{NoQuorum: true, FetchedAt: fleetNow}
	f.rebuild(t)

	got := f.failing(t, "", "")
	want := []fleetNotIncluded{
		{EnvID: f.id("Alpha"), EnvName: "Alpha", Reason: "waiting_first_agent"},
		{EnvID: f.id("Beta"), EnvName: "Beta", Reason: "not_reporting"},
		{EnvID: f.id("Delta"), EnvName: "Delta", Reason: "not_reporting"},
		{EnvID: f.id("Gamma"), EnvName: "Gamma", Reason: "not_reporting"},
	}
	if !slices.Equal(got.NotIncluded, want) {
		t.Errorf("notIncluded = %+v, want %+v", got.NotIncluded, want)
	}
}

func TestFleetEnvFilterIsCapped(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	f.fake.swarmWith(f.id("Alpha"), nil)
	f.rebuild(t)
	query := "?env=x"
	for range 100 {
		query += "&env=x"
	}
	if rec := f.fleetGet(t, "", "/api/fleet/failing"+query); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("101 environment IDs = %d %s, want 422", rec.code, rec.body)
	}
}

func TestFleetReadsAtMostEightEnvironmentsAtOnce(t *testing.T) {
	t.Parallel()
	names := make([]string, 12)
	for i := range names {
		names[i] = fmt.Sprintf("Env%02d", i)
	}
	f := newFleetFix(t, names...)
	for _, n := range names {
		f.fake.swarmWith(f.id(n), nil)
	}
	// Every read holds until eight are in at once, so a limit below eight fails the test as
	// surely as a limit above it, and the machine's speed decides nothing.
	var arrived atomic.Int32
	full := make(chan struct{})
	f.fake.wait = func(ctx context.Context) error {
		if arrived.Add(1) == 8 {
			close(full)
		}
		select {
		case <-full:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.rebuild(t)

	if got := f.failing(t, "", ""); len(got.NotIncluded) != 0 {
		t.Fatalf("notIncluded = %+v", got.NotIncluded)
	}
	if peak := f.fake.peak; peak != 8 {
		t.Errorf("environments read at once = %d, want 8", peak)
	}
	if n := len(f.fake.asked()); n != 12 {
		t.Errorf("environments read = %d, want all 12", n)
	}
}

func TestFleetAnEnvironmentThatTakesTooLongIsNamedNotReporting(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	f.fake.swarmWith(f.id("Alpha"), []dockerx.ServiceRow{degraded("s1", "web", 0, 1)})
	f.fake.swarmWith(f.id("Beta"), []dockerx.ServiceRow{degraded("s2", "slow", 0, 1)})
	// Long enough for Alpha to answer on a loaded runner: the test waits this out once.
	f.fleet.EnvTimeout = 2 * time.Second
	// Beta never answers its snapshot; Alpha does.
	blocking := &blockOn{id: f.id("Beta"), fleetFake: f.fake, hang: testsupport.Context(t)}
	f.fleet.Aggregator = blocking
	f.rebuild(t)

	got := f.failing(t, "", "")
	if len(got.Items) != 1 || got.Items[0].Name != "web" {
		t.Errorf("items = %+v, want Alpha's row", got.Items)
	}
	want := []fleetNotIncluded{{EnvID: f.id("Beta"), EnvName: "Beta", Reason: "not_reporting"}}
	if !slices.Equal(got.NotIncluded, want) {
		t.Errorf("notIncluded = %+v, want %+v", got.NotIncluded, want)
	}
}

// blockOn answers every environment from the fake but never answers id: it waits for its
// context. hang ends a test whose environment limit never fires.
type blockOn struct {
	id   string
	hang context.Context
	*fleetFake
}

func (b *blockOn) Snapshot(ctx context.Context, id string) (envs.Aggregate, envs.Health, error) {
	if id == b.id {
		select {
		case <-ctx.Done():
		case <-b.hang.Done():
			return envs.Aggregate{}, envs.Health{}, errors.New("the environment limit never fired")
		}
		return envs.Aggregate{}, envs.Health{}, ctx.Err()
	}
	return b.fleetFake.Snapshot(ctx, id)
}
