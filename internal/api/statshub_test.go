// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The shared, ref-counted stats sampler (plan 06-32, RES-11, D-29 to D-31, RESEARCH
// Pattern 7). No test here waits for a clock: the sampler's ticker is the test's, and
// every wait is for a signal.

// manualTicks hands every sampler a ticker the test fires.
type manualTicks struct {
	mu      sync.Mutex
	chans   []chan time.Time
	stopped chan struct{}
	made    atomic.Int32
}

func newManualTicks() *manualTicks { return &manualTicks{stopped: make(chan struct{}, 64)} }

func (m *manualTicks) newTicker(time.Duration) (<-chan time.Time, func()) {
	// One slot, like time.Ticker: a tick that finds the sampler busy waits, and a second
	// one is dropped.
	ch := make(chan time.Time, 1)
	m.mu.Lock()
	m.chans = append(m.chans, ch)
	m.mu.Unlock()
	m.made.Add(1)
	var once sync.Once
	return ch, func() { once.Do(func() { m.stopped <- struct{}{} }) }
}

// fire ticks every sampler once.
func (m *manualTicks) fire() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.chans {
		select {
		case ch <- time.Now():
		default:
		}
	}
}

func (m *manualTicks) waitStopped(t *testing.T, why string) {
	t.Helper()
	testsupport.Await(t, "a sampler to stop: "+why, m.stopped)
}

// statsSrc runs a node call against one fake engine, and can hold calls, refuse them as a
// node with no agent does, and count them.
type statsSrc struct {
	cli *dockerx.Client
	// gate, when set, holds every call until the test sends on it.
	gate chan struct{}
	// entered, when set, gets one value as each call starts.
	entered chan struct{}

	down                      atomic.Bool
	calls, inflight, maxInfly atomic.Int32
	mu                        sync.Mutex
	maxPerNode, nowPerNode    map[string]int

	// changed is closed and replaced each time a call is counted or finishes, so a test
	// waits on the source's own signal instead of polling its counters.
	changeMu sync.Mutex
	changed  chan struct{}
}

// notify wakes every await.
func (s *statsSrc) notify() {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// await returns once cond is true, evaluated now and after each call counted or finished,
// and fails the test, naming what, if the test's context ends first.
func (s *statsSrc) await(t testing.TB, what string, cond func() bool) {
	t.Helper()
	ctx := testsupport.Context(t)
	for {
		// Taken before cond is evaluated, so a change in between is not missed.
		s.changeMu.Lock()
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch := s.changed
		s.changeMu.Unlock()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v; %d calls counted, %d in flight", what, ctx.Err(), s.calls.Load(), s.inflight.Load())
		}
	}
}

func (s *statsSrc) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	s.calls.Add(1)
	cur := s.inflight.Add(1)
	defer func() {
		s.inflight.Add(-1)
		s.notify()
	}()
	s.notify()
	for {
		m := s.maxInfly.Load()
		if cur <= m || s.maxInfly.CompareAndSwap(m, cur) {
			break
		}
	}
	s.mu.Lock()
	if s.nowPerNode == nil {
		s.nowPerNode, s.maxPerNode = map[string]int{}, map[string]int{}
	}
	s.nowPerNode[nodeID]++
	s.maxPerNode[nodeID] = max(s.maxPerNode[nodeID], s.nowPerNode[nodeID])
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.nowPerNode[nodeID]--
		s.mu.Unlock()
	}()
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.down.Load() {
		return fmt.Errorf("node %s of %s: %w", nodeID, envID, envs.ErrNodeUnavailable)
	}
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fn(s.cli)
}

func (s *statsSrc) maxOn(node string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxPerNode[node]
}

func newStatsSrc(t *testing.T, cfg dockertest.FakeConfig) (*statsSrc, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, cfg)
	cli, err := dockerx.New(fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &statsSrc{cli: cli}, fake
}

