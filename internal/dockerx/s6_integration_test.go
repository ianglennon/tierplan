// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The two environment strings spike S6 (D-01) puts in a global service's spec. Phase 24's deployer writes the same
// bytes, and 22-S6-RESULT.md quotes them: swarmkit expands the placeholders per task on the node that runs it, so the
// spec itself is node-independent.
const (
	s6NodeIDEnv   = "TIERPLAN_INGRESS_NODE_ID={{.Node.ID}}"
	s6NodeNameEnv = "TIERPLAN_INGRESS_NODE_NAME={{.Node.Hostname}}"
)

// TestS6NodeIDTemplateExpandsOnEachNode is spike S6: a global service created through the Engine API on a swarm of one
// manager and one worker, whose env holds {{.Node.ID}} and {{.Node.Hostname}}, runs one task per node, and the task
// container on each node sees that node's own ID and hostname as its engine reports them. The service spec read back
// keeps the literal templates. The engine is TIERPLAN_TEST_ENGINE, so every CI leg tests its own engine.
func TestS6NodeIDTemplateExpandsOnEachNode(t *testing.T) {
	sw := dockertest.StartSwarm(t, "", 1, 1)
	nodes := sw.Nodes()
	dockertest.LoadImage(t, dockertest.ServiceTestImage, nodes...)
	manager := sw.Managers[0]
	cli := manager.Client(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	grace := time.Second
	const name = "s6-node-id"
	created, err := cli.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:   dockertest.ServiceTestImage,
			Command: []string{"sleep", "3600"},
			Env:     []string{s6NodeIDEnv, s6NodeNameEnv},
			// sleep as PID 1 ignores SIGTERM: without this a removed task takes ten seconds to stop.
			StopGracePeriod: &grace,
		}},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
	}})
	if err != nil {
		t.Fatalf("create the service %s on %s: %v", name, manager.Hostname, err)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
		defer rcancel()
		_, _ = cli.ServiceRemove(rctx, created.ID, client.ServiceRemoveOptions{})
	})

	type observed struct {
		host, wantID, wantName, gotID, gotName string
	}
	var seen []observed
	for _, n := range nodes {
		info, err := n.Info(ctx, t)
		if err != nil {
			t.Fatal(err)
		}
		var ctr string
		dockertest.Wait(t, 3*time.Minute, "the task container of "+name+" on "+n.Hostname, func() (bool, string) {
			out, err := n.Exec(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.swarm.service.name="+name)
			if err != nil {
				return false, err.Error()
			}
			ids := strings.Fields(out)
			if len(ids) != 1 {
				return false, "containers: " + strings.TrimSpace(out)
			}
			ctr = ids[0]
			return true, ""
		})
		gotID, err := n.Exec(ctx, "docker", "exec", ctr, "printenv", "TIERPLAN_INGRESS_NODE_ID")
		if err != nil {
			t.Fatal(err)
		}
		gotName, err := n.Exec(ctx, "docker", "exec", ctr, "printenv", "TIERPLAN_INGRESS_NODE_NAME")
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, observed{n.Hostname, info.NodeID, info.Name, strings.TrimSpace(gotID), strings.TrimSpace(gotName)})
	}

	expanded := true
	for _, o := range seen {
		t.Logf("engine %s node %s: node ID %s, engine name %s; task saw TIERPLAN_INGRESS_NODE_ID=%q TIERPLAN_INGRESS_NODE_NAME=%q",
			sw.Engine, o.host, o.wantID, o.wantName, o.gotID, o.gotName)
		if o.gotID != o.wantID || o.gotName != o.wantName || strings.Contains(o.gotID+o.gotName, "{") {
			expanded = false
		}
	}
	if len(seen) != 2 || seen[0].gotID == seen[1].gotID || seen[0].gotName == seen[1].gotName {
		expanded = false
	}
	if !expanded {
		t.Fatalf("S6 outcome: not expanded on engine %s: %+v", sw.Engine, seen)
	}

	// A2: the spec keeps the template, the task container holds the expansion.
	res, err := cli.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	env := res.Service.Spec.TaskTemplate.ContainerSpec.Env
	if want := []string{s6NodeIDEnv, s6NodeNameEnv}; !slices.Equal(env, want) {
		t.Fatalf("the service spec env = %q, want the literal templates %q", env, want)
	}
	t.Logf("S6 outcome: expanded on engine %s", sw.Engine)
}
