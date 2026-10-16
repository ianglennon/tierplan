// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Real-engine checks (D-28). CI runs this against docker:24-dind through
// docker:29-dind with BAYPLAN_TEST_DOCKER_HOST and BAYPLAN_TEST_ENGINE set;
// `make test-int` starts one dind with testcontainers. The product only ever
// builds a local socket transport, so the tests reach the dind over TCP through
// the same dockerx code path with the test-only TCPTransport (D-21).
//
// The cases share one engine and run in order, because the swarm state they
// create is part of what the next one checks.
func TestIntegrationProbeAgainstARealEngine(t *testing.T) {
	host := dockertest.DockerHost(t)
	cli, err := dockerx.New(dockertest.TCPTransportFor(host))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)

	// What the engine reports about itself, for the record: these are the strings
	// that classification reads (rootless and Docker Desktop detection are
	// assumptions until someone runs this against those engines).
	if info, err := raw.Info(ctx, client.InfoOptions{}); err == nil {
		t.Logf("observed Info.SecurityOptions = %q", info.Info.SecurityOptions)
		t.Logf("observed Info.OperatingSystem = %q, ServerVersion = %q", info.Info.OperatingSystem, info.Info.ServerVersion)
	}
	if ver, err := raw.ServerVersion(ctx, client.ServerVersionOptions{}); err == nil {
		t.Logf("observed /version Platform.Name = %q, API = %s (min %s)", ver.Platform.Name, ver.APIVersion, ver.MinAPIVersion)
		for _, c := range ver.Components {
			t.Logf("observed /version component %q = %s", c.Name, c.Version)
		}
	}

	t.Run("a fresh engine is a supported standalone host", func(t *testing.T) {
		res, err := cli.Probe(ctx)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		t.Logf("probe: %+v", res)
		if !res.Reachable || !res.Supported {
			t.Fatalf("reachable=%v supported=%v reason=%q detail=%q", res.Reachable, res.Supported, res.Reason, res.Detail)
		}
		if res.Type != dockerx.TypeStandalone {
			t.Errorf("Type = %q, want %q", res.Type, dockerx.TypeStandalone)
		}
		if res.EngineName != "Docker Engine" || res.NegotiatedAPI == "" || res.Hostname == "" {
			t.Errorf("engine %q negotiated %q hostname %q", res.EngineName, res.NegotiatedAPI, res.Hostname)
		}
		if want := os.Getenv(dockertest.EnvEngine); want != "" {
			major, _, _ := strings.Cut(res.EngineVersion, ".")
			if major != want {
				t.Errorf("EngineVersion = %q, want major %s (BAYPLAN_TEST_ENGINE)", res.EngineVersion, want)
			}
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(strings.SplitN(res.EngineVersion, ".", 2)[0], "v")); err != nil || n < 24 {
			t.Errorf("EngineVersion = %q, want Docker 24 or newer", res.EngineVersion)
		}
	})

	t.Run("probing again renegotiates and gives the same answer", func(t *testing.T) {
		first, err := cli.Probe(ctx)
		if err != nil {
			t.Fatalf("first Probe: %v", err)
		}
		second, err := cli.Probe(ctx)
		if err != nil {
			t.Fatalf("second Probe: %v", err)
		}
		if first.NegotiatedAPI != second.NegotiatedAPI || second.NegotiatedAPI == "" {
			t.Errorf("negotiated %q then %q", first.NegotiatedAPI, second.NegotiatedAPI)
		}
	})

	t.Run("after swarm init the host is a swarm manager", func(t *testing.T) {
		if _, err := raw.SwarmInit(ctx, client.SwarmInitOptions{ListenAddr: "0.0.0.0:2377", AdvertiseAddr: "127.0.0.1"}); err != nil {
			t.Fatalf("swarm init: %v", err)
		}
		t.Cleanup(func() {
			leave, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = raw.SwarmLeave(leave, client.SwarmLeaveOptions{Force: true})
		})
		res, err := cli.Probe(ctx)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if res.Type != dockerx.TypeSwarmManager || res.SwarmNodeState != "active" {
			t.Errorf("Type = %q, SwarmNodeState = %q, want swarm-manager and active", res.Type, res.SwarmNodeState)
		}
		if !res.Supported {
			t.Errorf("a swarm manager must still be supported: reason %q", res.Reason)
		}
	})
}