func hubWith(t *testing.T, src *statsSrc, ticks *manualTicks, perNode int) *api.StatsHub {
	t.Helper()
	h := api.NewStatsHub(src, api.StatsHubOptions{Interval: 2 * time.Second, PerNode: perNode, NewTicker: ticks.newTicker, Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(h.Close)
	return h
}

// one is a running container with the stats of a busy one: 100 percent of a core over a
// two second tick, 1000 B/s each way on the network and 500 B/s of disk reads.
var oneBusy = dockertest.FakeStats{
	OnlineCPUs: 4, CPUPerCall: 500_000_000, SystemPerCall: 2_000_000_000,
	MemUsage: 300 << 20, InactiveFile: 100 << 20, MemLimit: 64 << 30,
	NetRxPerCall: 2000, NetTxPerCall: 2000, BlkReadPerCall: 1000,
}

func oneContainer() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{{ID: "web-id", Names: []string{"/web"}, Image: "nginx:1.27", State: "running"}},
		Stats:      map[string]dockertest.FakeStats{"web-id": oneBusy},
	}
}

func recvUpdate(t *testing.T, ch <-chan api.StatsUpdate) api.StatsUpdate {
	t.Helper()
	select {
	case u, ok := <-ch:
		if !ok {
			t.Fatal("the subscription was closed")
		}
		return u
	case <-testsupport.Context(t).Done():
		t.Fatal("no update arrived")
	}
	return api.StatsUpdate{}
}

func waitEntered(t *testing.T, ch <-chan struct{}, n int) {
	t.Helper()
	for range n {
		testsupport.Await(t, "a sample request to start", ch)
	}
}

func TestStatsHubTakesTheFirstSampleAtOnceAndThenOneOnEveryTick(t *testing.T) {
	t.Parallel()
	src, _ := newStatsSrc(t, oneContainer())
	ticks := newManualTicks()
	h := hubWith(t, src, ticks, 0)
	ch, stop := h.Subscribe("env1", "n1", "web-id")
	defer stop()

	first := recvUpdate(t, ch)
	if first.Kind != api.StatsKindSample || first.Point.CPUPercent != nil || first.Point.HasRates {
		t.Fatalf("the first update = %+v, want a sample with no CPU and no rates, a reading with nothing to compare", first)
	}
	if first.Point.MemBytes != 200<<20 {
		t.Errorf("memory = %d, want the usage less the file cache, %d", first.Point.MemBytes, 200<<20)
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("%d requests before any tick, want the one first sample", n)
	}

	ticks.fire()
	second := recvUpdate(t, ch)
	if second.Kind != api.StatsKindSample || second.Point.CPUPercent == nil || math.Abs(*second.Point.CPUPercent-100) > 1e-6 {
		t.Fatalf("the second update = %+v, want 100 percent of a core", second)
	}
	if !second.Point.HasRates || math.Abs(second.Point.NetRxRate-1000) > 1e-6 || math.Abs(second.Point.NetTxRate-1000) > 1e-6 || math.Abs(second.Point.BlkReadRate-500) > 1e-6 {
		t.Errorf("rates = %+v, want 1000 B/s each way and 500 B/s of reads", second.Point)
	}
	if n := src.calls.Load(); n != 2 {
		t.Errorf("%d requests after one tick, want 2", n)
	}
}

