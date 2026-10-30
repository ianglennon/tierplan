// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/registry"
)

// sharedImagePullFanOut pulls an image on every node of a real swarm of three
// managers and a worker with an agent on each (plan 05-03, RES-09, IMG-02, D-25, P4
// D-13): the default targets are all four nodes, each pulls from a registry inside the
// test network and ends Done with the same digest, and every node has the image. Then
// the worker's agent is revoked, which leaves the node in the swarm and out of
// Coaming's reach: a second pull fails that node alone with node_unreachable while the
// other three are Done, and every audit row of the job carries the job ID as its bulk.
func sharedImagePullFanOut(t *testing.T, f *swarmFix) {
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

// sharedPrivatePullEveryNode pulls an image from a registry that requires a sign-in on
// every node of a real swarm of three managers and a worker, with one stored credential and
// no docker login anywhere (plan 05-09, IMG-02, D-25, D-27): the job ends Done on all four
// nodes and none holds a Docker client configuration. Then the header the resolver makes
// creates a service pinned to the worker, after the worker's copy of the image is removed, and
// its task reaches running there, which only works if the engine carried the credential from
// the create to a node that never signed in. The raw service inspect holds neither the
// password nor the encoded header.
func sharedPrivatePullEveryNode(t *testing.T, f *swarmFix) {
	const user, password = "bot", "registry-pass-0123456789"
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{Htpasswd: dockertest.HtpasswdFor(user, password)})
	ref := addr + "/private/busybox:1"
	mgr := f.sw.Managers[0]
	pushed := dockertest.PushImage(t, mgr, "busybox:1.36.1", ref, &dockertest.Login{User: user, Password: password})
	// Signed in for the push only. Nobody has the image locally: every node must fetch it.
	dockertest.Logout(t, mgr)
	for _, n := range f.sw.Nodes() {
		sh(t, n, "docker image rm -f busybox:1.36.1 "+ref+" >/dev/null 2>&1 || true")
	}
	f.install(t)
	f.waitSwarmOnline(t, 4)

	ctx := context.Background()
	creds := registry.NewStore(f.srv.Core().Store(), f.srv.Core().Vault(), nil)
	cred, err := creds.Create(ctx, registry.CreateInput{
		Name: "private-bot", Type: registry.TypeGeneric, Host: addr, Prefix: "private", Username: user, Secret: password,
	}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	job := pullAndWait(t, f.srv, f.env.ID, ref, nil)
	if job.State != "done" || len(job.Items) != 4 {
		t.Fatalf("job = %+v, want done with an item for each of the four nodes", job)
	}
	for _, it := range job.Items {
		if it.State != "done" || it.Digest != pushed {
			t.Errorf("item %+v, want done with the digest %s", it, pushed)
		}
	}
	for _, r := range f.srv.AuditRows("image.pulled") {
		if r.Result != "success" || r.Details["credential"] != "private-bot" {
			t.Errorf("audit row %+v, want a success naming the credential", r)
		}
	}
	for _, n := range f.sw.Nodes() {
		if id := strings.TrimSpace(sh(t, n, "docker image inspect -f '{{.Id}}' "+ref)); !strings.HasPrefix(id, "sha256:") {
			t.Errorf("%s does not have %s after the pull: %q", n.Hostname, ref, id)
		}
		if dockertest.HasClientConfig(t, n) {
			t.Errorf("%s has a Docker client configuration after the pull: Coaming must not log in", n.Hostname)
		}
	}

	// A service created with the resolver's header runs on a worker that never logged in. The
	// worker's copy goes first, so the task has to pull with what the engine carries.
	parsed, err := registry.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	header, used, err := f.srv.Core().Resolver().AuthFor(ctx, f.env.ID, parsed)
	if err != nil || used == nil || used.ID != cred.ID || header == "" {
		t.Fatalf("AuthFor = %q, %+v, %v, want the stored credential", header, used, err)
	}
	worker := f.sw.Workers[0]
	sh(t, worker, "docker image rm -f "+ref)
	id := dockertest.CreateServiceWithAuth(t, mgr, "cm-private", ref, header)
	dockertest.Wait(t, 3*time.Minute, "the task to run on the worker", func() (bool, string) {
		out, err := mgr.Sh(ctx, "docker service ps --filter desired-state=running --format '{{.Node}} {{.CurrentState}}' "+id)
		if err != nil {
			return false, err.Error()
		}
		return strings.HasPrefix(strings.TrimSpace(out), worker.Hostname+" Running"), strings.TrimSpace(out)
	})
	if got := strings.TrimSpace(sh(t, worker, "docker image inspect -f '{{.Id}}' "+ref)); !strings.HasPrefix(got, "sha256:") {
		t.Errorf("the worker does not have %s after the service started: %q", ref, got)
	}
	if raw := dockertest.InspectServiceRaw(t, mgr, id); strings.Contains(raw, password) || strings.Contains(raw, header) {
		t.Errorf("service inspect holds the password or the encoded header")
	}
	for _, n := range f.sw.Nodes() {
		if dockertest.HasClientConfig(t, n) {
			t.Errorf("%s has a Docker client configuration after the service: nothing may log in", n.Hostname)
		}
	}
}
