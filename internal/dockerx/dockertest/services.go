// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// ServiceTestImage is the small image the service helpers run. A test loads it into
// every node that may run a task (LoadImage), so no node reaches a public registry.
const ServiceTestImage = "busybox:1.36.1"

// CreateCrashLoopService creates a one-replica service whose task prints a line and
// exits 1, and which the swarm restarts after a second, for ever: a restart loop. The
// image is the one ServiceTestImage names, which every node that can run the task must
// already have. It returns the service ID. constraints narrow where the task may run.
func CreateCrashLoopService(t testing.TB, manager *Node, name string, constraints ...string) string {
	t.Helper()
	one := uint64(1)
	delay := time.Second
	return createService(t, manager, swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   ServiceTestImage,
				Command: []string{"sh", "-c", "echo crashing on purpose; exit 1"},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny, Delay: &delay},
			Placement:     placement(constraints),
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
	})
}

// CreateLoggingService creates a service of replicas tasks that each print a line
// every 200 ms: "slot=<slot> n=<counter>", the slot coming from the swarm's own task
// template, so a merged log shows which task wrote what. The image is the one
// ServiceTestImage names. It returns the service ID. constraints narrow where the
// tasks may run.
func CreateLoggingService(t testing.TB, manager *Node, name string, replicas int, constraints ...string) string {
	t.Helper()
	if replicas < 1 {
		t.Fatalf("a logging service needs at least one replica, got %d", replicas)
	}
	n := uint64(replicas) //nolint:gosec // G115: checked to be at least 1 just above
	grace := time.Second
	return createService(t, manager, swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   ServiceTestImage,
				Env:     []string{"SLOT={{.Task.Slot}}"},
				Command: []string{"sh", "-c", "i=0; while true; do echo \"slot=$SLOT n=$i\"; i=$((i+1)); sleep 0.2; done"},
				// A shell as PID 1 ignores SIGTERM: without this a replaced task takes ten seconds to stop.
				StopGracePeriod: &grace,
			},
			Placement: placement(constraints),
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &n}},
	})
}

func placement(constraints []string) *swarm.Placement {
	if len(constraints) == 0 {
		return nil
	}
	return &swarm.Placement{Constraints: constraints}
}

// createService creates the service through the manager's own client and returns its ID.
// The image is not resolved: it is the tag a node already has.
func createService(t testing.TB, manager *Node, spec swarm.ServiceSpec) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := manager.Client(t).ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec})
	if err != nil {
		t.Fatalf("create the service %s on %s: %v", spec.Name, manager.Hostname, err)
	}
	return res.ID
}

// WaitServiceUpdateState polls the service's UpdateStatus.State through the manager until
// it is state (updating, completed, rollback_started, rollback_completed, ...), failing the
// test with the last state and message seen when timeout passes first.
func WaitServiceUpdateState(t testing.TB, manager *Node, id, state string, timeout time.Duration) {
	t.Helper()
	Wait(t, timeout, "service "+id+" to reach the update state "+state, func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := manager.Client(t).ServiceInspect(ctx, id, client.ServiceInspectOptions{})
		if err != nil {
			return false, err.Error()
		}
		u := res.Service.UpdateStatus
		if u == nil {
			return false, "no UpdateStatus yet"
		}
		return string(u.State) == state, fmt.Sprintf("%s: %s", u.State, u.Message)
	})
}

// WaitRunningTasks polls until exactly want tasks of the service are running now (desired
// state running and current state running), and returns them. The failure message lists
// every task of the service with its state and error.
func WaitRunningTasks(t testing.TB, manager *Node, serviceID string, want int, timeout time.Duration) []swarm.Task {
	t.Helper()
	var running []swarm.Task
	Wait(t, timeout, fmt.Sprintf("%d running tasks of service %s", want, serviceID), func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		filters := make(client.Filters)
		filters.Add("service", serviceID)
		res, err := manager.Client(t).TaskList(ctx, client.TaskListOptions{Filters: filters})
		if err != nil {
			return false, err.Error()
		}
		running = running[:0]
		var seen []string
		for _, tk := range res.Items {
			seen = append(seen, fmt.Sprintf("slot %d %s->%s %s", tk.Slot, tk.Status.State, tk.DesiredState, tk.Status.Err))
			if tk.DesiredState == swarm.TaskStateRunning && tk.Status.State == swarm.TaskStateRunning {
				running = append(running, tk)
			}
		}
		return len(running) == want, strings.Join(seen, "; ")
	})
	return running
}

// PushVariant makes an image that differs from base by one file, on the node, and pushes
// it as target, signing in first when login is given. Pushing it under a tag that already
// holds another image moves the tag: the registry answers a new digest, which is what a
// maintainer re-pushing a tag does. marker is the file's content, so two variants of one
// base differ. It returns the digest the registry answered with.
func PushVariant(t testing.TB, node *Node, base, target, marker string, login *Login) string {
	t.Helper()
	LoadImage(t, base, node)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := "bp-variant-" + randHex(3)
	if out, err := node.Sh(ctx, fmt.Sprintf("docker run --name %s %s sh -c 'echo %s > /bp-marker' && docker commit %s %s && docker rm %s", name, base, marker, name, target, name)); err != nil {
		t.Fatalf("make a variant of %s on %s: %v\n%s", base, node.Hostname, err, out)
	}
	return pushTagged(t, node, target, login)
}