func TestStatsHubTwoViewersOfOneContainerShareOneSamplerAndItStopsWithTheLast(t *testing.T) {
	t.Parallel()
	src, _ := newStatsSrc(t, oneContainer())
	ticks := newManualTicks()
	h := hubWith(t, src, ticks, 0)
	ch1, stop1 := h.Subscribe("env1", "n1", "web-id")
	recvUpdate(t, ch1)
	ch2, stop2 := h.Subscribe("env1", "n1", "web-id")
	if n := h.Samplers(); n != 1 {
		t.Fatalf("%d samplers for two viewers of one container, want 1", n)
	}
	// A viewer of another container is another sampler.
	if _, stop := h.Subscribe("env1", "n1", "other"); h.Samplers() != 2 {
		t.Errorf("%d samplers, want 2 with a second container", h.Samplers())
	} else {
		stop()
	}

	ticks.fire()
	a := recvUpdate(t, ch1)
	var b api.StatsUpdate
	for {
		// The second viewer may have joined before or after the first sample.
		b = recvUpdate(t, ch2)
		if b.Point.CPUPercent != nil {
			break
		}
	}
	if !a.Point.At.Equal(b.Point.At) {
		t.Errorf("the viewers got different samples: %v and %v", a.Point.At, b.Point.At)
	}

	stop1()
	if n := h.Samplers(); n != 1 {
		t.Errorf("%d samplers after one viewer left, want 1", n)
	}
	if _, ok := <-ch1; ok {
		t.Error("a viewer's channel stayed open after it left")
	}
	stop2()
	stop2() // leaving twice is harmless
	if n := h.Samplers(); n != 0 {
		t.Errorf("%d samplers after the last viewer left, want 0", n)
	}
	// Each sampler's ticker is stopped as its goroutine returns: both are gone.
	ticks.waitStopped(t, "the last viewer left")
	ticks.waitStopped(t, "the other container's viewer left")
}

func TestStatsHubNeverHasTwoRequestsForOneContainerInFlight(t *testing.T) {
	t.Parallel()
	src, _ := newStatsSrc(t, oneContainer())
	src.gate = make(chan struct{})
	src.entered = make(chan struct{}, 64)
	ticks := newManualTicks()
	h := hubWith(t, src, ticks, 0)
	ch, stop := h.Subscribe("env1", "n1", "web-id")
	defer stop()

	waitEntered(t, src.entered, 1)
	for range 5 {
		ticks.fire() // the first sample is still pending
	}
	src.gate <- struct{}{}
	recvUpdate(t, ch)
	// The tick that waited is served after the first request is done, not beside it.
	waitEntered(t, src.entered, 1)
	src.gate <- struct{}{}
	recvUpdate(t, ch)
	if m := src.maxInfly.Load(); m != 1 {
		t.Errorf("up to %d requests for one container in flight at once, want 1", m)
	}
}

func watchedContainers(n int, prefix string) dockertest.FakeConfig {
	cfg := dockertest.FakeConfig{Stats: map[string]dockertest.FakeStats{}}
	for i := range n {
		id := prefix + strconv.Itoa(i)
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{ID: id, Names: []string{"/" + id}, Image: "nginx", State: "running"})
		cfg.Stats[id] = oneBusy
	}
	return cfg
}

func TestStatsHubHasAtMostEightRequestsInFlightOnOneNode(t *testing.T) {
	t.Parallel()
	src, _ := newStatsSrc(t, watchedContainers(20, "c"))
	src.gate = make(chan struct{})
	src.entered = make(chan struct{}, 64)
	h := hubWith(t, src, newManualTicks(), 0)
	for i := range 20 {
		_, stop := h.Subscribe("env1", "n1", "c"+strconv.Itoa(i))
		defer stop()
	}
	waitEntered(t, src.entered, 8)
	// Each sample that finishes lets exactly one waiting container in.
	for range 12 {
		src.gate <- struct{}{}
		waitEntered(t, src.entered, 1)
	}
	for range 8 {
		src.gate <- struct{}{}
	}
	src.await(t, "every container's first sample", func() bool { return src.calls.Load() == 20 && src.inflight.Load() == 0 })
	if m := src.maxOn("n1"); m != 8 {
		t.Errorf("up to %d requests in flight on one node, want exactly 8", m)
	}
}

func TestStatsHubCapsEachNodeOnItsOwn(t *testing.T) {
	t.Parallel()
	src, _ := newStatsSrc(t, watchedContainers(20, "c"))
	src.gate = make(chan struct{})
	src.entered = make(chan struct{}, 64)
	h := hubWith(t, src, newManualTicks(), 0)
	for i := range 20 {
		node := "n1"
		if i%2 == 1 {
			node = "n2"
		}
		_, stop := h.Subscribe("env1", node, "c"+strconv.Itoa(i))
		defer stop()
	}
	// Ten containers on each of two nodes: eight run on each at once, sixteen in all.
	waitEntered(t, src.entered, 16)
	for range 20 {
		src.gate <- struct{}{}
	}
	src.await(t, "every container's first sample", func() bool { return src.calls.Load() == 20 && src.inflight.Load() == 0 })
	if a, b := src.maxOn("n1"), src.maxOn("n2"); a != 8 || b != 8 {
		t.Errorf("most in flight on n1 = %d and on n2 = %d, want 8 on each", a, b)
	}
}

