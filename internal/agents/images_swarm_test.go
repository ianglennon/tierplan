// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// TestSwarmImagePullFanOut pulls an image on every node of a real swarm of three
// managers and a worker with an agent on each (plan 05-03, RES-09, IMG-02, D-25, P4
// D-13): the default targets are all four nodes, each pulls from a registry inside the
// test network and ends Done with the same digest, and every node has the image. Then
// the worker's agent is revoked, which leaves the node in the swarm and out of
// Bayplan's reach: a second pull fails that node alone with node_unreachable while the
// other three are Done, and every audit row of the job carries the job ID as its bulk.
func TestSwarmImagePullFanOut(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	registry := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	first, second := registry+"/public/busybox:1", registry+"/public/busybox:2"
	pushed := dockertest.PushImage(t, f.sw.Managers[0], "busybox:1.36.1", first, nil)
	if again := dockertest.PushImage(t, f.sw.Managers[0], "busybox:1.36.1", second, nil); again != pushed {
		t.Fatalf("the same image pushed under two tags has digests %s and %s", pushed, again)
	}
	// Nobody has the image locally: every node must fetch it through the agent.
	for _, n := range f.sw.Nodes() {
		sh(t, n, "docker image rm -f busybox:1.36.1 "+first+" "+second+" >/dev/null 2>&1 || true")
	}
	f.install(t)
	f.waitSwarmOnline(t, 4)

	job := pullAndWait(t, f.srv, f.env.ID, first, nil)
	if job.State != "done" || len(job.Items) != 4 {
		t.Fatalf("job = %+v, want done with an item for each of the four nodes", job)
	}
	seen := map[string]bool{}
	for _, it := range job.Items {
		if it.State != "done" || it.Digest != pushed {
			t.Errorf("item %+v, want done with the digest %s", it, pushed)
		}
		seen[it.Name] = true
	}
	for _, n := range f.sw.Nodes() {
		if !seen[n.Hostname] {
			t.Errorf("no item for %s: %+v", n.Hostname, job.Items)
		}
		if id := strings.TrimSpace(sh(t, n, "docker image inspect -f '{{.Id}}' "+first)); !strings.HasPrefix(id, "sha256:") {
			t.Errorf("%s does not have %s after the pull: %q", n.Hostname, first, id)
		}
	}
	rows := f.srv.AuditRows("image.pulled")
	if len(rows) != 4 {
		t.Fatalf("audit rows after the first pull = %d, want one per node", len(rows))
	}
	for _, r := range rows {
		if r.Result != "success" || r.Details["bulk"] != job.ID || r.Details["digest"] != pushed {
			t.Errorf("audit row %+v, want a success with the job ID %s as its bulk", r, job.ID)
		}
	}

	// The node list the chooser reads names every node, reachable.
	targets := func() (nodes []struct {
		Name      string `json:"name"`
		Reachable bool   `json:"reachable"`
	}) {
		t.Helper()
		code, raw := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/images/pull-targets", nil)
		if code != http.StatusOK {
			t.Fatalf("GET pull-targets = %d %s", code, raw)
		}
		var out struct {
			Swarm bool `json:"swarm"`
			Nodes []struct {
				Name      string `json:"name"`
				Reachable bool   `json:"reachable"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || !out.Swarm {
			t.Fatalf("pull-targets %s: swarm=%v err=%v", raw, out.Swarm, err)
		}
		return out.Nodes
	}
	if nodes := targets(); len(nodes) != 4 {
		t.Fatalf("targets = %+v, want the four nodes", nodes)
	}

	// Take the worker out of reach without taking it out of the swarm.
	worker := f.sw.Workers[0]
	agent, ok := f.agentOn(worker)
	if !ok {
		t.Fatalf("no agent row for %s", worker.Hostname)
	}
	if code, body := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/agents/"+agent.ID+"/revoke", map[string]string{"reason": "fan-out test"}); code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", code, body)
	}
	f.srv.WaitFor("the worker to be listed unreachable", 90*time.Second, func() (bool, string) {
		nodes := targets()
		for _, n := range nodes {
			if n.Name == worker.Hostname {
				return !n.Reachable, "reachable"
			}
		}
		return false, "not listed"
	})

	job = pullAndWait(t, f.srv, f.env.ID, second, nil)
	if job.State != "partial" || len(job.Items) != 4 {
		t.Fatalf("second job = %+v, want partial with an item for each of the four nodes", job)
	}
	for _, it := range job.Items {
		if it.Name == worker.Hostname {
			if it.State != "failed" || it.Code != "node_unreachable" {
				t.Errorf("the worker's item = %+v, want failed with node_unreachable", it)
			}
			continue
		}
		if it.State != "done" || it.Digest != pushed {
			t.Errorf("item %+v, want done with the digest %s", it, pushed)
		}
	}
	var bulk int
	for _, r := range f.srv.AuditRows("image.pulled") {
		if r.Details["bulk"] == job.ID {
			bulk++
		}
	}
	if bulk != 4 {
		t.Errorf("audit rows of the second job = %d, want one per node, all with the job ID as their bulk", bulk)
	}
	if id := strings.TrimSpace(sh(t, worker, "docker image inspect -f '{{.Id}}' "+second+" 2>/dev/null || echo none")); id != "none" {
		t.Errorf("the unreachable worker has %s (%s): nothing should have reached it", second, id)
	}
}
