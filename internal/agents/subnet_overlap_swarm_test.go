// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The real-swarm proof of UAT gap G-10-8 (plans 10-28, 10-29, 10-31, 10-32): a swarm's allocator
// accepts a network whose subnet overlaps another swarm network and then never gives it addresses,
// so a service on it never starts. TierPlan refuses that overlap whatever createAnyway says, reads
// every swarm network it creates back, and removes one the swarm never allocated. Each answer
// below is held against what the managers themselves list.

// overlapFinding is a finding of the subnet check, as far as this case reads it.
type overlapFinding struct {
	Kind       string `json:"kind"`
	Network    string `json:"network"`
	NoOverride bool   `json:"noOverride"`
}

// managersHave reports whether any manager lists a network called name, and the IDs it found.
func managersHave(t *testing.T, f *swarmFix, name string) (bool, string) {
	t.Helper()
	var found []string
	for _, m := range f.sw.Managers {
		for _, id := range networkIDs(t, m, name) {
			found = append(found, m.Hostname+":"+id)
		}
	}
	return len(found) > 0, strings.Join(found, ",")
}

// requireNoNetwork waits until no manager lists a network called name. A refused create removes
// its stub before it answers, but a manager may still list it for a moment.
func requireNoNetwork(t *testing.T, f *swarmFix, name string) {
	t.Helper()
	f.srv.WaitFor("no network "+name+" on any manager", 30*time.Second, func() (bool, string) {
		have, found := managersHave(t, f, name)
		return !have, found
	})
}

