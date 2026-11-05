// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/regclient/regclient/types/errs"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// What a container runs, on either image store, and the verdict it gets (plan 12-06, IMG-05, D-02, D-03,
// D-20, RESEARCH Pitfalls 1 and 7).

const nodeA = "node-a"

// aggOf is an aggregate of one amd64 node holding these images and running these containers.
func aggOf(images []envs.ImageRow, containers ...envs.ContainerRow) envs.Aggregate {
	return envs.Aggregate{
		Nodes:      []envs.NodeRef{{ID: nodeA, Name: nodeA, Arch: "amd64"}},
		Images:     images,
		Containers: containers,
	}
}

func img(id string, digests ...string) envs.ImageRow {
	return envs.ImageRow{ImageRow: dockerx.ImageRow{ID: id, RepoDigests: digests}, NodeID: nodeA, Node: nodeA}
}

func ctr(id, image, imageID string, labels map[string]string) envs.ContainerRow {
	return envs.ContainerRow{
		ContainerRow: dockerx.ContainerRow{ID: id, Name: "c-" + id, State: "running", Image: image, ImageID: imageID, Labels: labels},
		NodeID:       nodeA, Node: nodeA,
	}
}

func workloadOf(t *testing.T, ws []updates.Workload, id string) updates.Workload {
	t.Helper()
	for _, w := range ws {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("no workload %q in %+v", id, ws)
	return updates.Workload{}
}

func TestContainerWorkloadsBothStores(t *testing.T) {
	t.Parallel()
	index, config := dg('a'), dg('c')
	agg := aggOf([]envs.ImageRow{
		// The containerd store: the ID is the index digest, and a pulled image lists it as its digest.
		img(index, "docker.io/library/nginx@"+index),
		// The containerd store, a build that was never pushed: its only digest is its own ID too.
		img(dg('b'), "myapp@"+dg('b')),
		// The classic store, a build: no repo digest at all.
		img(dg('d')),
		// The classic store, a pulled image: the ID is the config digest and the repo digest is another.
		img(config, "docker.io/library/redis@"+dg('e')),
		// An image known by a name of another repository than the container's.
		img(dg('f'), "ghcr.io/acme/other@"+dg('9')),
	},
		ctr("pulled", "nginx:1.27", index, nil),
		ctr("built-containerd", "myapp:1", dg('b'), nil),
		ctr("built-classic", "localbuild:1", dg('d'), nil),
		ctr("classic-pulled", "redis:7", config, nil),
		ctr("pinned", "redis@"+dg('e'), config, map[string]string{stacks.LabelStackImage: "redis@" + dg('e')}),
		ctr("task", "nginx:1.27@"+index, index, map[string]string{labelSvcID: "svc1", stacks.LabelSwarmNamespace: "shop"}),
		ctr("moved", dg('1'), config, nil),
		ctr("moved-known", dg('1'), config, nil),
		ctr("elsewhere", "docker.io/library/nginx:1.27", dg('f'), nil),
		ctr("agent", "ghcr.io/ianglennon/tierplan-agent:1", index, map[string]string{"tierplan.agent": "true"}),
	)
	text := func(id string) (string, bool) {
		if id == "moved-known" {
			return "docker.io/library/redis:7", true
		}
		return "", false
	}
	ws := updates.ContainerWorkloads(agg, text)

	if len(ws) != 9 {
		t.Fatalf("got %d workloads, want 9 (the agent is not one): %+v", len(ws), ws)
	}
	if got := workloadOf(t, ws, "pulled"); got.RunningDigest != index || got.LocalStore != updates.LocalContainerd || got.Ref.Tag != "1.27" || got.Task {
		t.Errorf("containerd pulled = %+v, want it to run the index digest and read as containerd", got)
	}
	if got := workloadOf(t, ws, "built-containerd"); got.RunningDigest != dg('b') || got.LocalStore != updates.LocalContainerd {
		t.Errorf("containerd build = %+v", got)
	}
	built := workloadOf(t, ws, "built-classic")
	if built.LocalStore != updates.LocalClassic || built.RunningDigest != "" || built.Checkable() {
		t.Errorf("classic build = %+v, want a local build nobody asks a registry about", built)
	}
	// The digest compared is the repo digest, never the image ID (RESEARCH Pitfall 1).
	if got := workloadOf(t, ws, "classic-pulled"); got.RunningDigest != dg('e') || got.RunningDigest == config || got.LocalStore != "" || !got.Checkable() {
		t.Errorf("classic pulled = %+v, want the repo digest %s and not the config digest", got, dg('e'))
	}
	if got := workloadOf(t, ws, "pinned"); !got.PinnedInFile() {
		t.Errorf("pinned = %+v, want pinned in the file", got)
	}
	task := workloadOf(t, ws, "task")
	if !task.Task || task.Stack != "shop" || task.Ref.Tag != "1.27" || task.Ref.Digest != "" || task.RunningDigest != index {
		t.Errorf("task = %+v, want a task container of stack shop running the index of the tag it follows", task)
	}
	if got := workloadOf(t, ws, "moved"); got.Ref.Tag != "" || got.Checkable() || got.Written != "" {
		t.Errorf("moved with no text = %+v, want no reference", got)
	}
	if got := workloadOf(t, ws, "moved-known"); got.Ref.Repo() != "docker.io/library/redis" || got.Ref.Tag != "7" || got.RunningDigest != dg('e') {
		t.Errorf("moved with a text = %+v, want redis:7 running %s", got, dg('e'))
	}
	// An image holding digests of other repositories only runs nothing that can be compared.
	if got := workloadOf(t, ws, "elsewhere"); got.RunningDigest != "" || got.LocalStore != "" {
		t.Errorf("elsewhere = %+v, want no digest to compare", got)
	}
	for _, w := range ws {
		if len(w.Platforms) != 1 || w.Platforms[0] != "linux/amd64" || len(w.NodeIDs) != 1 || w.NodeIDs[0] != nodeA {
			t.Errorf("%s: platforms %v nodes %v, want the node's own", w.ID, w.Platforms, w.NodeIDs)
		}
	}
}

const labelSvcID = "com.docker.swarm.service.id"

// A stack's container whose tag moved on its own host: the store no longer lists the image it runs, and the
// list shows only an ID, but the text it was created with still names the digest.
func TestContainerWhoseImageTheStoreDroppedKeepsItsDigest(t *testing.T) {
	t.Parallel()
	labels := map[string]string{stacks.LabelStackImage: "127.0.0.1:5000/probe:1"}
	agg := aggOf(nil,
		ctr("kept", dg('a'), dg('a'), labels),
		ctr("other-repo", dg('b'), dg('b'), labels),
	)
	texts := map[string]string{
		"kept":       "127.0.0.1:5000/probe:1@" + dg('a'),
		"other-repo": "127.0.0.1:5000/else:1@" + dg('b'),
	}
	ws := updates.ContainerWorkloads(agg, func(id string) (string, bool) { t, ok := texts[id]; return t, ok })
	if got := workloadOf(t, ws, "kept"); got.RunningDigest != dg('a') || !got.Checkable() {
		t.Errorf("kept = %+v, want the digest of the text it was created with", got)
	}
	if got := workloadOf(t, ws, "other-repo"); got.RunningDigest != "" {
		t.Errorf("other repo = %+v, want no digest from a text of another repository", got)
	}
}

func TestContainerVerdictsFromStoredChecks(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	old, now := dg('1'), dg('9')
	agg := aggOf([]envs.ImageRow{
		img(dg('c'), "docker.io/library/redis@"+old),
		img(dg('d')),
	},
		ctr("stale", "redis:7", dg('c'), nil),
		ctr("local", "build:1", dg('d'), nil),
		ctr("task", "redis:7@"+old, dg('c'), map[string]string{labelSvcID: "svc1"}),
	)
	r.heads.set("docker.io/library/redis:7", answer{digest: now})
	r.heads.set("docker.io/library/build:1", answer{err: unauthorized("denied")})
	ws := updates.ContainerWorkloads(agg, nil)
	for i := range ws {
		ws[i].EnvID = "env-1"
	}
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatalf("CheckEnv: %v", err)
	}
	if n := r.heads.requests(); n != 1 {
		t.Errorf("%d registry requests, want 1: the local build is not asked about and the task shares the container's tag", n)
	}
	r.clk.advance(time.Minute)
	j, err := r.svc.ContainerJudgements(ctx, "env-1", agg)
	if err != nil {
		t.Fatalf("ContainerJudgements: %v", err)
	}
	if v := j.Verdicts["stale"]; v.State != updates.StateUpdateAvailable || v.RunningDigest != old || v.RemoteDigest != now {
		t.Errorf("stale = %+v, want update_available", v)
	}
	if v := j.Verdicts["local"]; v.State != updates.StateNotFromRegistry {
		t.Errorf("local = %+v, want not_from_registry", v)
	}
	if v := j.Verdicts["task"]; v.State != updates.StateUpdateAvailable || !j.Workloads["task"].Task {
		t.Errorf("task = %+v (task %v), want the state of a plain container on that image", v, j.Workloads["task"].Task)
	}
	if v, err := r.svc.ContainerVerdicts(ctx, "env-1", agg); err != nil || v["stale"].State != updates.StateUpdateAvailable {
		t.Errorf("ContainerVerdicts = %+v, %v", v, err)
	}
}