// TestIntegrationLocalSocketTransport runs the transport the product really
// uses, a local unix socket, against whatever Docker this machine has. It skips
// when there is no socket or the user may not open it, so it never fails a
// machine that simply has no Docker.
func TestIntegrationLocalSocketTransport(t *testing.T) {
	path, err := engine.ParseDockerHost(os.Getenv("DOCKER_HOST"))
	if err != nil {
		path = engine.DefaultSocketPath
	}
	if !engine.SocketExists(path) {
		t.Skipf("no Docker socket at %s", path)
	}
	cli, err := dockerx.New(engine.LocalSocketTransport{Path: path})
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cli.Probe(ctx)
	if !res.Reachable {
		t.Skipf("the Docker socket at %s is not usable by this user: %v", path, err)
	}
	t.Logf("local engine: %s %s, API %s (negotiated %s), type %s, supported=%v reason=%q",
		res.EngineName, res.EngineVersion, res.APIVersion, res.NegotiatedAPI, res.Type, res.Supported, res.Reason)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Hostname == "" || res.NegotiatedAPI == "" {
		t.Errorf("hostname %q negotiated %q", res.Hostname, res.NegotiatedAPI)
	}
}

// TestIntegrationSnapshotCountsEqualTheCreatedResources checks Pitfall 22 and
// A3 on a real engine of whichever major CI runs: the counts are the lengths of
// the lists, and the lists hold what was created, including a container that
// was created but never started. Engine 29 may use the containerd image store,
// so image counts are only compared with the image list, never with Info.
func TestIntegrationSnapshotCountsEqualTheCreatedResources(t *testing.T) {
	host := dockertest.DockerHost(t)
	cli, err := dockerx.New(dockertest.TCPTransportFor(host))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)
	cleanup := func(f func(context.Context)) {
		t.Cleanup(func() {
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			f(c)
		})
	}

	pull, err := raw.ImagePull(ctx, "alpine:3", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull alpine: %v", err)
	}
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	running, stopped, vol, net := "bp-run-"+sfx, "bp-stop-"+sfx, "bp-vol-"+sfx, "bp-net-"+sfx
	for _, name := range []string{running, stopped} {
		if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: "alpine:3", Cmd: []string{"sleep", "300"}},
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		cleanup(func(c context.Context) {
			_, _ = raw.ContainerRemove(c, name, client.ContainerRemoveOptions{Force: true})
		})
	}
	if _, err := raw.ContainerStart(ctx, running, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start %s: %v", running, err)
	}
	if _, err := raw.VolumeCreate(ctx, client.VolumeCreateOptions{Name: vol}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	cleanup(func(c context.Context) { _, _ = raw.VolumeRemove(c, vol, client.VolumeRemoveOptions{Force: true}) })
	if _, err := raw.NetworkCreate(ctx, net, client.NetworkCreateOptions{}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	cleanup(func(c context.Context) { _, _ = raw.NetworkRemove(c, net, client.NetworkRemoveOptions{}) })

	// The client negotiates in Probe, as the monitor does before any read.
	if _, err := cli.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	assertConsistent := func(t *testing.T, s dockerx.Snapshot) {
		t.Helper()
		c := s.Counts
		if c.Containers != len(s.Containers) || c.Images != len(s.Images) || c.Volumes != len(s.Volumes) || c.Networks != len(s.Networks) {
			t.Errorf("counts %+v do not equal the list lengths %d/%d/%d/%d",
				c, len(s.Containers), len(s.Images), len(s.Volumes), len(s.Networks))
		}
		if c.ContainersRunning+c.ContainersStopped != c.Containers {
			t.Errorf("running %d + stopped %d != containers %d", c.ContainersRunning, c.ContainersStopped, c.Containers)
		}
	}

	t.Run("a standalone engine lists what was created", func(t *testing.T) {
		snap, err := cli.Snapshot(ctx, false)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		assertConsistent(t, snap)
		byName := map[string]dockerx.ContainerRow{}
		for _, c := range snap.Containers {
			byName[c.Name] = c
		}
		if byName[running].State != "running" || byName[stopped].State != "created" {
			t.Errorf("states = %q and %q, want running and created", byName[running].State, byName[stopped].State)
		}
		if snap.Counts.ContainersRunning < 1 || snap.Counts.ContainersStopped < 1 {
			t.Errorf("counts = %+v, want at least one running and one stopped", snap.Counts)
		}
		if !slices.ContainsFunc(snap.Volumes, func(v dockerx.VolumeRow) bool { return v.Name == vol && v.Driver == "local" }) {
			t.Errorf("volume %s is missing from %+v", vol, snap.Volumes)
		}
		if !slices.ContainsFunc(snap.Networks, func(n dockerx.NetworkRow) bool { return n.Name == net && n.Driver == "bridge" }) {
			t.Errorf("network %s is missing from %+v", net, snap.Networks)
		}
		if !slices.ContainsFunc(snap.Images, func(i dockerx.ImageRow) bool { return slices.Contains(i.RepoTags, "alpine:3") }) {
			t.Errorf("alpine:3 is missing from %+v", snap.Images)
		}
		if snap.Counts.Nodes != nil || snap.Counts.Services != nil {
			t.Errorf("a standalone engine reported swarm counts: %+v", snap.Counts)
		}
	})

	t.Run("a swarm manager also counts its node and service", func(t *testing.T) {
		if _, err := raw.SwarmInit(ctx, client.SwarmInitOptions{ListenAddr: "0.0.0.0:2377", AdvertiseAddr: "127.0.0.1"}); err != nil {
			t.Fatalf("swarm init: %v", err)
		}
		cleanup(func(c context.Context) { _, _ = raw.SwarmLeave(c, client.SwarmLeaveOptions{Force: true}) })
		svc, err := raw.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: "bp-svc-" + sfx},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "alpine:3", Command: []string{"sleep", "300"}}},
		}})
		if err != nil {
			t.Fatalf("create service: %v", err)
		}
		cleanup(func(c context.Context) { _, _ = raw.ServiceRemove(c, svc.ID, client.ServiceRemoveOptions{}) })

		// The engine now reports itself as a manager; the probe is what tells
		// the monitor to pass true.
		res, err := cli.Probe(ctx)
		if err != nil || res.Type != dockerx.TypeSwarmManager {
			t.Fatalf("Probe type = %q, err %v, want swarm-manager", res.Type, err)
		}
		snap, err := cli.Snapshot(ctx, true)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		assertConsistent(t, snap)
		if snap.Counts.Nodes == nil || *snap.Counts.Nodes != 1 || snap.Counts.Services == nil || *snap.Counts.Services != 1 {
			t.Errorf("nodes/services = %v/%v, want 1/1", snap.Counts.Nodes, snap.Counts.Services)
		}
	})
}

