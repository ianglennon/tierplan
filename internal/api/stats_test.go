// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The container stats stream (plan 06-32, RES-11, D-29 to D-31). Docker is a FakeDaemon
// with scripted stats behind the real monitor, aggregator and router; the sampler's ticker
// is the test's, so nothing here waits for a clock.

type statsFix struct {
	*envFix
	env   envBody
	ticks *manualTicks
}

// downable wraps the aggregator so the sampler's node can be taken away.
type downable struct {
	api.StatsSource
	down atomic.Bool
}

func (d *downable) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	if d.down.Load() {
		return fmt.Errorf("node %s: %w", nodeID, envs.ErrNodeUnavailable)
	}
	return d.StatsSource.OnNode(ctx, envID, nodeID, fn)
}

func newStatsFix(t *testing.T, cfg dockertest.FakeConfig, tune func(*envFix, *downable)) *statsFix {
	t.Helper()
	f := newEnvFix(t, cfg)
	ticks := newManualTicks()
	src := &downable{StatsSource: f.agg}
	f.statsHub = api.NewStatsHub(src, api.StatsHubOptions{NewTicker: ticks.newTicker, Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(f.statsHub.Close)
	if tune != nil {
		tune(f, src)
	}
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return &statsFix{envFix: f, env: e, ticks: ticks}
}

func (f *statsFix) open(t *testing.T, cookie, container, query string) *sseConn {
	t.Helper()
	return openSSE(t, f.authFixture, "/api/env/"+f.env.ID+"/containers/"+container+"/stats/stream?"+query, cookie, "")
}

func statsEngine(inspect dockertest.FakeInspect, state string) dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{{
			ID: webID, Names: []string{"/web"}, Image: "nginx:1.27", State: state, Status: "Up 2 hours", Inspect: inspect,
		}},
		Stats: map[string]dockertest.FakeStats{webID: {
			OnlineCPUs: 4, CPUPerCall: 500_000_000, SystemPerCall: 2_000_000_000,
			MemUsage: 777_333_111, InactiveFile: 111_111_111, MemLimit: 64 << 30,
			NetRxPerCall: 2000, NetTxPerCall: 4000, BlkReadPerCall: 1000, BlkWritePerCall: 3000,
		}},
	}
}

type statsReady struct {
	LimitBytes *int64 `json:"limitBytes"`
	NanoCPUs   *int64 `json:"nanoCpus"`
	Running    bool   `json:"running"`
}

type statsSampleEvent struct {
	T       int64    `json:"t"`
	CPU     *float64 `json:"cpu"`
	Mem     uint64   `json:"mem"`
	NetRx   *float64 `json:"netRx"`
	NetTx   *float64 `json:"netTx"`
	BlkRead *float64 `json:"blkRead"`
	BlkWrit *float64 `json:"blkWrite"`
}

func readyOf(t *testing.T, c *sseConn) statsReady {
	t.Helper()
	fr := nextEvent(t, c, "ready")
	var r statsReady
	if err := json.Unmarshal([]byte(fr.data), &r); err != nil {
		t.Fatalf("ready %q: %v", fr.data, err)
	}
	return r
}

func sampleOf(t *testing.T, c *sseConn) statsSampleEvent {
	t.Helper()
	fr := nextEvent(t, c, "sample")
	var s statsSampleEvent
	if err := json.Unmarshal([]byte(fr.data), &s); err != nil {
		t.Fatalf("sample %q: %v", fr.data, err)
	}
	return s
}

func TestContainerStatsStreamSendsReadyWithTheContainersOwnLimitsThenSamplesWithRates(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000}, "running"), nil)
	c := f.open(t, f.cookie, "web", "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	if got := c.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	r := readyOf(t, c)
	if !r.Running || r.LimitBytes == nil || *r.LimitBytes != 512<<20 || r.NanoCPUs == nil || *r.NanoCPUs != 1_500_000_000 {
		t.Errorf("ready = %+v, want running with the container's own limits", r)
	}

	first := sampleOf(t, c)
	if first.CPU != nil || first.NetRx != nil || first.BlkRead != nil || first.Mem != 777_333_111-111_111_111 {
		t.Errorf("first sample = %+v, want memory only: usage less the file cache, no CPU and no rates yet", first)
	}
	f.ticks.fire()
	second := sampleOf(t, c)
	if second.CPU == nil || math.Abs(*second.CPU-100) > 1e-6 {
		t.Fatalf("cpu = %v, want 100 (percent of one core)", second.CPU)
	}
	if second.NetRx == nil || math.Abs(*second.NetRx-1000) > 1e-6 || math.Abs(*second.NetTx-2000) > 1e-6 ||
		math.Abs(*second.BlkRead-500) > 1e-6 || math.Abs(*second.BlkWrit-1500) > 1e-6 {
		t.Errorf("rates = %+v, want 1000 and 2000 B/s on the network, 500 B/s read and 1500 B/s written", second)
	}
	if second.T <= first.T {
		t.Errorf("t = %d then %d, want it to move on (milliseconds of the engine's read time)", first.T, second.T)
	}
}