func TestContainerdBuildReadsFromTheRegistryAnswer(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	agg := aggOf([]envs.ImageRow{
		// Both are on the containerd store: the ID is the digest, and the only repo digest is that ID.
		img(dg('a'), "docker.io/acme/pushed@"+dg('a')),
		img(dg('b'), "docker.io/acme/unpushed@"+dg('b')),
		img(dg('c'), "ghcr.io/acme/slow@"+dg('c')),
	},
		ctr("pushed", "acme/pushed:1", dg('a'), nil),
		ctr("unpushed", "acme/unpushed:1", dg('b'), nil),
		ctr("slow", "ghcr.io/acme/slow:1", dg('c'), nil),
	)
	r.heads.set("docker.io/acme/pushed:1", answer{digest: dg('a')})
	r.heads.set("docker.io/acme/unpushed:1", answer{err: fmt.Errorf("manifest unknown: %w", errs.ErrNotFound)})
	// On another registry, so the breaker its refusal opens is not the one the other two meet.
	r.heads.set("ghcr.io/acme/slow:1", answer{err: fmt.Errorf("too many requests: %w", errs.ErrHTTPRateLimit)})
	ws := updates.ContainerWorkloads(agg, nil)
	for i := range ws {
		ws[i].EnvID = "env-1"
	}
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatal(err)
	}
	r.clk.advance(time.Minute)
	got, err := r.svc.ContainerVerdicts(ctx, "env-1", agg)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["pushed"]; v.State != updates.StateUpToDate {
		t.Errorf("pushed = %+v, want up_to_date: the image the registry has is not a local build", v)
	}
	if v := got["unpushed"]; v.State != updates.StateNotFromRegistry {
		t.Errorf("unpushed = %+v, want not_from_registry", v)
	}
	if v := got["slow"]; v.State != updates.StateCheckFailed || v.Reason != updates.ReasonRateLimited {
		t.Errorf("slow = %+v, want check_failed rate_limited", v)
	}
}

