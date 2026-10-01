// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/engine"
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