func TestContainerStatsStreamOfAContainerWithNoLimitSendsNullNotTheHostsRAM(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), nil)
	r := readyOf(t, f.open(t, f.cookie, "web", ""))
	// The engine reports the host's 64 GiB as memory_stats.limit for this container.
	if r.LimitBytes != nil || r.NanoCPUs != nil {
		t.Errorf("ready = %+v, want no memory limit and no CPU limit (RESEARCH E21)", r)
	}
}

func TestContainerStatsStreamOfAContainerThatIsNotRunningIsReadyWithNoSamplesThenClosed(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "exited"), nil)
	c := f.open(t, f.cookie, "web", "")
	if r := readyOf(t, c); r.Running {
		t.Errorf("ready = %+v, want running false", r)
	}
	closed := nextEvent(t, c, "closed")
	if closed.data != `{"reason":"not_running"}` {
		t.Errorf("closed = %s", closed.data)
	}
	c.waitEnded(t, 5*time.Second, "it told the viewer the container is not running")
	if n := f.fake.StatsCalls(webID); n != 0 {
		t.Errorf("%d stats requests for a container that is not running, want none", n)
	}
	if n := f.statsHub.Samplers(); n != 0 {
		t.Errorf("%d samplers for a container that is not running", n)
	}
}

func TestContainerStatsStreamOfAnUnknownContainerIs404(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), nil)
	c := f.open(t, f.cookie, "nope", "")
	c.waitEnded(t, 5*time.Second, "the 404 was answered")
	if c.status0() != http.StatusNotFound || !strings.Contains(c.bodyString(), `"type":"container_not_found"`) {
		t.Errorf("status = %d body = %s, want 404 container_not_found", c.status0(), c.bodyString())
	}
}

func TestContainerStatsStreamTellsWhenTheNodeIsNotReportingAndWhenItIs(t *testing.T) {
	t.Parallel()
	var src *downable
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), func(_ *envFix, d *downable) { src = d })
	src.down.Store(true)
	c := f.open(t, f.cookie, "web", "")
	readyOf(t, c)
	if fr := nextEvent(t, c, "node"); fr.data != `{"state":"not_reporting"}` {
		t.Fatalf("node event = %s", fr.data)
	}
	src.down.Store(false)
	f.ticks.fire()
	if fr := nextEvent(t, c, "node"); fr.data != `{"state":"reporting"}` {
		t.Fatalf("node event = %s, want reporting before the first sample", fr.data)
	}
	sampleOf(t, c)
}

func TestContainerStatsStreamTellsWhenTheContainerStops(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), nil)
	c := f.open(t, f.cookie, "web", "")
	readyOf(t, c)
	sampleOf(t, c)
	cfg := statsEngine(dockertest.FakeInspect{}, "exited")
	f.fake.Reconfigure(cfg)
	f.ticks.fire()
	if fr := nextEvent(t, c, "state"); fr.data != `{"state":"stopped"}` {
		t.Errorf("state event = %s, want stopped", fr.data)
	}
}

func TestContainerStatsStreamOfTwoViewersCostsOneSamplerAndOneRequestPerTick(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), nil)
	a := f.open(t, f.cookie, "web", "")
	readyOf(t, a)
	sampleOf(t, a)
	b := f.open(t, f.cookie, webID, "")
	readyOf(t, b)
	if n := f.statsHub.Samplers(); n != 1 {
		t.Fatalf("%d samplers for two viewers of one container (by name and by ID), want 1", n)
	}
	f.ticks.fire()
	sa := sampleOf(t, a)
	var sb statsSampleEvent
	for sb.T != sa.T {
		sb = sampleOf(t, b)
	}
	if n := f.fake.StatsCalls(webID); n != 2 {
		t.Errorf("%d stats requests, want 2: the first sample and one tick, for both viewers", n)
	}
	// The sampler stops with the last viewer.
	a.close()
	if n := f.statsHub.Samplers(); n != 1 {
		t.Errorf("%d samplers after one viewer left, want 1", n)
	}
	b.close()
	awaitSamplersStopped(t, f.statsHub, "the sampler to stop")
	f.ticks.waitStopped(t, "the last viewer left")
}

