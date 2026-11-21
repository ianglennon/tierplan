// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// Spike S5 (D-01, D-05): the network Phase 24's deployer creates for the ingress proxy and the routed backends is fixed
// at creation, so whether it may be non-attachable is decided on a real swarm before it exists anywhere. A service
// attaches to a network through its spec (TaskTemplate.Networks), a plain container only through `network connect` or
// `--network`; the question is whether the engine lets the first through and refuses the second on a non-attachable
// overlay.
const (
	// s5Settle bounds a wait for a task to start or finish; a call that only talks to the engine is bounded by the
	// 30 minute context the helpers create, so a slow engine never fails on a short real-time bound.
	s5Settle = 3 * time.Minute
	// s5Refusal is the engine's words for a plain container on a non-attachable overlay.
	s5Refusal = "not manually attachable"
)

// s5Overlay creates an overlay network on the swarm through the manager and registers its removal.
func s5Overlay(t *testing.T, cli *client.Client, name, subnet string, attachable bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	_, err := cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:     "overlay",
		Scope:      "swarm",
		Attachable: attachable,
		IPAM:       &network.IPAM{Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix(subnet)}}},
	})
	if err != nil {
		t.Fatalf("create the overlay %s (attachable %v): %v", name, attachable, err)
	}
	t.Cleanup(func() {
		// The swarm goes with the test; this only keeps a failing run from leaving the network to the next step.
		rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
		defer rcancel()
		_, _ = cli.NetworkRemove(rctx, name, client.NetworkRemoveOptions{})
	})
}

// s5Service creates a one-replica service on the given networks and registers its removal. command runs in busybox.
func s5Service(t *testing.T, cli *client.Client, name string, nets []string, restart swarm.RestartPolicyCondition, constraints []string, command ...string) string {
	t.Helper()
	one := uint64(1)
	grace := time.Second
	var attach []swarm.NetworkAttachmentConfig
	for _, n := range nets {
		attach = append(attach, swarm.NetworkAttachmentConfig{Target: n})
	}
	var place *swarm.Placement
	if len(constraints) > 0 {
		place = &swarm.Placement{Constraints: constraints}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := cli.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   dockertest.ServiceTestImage,
				Command: command,
				// sh as PID 1 ignores SIGTERM: without this a removed task takes ten seconds to stop.
				StopGracePeriod: &grace,
			},
			Networks:      attach,
			RestartPolicy: &swarm.RestartPolicy{Condition: restart},
			Placement:     place,
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
	}})
	if err != nil {
		t.Fatalf("create the service %s: %v", name, err)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
		defer rcancel()
		_, _ = cli.ServiceRemove(rctx, res.ID, client.ServiceRemoveOptions{})
	})
	return res.ID
}

// s5WaitCompleted waits until a task of the service has finished with exit code 0, and fails on a task that failed.
func s5WaitCompleted(t *testing.T, cli *client.Client, serviceID, what string) {
	t.Helper()
	dockertest.Wait(t, s5Settle, what, func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		filters := make(client.Filters)
		filters.Add("service", serviceID)
		res, err := cli.TaskList(ctx, client.TaskListOptions{Filters: filters})
		if err != nil {
			return false, err.Error()
		}
		var seen []string
		for _, tk := range res.Items {
			seen = append(seen, fmt.Sprintf("%s->%s %s", tk.Status.State, tk.DesiredState, tk.Status.Err))
			if tk.Status.State == swarm.TaskStateComplete {
				return true, ""
			}
			if tk.Status.State == swarm.TaskStateFailed || tk.Status.State == swarm.TaskStateRejected {
				t.Fatalf("%s: a task ended %s: %s", what, tk.Status.State, tk.Status.Err)
			}
		}
		return false, strings.Join(seen, "; ")
	})
}

// s5TaskContainer is the running container of the service's task on the node, by the swarm's own service label.
func s5TaskContainer(t *testing.T, n *dockertest.Node, service string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	filters := make(client.Filters)
	filters.Add("label", "com.docker.swarm.service.name="+service)
	filters.Add("status", "running")
	res, err := n.Client(t).ContainerList(ctx, client.ContainerListOptions{Filters: filters})
	if err != nil {
		t.Fatalf("list the containers of %s on %s: %v", service, n.Hostname, err)
	}
	if len(res.Items) != 1 {
		return "", false
	}
	return res.Items[0].ID, true
}

