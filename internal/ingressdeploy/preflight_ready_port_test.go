// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestSwarmPreflightRefusesTheReadinessPort: a swarm service that publishes host TCP 7483, and a plain container that
// publishes 0.0.0.0:7483->9000/tcp on a node, are refused and named, as 80 and 443 are, under the sentence that names all
// three ports (D-13, A-06).
func TestSwarmPreflightRefusesTheReadinessPort(t *testing.T) {
	r, ne := newNodeRig(t, rigOptions{config: func(c *dockertest.FakeConfig) {
		c.ServiceRows = []dockertest.FakeService{
			{ID: "s1", Name: "metrics", Mode: "global",
				Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 9000, PublishedPort: 7483, PublishMode: "host"}}},
			{ID: "s2", Name: "quic", Mode: "replicated", Desired: 1,
				Ports: []swarm.PortConfig{{Protocol: "udp", TargetPort: 9000, PublishedPort: 7483, PublishMode: "host"}}},
		}
	}})
	withWorker(t, r, ne,
		dockertest.FakeContainer{ID: "c1", Names: []string{"/exporter"}, State: "running",
			Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 9000, Public: 7483, Type: "tcp"}}},
		dockertest.FakeContainer{ID: "c2", Names: []string{"/udp-exporter"}, State: "running",
			Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 9000, Public: 7483, Type: "udp"}}},
	)

	res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	var services, containers *ingressdeploy.Finding
	for i := range res.Findings {
		f := &res.Findings[i]
		if f.Code != ingressdeploy.CodePortInUse {
			continue
		}
		if strings.HasSuffix(f.Message, "These publish them:") {
			services = f
		} else {
			containers = f
		}
	}
	if services == nil || services.Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want the service finding as an error", res.Findings)
	}
	if want := "Ports 80, 443 and 7483 must be free on every node. These publish them:"; services.Message != want {
		t.Errorf("service message = %q, want %q", services.Message, want)
	}
	if want := []string{"metrics publishes 7483 (host mode)"}; !slices.Equal(services.Items, want) {
		t.Errorf("service items = %q, want %q", services.Items, want)
	}
	if containers == nil || containers.Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want the container finding as an error", res.Findings)
	}
	if want := "Ports 80, 443 and 7483 must be free on every node. These containers publish them:"; containers.Message != want {
		t.Errorf("container message = %q, want %q", containers.Message, want)
	}
	if want := []string{"worker-1: exporter publishes 7483"}; !slices.Equal(containers.Items, want) {
		t.Errorf("container items = %q, want %q", containers.Items, want)
	}
	wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodePortInUse)
}

// TestStandalonePreflightIgnoresTheReadinessPort: the readiness port is a swarm concern (A-06), so a standalone host that
// has a container on host TCP 7483 passes, and the standalone message still names 80 and 443 only.
func TestStandalonePreflightIgnoresTheReadinessPort(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	r.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "r1", Names: []string{"/exporter"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 9000, Public: 7483, Type: "tcp"}}},
			dockertest.FakeContainer{ID: "e1", Names: []string{"/old-exporter"}, State: "exited", Inspect: bound(t, [2]string{"9000/tcp", "7483"})},
		)
	})
	res := previewStandalone(t, r, ingressdeploy.EnableRequest{})
	if f, ok := findingOf(res.Findings, ingressdeploy.CodePortInUse); ok {
		t.Errorf("finding = %+v, want none: the standalone proxy does not use 7483", f)
	}

	r.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "r2", Names: []string{"/web"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8080, Public: 80, Type: "tcp"}}})
	})
	res = previewStandalone(t, r, ingressdeploy.EnableRequest{})
	f, ok := findingOf(res.Findings, ingressdeploy.CodePortInUse)
	if !ok || !strings.HasPrefix(f.Message, "Ports 80 and 443 must be free on this host.") {
		t.Errorf("finding = %+v, want the standalone sentence naming 80 and 443", f)
	}
	if want := []string{"web publishes 80 (running)"}; !slices.Equal(f.Items, want) {
		t.Errorf("items = %q, want %q (the 7483 containers must not be named)", f.Items, want)
	}
}
