// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
)

// Spike S2 (D-01): what Phases 26 and 28 may rely on when the ingress proxy publishes ports 80 and 443 in host mode on
// each node. Two things are recorded. First, a client outside the swarm reaches the proxy with its own address, not a
// mesh address (so Phase 28's trusted proxies and the access log see real clients). Second, a probe on the node's
// loopback reaches the published port, and the address the backend sees for it is a bridge gateway address of the node
// (docker0's here, where the task is on no overlay; the gwbridge's when it is), never 127.0.0.1.
const (
	s2Settle = 3 * time.Minute
	// s2MeshPort is the ingress-mode contrast: the routing mesh rewrites the client address.
	s2MeshPort = 18080
)

// s2RemoteAddr matches the line the whoami backend prints for the peer it saw.
var s2RemoteAddr = regexp.MustCompile(`(?m)^RemoteAddr: (.+?)\s*$`)

// s2Peer reads the peer address out of a whoami answer, unmapping an IPv4-in-IPv6 form.
func s2Peer(t *testing.T, body string) netip.Addr {
	t.Helper()
	m := s2RemoteAddr.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the answer has no RemoteAddr line:\n%s", body)
	}
	ap, err := netip.ParseAddrPort(m[1])
	if err != nil {
		t.Fatalf("parse the RemoteAddr %q: %v", m[1], err)
	}
	return ap.Addr().Unmap()
}

// s2Global creates a global service of the whoami image that listens on port (the image's --port flag) and publishes it
// in host mode on the same port number, with one running task per node.
func s2Global(t *testing.T, sw *dockertest.Swarm, cli *client.Client, name string, port uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	grace := time.Second
	res, err := cli.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:           traefiktest.WhoamiImage,
			Args:            []string{"--port", strconv.Itoa(int(port))},
			StopGracePeriod: &grace,
		}},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{
			{Protocol: network.TCP, TargetPort: port, PublishedPort: port, PublishMode: swarm.PortConfigPublishModeHost},
		}},
	}})
	if err != nil {
		t.Fatalf("create %s with host-mode port %d: %v", name, port, err)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
		defer rcancel()
		_, _ = cli.ServiceRemove(rctx, res.ID, client.ServiceRemoveOptions{})
	})
	dockertest.WaitRunningTasks(t, sw.Managers[0], res.ID, len(sw.Nodes()), s2Settle)
}

// s2Swarm starts one manager and one worker with the whoami image and two global services that publish 80 and 443 in
// host mode, each to its own container port as the proxy's two entry points do. The engine maps only one of two
// published ports that share a target port (a probe with both of them going to port 80 listened on 443 alone), which
// is why these are two services and why the proxy spec's target ports differ.
func s2Swarm(t *testing.T) (*dockertest.Swarm, *client.Client) {
	t.Helper()
	sw := dockertest.StartSwarm(t, "", 1, 1)
	dockertest.LoadImage(t, traefiktest.WhoamiImage, sw.Nodes()...)
	cli := sw.Managers[0].Client(t)
	s2Global(t, sw, cli, "s2-whoami-80", 80)
	s2Global(t, sw, cli, "s2-whoami-443", 443)
	return sw, cli
}

// TestS2HostModeKeepsClientAddress is spike S2a: host-mode 80 and 443 answer from outside the swarm on each node's own
// address, and the backend sees the client's own address (the network's gateway, which is this machine as the nodes see
// it), not a mesh address in 10.0.0.0/8. A second service published in ingress mode records the contrast.
func TestS2HostModeKeepsClientAddress(t *testing.T) {
	sw, cli := s2Swarm(t)
	nodes := sw.Nodes()
	gateway := netip.MustParseAddr(sw.Gateway)

	// The contrast: the same image published through the routing mesh.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	one := uint64(1)
	grace := time.Second
	mesh, err := cli.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "s2-mesh"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:           traefiktest.WhoamiImage,
			StopGracePeriod: &grace,
		}},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{
			{Protocol: network.TCP, TargetPort: 80, PublishedPort: s2MeshPort, PublishMode: swarm.PortConfigPublishModeIngress},
		}},
	}})
	if err != nil {
		t.Fatalf("create s2-mesh: %v", err)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
		defer rcancel()
		_, _ = cli.ServiceRemove(rctx, mesh.ID, client.ServiceRemoveOptions{})
	})
	dockertest.WaitRunningTasks(t, sw.Managers[0], mesh.ID, 1, s2Settle)

	httpc := &http.Client{Timeout: time.Minute}
	get := func(url string) (string, error) {
		resp, err := httpc.Get(url)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// The first answer can lag the running task by a moment: wait for it, then assert on the body.
	answer := func(url string) string {
		var body string
		dockertest.Wait(t, s2Settle, "an answer from "+url, func() (bool, string) {
			b, err := get(url)
			body = b
			if err != nil {
				return false, err.Error()
			}
			return true, ""
		})
		return body
	}

	for _, n := range nodes {
		for _, port := range []int{80, 443} {
			url := fmt.Sprintf("http://%s:%d/", n.IP, port)
			peer := s2Peer(t, answer(url))
			t.Logf("engine %s node %s host-mode %s: the backend saw the client as %s (gateway %s)", sw.Engine, n.Hostname, url, peer, gateway)
			if peer != gateway {
				t.Errorf("host-mode %s: the backend saw %s, want the client's own address %s", url, peer, gateway)
			}
			if peer.Is4() && netip.MustParsePrefix("10.0.0.0/8").Contains(peer) && peer != gateway {
				t.Errorf("host-mode %s: %s is a mesh address", url, peer)
			}
		}
		meshURL := fmt.Sprintf("http://%s:%d/", n.IP, s2MeshPort)
		t.Logf("engine %s node %s ingress-mode %s (contrast, not asserted): the backend saw the client as %s",
			sw.Engine, n.Hostname, meshURL, s2Peer(t, answer(meshURL)))
	}
	if t.Failed() {
		t.Fatalf("S2 outcome: host-mode does not keep the client address on engine %s", sw.Engine)
	}
	t.Logf("S2 outcome: host-mode keeps the client address and answers on loopback on engine %s", sw.Engine)
}

// TestS2HostModeOnLoopback is spike S2b: a probe on each node's own loopback reaches host-mode 80 and 443. The peer the
// backend sees is recorded, not asserted: a bridge gateway address of the node, not 127.0.0.1, so a trusted-proxies
// list must not treat that address as a real client (Phase 28), and a loopback probe of 443 works for readiness
// (Phase 26).
func TestS2HostModeOnLoopback(t *testing.T) {
	sw, _ := s2Swarm(t)
	nodes := sw.Nodes()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	for _, n := range nodes {
		for _, port := range []int{80, 443} {
			line := fmt.Sprintf("wget -qO- http://127.0.0.1:%d/", port)
			var body string
			dockertest.Wait(t, s2Settle, fmt.Sprintf("host-mode %d to answer on the loopback of %s", port, n.Hostname), func() (bool, string) {
				out, err := n.Sh(ctx, line)
				body = out
				if err != nil {
					return false, err.Error()
				}
				return true, ""
			})
			peer := s2Peer(t, body)
			t.Logf("engine %s node %s loopback :%d: the backend saw the peer as %s", sw.Engine, n.Hostname, port, peer)
			if peer.IsLoopback() {
				t.Logf("node %s :%d: the peer is a loopback address, which differs from the bridge gateway address seen so far", n.Hostname, port)
			}
		}
	}
	t.Logf("S2 outcome: host-mode keeps the client address and answers on loopback on engine %s", sw.Engine)
}