// s5Networks lists the networks a container is on, sorted.
func s5Networks(t *testing.T, cli *client.Client, id string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect container %s: %v", id, err)
	}
	var names []string
	if res.Container.NetworkSettings != nil {
		for name := range res.Container.NetworkSettings.Networks {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// TestS5ServiceAttachOnNonAttachableOverlay is spike S5a: on a swarm of one manager and one worker, an overlay created
// with Attachable false holds a replicated service attached through its spec, the task reaches running, a second service
// on the same network reaches the first by its service name through the VIP, and a plain container created with that
// network is refused with the engine's words. The network inspect still reports Attachable false.
func TestS5ServiceAttachOnNonAttachableOverlay(t *testing.T) {
	sw := dockertest.StartSwarm(t, "", 1, 1)
	nodes := sw.Nodes()
	dockertest.LoadImage(t, dockertest.ServiceTestImage, nodes...)
	manager := sw.Managers[0]
	cli := manager.Client(t)

	const (
		netName = "s5-ingress"
		web     = "s5-web"
		probe   = "s5-probe"
	)
	s5Overlay(t, cli, netName, "10.77.0.0/24", false)

	webID := s5Service(t, cli, web, []string{netName}, swarm.RestartPolicyConditionAny, nil,
		"sh", "-c", "echo s5-web-ok > /tmp/index.html && exec httpd -f -p 8080 -h /tmp")
	dockertest.WaitRunningTasks(t, manager, webID, 1, s5Settle)

	// The second service resolves s5-web by its service name (embedded DNS answers the VIP) and fetches the page.
	probeID := s5Service(t, cli, probe, []string{netName}, swarm.RestartPolicyConditionNone, nil,
		"sh", "-c", "for i in $(seq 60); do wget -q -T 2 -O- http://"+web+":8080/ | grep -q s5-web-ok && exit 0; sleep 1; done; exit 1")
	s5WaitCompleted(t, cli, probeID, "the probe service to reach "+web+" by name over "+netName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	insp, err := cli.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", netName, err)
	}
	if insp.Network.Attachable {
		t.Fatalf("S5 outcome: %s reports Attachable true on engine %s although it was created with false", netName, sw.Engine)
	}

	// A plain container on the same network is what attachable would allow and the spec attach does not need.
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           &container.Config{Image: dockertest.ServiceTestImage, Cmd: []string{"sleep", "60"}},
		HostConfig:       &container.HostConfig{NetworkMode: container.NetworkMode(netName)},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
	})
	var refusal error
	if err != nil {
		refusal = err
	} else {
		t.Cleanup(func() {
			rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
			defer rcancel()
			_, _ = cli.ContainerRemove(rctx, created.ID, client.ContainerRemoveOptions{Force: true})
		})
		_, refusal = cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	}
	if refusal == nil {
		t.Fatalf("S5 outcome: a plain container started on the non-attachable overlay %s on engine %s", netName, sw.Engine)
	}
	if !strings.Contains(refusal.Error(), s5Refusal) {
		t.Fatalf("the plain container on %s was refused, but not with %q: %v", netName, s5Refusal, refusal)
	}
	t.Logf("network %s: Attachable %v, scope %s, driver %s; the plain container was refused: %v",
		netName, insp.Network.Attachable, insp.Network.Scope, insp.Network.Driver, refusal)
	t.Logf("S5 outcome: spec attach works on a non-attachable overlay on engine %s", sw.Engine)
}