// sharedSubnetOverlap holds the override rule, Docker's immediate refusal and the read-back on a
// real swarm of three managers and a worker, on the network route, service create and a stack
// deploy. It uses subnets of 10.231.0.0/16 to 10.234.0.0/16, far from the first blocks of the
// swarm's default pool.
func sharedSubnetOverlap(t *testing.T, f *swarmFix) {
	dockertest.LoadImage(t, "busybox", f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	env := "/api/env/" + f.env.ID
	networks := env + "/networks"

	// b. A first overlay, and the subnet check's view of it.
	if code, raw := f.srv.Do(http.MethodPost, networks, map[string]any{"name": "cm-ov-a", "driver": "overlay", "subnet": "10.231.0.0/24"}); code != http.StatusCreated {
		t.Fatalf("create cm-ov-a: %d %s", code, raw)
	}
	f.srv.WaitFor("the subnet check to name cm-ov-a with noOverride", 60*time.Second, func() (bool, string) {
		code, raw := f.srv.Do(http.MethodPost, env+"/address-space/subnet-check", map[string]any{"subnet": "10.231.0.0/25", "driver": "overlay"})
		if code != http.StatusOK {
			return false, fmt.Sprintf("%d %s", code, raw)
		}
		var out struct {
			Findings []overlapFinding `json:"findings"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return false, string(raw)
		}
		for _, fd := range out.Findings {
			if fd.Kind == "same_env" && fd.Network == "cm-ov-a" && fd.NoOverride {
				return true, ""
			}
		}
		return false, string(raw)
	})

	// c. An overlay over it is refused even with createAnyway, and nothing is left behind.
	code, raw := f.srv.Do(http.MethodPost, networks, map[string]any{"name": "cm-ov-b", "driver": "overlay", "subnet": "10.231.0.0/25", "createAnyway": true})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "subnet_overlap_swarm") {
		t.Fatalf("an overlay over an overlay with createAnyway: %d %s, want 422 subnet_overlap_swarm", code, raw)
	}
	requireNoNetwork(t, f, "cm-ov-b")

	// d. A staged network of a new service follows the same rule, before any image lookup.
	spec := quickServiceSpec("cm-svc-ov", dockertest.ServiceTestImage, 1)
	spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "cm-ov-staged"}}
	code, raw = f.srv.Do(http.MethodPost, env+"/services", map[string]any{
		"spec": spec,
		"objects": []map[string]any{{
			"kind": "network", "name": "cm-ov-staged", "subnet": "10.231.0.64/26", "createAnyway": true,
		}},
	})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "subnet_overlap_swarm") {
		t.Fatalf("a staged overlay over an overlay with createAnyway: %d %s, want 422 subnet_overlap_swarm", code, raw)
	}
	requireNoNetwork(t, f, "cm-ov-staged")
	if out := strings.TrimSpace(sh(t, mgr, "docker service ls --filter name=cm-svc-ov --format '{{.Name}}'")); out != "" {
		t.Errorf("the refused create left a service: %q", out)
	}

	// e. A bridge on the worker over the overlay can be created anyway.
	var workerAgent string
	for _, a := range f.srv.Agents(f.env.ID) {
		if a.Node == worker.Hostname {
			workerAgent = a.ID
		}
	}
	if workerAgent == "" {
		t.Fatalf("no agent for the worker %s", worker.Hostname)
	}
	bridge := map[string]any{"name": "cm-br-x", "driver": "bridge", "nodeId": workerAgent, "subnet": "10.231.0.0/26", "createAnyway": true}
	if code, raw := f.srv.Do(http.MethodPost, networks, bridge); code != http.StatusCreated {
		t.Fatalf("a worker bridge over the overlay with createAnyway: %d %s, want 201", code, raw)
	}
	if got := strings.TrimSpace(sh(t, worker, "docker network inspect -f '{{.Driver}} {{range .IPAM.Config}}{{.Subnet}}{{end}}' cm-br-x")); got != "bridge 10.231.0.0/26" {
		t.Errorf("cm-br-x on the worker = %q, want %q", got, "bridge 10.231.0.0/26")
	}

	// f. A second bridge on the same subnet on the same worker is refused by Docker at once.
	bridge["name"] = "cm-br-y"
	code, raw = f.srv.Do(http.MethodPost, networks, bridge)
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "subnet_refused") {
		t.Fatalf("a second bridge on one subnet: %d %s, want 422 subnet_refused", code, raw)
	}
	if ids := networkIDs(t, worker, "cm-br-y"); len(ids) != 0 {
		t.Errorf("the refused bridge exists on the worker: %v", ids)
	}

	// g. The read-back itself, in both directions, outside TierPlan's overlap check.
	cli := dockerxFor(t, mgr)
	ctx := execCtx(t, 5*time.Minute)
	_, _, err := cli.NetworkCreateAllocated(ctx, dockerx.NetworkCreateSpec{Name: "cm-raw-stub", Driver: "overlay", Scope: dockerx.ScopeSwarm, Subnet: "10.231.0.128/25"}, dockerx.AllocationWait{})
	var na *dockerx.NotAllocatedError
	if !errors.As(err, &na) || !na.Removed {
		t.Fatalf("an overlay over cm-ov-a created straight through dockerx: %v, want a NotAllocatedError that was removed", err)
	}
	requireNoNetwork(t, f, "cm-raw-stub")
	okID, _, err := cli.NetworkCreateAllocated(ctx, dockerx.NetworkCreateSpec{Name: "cm-raw-ok", Driver: "overlay", Scope: dockerx.ScopeSwarm, Subnet: "10.232.0.0/24"}, dockerx.AllocationWait{})
	if err != nil {
		t.Fatalf("a free overlay through dockerx: %v", err)
	}
	if err := cli.NetworkRemove(ctx, okID); err != nil {
		t.Errorf("remove cm-raw-ok: %v", err)
	}

	// h. A network TierPlan cannot yet see: either its view already holds it and says
	// subnet_overlap_swarm, or the read-back catches the stub and says network_not_allocated.
	if _, _, err := cli.NetworkCreate(ctx, dockerx.NetworkCreateSpec{Name: "cm-raw-c", Driver: "overlay", Scope: dockerx.ScopeSwarm, Subnet: "10.233.0.0/24"}); err != nil {
		t.Fatalf("create cm-raw-c: %v", err)
	}
	code, raw = f.srv.Do(http.MethodPost, networks, map[string]any{"name": "cm-ov-d", "driver": "overlay", "subnet": "10.233.0.0/25", "createAnyway": true})
	if code != http.StatusUnprocessableEntity || (problemType(raw) != "subnet_overlap_swarm" && problemType(raw) != "network_not_allocated" &&
		!strings.Contains(string(raw), "subnet_overlap_swarm") && !strings.Contains(string(raw), "network_not_allocated")) {
		t.Fatalf("an overlay over a network TierPlan has not read: %d %s, want 422 subnet_overlap_swarm or network_not_allocated", code, raw)
	}
	t.Logf("an overlay over a network TierPlan has not read: 422 %s", problemType(raw))
	requireNoNetwork(t, f, "cm-ov-d")

	// i. A stack whose network overlaps the overlay fails at the networks step, with nothing left.
	// The agents are already installed and online (stackEnvReady would install them again).
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	digest := dockertest.PushImage(t, mgr, dockertest.ServiceTestImage, addr+"/subnetovl/app:1", nil)
	image := addr + "/subnetovl/app@" + digest
	const stack = "cmovl"
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	id := f.createStackTierPlan(t, stack, fmt.Sprintf(subnetOverlapComposeFmt, image))
	job := f.deployStackUntilEnded(t, id)
	rawJob, _ := json.Marshal(job)
	t.Logf("the stack deploy job: %s", rawJob)
	if job.State == "done" {
		t.Fatalf("the deploy of a stack over an existing overlay ended done: %s", rawJob)
	}
	if nets := job.item(t, "networks"); nets.State != "failed" || nets.Code != "network_not_allocated" {
		t.Errorf("the networks step = state %q code %q, want failed and network_not_allocated: %s", nets.State, nets.Code, rawJob)
	}
	requireNoNetwork(t, f, stack+"_net")
	if out := strings.TrimSpace(sh(t, mgr, "docker service ls --filter label=com.docker.stack.namespace="+stack+" --format '{{.Name}}'")); out != "" {
		t.Errorf("the failed deploy left services of the stack: %q", out)
	}
	f.awaitStackUnlocked(t, id)

	// What the case made and left: the swarm reset removes the rest.
	cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, line := range []struct {
		n    *dockertest.Node
		line string
	}{{mgr, "docker network rm cm-ov-a cm-raw-c"}, {worker, "docker network rm cm-br-x"}} {
		if out, err := line.n.Sh(cleanup, line.line); err != nil {
			t.Logf("clean up (%s): %v\n%s", line.line, err, out)
		}
	}
}

// subnetOverlapComposeFmt is a stack whose network declares a subnet inside cm-ov-a's. The one
// argument is the image.
const subnetOverlapComposeFmt = `services:
  app:
    image: %s
    command: ["sleep", "3600"]
    networks: [net]
networks:
  net:
    ipam:
      config:
        - subnet: 10.231.0.192/26
`