// TestIntegrationEventsHubSeesAContainerStart checks the events feed against a
// real engine of whichever major CI runs (plan 21, ENV-07): a subscriber of the
// hub receives the container start event within 5 seconds, with the attribute
// allowlist applied to what a real daemon sends (T-01-88). The hub runs behind
// a real monitor over the test-only TCP transport, the same code path as the
// product.
func TestIntegrationEventsHubSeesAContainerStart(t *testing.T) {
	host := dockertest.DockerHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)

	pull, err := raw.ImagePull(ctx, "alpine:3", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull alpine: %v", err)
	}
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	const env = "it-env"
	mon := envs.NewMonitor(slog.New(slog.DiscardHandler), nil, nil)
	hub := envs.NewHub(mon, nil, slog.New(slog.DiscardHandler), time.Now)
	t.Cleanup(mon.StopAll)
	t.Cleanup(hub.Close)
	if err := mon.Start(ctx, env, dockertest.TCPTransportFor(host)); err != nil {
		t.Fatalf("start the monitor: %v", err)
	}
	if _, err := mon.AwaitStatus(testsupport.Context(t), env, func(st envs.Status) bool { return st.State == envs.StateOnline }); err != nil {
		t.Fatalf("the environment never came online: %v; last status %+v", err, mon.Status(env))
	}

	backlog, ch, stop := hub.Subscribe(ctx, env)
	defer func() { stop() }()

	name := "bp-ev-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: "alpine:3", Cmd: []string{"sleep", "60"}, Labels: map[string]string{"bayplan.test.secret": "hunter2"}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, name, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := raw.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// isStart checks the start event of the test's container, with the allowlist applied.
	isStart := func(ev dockerx.EventRow) bool {
		if ev.Type != "container" || ev.Action != "start" || ev.Name != name {
			return false
		}
		for k := range ev.Attributes {
			if k != "name" && k != "image" && k != "exitCode" && k != "signal" && k != "com.docker.compose.project" && k != "com.docker.stack.namespace" {
				t.Errorf("attribute %q passed the allowlist", k)
			}
		}
		if ev.Attributes["image"] != "alpine:3" || ev.ID == "" || ev.TimeNano == 0 {
			t.Errorf("start event = %+v", ev)
		}
		return true
	}

	// The feed's first stream replays what the daemon still buffers (up to 256 events
	// from the backfill window). On an engine other tests have used, that burst can put
	// a fresh subscriber more than 64 events behind, and the hub then closes its channel
	// so it reconnects, as a browser does with Last-Event-ID. The test does the same:
	// subscribe again and look in the backlog as well as the live channel.
	timeout := time.After(15 * time.Second)
	for {
		for _, ev := range backlog {
			if isStart(ev) {
				return
			}
		}
		backlog = nil
		select {
		case ev, ok := <-ch:
			if !ok {
				stop()
				backlog, ch, stop = hub.Subscribe(ctx, env)
				continue
			}
			if isStart(ev) {
				return
			}
		case <-timeout:
			t.Fatal("no container start event within 15 seconds")
		}
	}
}