// TestS5ConnectRunningTask is spike S5b. Asserted: a running task container on a manager can be connected to an
// attachable overlay, and loses that attachment when the service is force-updated (the replacement task is a new
// container). Recorded, because the engines word and order it differently: a swarm network that nothing on a node uses
// is unknown to that node's engine, so a connect to it answers "not found" (worker and manager, attachable or not); and
// once the node holds a non-attachable overlay for a service task, a connect of a running container to it is not
// refused. Only the cluster-level attach (`docker run --network`, or a connect on a node that does not hold the
// network yet) checks the attachable flag, which is what S5a asserts. Phase 28 reaches the proxy's network with a
// service update for these reasons, never a network connect.
func TestS5ConnectRunningTask(t *testing.T) {
	sw := dockertest.StartSwarm(t, "", 1, 1)
	nodes := sw.Nodes()
	dockertest.LoadImage(t, dockertest.ServiceTestImage, nodes...)
	manager, worker := sw.Managers[0], sw.Workers[0]
	cli := manager.Client(t)
	wcli := worker.Client(t)

	const (
		att    = "s5-att"
		nonatt = "s5-nonatt"
		svc    = "s5-task"
		wsvc   = "s5-wtask"
		holder = "s5-holder"
	)
	s5Overlay(t, cli, att, "10.78.0.0/24", true)
	s5Overlay(t, cli, nonatt, "10.79.0.0/24", false)

	svcID := s5Service(t, cli, svc, nil, swarm.RestartPolicyConditionAny, []string{"node.role==manager"}, "sleep", "3600")
	wsvcID := s5Service(t, cli, wsvc, nil, swarm.RestartPolicyConditionAny, []string{"node.role==worker"}, "sleep", "3600")
	dockertest.WaitRunningTasks(t, manager, svcID, 1, s5Settle)
	dockertest.WaitRunningTasks(t, manager, wsvcID, 1, s5Settle)
	var first, wfirst string
	dockertest.Wait(t, s5Settle, "the task container of "+svc+" on "+manager.Hostname, func() (bool, string) {
		id, ok := s5TaskContainer(t, manager, svc)
		first = id
		return ok, "no single running container yet"
	})
	dockertest.Wait(t, s5Settle, "the task container of "+wsvc+" on "+worker.Hostname, func() (bool, string) {
		id, ok := s5TaskContainer(t, worker, wsvc)
		wfirst = id
		return ok, "no single running container yet"
	})
	before := s5Networks(t, cli, first)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Recorded, not asserted: the wording differs between engines. Nothing on either node uses the networks yet.
	_, mAtt := cli.NetworkConnect(ctx, att, client.NetworkConnectOptions{Container: first})
	_, mNon := cli.NetworkConnect(ctx, nonatt, client.NetworkConnectOptions{Container: first})
	_, wAtt := wcli.NetworkConnect(ctx, att, client.NetworkConnectOptions{Container: wfirst})
	t.Logf("networks no task uses yet, connecting a running task: on the manager to %s: %v; to %s: %v; on the worker to %s: %v",
		att, mAtt, nonatt, mNon, att, wAtt)
	// The manager now holds both networks: a plain container on the attachable one, a service task on the other.
	plain, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           &container.Config{Image: dockertest.ServiceTestImage, Cmd: []string{"sleep", "3600"}},
		HostConfig:       &container.HostConfig{NetworkMode: container.NetworkMode(att)},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{att: {}}},
	})
	if err != nil {
		t.Fatalf("create a plain container on the attachable overlay %s: %v", att, err)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
		defer rcancel()
		_, _ = cli.ContainerRemove(rctx, plain.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(ctx, plain.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start a plain container on the attachable overlay %s: %v", att, err)
	}
	holderID := s5Service(t, cli, holder, []string{nonatt}, swarm.RestartPolicyConditionAny, []string{"node.role==manager"}, "sleep", "3600")
	dockertest.WaitRunningTasks(t, manager, holderID, 1, s5Settle)

	if _, err := cli.NetworkConnect(ctx, att, client.NetworkConnectOptions{Container: first}); err != nil {
		t.Fatalf("connect the running task container to the attachable overlay %s: %v", att, err)
	}
	connected := s5Networks(t, cli, first)
	if !slices.Contains(connected, att) {
		t.Fatalf("after the connect the task container lists %v, want %s among them", connected, att)
	}

	// Force-update: a new spec version with a bumped ForceUpdate replaces the task.
	cur, err := cli.ServiceInspect(ctx, svcID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", svc, err)
	}
	spec := cur.Service.Spec
	spec.TaskTemplate.ForceUpdate++
	if _, err := cli.ServiceUpdate(ctx, svcID, client.ServiceUpdateOptions{Version: cur.Service.Version, Spec: spec}); err != nil {
		t.Fatalf("force-update %s: %v", svc, err)
	}
	var second string
	dockertest.Wait(t, s5Settle, "a replacement task container of "+svc+" on "+manager.Hostname, func() (bool, string) {
		id, ok := s5TaskContainer(t, manager, svc)
		if !ok {
			return false, "no single running container"
		}
		second = id
		return id != first, "still the first container " + first
	})
	after := s5Networks(t, cli, second)
	if slices.Contains(after, att) {
		t.Fatalf("the replacement task container still lists %s: %v", att, after)
	}

	// Recorded, not asserted (see the comment on the test): the node holds the non-attachable overlay for the holder.
	_, nonErr := cli.NetworkConnect(ctx, nonatt, client.NetworkConnectOptions{Container: second})
	t.Logf("task container networks: before the connect %v, after connecting to %s %v, after the force-update (new container) %v",
		before, att, connected, after)
	if nonErr == nil {
		t.Logf("connecting the running task to the non-attachable overlay %s once the node holds it: allowed, the container lists %v",
			nonatt, s5Networks(t, cli, second))
		t.Logf("S5 outcome: a connect to an attachable overlay is lost on replacement; a non-attachable overlay the node holds still takes a connect on engine %s", sw.Engine)
		return
	}
	t.Logf("connecting the running task to the non-attachable overlay %s once the node holds it: %v", nonatt, nonErr)
	t.Logf("S5 outcome: a connect to an attachable overlay is lost on replacement and refused on a non-attachable one on engine %s", sw.Engine)
}
