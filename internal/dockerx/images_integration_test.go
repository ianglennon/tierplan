// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build integration

package dockerx_test

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// TestImageRemoveAndUntag proves against the engine COAMING_TEST_ENGINE names (29, whose
// image store is containerd's, and 24, the classic one) that an image with several names is
// removed without force by untagging each name and then deleting it, that untagging one name
// keeps the image under the others, and that an image a container uses, started or not, is
// refused with a conflict and keeps all its names (plan 05-16, RES-09, D-28, D-29,
// RESEARCH Pitfall 4 and 5).
func TestImageRemoveAndUntag(t *testing.T) {
	host := dockertest.DockerHost(t)
	cli, err := dockerx.New(dockertest.TCPTransportFor(host))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	raw := dockertest.RawClient(t, host)
	pullImage(t, ctx, raw, "busybox:1")

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	repo := "bp-rm-" + suffix + "/app"
	tags := []string{repo + ":1", repo + ":2", repo + ":3"}
	// An image of the test's own, made by committing a container, so that removing every
	// name of it never touches busybox:1, which an engine that is not the test's may keep.
	seed := "bp-imgseed-" + suffix
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: seed, Config: &container.Config{Image: "busybox:1", Cmd: []string{"true"}},
	}); err != nil {
		t.Fatalf("create %s: %v", seed, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, seed, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := raw.ContainerCommit(ctx, seed, client.ContainerCommitOptions{Reference: tags[0], Comment: "coaming test " + suffix}); err != nil {
		t.Fatalf("commit %s: %v", seed, err)
	}
	if _, err := raw.ContainerRemove(ctx, seed, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove %s: %v", seed, err)
	}
	for _, tag := range tags[1:] {
		if _, err := raw.ImageTag(ctx, client.ImageTagOptions{Source: tags[0], Target: tag}); err != nil {
			t.Fatalf("tag %s: %v", tag, err)
		}
	}
	first, _, err := cli.ImageDetail(ctx, tags[0])
	if err != nil {
		t.Fatalf("ImageDetail: %v", err)
	}
	id := first.ID
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, tag := range tags {
			_, _ = raw.ImageRemove(c, tag, client.ImageRemoveOptions{})
		}
	})

	// The premise: by its ID an image with several names is refused, and force is not an
	// option Coaming has.
	if _, err := raw.ImageRemove(ctx, id, client.ImageRemoveOptions{}); !dockerx.IsConflict(err) {
		t.Fatalf("a delete by ID of an image with several names = %v, want a conflict (must be forced)", err)
	}

	// Untagging one name keeps the image under the rest.
	removed, err := cli.ImageUntag(ctx, tags[2])
	if err != nil || removed {
		t.Fatalf("ImageUntag(%s) = %v, %v, want the image kept", tags[2], removed, err)
	}
	d, _, err := cli.ImageDetail(ctx, id)
	if err != nil || slices.Contains(d.RepoTags, tags[2]) || !slices.Contains(d.RepoTags, tags[0]) {
		t.Fatalf("after the untag the image has %v (%v), want it without %s", d.RepoTags, err, tags[2])
	}
	// A name it no longer has is not found.
	if _, err := cli.ImageUntag(ctx, tags[2]); !dockerx.IsNotFound(err) {
		t.Fatalf("a second untag = %v, want not found", err)
	}

	// A container that was created and never started still uses the image: the remove is
	// refused as a conflict and the image keeps every name.
	ctr := "bp-imguser-" + suffix
	if _, err := raw.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: ctr, Config: &container.Config{Image: tags[0], Cmd: []string{"true"}},
	}); err != nil {
		t.Fatalf("create %s: %v", ctr, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = raw.ContainerRemove(c, ctr, client.ContainerRemoveOptions{Force: true})
	})
	users, err := cli.ImageUsers(ctx, id)
	if err != nil || len(users) != 1 || users[0] != ctr {
		t.Fatalf("ImageUsers = %v, %v, want just %s", users, err, ctr)
	}
	all := func() []string {
		d, _, err := cli.ImageDetail(ctx, id)
		if err != nil {
			t.Fatalf("ImageDetail: %v", err)
		}
		return append(slices.Clone(d.RepoTags), d.RepoDigests...)
	}
	before := all()
	if err := cli.ImageRemove(ctx, id, before); !dockerx.IsConflict(err) {
		t.Fatalf("ImageRemove of an image a container uses = %v, want a conflict", err)
	}
	if after := all(); !slices.Equal(before, after) {
		t.Fatalf("a refused remove changed the names: %v, now %v", before, after)
	}

	// With the container gone every name is taken off and the image is deleted.
	if _, err := raw.ContainerRemove(ctx, ctr, client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove %s: %v", ctr, err)
	}
	if err := cli.ImageRemove(ctx, id, all()); err != nil {
		t.Fatalf("ImageRemove: %v", err)
	}
	if _, _, err := cli.ImageDetail(ctx, id); !dockerx.IsNotFound(err) {
		t.Fatalf("ImageDetail after the remove = %v, want not found", err)
	}
	if err := cli.ImageRemove(ctx, id, nil); !dockerx.IsNotFound(err) {
		t.Fatalf("a second ImageRemove = %v, want not found", err)
	}
}