// snap is a Snapshotter over a fixed aggregate.
type snap struct{ agg envs.Aggregate }

func (s snap) Snapshot(context.Context, string) (envs.Aggregate, envs.Health, error) {
	return s.agg, envs.Health{}, nil
}

func TestCountLeavesTasksToTheirService(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	old := dg('1')
	agg := aggOf([]envs.ImageRow{img(dg('c'), "docker.io/library/redis@"+old)},
		ctr("a", "redis:7", dg('c'), nil),
		ctr("b", "redis:7", dg('c'), nil),
		ctr("task", "redis:7@"+old, dg('c'), map[string]string{labelSvcID: "svc1"}),
	)
	r.heads.set("docker.io/library/redis:7", answer{digest: dg('9')})
	r.svc.Aggregator = snap{agg}
	ws := updates.ContainerWorkloads(agg, nil)
	for i := range ws {
		ws[i].EnvID = "env-1"
	}
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatal(err)
	}
	n, err := r.svc.Count(ctx, "env-1")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want 2: the two plain containers and not the task", n)
	}
}

func TestImageTextCacheIsBoundedAndRemembers(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	var mu sync.Mutex
	var asked [][]string
	clk := &clock{t: t0}
	cache := &updates.ImageTextCache{Now: clk.Now, Inspect: func(_ context.Context, _ string, ids []string) (map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, append([]string(nil), ids...))
		out := map[string]string{}
		for _, id := range ids {
			if id != "gone" {
				out[id] = "img-" + id
			}
		}
		return out, nil
	}}
	ids := make([]string, 120)
	for i := range ids {
		ids[i] = fmt.Sprintf("c%03d", i)
	}
	got := cache.Texts(ctx, "env-1", ids)
	if len(asked) != 1 || len(asked[0]) != updates.MaxImageInspects || len(got) != updates.MaxImageInspects {
		t.Fatalf("first read asked %v and answered %d, want one batch of %d", asked, len(got), updates.MaxImageInspects)
	}
	got = cache.Texts(ctx, "env-1", ids)
	if len(asked) != 2 || len(asked[1]) != updates.MaxImageInspects || len(got) != 2*updates.MaxImageInspects {
		t.Fatalf("second read asked %d and answered %d: the 50 known are not asked again", len(asked[1]), len(got))
	}
	if asked[1][0] != "c050" {
		t.Errorf("second batch starts at %s, want c050", asked[1][0])
	}
	cache.Texts(ctx, "env-1", ids)
	cache.Texts(ctx, "env-1", ids)
	if len(asked) != 3 || len(asked[2]) != 20 {
		t.Fatalf("after all are known the cache asked %d times (last %d ids), want 3 and 20", len(asked), len(asked[len(asked)-1]))
	}
	if got["c000"] != "img-c000" {
		t.Errorf("c000 = %q", got["c000"])
	}

	// A container that cannot be read is left alone for a while, then asked again.
	n := len(asked)
	cache.Texts(ctx, "env-2", []string{"gone"})
	cache.Texts(ctx, "env-2", []string{"gone"})
	if len(asked) != n+1 {
		t.Errorf("a container that could not be read was asked about again at once")
	}
	clk.advance(2 * time.Minute)
	cache.Texts(ctx, "env-2", []string{"gone"})
	if len(asked) != n+2 {
		t.Errorf("a container that could not be read was never asked about again")
	}
	// One that is not wanted any more is forgotten, so it is asked about when it returns.
	cache.Texts(ctx, "env-1", nil)
	before := len(asked)
	cache.Texts(ctx, "env-1", []string{"c000"})
	if len(asked) != before+1 {
		t.Errorf("a container that was dropped from the cache was not asked about again")
	}
}
