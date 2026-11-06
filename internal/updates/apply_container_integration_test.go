//go:build integration

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestPlainContainerRecreateRealEngine updates a docker run container on a real engine (plan 12-20, IMG-06, D-07, D-12, T-12-56,
// T-12-57, RESEARCH Pitfall 11): the environment is a local socket (a proxy to a docker dind container), the image is in a
// registry of the node's own and moves from one build to another under one tag. The container has two user networks (one with an
// alias and a static address), an anonymous volume holding a file, a named volume, a restart policy, a label and a variable. The
// update must bring it back under the same name with all of that, on the new image, with the old container gone and its volume's
// file still there; and a new build whose command exits at once must leave the old container running as it was.
func TestPlainContainerRecreateRealEngine(t *testing.T) {
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/tp/app:1"
	digestA := dockertest.PushVariantCmd(t, node, dockertest.ServiceTestImage, image, "A", `["sleep","3600"]`, nil)

	ctx := testsupport.Context(t)
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	docker := func(args ...string) string {
		t.Helper()
		out, err := node.Exec(ctx, append([]string{"docker"}, args...)...)
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(out)
	}
	// containerNames are the names of the containers on the node but the registry's own, sorted.
	containerNames := func() []string {
		t.Helper()
		var names []string
		for _, n := range strings.Fields(docker("ps", "-a", "--format", "{{.Names}}")) {
			if n != "tp-registry" {
				names = append(names, n)
			}
		}
		slices.Sort(names)
		return names
	}
	// The running image is known by its digest even after the tag moves: the node holds a name for it by digest.
	docker("pull", image+"@"+digestA)
	docker("network", "create", "--subnet", "10.211.1.0/24", "tp-n1")
	docker("network", "create", "tp-n2")
	docker("run", "-d", "--name", "app", "--network", "tp-n1", "--ip", "10.211.1.50", "--network-alias", "web", "--label", "team=core",
		"-e", "MODE=prod", "--restart", "unless-stopped", "--stop-timeout", "2", "-v", "/cache", "-v", "appdata:/data", image)
	docker("network", "connect", "--alias", "backend", "tp-n2", "app")
	docker("exec", "app", "sh", "-c", "echo marker > /cache/marker && echo data > /data/file")

	before, err := cli.ContainerInspectFull(ctx, "app")
	if err != nil {
		t.Fatalf("inspect the container before: %v", err)
	}
	if before.State == nil || !before.State.Running {
		t.Fatalf("the container is not running before the update: %+v", before.State)
	}

	r := newApplyRigOn(t, "unix://"+dockertest.UnixProxy(t, node.Host()), nil)
	r.applier.Clock = stackops.RealClock()
	r.applier.ConvergeTimeout = time.Minute
	// The tag moves under the running container, so its list row shows an image ID and its image is named by an inspect, as on a
	// server.
	r.applier.Service.ImageTexts = &updates.ImageTextCache{Inspect: updates.InspectImageTexts(r.applier.Aggregator)}
	// A forced check is satisfied by one completed since it began, so the checker's clock must move between two updates.
	var ticks atomic.Int64
	r.applier.Service.Checker.Now = func() time.Time { return t0.Add(time.Duration(ticks.Add(1)) * time.Minute) }
	headKey := image
	update := func(digest string) (jobs.Job, updates.Run) {
		t.Helper()
		r.heads.set(headKey, answer{digest: digest})
		// The cached read of the node is from before the containers existed.
		if _, _, err := r.applier.Aggregator.FreshSnapshot(ctx, r.envID); err != nil {
			t.Fatalf("read the node: %v", err)
		}
		req := r.containerRequest("app", digest)
		id, err := r.applier.Start(ctx, req)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		job, err := r.hub.Wait(ctx, id)
		if err != nil {
			t.Fatalf("waiting for the job: %v", err)
		}
		return job, r.runRow(t, job)
	}
	describe := func(job jobs.Job) string {
		var b strings.Builder
		for _, it := range job.Items {
			b.WriteString(it.ID + "=" + it.State + "(" + it.Code + ") ")
		}
		return b.String()
	}

	// 1. The tag moves to a good build; the container is updated.
	digestB := dockertest.PushVariantCmd(t, node, dockertest.ServiceTestImage, image, "B", `["sleep","3600"]`, nil)
	if digestB == digestA {
		t.Fatalf("the two builds have one digest %s", digestA)
	}
	job, run := update(digestB)
	if job.State != jobs.JobDone || run.State != updates.RunDone {
		t.Fatalf("the update: job %s run %s %q steps %s", job.State, run.State, run.Reason, describe(job))
	}
	after, err := cli.ContainerInspectFull(ctx, "app")
	if err != nil {
		t.Fatalf("inspect the container after: %v", err)
	}
	if after.ID == before.ID || run.OldContainer != before.ID {
		t.Errorf("container %s after, %s before, run recorded %s: want a new container and the old one recorded", after.ID, before.ID, run.OldContainer)
	}
	if after.State == nil || !after.State.Running {
		t.Errorf("the replacement is not running: %+v", after.State)
	}
	if after.Config.Image != image+"@"+digestB {
		t.Errorf("image = %q, want the tag pinned to the new digest", after.Config.Image)
	}
	if d, _, err := cli.ImageDetail(ctx, after.Image); err != nil || !slices.Contains(d.RepoDigests, registry+"/tp/app@"+digestB) {
		t.Errorf("the replacement's image = %+v %v, want the new digest among its repo digests", d, err)
	}
	if after.Config.Labels["team"] != "core" || !slices.Contains(after.Config.Env, "MODE=prod") || !slices.ContainsFunc(after.Config.Env, func(e string) bool { return strings.HasPrefix(e, "PATH=") }) {
		t.Errorf("labels %v env %v, want the label and the variable kept, and the image's own PATH", after.Config.Labels, after.Config.Env)
	}
	if after.HostConfig.RestartPolicy.Name != container.RestartPolicyUnlessStopped || after.Config.StopTimeout == nil || *after.Config.StopTimeout != 2 {
		t.Errorf("restart policy %q stop timeout %v, want unless-stopped and 2", after.HostConfig.RestartPolicy.Name, after.Config.StopTimeout)
	}
	n1, n2 := after.NetworkSettings.Networks["tp-n1"], after.NetworkSettings.Networks["tp-n2"]
	if n1 == nil || n2 == nil {
		t.Fatalf("networks after = %v, want tp-n1 and tp-n2", after.NetworkSettings.Networks)
	}
	if n1.IPAddress.String() != "10.211.1.50" || !slices.Contains(n1.Aliases, "web") || !slices.Contains(n2.Aliases, "backend") {
		t.Errorf("tp-n1 address %s aliases %v, tp-n2 aliases %v, want the static address and both aliases", n1.IPAddress, n1.Aliases, n2.Aliases)
	}
	for name, aliases := range map[string][]string{"tp-n1": n1.Aliases, "tp-n2": n2.Aliases} {
		if slices.Contains(aliases, before.ID[:12]) {
			t.Errorf("%s aliases %v carry the old container's ID", name, aliases)
		}
	}
	if len(n2.MacAddress) == 0 || n2.MacAddress.String() == before.NetworkSettings.Networks["tp-n2"].MacAddress.String() && n2.IPAddress != before.NetworkSettings.Networks["tp-n2"].IPAddress {
		t.Errorf("tp-n2 MAC %s with address %s (was %s with %s), want a MAC that goes with the new address",
			n2.MacAddress, n2.IPAddress, before.NetworkSettings.Networks["tp-n2"].MacAddress, before.NetworkSettings.Networks["tp-n2"].IPAddress)
	}
	if n1.MacAddress.String() != before.NetworkSettings.Networks["tp-n1"].MacAddress.String() {
		t.Errorf("tp-n1 MAC %s, want %s: a static address gives the same MAC", n1.MacAddress, before.NetworkSettings.Networks["tp-n1"].MacAddress)
	}
	if got := docker("exec", "app", "cat", "/cache/marker"); got != "marker" {
		t.Errorf("the anonymous volume's file = %q, want marker: the data stayed", got)
	}
	if got := docker("exec", "app", "cat", "/data/file"); got != "data" {
		t.Errorf("the named volume's file = %q, want data", got)
	}
	if names := containerNames(); !slices.Equal(names, []string{"app"}) {
		t.Errorf("containers after the update = %v, want app alone: no old or temporary container is left", names)
	}
	if _, err := cli.ContainerInspectFull(ctx, before.ID); !dockerx.IsNotFound(err) {
		t.Errorf("inspect of the old container = %v, want not found", err)
	}

	// 2. The tag moves to a build whose command exits at once; the update fails and the container stays as it was.
	digestC := dockertest.PushVariantCmd(t, node, dockertest.ServiceTestImage, image, "C", "", nil)
	job, run = update(digestC)
	if job.State == jobs.JobDone || run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart {
		t.Fatalf("the bad update: job %s run %s %q steps %s, want rolled_back didnt_start", job.State, run.State, run.Reason, describe(job))
	}
	kept, err := cli.ContainerInspectFull(ctx, "app")
	if err != nil {
		t.Fatalf("inspect the container after the failure: %v", err)
	}
	if kept.ID != after.ID || kept.State == nil || !kept.State.Running || kept.Config.Image != after.Config.Image {
		t.Errorf("after the failure: container %s running %v image %q, want %s running on %q as before", kept.ID, kept.State != nil && kept.State.Running, kept.Config.Image, after.ID, after.Config.Image)
	}
	if got := docker("exec", "app", "cat", "/cache/marker"); got != "marker" {
		t.Errorf("the anonymous volume's file after the failure = %q, want marker", got)
	}
	if names := containerNames(); !slices.Equal(names, []string{"app"}) {
		t.Errorf("containers after the failure = %v, want app alone", names)
	}
	if k := kept.NetworkSettings.Networks["tp-n1"]; k == nil || k.IPAddress.String() != "10.211.1.50" {
		t.Errorf("tp-n1 after the failure = %+v, want the static address", k)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after the failure")
	}
}