func TestContainerStatsStreamRefusesAStreamOverThePerUserLimitWith429(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), func(f *envFix, _ *downable) { f.statsMaxStreams = 2 })
	first := f.open(t, f.cookie, "web", "")
	second := f.open(t, f.cookie, "web", "")
	for i, c := range []*sseConn{first, second} {
		if c.status0() != http.StatusOK {
			t.Fatalf("stream %d status = %d", i+1, c.status0())
		}
	}
	third := f.open(t, f.cookie, "web", "")
	third.waitEnded(t, 5*time.Second, "the 429 was answered")
	if third.status0() != http.StatusTooManyRequests || !strings.Contains(third.bodyString(), `"type":"rate_limited"`) {
		t.Errorf("third stream = %d %s, want 429 rate_limited", third.status0(), third.bodyString())
	}
	first.close()
	if c := f.open(t, f.cookie, "web", ""); c.status0() != http.StatusOK {
		t.Errorf("a stream after one closed = %d, want 200", c.status0())
	}
}

func TestContainerStatsStreamEndsCleanlyAtItsMaximumLifetime(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), func(f *envFix, _ *downable) { f.statsMaxLifetime = 50 * time.Millisecond })
	c := f.open(t, f.cookie, "web", "")
	readyOf(t, c)
	c.waitEnded(t, 5*time.Second, "its lifetime of 50 ms")
	awaitSamplersStopped(t, f.statsHub, "the sampler to stop with its only viewer")
}

func TestContainerStatsStreamEndsWithClosedAccessLostWithinAHeartbeatOfLosingTheGrant(t *testing.T) {
	t.Parallel()
	const heartbeat = 40 * time.Millisecond
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), func(f *envFix, _ *downable) { f.statsHeartbeat = heartbeat })
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", f.env.ID, "viewer")
	cookie, _ := f.signIn(t, "viewer", false)

	c := f.open(t, cookie, "web", "")
	readyOf(t, c)
	if _, err := f.st.Write.Exec(`DELETE FROM role_assignments WHERE principal_id = 'id-viewer'`); err != nil {
		t.Fatal(err)
	}
	closed := nextEvent(t, c, "closed")
	if closed.data != `{"reason":"access_lost"}` {
		t.Errorf("closed = %s", closed.data)
	}
	c.waitEnded(t, 5*time.Second, "it told the viewer the access was lost")
	awaitSamplersStopped(t, f.statsHub, "the sampler to stop")
}

// RES-11 prohibition (T-06-95): Bayplan does not persist stats samples on the server. A
// sample of a container reaches the browser and nothing else: no audit row, no log line.
func TestContainerStatsStreamStoresNothingAndLogsNothingOfASample(t *testing.T) {
	t.Parallel()
	f := newStatsFix(t, statsEngine(dockertest.FakeInspect{}, "running"), nil)
	count := func() int {
		var n int
		if err := f.st.Read.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	c := f.open(t, f.cookie, "web", "")
	readyOf(t, c)
	sampleOf(t, c)
	f.ticks.fire()
	sampleOf(t, c)
	c.close()
	awaitSamplersStopped(t, f.statsHub, "the sampler to stop")
	if after := count(); after != before {
		t.Errorf("%d audit rows after watching stats, was %d: a sample or a viewing was recorded", after, before)
	}
	for _, v := range []string{"777333111", "666222000"} {
		if strings.Contains(f.logs.String(), v) {
			t.Errorf("the server log mentions %q after watching stats", v)
		}
	}
}

// awaitSamplersStopped waits until the hub has no sampler left, on the hub's own signal, and
// fails the test naming what.
func awaitSamplersStopped(t testing.TB, hub *api.StatsHub, what string) {
	t.Helper()
	if err := hub.Await(testsupport.Context(t), func() bool { return hub.Samplers() == 0 }); err != nil {
		t.Fatalf("waiting for %s: %v; %d samplers are still running", what, err, hub.Samplers())
	}
}
