// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// withBase adds to what the fake host holds besides the proxy's own objects: other people's containers, networks and
// volumes.
func (r *standaloneRig) withBase(fn func(*dockertest.FakeConfig)) {
	r.t.Helper()
	r.sys.mu.Lock()
	defer r.sys.mu.Unlock()
	fn(&r.sys.base)
	r.sys.reconfigure()
}

// bound is an inspect document whose container was created with the -p bindings given as container port and host port.
func bound(t *testing.T, pairs ...[2]string) dockertest.FakeInspect {
	t.Helper()
	m := network.PortMap{}
	for _, p := range pairs {
		port, err := network.ParsePort(p[0])
		if err != nil {
			t.Fatalf("ParsePort(%q): %v", p[0], err)
		}
		m[port] = append(m[port], network.PortBinding{HostPort: p[1]})
	}
	return dockertest.FakeInspect{Document: &container.InspectResponse{HostConfig: &container.HostConfig{PortBindings: m}}}
}

// previewStandalone is the preview of a standalone rig, which must not fail.
func previewStandalone(t *testing.T, r *standaloneRig, req ingressdeploy.EnableRequest) ingressdeploy.PreviewResult {
	t.Helper()
	res, err := r.svc.Preview(testsupport.Context(t), r.envID, req)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	return res
}

// TestStandalonePreflightRefusesPublishedPorts is the tracer: every container of the host that binds host TCP 80 or 443,
// whatever its state, refuses the preview and the enable and is named with its state; a UDP 443 and a TCP 8443 do not
// clash (D-12, D-31).
func TestStandalonePreflightRefusesPublishedPorts(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	r.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "r1", Names: []string{"/web"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8080, Public: 80, Type: "tcp"}}},
			// TierPlan's own server container is a container like any other.
			dockertest.FakeContainer{ID: "r2", Names: []string{"/tierplan"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 7480, Public: 443, Type: "tcp"}}},
			// An engine lists a created or exited container with no ports, so only its bindings show them.
			dockertest.FakeContainer{ID: "c1", Names: []string{"/fresh"}, State: "created", Inspect: bound(t, [2]string{"80/tcp", "80"})},
			dockertest.FakeContainer{ID: "e1", Names: []string{"/old-proxy"}, State: "exited", Inspect: bound(t, [2]string{"443/tcp", "443"})},
			dockertest.FakeContainer{ID: "u1", Names: []string{"/quic"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 443, Public: 443, Type: "udp"}}},
			dockertest.FakeContainer{ID: "u2", Names: []string{"/stopped-quic"}, State: "exited", Inspect: bound(t, [2]string{"443/udp", "443"})},
			dockertest.FakeContainer{ID: "o1", Names: []string{"/other"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8443, Public: 8443, Type: "tcp"}}},
		)
	})

	want := []string{"fresh publishes 80 (created)", "old-proxy publishes 443 (exited)", "tierplan publishes 443 (running)", "web publishes 80 (running)"}
	f, ok := findingOf(previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodePortInUse)
	if !ok || f.Severity != ingressdeploy.SeverityError {
		t.Fatalf("no ingress_port_in_use error in the preview")
	}
	if !slices.Equal(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}
	got := wantRefusal(t, r.rig, ingressdeploy.EnableRequest{}, ingressdeploy.CodePortInUse)
	if !slices.Equal(got.Items, want) {
		t.Errorf("Enable's items = %q, want %q", got.Items, want)
	}
	if r.sys.deployCount() != 0 {
		t.Error("a refused enable deployed")
	}

	// A host whose only bindings are UDP 443 and TCP 8443 has the ports free.
	free := newStandaloneRig(t, standaloneRigOptions{})
	free.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "u1", Names: []string{"/quic"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 443, Public: 443, Type: "udp"}}},
			dockertest.FakeContainer{ID: "u2", Names: []string{"/stopped-quic"}, State: "exited", Inspect: bound(t, [2]string{"443/udp", "443"})},
			dockertest.FakeContainer{ID: "o1", Names: []string{"/other"}, State: "exited", Inspect: bound(t, [2]string{"8443/tcp", "8443"})},
		)
	})
	if _, ok := findingOf(previewStandalone(t, free, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodePortInUse); ok {
		t.Error("a UDP 443 or a TCP 8443 refused the enable")
	}
}

// TestStandalonePreflightIgnoresItsOwnProxy: the proxy's container, whatever its state, never counts as a holder of
// its own ports, so an enable that is run again after a failed one does not refuse itself; a container that only has
// the project's name does count.
func TestStandalonePreflightIgnoresItsOwnProxy(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	own := ingress.ObjectLabels()
	own["com.docker.compose.project"] = ingress.ProjectName
	own["com.docker.compose.service"] = ingress.ProxyComposeService
	r.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "p1", Names: []string{"/tierplan-ingress-proxy-1"}, State: "running", Labels: own,
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 80, Public: 80, Type: "tcp"}, {IP: "0.0.0.0", Private: 443, Public: 443, Type: "tcp"}}},
		)
	})
	for _, f := range previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings {
		if f.Code == ingressdeploy.CodePortInUse {
			t.Fatalf("finding = %+v, want the proxy's own container left out", f)
		}
	}

	// The same proxy stopped, its bindings known only to its inspect.
	stopped := newStandaloneRig(t, standaloneRigOptions{})
	stopped.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "p1", Names: []string{"/tierplan-ingress-proxy-1"}, State: "exited", Labels: own,
				Inspect: bound(t, [2]string{"80/tcp", "80"}, [2]string{"443/tcp", "443"})},
		)
	})
	for _, f := range previewStandalone(t, stopped, ingressdeploy.EnableRequest{}).Findings {
		if f.Code == ingressdeploy.CodePortInUse {
			t.Fatalf("finding = %+v, want the stopped proxy left out", f)
		}
	}

	// A container that carries the component label but is not the proxy's service is somebody else's.
	impostor := newStandaloneRig(t, standaloneRigOptions{})
	forged := ingress.ObjectLabels()
	impostor.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "x1", Names: []string{"/lookalike"}, State: "running", Labels: forged,
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 80, Public: 80, Type: "tcp"}}},
		)
	})
	f, ok := findingOf(previewStandalone(t, impostor, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodePortInUse)
	if !ok || !slices.Equal(f.Items, []string{"lookalike publishes 80 (running)"}) {
		t.Errorf("finding = %+v, want the container that only carries the label named", f)
	}
}
