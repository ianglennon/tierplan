// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
)

// Which digests are running (plan 05-19 task 2, IMG-03, D-22, Pitfall 5): matched by digest
// and the normalised repository, never by tag text.

func digestOf(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func aggOf(images []envs.ImageRow, containers []envs.ContainerRow) envs.Aggregate {
	return envs.Aggregate{Images: images, Containers: containers}
}

func TestRunningDigestsTakesTheRepoDigestsOfTheImageAContainerWasCreatedFromInTheShortForm(t *testing.T) {
	t.Parallel()
	d := digestOf("a")
	agg := aggOf(
		[]envs.ImageRow{{NodeID: "n1", ImageRow: dockerx.ImageRow{ID: digestOf("1"), RepoDigests: []string{"docker@sha256:" + strings.Repeat("a", 64)}}}},
		[]envs.ContainerRow{{NodeID: "n1", Node: "node-1", ContainerRow: dockerx.ContainerRow{ID: "c1", Name: "dind", State: "running", Image: "docker:27", ImageID: digestOf("1")}}},
	)
	got := runningDigests(agg)
	refs := got["docker.io/library/docker@"+d]
	if len(refs) != 1 || refs[0].Kind != "container" || refs[0].ID != "c1" || refs[0].Name != "dind" || refs[0].NodeID != "n1" || refs[0].Node != "node-1" {
		t.Fatalf("running = %+v, want the container under docker.io/library/docker@%s", got, d)
	}
	if len(got) != 1 {
		t.Errorf("keys = %v, want one", got)
	}
}

func TestRunningDigestsTakesTheDigestOfASwarmTaskContainerFromItsImageName(t *testing.T) {
	t.Parallel()
	p := digestOf("b")
	agg := aggOf(nil, []envs.ContainerRow{
		{NodeID: "n2", Node: "node-2", ContainerRow: dockerx.ContainerRow{ID: "t1", Name: "web.1.abc", State: "running", Image: "ghcr.io/acme/app:2.0@" + p, ImageID: digestOf("2")}},
	})
	got := runningDigests(agg)
	if refs := got["ghcr.io/acme/app@"+p]; len(refs) != 1 || refs[0].ID != "t1" {
		t.Fatalf("running = %+v, want the task container under ghcr.io/acme/app@%s", got, p)
	}
}

func TestRunningDigestsLeavesOutContainersThatAreNotRunningAndKeepsRepositoriesApart(t *testing.T) {
	t.Parallel()
	p := digestOf("c")
	agg := aggOf(nil, []envs.ContainerRow{
		{NodeID: "n1", ContainerRow: dockerx.ContainerRow{ID: "up", Name: "up", State: "running", Image: "ghcr.io/acme/app@" + p}},
		{NodeID: "n1", ContainerRow: dockerx.ContainerRow{ID: "old", Name: "old", State: "exited", Image: "ghcr.io/acme/app@" + p}},
		{NodeID: "n1", ContainerRow: dockerx.ContainerRow{ID: "made", Name: "made", State: "created", Image: "ghcr.io/acme/app@" + p}},
		{NodeID: "n1", ContainerRow: dockerx.ContainerRow{ID: "other", Name: "other", State: "running", Image: "ghcr.io/else/app@" + p}},
		{NodeID: "n1", ContainerRow: dockerx.ContainerRow{ID: "noref", Name: "noref", State: "running", Image: "ghcr.io/acme/app:latest"}},
	})
	got := runningDigests(agg)
	ids := func(key string) []string {
		var out []string
		for _, r := range got[key] {
			out = append(out, r.ID)
		}
		return out
	}
	if !slices.Equal(ids("ghcr.io/acme/app@"+p), []string{"up"}) || !slices.Equal(ids("ghcr.io/else/app@"+p), []string{"other"}) {
		t.Errorf("running = %+v, want only the running container of each repository", got)
	}
}

func TestRunningDigestsListsTheSameDigestOnTwoNodesAsTwoContainers(t *testing.T) {
	t.Parallel()
	p := digestOf("d")
	agg := aggOf(nil, []envs.ContainerRow{
		{NodeID: "n2", Node: "b", ContainerRow: dockerx.ContainerRow{ID: "x2", Name: "web.2", State: "running", Image: "nginx:1@" + p}},
		{NodeID: "n1", Node: "a", ContainerRow: dockerx.ContainerRow{ID: "x1", Name: "web.1", State: "running", Image: "nginx:1@" + p}},
	})
	refs := runningDigests(agg)["docker.io/library/nginx@"+p]
	if len(refs) != 2 || refs[0].Name != "web.1" || refs[1].Name != "web.2" {
		t.Errorf("refs = %+v, want both, by name", refs)
	}
}

func TestRunningTagsMatchByTheIndexDigestOrAnyPlatformDigest(t *testing.T) {
	t.Parallel()
	idx, plat := digestOf("e"), digestOf("f")
	running := map[string][]RunningRef{"docker.io/library/nginx@" + plat: {{ID: "c"}}}
	if !isRunning(running, "docker.io/library/nginx", idx, []string{plat}) {
		t.Error("a tag with a running platform digest is not running")
	}
	if isRunning(running, "docker.io/library/nginx", idx, []string{digestOf("0")}) {
		t.Error("a tag with no running digest is running")
	}
	if isRunning(running, "docker.io/library/redis", idx, []string{plat}) {
		t.Error("another repository's tag is running")
	}
}