// TestEventsSwarmTypesOnStandalone checks, against whichever engine major CI runs
// (plan 04-26, D-48, RESEARCH A6), that the events stream filtered with the
// widened types (service, node, secret and config as well) opens on a standalone
// engine that has no swarm, is not ended by it, and still delivers a container
// start with only the allowlisted attributes: the compose project label passes
// (D-61) and a secret-looking label does not.
func TestEventsSwarmTypesOnStandalone(t *testing.T) {
	host := dockertest.DockerHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)
	if res, err := raw.Info(ctx, client.InfoOptions{}); err != nil {
		t.Fatalf("info: %v", err)
	} else if res.Info.Swarm.LocalNodeState != swarm.LocalNodeStateInactive {
		t.Fatalf("the engine is in a swarm (%q); this test is about a standalone engine", res.Info.Swarm.LocalNodeState)
	}

	pull, err := raw.ImagePull(ctx, "alpine:3", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull alpine: %v", err)
	}
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	cli, err := dockerx.New(dockertest.TCPTransportFor(host))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	rows, errc := cli.Events(sctx, time.Time{})

	name := "bp-evt-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{Image: "alpine:3", Cmd: []string{"sleep", "60"}, Labels: map[string]string{
			"bayplan.test.secret": "hunter2", "com.docker.compose.project": "bp-test",
		}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, name, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := raw.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	timeout := time.After(10 * time.Second)
	for {
		select {
		case row, ok := <-rows:
			if !ok {
				t.Fatalf("the events stream ended instead of delivering the start: %v", <-errc)
			}
			t.Logf("event: %s %s %s %v", row.Type, row.Action, row.Name, row.Attributes)
			if row.Type != "container" || row.Action != "start" || row.Name != name {
				continue
			}
			if row.Attributes["com.docker.compose.project"] != "bp-test" {
				t.Errorf("the compose project label did not pass: %v", row.Attributes)
			}
			if _, leaked := row.Attributes["bayplan.test.secret"]; leaked {
				t.Errorf("a label outside the allowlist reached the row: %v", row.Attributes)
			}
			return
		case <-timeout:
			t.Fatal("no container start event within 10 seconds")
		}
	}
}

// TestContainerHealthInList checks, against the engine BAYPLAN_TEST_ENGINE names
// (24 to 29), that a container whose HEALTHCHECK keeps failing reads unhealthy in
// the list within its retries (plan 04-06, RESEARCH A2). Engines before API 1.52
// send no health summary, so this is what proves the status-text fallback.
func TestContainerHealthInList(t *testing.T) {
	host := dockertest.DockerHost(t)
	cli, err := dockerx.New(dockertest.TCPTransportFor(host))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)

	pull, err := raw.ImagePull(ctx, "alpine:3", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull alpine: %v", err)
	}
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	name := "bp-health-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image: "alpine:3", Cmd: []string{"sleep", "300"},
			Healthcheck: &container.HealthConfig{Test: []string{"CMD-SHELL", "false"}, Interval: time.Second, Timeout: time.Second, Retries: 1},
		},
	}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, name, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := raw.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	if _, err := cli.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	testsupport.Poll(t, 90*time.Second, "the failing container to read unhealthy in the list", func() (bool, string) {
		snap, err := cli.Snapshot(ctx, false)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		var seen string
		for _, c := range snap.Containers {
			if c.Name == name {
				seen = c.Health + " (" + c.Status + ")"
				if c.Health == dockerx.HealthUnhealthy {
					return true, seen
				}
			}
		}
		return false, seen
	})
}