func TestStatsHubSaysSoOnceWhenANodeStopsReportingAndWhenItDoesAgain(t *testing.T) {
	t.Parallel()
	src, _ := newStatsSrc(t, oneContainer())
	src.down.Store(true)
	ticks := newManualTicks()
	h := hubWith(t, src, ticks, 0)
	ch, stop := h.Subscribe("env1", "n1", "web-id")
	defer stop()
	if u := recvUpdate(t, ch); u.Kind != api.StatsKindNotReporting {
		t.Fatalf("first update = %+v, want not_reporting", u)
	}
	// More failed ticks say nothing more.
	for i := range 3 {
		ticks.fire()
		want := int32(i + 2)
		src.await(t, "the failed request", func() bool { return src.calls.Load() >= want })
	}
	// A viewer who joins while the node is silent is told at once.
	ch2, stop2 := h.Subscribe("env1", "n1", "web-id")
	defer stop2()
	if u := recvUpdate(t, ch2); u.Kind != api.StatsKindNotReporting {
		t.Errorf("a late viewer's first update = %+v, want not_reporting", u)
	}

	src.down.Store(false)
	ticks.fire()
	if u := recvUpdate(t, ch); u.Kind != api.StatsKindSample {
		t.Fatalf("after the node came back, update = %+v, want a sample (and no second not_reporting)", u)
	}
}

func TestStatsHubReportsAContainerThatIsGoneOrNotRunningAsStopped(t *testing.T) {
	t.Parallel()
	cfg := dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{{ID: "off-id", Names: []string{"/off"}, Image: "nginx", State: "exited"}},
		Stats:      map[string]dockertest.FakeStats{"off-id": oneBusy},
	}
	src, _ := newStatsSrc(t, cfg)
	h := hubWith(t, src, newManualTicks(), 0)
	for _, id := range []string{"off-id", "removed-id"} {
		ch, stop := h.Subscribe("env1", "n1", id)
		if u := recvUpdate(t, ch); u.Kind != api.StatsKindStopped {
			t.Errorf("%s: update = %+v, want stopped", id, u)
		}
		stop()
	}
}

func TestStatsHubCloseEndsEverySubscriptionAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	src, _ := newStatsSrc(t, oneContainer())
	ticks := newManualTicks()
	h := api.NewStatsHub(src, api.StatsHubOptions{NewTicker: ticks.newTicker, Log: slog.New(slog.DiscardHandler)})
	ch, _ := h.Subscribe("env1", "n1", "web-id")
	recvUpdate(t, ch)
	h.Close()
	h.Close()
	if _, ok := <-ch; ok {
		t.Error("a subscription stayed open after Close")
	}
	if n := h.Samplers(); n != 0 {
		t.Errorf("%d samplers after Close", n)
	}
	late, stop := h.Subscribe("env1", "n1", "web-id")
	defer stop()
	if _, ok := <-late; ok {
		t.Error("a subscription after Close was served")
	}
	ticks.waitStopped(t, "Close")
}

// The hub keeps the last raw sample of each container it watches, in memory, and nothing
// else (D-30): this file and the stream's must not reach a database, an audit writer or
// the disk. The stream test beside it proves a sample reaches no audit row and no log.
func TestStatsFilesImportNothingThatCouldStoreASample(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"statshub.go", "stats.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			switch path {
			case "database/sql", "os", "io/ioutil", "bufio", "encoding/gob",
				"github.com/ianglennon/tierplan/internal/store", "github.com/ianglennon/tierplan/internal/audit":
				t.Errorf("%s imports %s, which could store a stats sample", file, path)
			}
		}
	}
}
